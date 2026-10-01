// Command caldav-spike is a throwaway CalDAV task server for discovering how
// BusyCal talks CalDAV. It serves two in-memory VTODO lists seeded with
// sample to-dos plus a VEVENT "Deadlines" calendar, accepts writes into memory only (it never touches Things),
// and records every HTTP exchange to a captures directory so the exact
// iCalendar BusyCal writes can be studied and turned into test fixtures.
//
//	go run ./cmd/caldav-spike -addr localhost:8085 -captures caldav-captures
//
// Connect BusyCal with an "Other CalDAV" account pointing at
// http://localhost:8085/ and the username/password from SPIKE_USER and
// SPIKE_PASSWORD (defaults: spike / spike).
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	gosync "sync"
	"sync/atomic"
	"time"

	"github.com/emersion/go-ical"
	"github.com/emersion/go-webdav"
	"github.com/emersion/go-webdav/caldav"
)

const (
	principalPath = "/spike/"
	homeSetPath   = "/spike/calendars/"
)

type object struct {
	data    []byte // raw iCalendar, served back exactly as stored
	etag    string
	modTime time.Time
}

type calendar struct {
	info    caldav.Calendar
	objects map[string]*object // keyed by object file name
}

type backend struct {
	mu        gosync.Mutex
	calendars map[string]*calendar // keyed by calendar path
}

func newBackend() *backend {
	b := &backend{calendars: map[string]*calendar{}}
	b.addCalendar("inbox", "Inbox", ical.CompToDo)
	b.addCalendar("project-demo", "Demo Area › Demo Project", ical.CompToDo)
	b.addCalendar("deadlines", "Deadlines", ical.CompEvent)
	for _, s := range seeds() {
		comp := ical.CompToDo
		if s.cal == "deadlines" {
			comp = ical.CompEvent
		}
		b.seed(s.cal, comp, s.uid, s.body)
	}
	return b
}

func (b *backend) addCalendar(id, name, comp string) {
	p := homeSetPath + id + "/"
	b.calendars[p] = &calendar{
		info: caldav.Calendar{
			Path:                  p,
			Name:                  name,
			SupportedComponentSet: []string{comp},
		},
		objects: map[string]*object{},
	}
}

func (b *backend) seed(calID, comp, uid, body string) {
	stamp := time.Now().UTC().Format("20060102T150405Z")
	raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:-//things-cloud-mcp//caldav-spike//EN\r\n" +
		"BEGIN:" + comp + "\r\nUID:" + uid + "\r\nDTSTAMP:" + stamp + "\r\n" +
		strings.ReplaceAll(strings.TrimSpace(body), "\n", "\r\n") + "\r\n" +
		"END:" + comp + "\r\nEND:VCALENDAR\r\n"
	if _, err := ical.NewDecoder(strings.NewReader(raw)).Decode(); err != nil {
		log.Fatalf("seed %s: %v", uid, err)
	}
	b.calendars[homeSetPath+calID+"/"].objects[uid+".ics"] = newObject([]byte(raw))
}

func newObject(data []byte) *object {
	sum := sha256.Sum256(data)
	return &object{data: data, etag: hex.EncodeToString(sum[:16]), modTime: time.Now()}
}

func splitObjectPath(p string) (calPath, name string) {
	return path.Dir(p) + "/", path.Base(p)
}

func notFound(format string, args ...any) error {
	return webdav.NewHTTPError(http.StatusNotFound, fmt.Errorf(format, args...))
}

func (b *backend) toCalendarObject(p string, o *object) (caldav.CalendarObject, error) {
	cal, err := ical.NewDecoder(bytes.NewReader(o.data)).Decode()
	if err != nil {
		return caldav.CalendarObject{}, err
	}
	return caldav.CalendarObject{
		Path:          p,
		ModTime:       o.modTime,
		ContentLength: int64(len(o.data)),
		ETag:          o.etag,
		Data:          cal,
	}, nil
}

func (b *backend) CurrentUserPrincipal(ctx context.Context) (string, error) {
	return principalPath, nil
}

func (b *backend) CalendarHomeSetPath(ctx context.Context) (string, error) {
	return homeSetPath, nil
}

func (b *backend) CreateCalendar(ctx context.Context, c *caldav.Calendar) error {
	return webdav.NewHTTPError(http.StatusForbidden, fmt.Errorf("spike: calendar creation disabled"))
}

func (b *backend) ListCalendars(ctx context.Context) ([]caldav.Calendar, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []caldav.Calendar
	for _, c := range b.calendars {
		out = append(out, c.info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func (b *backend) GetCalendar(ctx context.Context, p string) (*caldav.Calendar, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.calendars[strings.TrimSuffix(p, "/")+"/"]
	if !ok {
		return nil, notFound("calendar %s not found", p)
	}
	info := c.info
	return &info, nil
}

func (b *backend) GetCalendarObject(ctx context.Context, p string, req *caldav.CalendarCompRequest) (*caldav.CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	calPath, name := splitObjectPath(p)
	c, ok := b.calendars[calPath]
	if !ok {
		return nil, notFound("calendar %s not found", calPath)
	}
	o, ok := c.objects[name]
	if !ok {
		return nil, notFound("object %s not found", p)
	}
	co, err := b.toCalendarObject(p, o)
	if err != nil {
		return nil, err
	}
	return &co, nil
}

func (b *backend) ListCalendarObjects(ctx context.Context, p string, req *caldav.CalendarCompRequest) ([]caldav.CalendarObject, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	calPath := strings.TrimSuffix(p, "/") + "/"
	c, ok := b.calendars[calPath]
	if !ok {
		return nil, notFound("calendar %s not found", p)
	}
	names := make([]string, 0, len(c.objects))
	for name := range c.objects {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]caldav.CalendarObject, 0, len(names))
	for _, name := range names {
		co, err := b.toCalendarObject(calPath+name, c.objects[name])
		if err != nil {
			return nil, err
		}
		out = append(out, co)
	}
	return out, nil
}

func (b *backend) QueryCalendarObjects(ctx context.Context, p string, query *caldav.CalendarQuery) ([]caldav.CalendarObject, error) {
	all, err := b.ListCalendarObjects(ctx, p, &query.CompRequest)
	if err != nil {
		return nil, err
	}
	return caldav.Filter(query, all)
}

func (b *backend) PutCalendarObject(ctx context.Context, p string, cal *ical.Calendar, opts *caldav.PutCalendarObjectOptions) (*caldav.CalendarObject, error) {
	if _, _, err := caldav.ValidateCalendarObject(cal); err != nil {
		return nil, webdav.NewHTTPError(http.StatusBadRequest, err)
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return nil, err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	calPath, name := splitObjectPath(p)
	c, ok := b.calendars[calPath]
	if !ok {
		return nil, notFound("calendar %s not found", calPath)
	}
	existing := c.objects[name]
	if opts.IfNoneMatch.IsWildcard() && existing != nil {
		return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("object exists"))
	}
	if opts.IfMatch.IsSet() {
		if existing == nil {
			return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("object missing"))
		}
		if ok, err := opts.IfMatch.MatchETag(existing.etag); err != nil || !ok {
			return nil, webdav.NewHTTPError(http.StatusPreconditionFailed, fmt.Errorf("etag mismatch"))
		}
	}
	o := newObject(buf.Bytes())
	c.objects[name] = o
	co, err := b.toCalendarObject(p, o)
	if err != nil {
		return nil, err
	}
	return &co, nil
}

func (b *backend) DeleteCalendarObject(ctx context.Context, p string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	calPath, name := splitObjectPath(p)
	c, ok := b.calendars[calPath]
	if !ok || c.objects[name] == nil {
		return notFound("object %s not found", p)
	}
	delete(c.objects, name)
	return nil
}

// ---------------------------------------------------------------------------
// Seed data: one to-do per Things field the real server will map.
// ---------------------------------------------------------------------------

type seedTodo struct{ cal, uid, body string }

func seeds() []seedTodo {
	today := time.Now()
	day := func(offset int) string { return today.AddDate(0, 0, offset).Format("20060102") }
	alarm := time.Date(today.Year(), today.Month(), today.Day()+1, 9, 30, 0, 0, time.Local).UTC().Format("20060102T150405Z")
	done := today.AddDate(0, 0, -2).UTC().Format("20060102T150405Z")
	return []seedTodo{
		{"inbox", "spike-inbox-plain", "SUMMARY:Inbox task (no dates)\nSTATUS:NEEDS-ACTION"},
		{"inbox", "spike-inbox-notes", "SUMMARY:Task with notes\nDESCRIPTION:First line of notes\\nSecond line\nSTATUS:NEEDS-ACTION"},
		{"project-demo", "spike-when-today", "SUMMARY:When = today (DTSTART)\nDTSTART;VALUE=DATE:" + day(0) + "\nSTATUS:NEEDS-ACTION"},
		{"project-demo", "spike-deadline", "SUMMARY:Deadline in 5 days (DUE)\nDUE;VALUE=DATE:" + day(5) + "\nSTATUS:NEEDS-ACTION"},
		{"project-demo", "spike-when-and-deadline", "SUMMARY:When tomorrow + deadline in 7 days\nDTSTART;VALUE=DATE:" + day(1) + "\nDUE;VALUE=DATE:" + day(7) + "\nSTATUS:NEEDS-ACTION"},
		{"project-demo", "spike-reminder", "SUMMARY:When tomorrow + 09:30 reminder\nDTSTART;VALUE=DATE:" + day(1) + "\nSTATUS:NEEDS-ACTION\nBEGIN:VALARM\nACTION:DISPLAY\nDESCRIPTION:Reminder\nTRIGGER;VALUE=DATE-TIME:" + alarm + "\nEND:VALARM"},
		{"project-demo", "spike-tags", "SUMMARY:Tagged errand + home\nCATEGORIES:errand,home\nSTATUS:NEEDS-ACTION"},
		{"project-demo", "spike-someday", "SUMMARY:Someday task (no date)\nCATEGORIES:Someday\nSTATUS:NEEDS-ACTION"},
		{"project-demo", "spike-completed", "SUMMARY:Completed two days ago\nSTATUS:COMPLETED\nCOMPLETED:" + done + "\nPERCENT-COMPLETE:100"},
		{"project-demo", "spike-cancelled", "SUMMARY:Cancelled task\nSTATUS:CANCELLED"},
		// Deadlines calendar: one all-day event per task deadline, titled
		// exactly like the task, linking back to it.
		{"deadlines", "deadline-spike-deadline", "SUMMARY:Deadline in 5 days (DUE)\nDTSTART;VALUE=DATE:" + day(5) + "\nDTEND;VALUE=DATE:" + day(6) + "\nTRANSP:TRANSPARENT\nURL:things:///show?id=spike-deadline"},
		{"deadlines", "deadline-spike-when-and-deadline", "SUMMARY:When tomorrow + deadline in 7 days\nDTSTART;VALUE=DATE:" + day(7) + "\nDTEND;VALUE=DATE:" + day(8) + "\nTRANSP:TRANSPARENT\nURL:things:///show?id=spike-when-and-deadline"},
	}
}

// ---------------------------------------------------------------------------
// HTTP plumbing: Basic auth and full request/response capture.
// ---------------------------------------------------------------------------

func basicAuth(user, pass string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(user)) != 1 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(pass)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="caldav-spike"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type recorder struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (r *recorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	r.body.Write(p)
	return r.ResponseWriter.Write(p)
}

func capture(dir string, next http.Handler) http.Handler {
	var seq atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqBody, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(reqBody))
		rec := &recorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)

		n := seq.Add(1)
		log.Printf("#%04d %-8s %-60s depth=%-8s -> %d (%s)", n, r.Method, r.URL.RequestURI(), r.Header.Get("Depth"), rec.status, time.Since(start).Round(time.Millisecond))

		var out bytes.Buffer
		fmt.Fprintf(&out, "%s %s\n", r.Method, r.URL.RequestURI())
		for _, k := range sortedKeys(r.Header) {
			v := strings.Join(r.Header[k], ", ")
			if k == "Authorization" {
				v = "[redacted]"
			}
			fmt.Fprintf(&out, "%s: %s\n", k, v)
		}
		fmt.Fprintf(&out, "\n%s\n\n===== RESPONSE %d =====\n", reqBody, rec.status)
		for _, k := range sortedKeys(rec.Header()) {
			fmt.Fprintf(&out, "%s: %s\n", k, strings.Join(rec.Header()[k], ", "))
		}
		fmt.Fprintf(&out, "\n%s\n", rec.body.Bytes())
		name := fmt.Sprintf("%04d-%s-%d.txt", n, r.Method, rec.status)
		if err := os.WriteFile(filepath.Join(dir, name), out.Bytes(), 0o644); err != nil {
			log.Printf("capture write failed: %v", err)
		}
	})
}

func sortedKeys(h http.Header) []string {
	keys := make([]string, 0, len(h))
	for k := range h {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func main() {
	addr := flag.String("addr", "localhost:8085", "listen address")
	dir := flag.String("captures", "caldav-captures", "directory for captured HTTP exchanges")
	flag.Parse()

	user := envOr("SPIKE_USER", "spike")
	pass := envOr("SPIKE_PASSWORD", "spike")
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatal(err)
	}

	handler := &caldav.Handler{Backend: newBackend()}
	srv := capture(*dir, basicAuth(user, pass, handler))
	log.Printf("caldav-spike listening on http://%s/ (captures → %s)", *addr, *dir)
	log.Fatal(http.ListenAndServe(*addr, srv))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
