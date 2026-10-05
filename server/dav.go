package main

// CalDAV endpoint (/dav/, /.well-known/caldav): exposes Things as task lists
// plus a Deadlines event calendar for BusyCal. Rendering lives in the
// thingsdav package; this file wires it to go-webdav, Basic auth and the
// shared sync throttle. Writes live in dav_write.go.

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	gosync "sync"
	"time"
	_ "time/tzdata" // the Alpine image has no zoneinfo; BusyCal sends TZIDs

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	things "github.com/arthursoares/things-cloud-sdk"
	"github.com/arthursoares/things-cloud-sdk/sync"
	"github.com/arthursoares/things-cloud-sdk/thingsdav"
)

const (
	davPrefix        = "/dav"
	davPrincipalPath = davPrefix + "/me/"
	davHomeSetPath   = davPrefix + "/me/calendars/"

	defaultCompletedDays = 30
)

// davPassword is the shared CalDAV password: CALDAV_PASSWORD, else API_KEY.
// CalDAV stays disabled without one, since it exposes every task.
func davPassword() string {
	if p := os.Getenv("CALDAV_PASSWORD"); p != "" {
		return p
	}
	return os.Getenv("API_KEY")
}

// davCompletedDays reads CALDAV_COMPLETED_DAYS: how many days of completed
// tasks to list (0 = all). Invalid values fall back to the default.
func davCompletedDays() int {
	raw := os.Getenv("CALDAV_COMPLETED_DAYS")
	if raw == "" {
		return defaultCompletedDays
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultCompletedDays
	}
	return n
}

// davAuth requires HTTP Basic auth. Any non-empty username is accepted — it
// names the device (e.g. "mac", "iphone") for per-device merge bases.
func davAuth(password string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user == "" || subtle.ConstantTimeCompare([]byte(pass), []byte(password)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="Things CalDAV", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), davDeviceKey, strings.ToLower(user))))
	})
}

// ---------------------------------------------------------------------------
// View cache
// ---------------------------------------------------------------------------

// davViewKey identifies a rendered view: it changes when Things syncs new
// items, when aliases or sidecars change (on this node or, via the shared
// store, on another), when the UTC day rolls over (tir-based Today,
// completed window), or when the window setting changes.
type davViewKey struct {
	serverIndex   int
	generation    int64
	storeGen      int64
	utcDay        string
	completedDays int
}

type davViewCache struct {
	mu   gosync.Mutex
	key  davViewKey
	view *thingsdav.View
}

var davCache davViewCache

// loadDAVView returns the current rendered view, rebuilding it only when the
// Things state changed. Overridable in tests.
var loadDAVView = func() (*thingsdav.View, error) {
	_ = syncForRead() // stale state still renders; errors are logged there
	now := time.Now()
	key := davViewKey{
		serverIndex:   syncer.LastSyncedIndex(),
		generation:    davGeneration.Load(),
		storeGen:      davStoreGeneration(),
		utcDay:        now.UTC().Format("2006-01-02"),
		completedDays: davCompletedDays(),
	}
	davCache.mu.Lock()
	defer davCache.mu.Unlock()
	if davCache.view != nil && davCache.key == key {
		return davCache.view, nil
	}
	start := time.Now()
	in, err := davInput(syncer.State(), now, key.completedDays)
	if err != nil {
		return nil, err
	}
	view, err := thingsdav.Build(in)
	if err != nil {
		return nil, err
	}
	davCache.key, davCache.view = key, view
	n := 0
	for _, c := range view.Calendars {
		n += len(c.Objects)
	}
	log.Printf("[DAV] rendered %d calendars, %d objects at index %d in %s",
		len(view.Calendars), n, key.serverIndex, time.Since(start).Round(time.Millisecond))
	return view, nil
}

// davStoreGeneration is the shared store's alias/sidecar counter, so a
// change written through another node invalidates this node's render.
func davStoreGeneration() int64 {
	if davDB == nil {
		return 0
	}
	return davDB.generation()
}

// davInput gathers the Things state thingsdav.Build needs.
func davInput(st *sync.State, now time.Time, completedDays int) (thingsdav.Input, error) {
	in := thingsdav.Input{
		Now:             now,
		SomedayCategory: davSomedayCategory(),
		Location:        thingsLocation(),
	}
	if err := davStoreInput(&in); err != nil {
		return in, err
	}
	open, err := st.AllTasks(sync.QueryOpts{})
	if err != nil {
		return in, fmt.Errorf("load open tasks: %w", err)
	}
	var since *time.Time
	if completedDays > 0 {
		in.CompletedSince = now.AddDate(0, 0, -completedDays)
		since = &in.CompletedSince
	}
	done, err := st.CompletedTasksInRange(1<<30, since, nil)
	if err != nil {
		return in, fmt.Errorf("load completed tasks: %w", err)
	}
	in.Tasks = append(open, done...)
	if in.Projects, err = st.AllProjects(sync.QueryOpts{IncludeCompleted: true}); err != nil {
		return in, fmt.Errorf("load projects: %w", err)
	}
	if in.Areas, err = st.AllAreas(); err != nil {
		return in, fmt.Errorf("load areas: %w", err)
	}
	tags, err := st.AllTags()
	if err != nil {
		return in, fmt.Errorf("load tags: %w", err)
	}
	in.Tags = make(map[string]string, len(tags))
	for _, t := range tags {
		in.Tags[t.UUID] = t.Title
	}
	parents := map[string]*things.Task{}
	in.Lookup = func(uuid string) *things.Task {
		if t, ok := parents[uuid]; ok {
			return t
		}
		t, err := st.Task(uuid)
		if err != nil {
			t = nil
		}
		parents[uuid] = t
		return t
	}
	return in, nil
}

// davStoreInput wires the CalDAV store's aliases and sidecars into a
// render.
func davStoreInput(in *thingsdav.Input) error {
	if davDB == nil {
		return nil
	}
	aliases, err := davDB.aliases()
	if err != nil {
		return fmt.Errorf("load aliases: %w", err)
	}
	sidecars, err := davDB.sidecars()
	if err != nil {
		return fmt.Errorf("load sidecars: %w", err)
	}
	in.Alias = func(uuid string) (string, string, bool) {
		a, ok := aliases[uuid]
		return a.uid, a.name, ok
	}
	in.Extras = func(uuid string) *ical.Calendar {
		raw, ok := sidecars[uuid]
		if !ok {
			return nil
		}
		cal, err := thingsdav.DecodeExtras(raw)
		if err != nil {
			logDAV("bad sidecar for %s: %v", uuid, err)
			return nil
		}
		return cal
	}
	return nil
}

// ---------------------------------------------------------------------------
// go-webdav backend
// ---------------------------------------------------------------------------

type davBackend struct{}

var errDAVReadOnly = webdav.NewHTTPError(http.StatusForbidden, fmt.Errorf("Things CalDAV is read-only"))

func davNotFound(format string, args ...any) error {
	return webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf(format, args...))
}

func davCalendarPath(id string) string { return davHomeSetPath + id + "/" }

// davSplitPath parses "/dav/me/calendars/<id>/[<name>]".
func davSplitPath(p string) (calID, name string, ok bool) {
	rest, found := strings.CutPrefix(p, davHomeSetPath)
	if !found {
		return "", "", false
	}
	calID, name, _ = strings.Cut(rest, "/")
	return calID, name, calID != ""
}

func toDAVCalendar(c *thingsdav.Calendar) caldav.Calendar {
	return caldav.Calendar{
		Path:                  davCalendarPath(c.ID),
		Name:                  c.Name,
		SupportedComponentSet: []string{string(c.Kind)},
	}
}

func toDAVObject(calID string, o *thingsdav.Object) caldav.CalendarObject {
	return caldav.CalendarObject{
		Path:          davCalendarPath(calID) + o.Name,
		ModTime:       o.ModTime,
		ContentLength: int64(len(o.Raw)),
		ETag:          o.ETag,
		Data:          o.Data,
	}
}

func (davBackend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return davPrincipalPath, nil
}

func (davBackend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return davHomeSetPath, nil
}

func (davBackend) CreateCalendar(ctx context.Context, c *caldav.Calendar) error {
	return errDAVReadOnly
}

func (davBackend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	view, err := loadDAVView()
	if err != nil {
		return nil, err
	}
	out := make([]caldav.Calendar, 0, len(view.Calendars))
	for _, c := range view.Calendars {
		out = append(out, toDAVCalendar(c))
	}
	return out, nil
}

func (davBackend) GetCalendar(ctx context.Context, p string) (*caldav.Calendar, error) {
	calID, _, ok := davSplitPath(p)
	if !ok {
		return nil, davNotFound("calendar %s not found", p)
	}
	view, err := loadDAVView()
	if err != nil {
		return nil, err
	}
	c := view.Calendar(calID)
	if c == nil {
		return nil, davNotFound("calendar %s not found", p)
	}
	cal := toDAVCalendar(c)
	return &cal, nil
}

func (davBackend) GetCalendarObject(ctx context.Context, p string, req *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	calID, name, ok := davSplitPath(p)
	if !ok || name == "" {
		return nil, davNotFound("object %s not found", p)
	}
	view, err := loadDAVView()
	if err != nil {
		return nil, err
	}
	c := view.Calendar(calID)
	if c == nil {
		return nil, davNotFound("calendar %s not found", calID)
	}
	o := c.Object(name)
	if o == nil {
		return nil, davNotFound("object %s not found", p)
	}
	davRecordServed(ctx, o)
	co := toDAVObject(calID, o)
	return &co, nil
}

// davCalendarObjects returns the rendered objects of the calendar at p.
func davCalendarObjects(p string) (string, []*thingsdav.Object, error) {
	calID, _, ok := davSplitPath(p)
	if !ok {
		return "", nil, davNotFound("calendar %s not found", p)
	}
	view, err := loadDAVView()
	if err != nil {
		return "", nil, err
	}
	c := view.Calendar(calID)
	if c == nil {
		return "", nil, davNotFound("calendar %s not found", p)
	}
	return calID, c.Objects, nil
}

func (davBackend) ListCalendarObjects(ctx context.Context, p string, req *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	calID, objs, err := davCalendarObjects(p)
	if err != nil {
		return nil, err
	}
	davRecordServed(ctx, objs...)
	out := make([]caldav.CalendarObject, 0, len(objs))
	for _, o := range objs {
		out = append(out, toDAVObject(calID, o))
	}
	return out, nil
}

func (davBackend) QueryCalendarObjects(ctx context.Context, p string, query *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	calID, objs, err := davCalendarObjects(p)
	if err != nil {
		return nil, err
	}
	all := make([]caldav.CalendarObject, 0, len(objs))
	byPath := make(map[string]*thingsdav.Object, len(objs))
	for _, o := range objs {
		co := toDAVObject(calID, o)
		all = append(all, co)
		byPath[co.Path] = o
	}
	matched, err := caldav.Filter(query, all)
	if err != nil {
		return nil, err
	}
	served := make([]*thingsdav.Object, 0, len(matched))
	for _, co := range matched {
		served = append(served, byPath[co.Path])
	}
	davRecordServed(ctx, served...)
	return matched, nil
}

// newDAVHandler returns the authenticated CalDAV handler, or nil when no
// password is configured.
func newDAVHandler() http.Handler {
	password := davPassword()
	if password == "" {
		return nil
	}
	h := &caldav.Handler{Backend: davBackend{}, Prefix: davPrefix}
	return davAuth(password, limitRequestBody(maxJSONBodyBytes, davMarkDelivery(davCalendarProps(h))))
}
