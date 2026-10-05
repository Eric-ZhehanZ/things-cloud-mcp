package main

// CalDAV writes. Every PUT is a three-way merge of the client's object
// against the device's base (what it last downloaded) and the current Things
// task; only the fields the client changed are written, and when both sides
// changed the same field Things wins. See thingsdav.MergeFields and
// docs/plans/2026-10-01-caldav-server-design.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"

	things "github.com/arthursoares/things-cloud-sdk"
	"github.com/arthursoares/things-cloud-sdk/thingsdav"
)

// davDB is the CalDAV side store; nil disables writes.
var davDB *davStore

// davTaskSource is the Things state CalDAV writes read from.
type davTaskSource interface {
	Task(uuid string) (*things.Task, error)
	AllTags() ([]*things.Tag, error)
}

// davState returns current Things state. Overridable in tests.
var davState = func() davTaskSource { return syncer.State() }

// davGeneration bumps on store changes that alter rendering without a
// Things write (sidecars, aliases), invalidating the view cache.
var davGeneration atomic.Int64

// davWriteMu serializes CalDAV writes so each merge sees fresh state.
var davWriteMu gosync.Mutex

const (
	davMoveWindow          = 10 * time.Minute
	davDeleteWindow        = time.Minute
	davMaxDeletesPerWindow = 20
)

func logDAV(format string, args ...any) { log.Printf("[DAV] "+format, args...) }

// ---------------------------------------------------------------------------
// Request context: device name and whether the response delivers content
// ---------------------------------------------------------------------------

type davCtxKey int

const (
	davDeviceKey davCtxKey = iota
	davDeliversKey
)

func davDevice(ctx context.Context) string {
	if d, ok := ctx.Value(davDeviceKey).(string); ok {
		return d
	}
	return "unknown"
}

func davDelivers(ctx context.Context) bool {
	d, _ := ctx.Value(davDeliversKey).(bool)
	return d
}

// davMarkDelivery flags requests whose responses carry calendar data — GET,
// and REPORT/PROPFIND bodies asking for calendar-data — so objects served by
// them become the device's merge base. ETag-only listings don't count: the
// device hasn't seen that content yet.
func davMarkDelivery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		delivers := r.Method == http.MethodGet
		if r.Method == "REPORT" || r.Method == "PROPFIND" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			delivers = bytes.Contains(body, []byte("calendar-data"))
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), davDeliversKey, delivers)))
	})
}

func davRecordServed(ctx context.Context, objs ...*thingsdav.Object) {
	if davDB == nil || !davDelivers(ctx) {
		return
	}
	if err := davDB.recordServed(davDevice(ctx), objs); err != nil {
		logDAV("record served: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Write guards: read-only mode and the mass-delete breaker
// ---------------------------------------------------------------------------

var davBreaker struct {
	mu      gosync.Mutex
	deletes []time.Time
	tripped bool
}

func davWritable() error {
	if os.Getenv("CALDAV_READ_ONLY") == "true" || davDB == nil {
		return errDAVReadOnly
	}
	davBreaker.mu.Lock()
	tripped := davBreaker.tripped
	davBreaker.mu.Unlock()
	if tripped {
		return webdav.NewHTTPError(http.StatusServiceUnavailable,
			fmt.Errorf("CalDAV writes paused after a burst of deletes; POST /api/dav/resume to re-enable"))
	}
	// Check the store before touching Things, so a write never lands in
	// Things without its merge base, alias or sidecar.
	if err := davDB.writable(); err != nil {
		return davStoreError(err)
	}
	return nil
}

// davStoreError turns a store outage into a 503 the client retries later.
func davStoreError(err error) error {
	if isStoreUnavailable(err) {
		return webdav.NewHTTPError(http.StatusServiceUnavailable,
			fmt.Errorf("CalDAV edits are briefly unavailable (the shared store has no leader); try again shortly: %v", err))
	}
	return err
}

// davFreshState syncs from Things Cloud before a merge, bypassing the read
// throttle: another node may have written moments ago, and merging against
// stale Things state would undo its change. The view cache keys on the
// synced index, so the next loadDAVView renders the fresh state.
func davFreshState() error {
	if err := forceSync(); err != nil {
		return webdav.NewHTTPError(http.StatusServiceUnavailable,
			fmt.Errorf("couldn't refresh from Things Cloud before applying the edit; try again shortly: %v", err))
	}
	return nil
}

// davAllowDelete counts a delete and trips the breaker past the limit.
func davAllowDelete(now time.Time) error {
	davBreaker.mu.Lock()
	defer davBreaker.mu.Unlock()
	cutoff := now.Add(-davDeleteWindow)
	kept := davBreaker.deletes[:0]
	for _, t := range davBreaker.deletes {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	davBreaker.deletes = kept
	if len(kept) >= davMaxDeletesPerWindow {
		davBreaker.tripped = true
		logDAV("breaker tripped: %d deletes within %s; writes paused", len(kept)+1, davDeleteWindow)
		return webdav.NewHTTPError(http.StatusServiceUnavailable,
			fmt.Errorf("too many deletes; CalDAV writes paused, POST /api/dav/resume to re-enable"))
	}
	davBreaker.deletes = append(davBreaker.deletes, now)
	return nil
}

func handleDAVResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		jsonError(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	davBreaker.mu.Lock()
	davBreaker.tripped = false
	davBreaker.deletes = nil
	davBreaker.mu.Unlock()
	logDAV("writes resumed")
	jsonResponse(w, map[string]string{"status": "resumed"})
}

// ---------------------------------------------------------------------------
// PUT / DELETE
// ---------------------------------------------------------------------------

func davBadRequest(format string, args ...any) error {
	return webdav.NewHTTPError(http.StatusBadRequest, fmt.Errorf(format, args...))
}

func davSomedayCategory() string { return os.Getenv("CALDAV_SOMEDAY_CATEGORY") }

func (davBackend) PutCalendarObject(ctx context.Context, p string, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	if err := davWritable(); err != nil {
		return nil, err
	}
	calID, name, ok := davSplitPath(p)
	if !ok || name == "" || strings.Contains(name, "/") {
		return nil, davNotFound("no calendar object at %s", p)
	}
	davWriteMu.Lock()
	defer davWriteMu.Unlock()
	if err := davFreshState(); err != nil {
		return nil, err
	}

	view, err := loadDAVView()
	if err != nil {
		return nil, err
	}
	c := view.Calendar(calID)
	if c == nil {
		return nil, davNotFound("calendar %s not found", calID)
	}
	existing := c.Object(name)
	if existing != nil && opts.IfNoneMatch.IsWildcard() {
		return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("%s already exists", name))
	}
	if existing == nil && opts.IfMatch.IsSet() {
		return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("%s does not exist", name))
	}
	device := davDevice(ctx)

	if c.Kind == thingsdav.KindEvent {
		if existing == nil {
			return nil, webdav.NewHTTPError(http.StatusForbidden, fmt.Errorf("deadlines come from Things tasks; create a task with a deadline instead"))
		}
		err = davPutDeadline(device, existing, cal, opts)
	} else {
		parsed, perr := thingsdav.ParseTodo(cal, davSomedayCategory(), thingsLocation())
		if perr != nil {
			return nil, davBadRequest("%v", perr)
		}
		if existing != nil {
			err = davUpdateTask(device, existing, parsed, opts)
		} else {
			err = davCreateOrMove(device, view, calID, name, parsed)
		}
	}
	if err != nil {
		if isInvalidInput(err) {
			return nil, davBadRequest("%v", err)
		}
		return nil, davStoreError(err)
	}
	// No ETag: the stored object differs from what the client sent (it is
	// re-rendered from Things), so the client must re-fetch it (RFC 4791 §5.3.4).
	return &caldav.CalendarObject{Path: p}, nil
}

func (davBackend) DeleteCalendarObject(ctx context.Context, p string) error {
	if err := davWritable(); err != nil {
		return err
	}
	calID, name, ok := davSplitPath(p)
	if !ok {
		return davNotFound("no calendar object at %s", p)
	}
	if name == "" {
		return webdav.NewHTTPError(http.StatusForbidden, fmt.Errorf("lists can't be deleted over CalDAV; delete the area or project in Things"))
	}
	davWriteMu.Lock()
	defer davWriteMu.Unlock()
	if err := davFreshState(); err != nil {
		return err
	}

	view, err := loadDAVView()
	if err != nil {
		return err
	}
	c := view.Calendar(calID)
	if c == nil {
		return davNotFound("calendar %s not found", calID)
	}
	o := c.Object(name)
	if o == nil {
		return davNotFound("%s not found", p)
	}
	device := davDevice(ctx)

	if c.Kind == thingsdav.KindEvent {
		// Deleting a deadline event clears the deadline; the task stays.
		if err := davWriteTask(o.TaskUUID, newTaskUpdate().clearDeadline()); err != nil {
			return err
		}
		davDB.logWrite(device, o.Key, "clear-deadline", nil)
		return nil
	}
	if err := davAllowDelete(time.Now()); err != nil {
		return err
	}
	if err := davWriteTask(o.TaskUUID, newTaskUpdate().trash(true)); err != nil {
		return err
	}
	if err := davDB.recordDelete(o.TaskUUID, o.Created, o.Title); err != nil {
		logDAV("record delete: %v", err)
	}
	davDB.logWrite(device, o.Key, "trash", map[string]string{"title": o.Title})
	return nil
}

// ---------------------------------------------------------------------------
// Task updates
// ---------------------------------------------------------------------------

// davBase returns the device's base for an object: its If-Match version
// when that is the current render, else what it last downloaded, else the
// current Things state (the client then wins every field it differs on).
func davBase(device string, o *thingsdav.Object, opts *caldav.PutCalendarObjectOptions, current any, dst any) error {
	if opts != nil && opts.IfMatch.IsSet() {
		if ok, _ := opts.IfMatch.MatchETag(o.ETag); ok {
			return jsonUnmarshalString(o.Snapshot, dst)
		}
	}
	found, err := davDB.snapshot(device, o.Key, dst)
	if err != nil {
		return err
	}
	if !found {
		logDAV("no base for %s on %s; client changes apply as-is", o.Key, device)
		return jsonUnmarshalString(mustJSON(current), dst)
	}
	return nil
}

func davUpdateTask(device string, o *thingsdav.Object, parsed *thingsdav.ParsedTodo, opts *caldav.PutCalendarObjectOptions) error {
	task, err := davState().Task(o.TaskUUID)
	if err != nil || task == nil {
		return davNotFound("task %s no longer exists", o.TaskUUID)
	}
	tags, err := davTagTitles()
	if err != nil {
		return err
	}
	current := thingsdav.FieldsFromTask(task, tags, davSomedayCategory(), time.Now())
	var base thingsdav.TaskFields
	if err := davBase(device, o, opts, current, &base); err != nil {
		return err
	}
	merged, conflicts := thingsdav.MergeFields(base, parsed.Fields, current)

	u := newTaskUpdate()
	if err := davApplyFields(u, task, current, merged, parsed.CompletedAt); err != nil {
		return err
	}
	if len(u.fields) > 1 { // more than md
		if err := davWriteTask(task.UUID, u); err != nil {
			return err
		}
	}
	davSaveClientState(device, task.UUID, parsed)
	davDB.logWrite(device, o.Key, "update", map[string]any{"conflicts": conflicts})
	for _, c := range conflicts {
		logDAV("conflict on %s.%s: client %q, Things %q — Things wins", task.UUID, c.Field, c.Client, c.Things)
	}
	return nil
}

// davSaveClientState stores the client's extras and records what the device
// now holds — what it sent — as its base for the next edit.
func davSaveClientState(device, taskUUID string, parsed *thingsdav.ParsedTodo) {
	ics, err := thingsdav.EncodeExtras(parsed.Extras)
	if err == nil {
		err = davDB.setSidecar(taskUUID, ics)
	}
	if err != nil {
		logDAV("save sidecar for %s: %v", taskUUID, err)
	}
	if err := davDB.setSnapshot(device, thingsdav.TaskKey(taskUUID), parsed.Fields); err != nil {
		logDAV("save snapshot for %s: %v", taskUUID, err)
	}
	davGeneration.Add(1)
}

// davApplyFields adds the changes from cur to want onto u.
func davApplyFields(u *taskUpdate, task *things.Task, cur, want thingsdav.TaskFields, completedAt *time.Time) error {
	if want.Title != cur.Title {
		u.title(want.Title)
	}
	if want.Notes != cur.Notes {
		if want.Notes == "" {
			u.clearNote()
		} else {
			u.note(want.Notes)
		}
	}
	if want.Status != cur.Status {
		switch want.Status {
		case thingsdav.StatusCompleted:
			ts := nowTs()
			if completedAt != nil {
				ts = float64(completedAt.UnixNano()) / 1e9
			}
			u.status(3).stopDate(ts)
		case thingsdav.StatusCanceled:
			u.status(2).stopDate(nowTs())
		default:
			u.status(0)
			u.fields["sp"] = nil
		}
	}
	scheduleChanged := want.When != cur.When || want.Someday != cur.Someday
	if scheduleChanged {
		switch {
		case want.When != "":
			d, err := time.Parse("2006-01-02", want.When)
			if err != nil {
				return invalidInputf("bad date %q", want.When)
			}
			ts := d.Unix()
			if ts <= todayMidnightIn(thingsLocation()) {
				u.schedule(1, ts, ts) // today or overdue: Today
			} else {
				u.schedule(2, ts, nil) // Upcoming
			}
		case want.Someday:
			u.schedule(2, nil, nil)
		default:
			u.schedule(1, nil, nil) // Anytime
		}
	}
	if scheduleChanged || want.Reminder != cur.Reminder {
		if want.Reminder == thingsdav.NoReminder {
			u.clearReminder()
		} else {
			u.reminder(want.Reminder)
		}
	}
	if want.Tags != cur.Tags {
		ids, err := davTagIDs(task, want.TagList())
		if err != nil {
			return err
		}
		u.tags(ids)
	}
	return nil
}

func davWriteTask(uuid string, u *taskUpdate) error {
	env := writeEnvelope{id: uuid, action: 1, kind: "Task6", payload: u.build()}
	if err := writeToHistory(env); err != nil {
		return err
	}
	syncAfterWrite()
	return nil
}

// ---------------------------------------------------------------------------
// Creates and list moves
// ---------------------------------------------------------------------------

// davContainer returns the project/area placement for a calendar.
func davContainer(calID string) (pr, ar []string, inbox bool, err error) {
	switch {
	case calID == thingsdav.InboxID:
		return []string{}, []string{}, true, nil
	case calID == thingsdav.NoProjectID:
		return []string{}, []string{}, false, nil
	case strings.HasPrefix(calID, "area-"):
		return []string{}, []string{strings.TrimPrefix(calID, "area-")}, false, nil
	case strings.HasPrefix(calID, "project-"):
		return []string{strings.TrimPrefix(calID, "project-")}, []string{}, false, nil
	}
	return nil, nil, false, davNotFound("calendar %s not found", calID)
}

// davFindMoveSource recognizes a list move: BusyCal moves a to-do by
// deleting it and creating a copy with a new UID but the same CREATED and
// title. The source is either just trashed over CalDAV, or (if the create
// arrives first) still live in another list.
func davFindMoveSource(view *thingsdav.View, calID string, parsed *thingsdav.ParsedTodo) (string, error) {
	if parsed.Created.IsZero() {
		return "", nil
	}
	uuid, err := davDB.takeRecentDelete(parsed.Created, parsed.Fields.Title, time.Now().Add(-davMoveWindow))
	if err != nil || uuid != "" {
		return uuid, err
	}
	for _, c := range view.Calendars {
		if c.ID == calID || c.Kind != thingsdav.KindTodo {
			continue
		}
		for _, o := range c.Objects {
			if o.Created.Equal(parsed.Created) && o.Title == parsed.Fields.Title {
				return o.TaskUUID, nil
			}
		}
	}
	return "", nil
}

func davCreateOrMove(device string, view *thingsdav.View, calID, name string, parsed *thingsdav.ParsedTodo) error {
	pr, ar, inbox, err := davContainer(calID)
	if err != nil {
		return err
	}
	if parsed.UID == "" {
		return davBadRequest("VTODO has no UID")
	}
	src, err := davFindMoveSource(view, calID, parsed)
	if err != nil {
		return err
	}
	if src != "" {
		return davMoveTask(device, src, calID, name, pr, ar, inbox, parsed)
	}
	return davCreateTask(device, calID, name, pr, ar, inbox, parsed)
}

func davMoveTask(device, uuid, calID, name string, pr, ar []string, inbox bool, parsed *thingsdav.ParsedTodo) error {
	task, err := davState().Task(uuid)
	if err != nil || task == nil {
		return davNotFound("moved task %s no longer exists", uuid)
	}
	tags, err := davTagTitles()
	if err != nil {
		return err
	}
	current := thingsdav.FieldsFromTask(task, tags, davSomedayCategory(), time.Now())
	var base thingsdav.TaskFields
	if found, err := davDB.snapshot(device, thingsdav.TaskKey(uuid), &base); err != nil {
		return err
	} else if !found {
		base = current
	}
	merged, conflicts := thingsdav.MergeFields(base, parsed.Fields, current)

	u := newTaskUpdate()
	u.fields["pr"], u.fields["ar"] = pr, ar
	u.clearHeading() // the heading belonged to the old project
	if task.InTrash {
		u.trash(false)
	}
	if err := davApplyFields(u, task, current, merged, parsed.CompletedAt); err != nil {
		return err
	}
	if _, set := u.fields["st"]; !set && merged.When == "" && !merged.Someday {
		// Undated tasks follow the list: Inbox in the Inbox, Anytime elsewhere.
		if inbox {
			u.schedule(0, nil, nil)
		} else if task.Schedule == things.TaskScheduleInbox {
			u.schedule(1, nil, nil)
		}
	}
	if err := davWriteTask(uuid, u); err != nil {
		return err
	}
	if err := davDB.setAlias(uuid, parsed.UID, name); err != nil {
		logDAV("save alias for %s: %v", uuid, err)
	}
	davSaveClientState(device, uuid, parsed)
	davDB.logWrite(device, thingsdav.TaskKey(uuid), "move", map[string]any{"to": calID, "conflicts": conflicts})
	return nil
}

func davCreateTask(device, calID, name string, pr, ar []string, inbox bool, parsed *thingsdav.ParsedTodo) error {
	f := parsed.Fields
	st := 1
	var sr, tir *int64
	switch {
	case f.When != "":
		d, err := time.Parse("2006-01-02", f.When)
		if err != nil {
			return invalidInputf("bad date %q", f.When)
		}
		ts := d.Unix()
		sr = &ts
		if ts <= todayMidnightIn(thingsLocation()) {
			tir = &ts
		} else {
			st = 2
		}
	case f.Someday:
		st = 2
	case inbox:
		st = 0
	}
	var ato *int
	if f.When != "" && f.Reminder != thingsdav.NoReminder {
		r := f.Reminder
		ato = &r
	}
	tg, err := davTagIDs(nil, f.TagList())
	if err != nil {
		return err
	}
	nt := emptyNote()
	if f.Notes != "" {
		nt = textNote(f.Notes)
	}
	ss := 0
	var sp *float64
	switch f.Status {
	case thingsdav.StatusCompleted:
		ss = 3
		ts := nowTs()
		if parsed.CompletedAt != nil {
			ts = float64(parsed.CompletedAt.UnixNano()) / 1e9
		}
		sp = &ts
	case thingsdav.StatusCanceled:
		ss = 2
		ts := nowTs()
		sp = &ts
	}

	uuid := generateUUID()
	payload := taskCreatePayload{
		Tp: 0, Sr: sr, Dds: nil, Rt: []string{}, Rmd: nil,
		Ss: ss, Tr: false, Dl: []string{}, Icp: false, St: st,
		Ar: ar, Tt: f.Title, Do: 0, Lai: nil, Tir: tir,
		Tg: tg, Agr: []string{}, Ix: 0, Cd: nowTs(), Lt: false,
		Icc: 0, Md: nil, Ti: 0, Dd: nil, Ato: ato, Nt: nt,
		Icsd: nil, Pr: pr, Rp: nil, Acrd: nil, Sp: sp,
		Sb: 0, Rr: nil, Xx: defaultExtension(),
	}
	if err := writeToHistory(writeEnvelope{id: uuid, action: 0, kind: "Task6", payload: payload}); err != nil {
		return err
	}
	syncAfterWrite()
	if err := davDB.setAlias(uuid, parsed.UID, name); err != nil {
		logDAV("save alias for %s: %v", uuid, err)
	}
	davSaveClientState(device, uuid, parsed)
	davDB.logWrite(device, thingsdav.TaskKey(uuid), "create", map[string]string{"calendar": calID, "title": f.Title})
	return nil
}

// ---------------------------------------------------------------------------
// Deadlines
// ---------------------------------------------------------------------------

func davPutDeadline(device string, o *thingsdav.Object, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) error {
	parsed, err := thingsdav.ParseDeadline(cal, thingsLocation())
	if err != nil {
		return davBadRequest("%v", err)
	}
	task, err := davState().Task(o.TaskUUID)
	if err != nil || task == nil {
		return davNotFound("task %s no longer exists", o.TaskUUID)
	}
	current := thingsdav.DeadlineFromTask(task)
	var base thingsdav.DeadlineFields
	if err := davBase(device, o, opts, current, &base); err != nil {
		return err
	}
	merged, conflicts := thingsdav.MergeDeadline(base, parsed.Fields, current)
	if merged != current {
		u := newTaskUpdate()
		if merged.Date == "" {
			u.clearDeadline()
		} else {
			d, err := time.Parse("2006-01-02", merged.Date)
			if err != nil {
				return invalidInputf("bad deadline %q", merged.Date)
			}
			u.deadline(d.Unix())
		}
		if err := davWriteTask(task.UUID, u); err != nil {
			return err
		}
	}
	if err := davDB.setSnapshot(device, o.Key, parsed.Fields); err != nil {
		logDAV("save snapshot for %s: %v", o.Key, err)
	}
	davDB.logWrite(device, o.Key, "deadline", map[string]any{"date": merged.Date, "conflicts": conflicts})
	return nil
}

// ---------------------------------------------------------------------------
// Tags
// ---------------------------------------------------------------------------

func davTagTitles() (map[string]string, error) {
	tags, err := davState().AllTags()
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		out[t.UUID] = t.Title
	}
	return out, nil
}

// davTagIDs resolves tag titles to UUIDs, creating tags Things doesn't have
// yet. Tags hidden from CalDAV (one titled like the Someday category) stay
// on the task.
func davTagIDs(task *things.Task, titles []string) ([]string, error) {
	byID, err := davTagTitles()
	if err != nil {
		return nil, err
	}
	byTitle := map[string]string{}
	for id, title := range byID {
		byTitle[title] = id
	}
	ids := []string{}
	for _, title := range titles {
		id, ok := byTitle[title]
		if !ok {
			if id, err = createTag(title, "", ""); err != nil {
				return nil, fmt.Errorf("create tag %q: %w", title, err)
			}
			byTitle[title] = id
		}
		ids = append(ids, id)
	}
	if task != nil {
		hidden := davSomedayCategory()
		if hidden == "" {
			hidden = thingsdav.DefaultSomedayCategory
		}
		for _, id := range task.TagIDs {
			if byID[id] == hidden {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func jsonUnmarshalString(s string, dst any) error { return json.Unmarshal([]byte(s), dst) }
