package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	things "github.com/arthursoares/things-cloud-sdk"
	"github.com/arthursoares/things-cloud-sdk/thingsdav"
)

// fakeDAV drives the full CalDAV stack against an in-memory Things state and
// records every Things write instead of sending it.
type fakeDAV struct {
	t      *testing.T
	tasks  map[string]*things.Task
	areas  []*things.Area
	tags   []*things.Tag
	writes []writeEnvelope
	srv    *httptest.Server
}

func (f *fakeDAV) Task(uuid string) (*things.Task, error) { return f.tasks[uuid], nil }
func (f *fakeDAV) AllTags() ([]*things.Tag, error)        { return f.tags, nil }

func (f *fakeDAV) add(t *things.Task) *things.Task {
	if t.CreationDate.IsZero() {
		t.CreationDate = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	}
	if t.Type == things.TaskTypeTask && t.Schedule == 0 && len(t.ParentTaskIDs) > 0 {
		t.Schedule = things.TaskScheduleAnytime
	}
	f.tasks[t.UUID] = t
	return t
}

func newFakeDAV(t *testing.T) *fakeDAV {
	t.Helper()
	f := &fakeDAV{t: t, tasks: map[string]*things.Task{}}
	f.add(&things.Task{UUID: "Proj1", Title: "Launch", Type: things.TaskTypeProject, AreaIDs: []string{"Area1"}})
	f.areas = []*things.Area{{UUID: "Area1", Title: "Work"}}
	f.tags = []*things.Tag{{UUID: "TagHome", Title: "home"}}

	t.Setenv("CALDAV_PASSWORD", testDAVPassword)
	t.Setenv("THINGS_TIMEZONE", "America/New_York")
	t.Setenv("CALDAV_READ_ONLY", "")

	store := newTestDAVStore(t)
	origDB, origState, origView, origWrite, origSync := davDB, davState, loadDAVView, writeToHistory, doSync
	davDB = store
	davState = func() davTaskSource { return f }
	loadDAVView = f.view
	writeToHistory = func(env writeEnvelope) error { f.writes = append(f.writes, env); return nil }
	doSync = func() error { return nil }
	davBreaker.tripped, davBreaker.deletes = false, nil
	t.Cleanup(func() {
		store.Close()
		davDB, davState, loadDAVView, writeToHistory, doSync = origDB, origState, origView, origWrite, origSync
		davBreaker.tripped, davBreaker.deletes = false, nil
	})

	mux := http.NewServeMux()
	h := newDAVHandler()
	mux.Handle(davPrefix+"/", h)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDAV) view() (*thingsdav.View, error) {
	in := thingsdav.Input{
		Areas:    f.areas,
		Tags:     map[string]string{},
		Lookup:   func(id string) *things.Task { return f.tasks[id] },
		Now:      time.Now(),
		Location: thingsLocation(),
	}
	for _, tg := range f.tags {
		in.Tags[tg.UUID] = tg.Title
	}
	for _, t := range f.tasks {
		switch t.Type {
		case things.TaskTypeProject:
			in.Projects = append(in.Projects, t)
		case things.TaskTypeTask:
			in.Tasks = append(in.Tasks, t)
		}
	}
	if err := davStoreInput(&in); err != nil {
		return nil, err
	}
	return thingsdav.Build(in)
}

func (f *fakeDAV) do(method, device, path, body string, header map[string]string) (int, string) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, strings.NewReader(body))
	req.SetBasicAuth(device, testDAVPassword)
	if body != "" {
		req.Header.Set("Content-Type", "text/calendar; charset=utf-8")
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := f.srv.Client().Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func (f *fakeDAV) get(device, path string) string {
	f.t.Helper()
	code, body := f.do(http.MethodGet, device, path, "", nil)
	if code != http.StatusOK {
		f.t.Fatalf("GET %s: %d %s", path, code, body)
	}
	return body
}

func (f *fakeDAV) put(device, path, body string, header map[string]string) {
	f.t.Helper()
	if code, resp := f.do(http.MethodPut, device, path, body, header); code/100 != 2 {
		f.t.Fatalf("PUT %s: %d %s", path, code, resp)
	}
}

// fields returns the update payload of the only write, failing otherwise.
func (f *fakeDAV) onlyUpdate() map[string]any {
	f.t.Helper()
	if len(f.writes) != 1 {
		f.t.Fatalf("want 1 Things write, got %d: %+v", len(f.writes), f.writes)
	}
	m, ok := f.writes[0].payload.(map[string]any)
	if !ok || f.writes[0].action != 1 {
		f.t.Fatalf("want an update, got %+v", f.writes[0])
	}
	return m
}

func replaceLine(ics, prefix, line string) string {
	var out []string
	for _, l := range strings.Split(ics, "\r\n") {
		if strings.HasPrefix(l, prefix) {
			if line != "" {
				out = append(out, line)
			}
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\r\n")
}

func addLine(ics, line string) string {
	return strings.Replace(ics, "END:VTODO", line+"\r\nEND:VTODO", 1)
}

func inboxTask(f *fakeDAV, uuid, title string) string {
	f.add(&things.Task{UUID: uuid, Title: title, Schedule: things.TaskScheduleInbox})
	return davCalendarPath(thingsdav.InboxID) + uuid + ".ics"
}

func TestDAVPutAppliesClientChange(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "old title")
	body := f.get("mac", path)
	f.put("mac", path, replaceLine(body, "SUMMARY:", "SUMMARY:new title"), nil)

	u := f.onlyUpdate()
	if u["tt"] != "new title" || len(u) != 2 { // tt + md
		t.Errorf("update = %+v, want only the title", u)
	}
}

func TestDAVPutStaleDeviceKeepsThingsChange(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "original")
	body := f.get("mac", path)
	f.tasks["T1"].Title = "renamed in Things" // after the Mac downloaded it

	// The Mac sets a date on its stale copy, which still has the old title.
	f.put("mac", path, addLine(addLine(body, "DUE;VALUE=DATE:20300105"), "DTSTART;VALUE=DATE:20300105"), nil)
	u := f.onlyUpdate()
	if _, ok := u["tt"]; ok {
		t.Errorf("stale title must not overwrite Things: %+v", u)
	}
	if u["st"] != 2 || u["sr"] != time.Date(2030, 1, 5, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("date not applied as Upcoming: %+v", u)
	}
}

func TestDAVPutConflictThingsWins(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "original")
	body := f.get("mac", path)
	f.tasks["T1"].Title = "things"
	f.put("mac", path, replaceLine(body, "SUMMARY:", "SUMMARY:client"), nil)
	if len(f.writes) != 0 {
		t.Errorf("conflicting title must not be written: %+v", f.writes)
	}
	rows, _ := davDB.sql.query(false, stmt("", `SELECT COUNT(*) FROM write_log WHERE detail LIKE '%"field":"title"%'`))
	if len(rows) != 1 || sqlInt(rows[0][0]) != 1 {
		t.Errorf("conflict not logged")
	}
}

func TestDAVPutWithoutBaseClientWins(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "original")
	body := f.get("mac", path)
	// The iPhone never downloaded it (no base): its changes apply as-is.
	f.put("iphone", path, replaceLine(body, "SUMMARY:", "SUMMARY:from iphone"), nil)
	if u := f.onlyUpdate(); u["tt"] != "from iphone" {
		t.Errorf("update = %+v", u)
	}
}

func TestDAVCompleteAndReopen(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "do it")
	body := f.get("mac", path)
	done := addLine(replaceLine(body, "STATUS:", "STATUS:COMPLETED"), "COMPLETED:20261001T113339Z")
	f.put("mac", path, done, nil)
	u := f.onlyUpdate()
	if u["ss"] != 3 || u["sp"] != float64(time.Date(2026, 10, 1, 11, 33, 39, 0, time.UTC).Unix()) {
		t.Errorf("completion = %+v", u)
	}

	f.writes = nil
	f.tasks["T1"].Status = things.TaskStatusCompleted
	// BusyCal reopens by dropping STATUS and COMPLETED entirely.
	f.put("mac", path, replaceLine(replaceLine(done, "STATUS:", ""), "COMPLETED:", ""), nil)
	if u := f.onlyUpdate(); u["ss"] != 0 || u["sp"] != nil {
		t.Errorf("reopen = %+v", u)
	}
}

func TestDAVBusyCalOnlyChangeKeepsThingsUntouched(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "with duration")
	body := f.get("mac", path)
	f.put("mac", path, addLine(addLine(body, "X-BUSYMAC-TASK-DURATION;SHARE-SCOPE=GLOBAL:3600"), "PRIORITY:5"), nil)
	if len(f.writes) != 0 {
		t.Errorf("BusyCal-only fields must not write to Things: %+v", f.writes)
	}
	got := f.get("iphone", path)
	for _, want := range []string{"X-BUSYMAC-TASK-DURATION;SHARE-SCOPE=GLOBAL:3600", "PRIORITY:5"} {
		if !strings.Contains(got, want) {
			t.Errorf("sidecar %q not served back:\n%s", want, got)
		}
	}
}

func TestDAVCreateInProjectWithNewTag(t *testing.T) {
	f := newFakeDAV(t)
	path := davCalendarPath("project-Proj1") + "CLIENT-1.ics"
	body := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:BusyCal\r\nBEGIN:VTODO\r\nUID:CLIENT-1\r\nDTSTAMP:20261001T112938Z\r\n" +
		"CREATED:20261001T112935Z\r\nSUMMARY:Try something new\r\nDUE;VALUE=DATE:20300105\r\nCATEGORIES:home,fresh\r\n" +
		"END:VTODO\r\nEND:VCALENDAR\r\n"
	f.put("mac", path, body, map[string]string{"If-None-Match": "*"})

	if len(f.writes) != 2 || f.writes[0].kind != "Tag4" {
		t.Fatalf("want a new tag then the task, got %+v", f.writes)
	}
	tagID := f.writes[0].id
	create := f.writes[1]
	p, ok := create.payload.(taskCreatePayload)
	if !ok || create.action != 0 {
		t.Fatalf("want a create, got %+v", create)
	}
	if p.Tt != "Try something new" || len(p.Pr) != 1 || p.Pr[0] != "Proj1" || p.St != 2 ||
		p.Sr == nil || *p.Sr != time.Date(2030, 1, 5, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("create payload = %+v", p)
	}
	if len(p.Tg) != 2 || p.Tg[0] != tagID || p.Tg[1] != "TagHome" { // by title: fresh, home
		t.Errorf("tags = %v", p.Tg)
	}

	// Once Things has it, the client keeps seeing its own UID and file name.
	f.tags = append(f.tags, &things.Tag{UUID: tagID, Title: "fresh"})
	f.add(&things.Task{UUID: create.id, Title: p.Tt, ParentTaskIDs: []string{"Proj1"}, Schedule: things.TaskScheduleSomeday})
	if got := f.get("mac", path); !strings.Contains(got, "UID:CLIENT-1\r\n") {
		t.Errorf("client UID not preserved:\n%s", got)
	}
}

func TestDAVMoveBetweenLists(t *testing.T) {
	f := newFakeDAV(t)
	from := inboxTask(f, "T1", "move me")
	f.tasks["T1"].DeadlineDate = ptrTime(time.Date(2030, 3, 1, 0, 0, 0, 0, time.UTC))
	body := f.get("mac", from)

	if code, _ := f.do(http.MethodDelete, "mac", from, "", nil); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", code)
	}
	if u := f.onlyUpdate(); u["tr"] != true {
		t.Fatalf("delete should trash: %+v", u)
	}
	f.writes = nil
	f.tasks["T1"].InTrash = true

	// BusyCal re-creates it in the project with a new UID, same CREATED + title.
	moved := replaceLine(body, "UID:", "UID:NEW-UID")
	to := davCalendarPath("project-Proj1") + "NEW-UID.ics"
	f.put("mac", to, moved, map[string]string{"If-None-Match": "*"})

	if len(f.writes) != 1 || f.writes[0].id != "T1" || f.writes[0].action != 1 {
		t.Fatalf("move must update the original task, got %+v", f.writes)
	}
	u := f.writes[0].payload.(map[string]any)
	if u["tr"] != false || len(u["pr"].([]string)) != 1 || u["pr"].([]string)[0] != "Proj1" {
		t.Errorf("move update = %+v", u)
	}
	if _, ok := u["dd"]; ok {
		t.Error("a move must keep the Things deadline")
	}
}

func TestDAVDeadlineDragAndDelete(t *testing.T) {
	f := newFakeDAV(t)
	inboxTask(f, "T1", "file taxes")
	f.tasks["T1"].DeadlineDate = ptrTime(time.Date(2030, 2, 1, 0, 0, 0, 0, time.UTC))
	path := davCalendarPath(thingsdav.DeadlinesID) + "deadline-T1.ics"
	body := f.get("mac", path)

	dragged := replaceLine(replaceLine(body, "DTSTART;", "DTSTART;VALUE=DATE:20300203"), "DTEND;", "DTEND;VALUE=DATE:20300204")
	f.put("mac", path, dragged, nil)
	if u := f.onlyUpdate(); u["dd"] != time.Date(2030, 2, 3, 0, 0, 0, 0, time.UTC).Unix() {
		t.Errorf("drag = %+v", u)
	}

	f.writes = nil
	if code, _ := f.do(http.MethodDelete, "mac", path, "", nil); code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", code)
	}
	if u := f.onlyUpdate(); u["dd"] != nil || u["tr"] != nil {
		t.Errorf("deleting a deadline event must only clear the deadline: %+v", u)
	}

	code, _ := f.do(http.MethodPut, "mac", davCalendarPath(thingsdav.DeadlinesID)+"new.ics", body, map[string]string{"If-None-Match": "*"})
	if code != http.StatusForbidden {
		t.Errorf("creating a deadline event = %d, want 403", code)
	}
}

func TestDAVDeleteBreaker(t *testing.T) {
	f := newFakeDAV(t)
	var paths []string
	for i := 0; i < davMaxDeletesPerWindow+1; i++ {
		paths = append(paths, inboxTask(f, "T"+string(rune('A'+i)), "task"))
	}
	for i, p := range paths {
		code, _ := f.do(http.MethodDelete, "mac", p, "", nil)
		if i < davMaxDeletesPerWindow && code != http.StatusNoContent {
			t.Fatalf("delete %d = %d", i, code)
		}
		if i == davMaxDeletesPerWindow && code != http.StatusServiceUnavailable {
			t.Fatalf("delete past the limit = %d, want 503", code)
		}
	}
	if code, _ := f.do(http.MethodPut, "mac", paths[0], f.get("mac", paths[0]), nil); code != http.StatusServiceUnavailable {
		t.Errorf("writes must stay paused, got %d", code)
	}
	rec := httptest.NewRecorder()
	handleDAVResume(rec, httptest.NewRequest(http.MethodPost, "/api/dav/resume", nil))
	if rec.Code != http.StatusOK || davWritable() != nil {
		t.Errorf("resume failed: %d", rec.Code)
	}
}

func TestDAVReadOnlyMode(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "x")
	body := f.get("mac", path)
	t.Setenv("CALDAV_READ_ONLY", "true")
	if code, _ := f.do(http.MethodPut, "mac", path, body, nil); code != http.StatusForbidden {
		t.Errorf("PUT in read-only mode = %d", code)
	}
	if len(f.writes) != 0 {
		t.Errorf("read-only mode wrote: %+v", f.writes)
	}
}

// ETag-only listings must not count as downloads: the device hasn't seen
// that content, so it can't be the base of its next edit.
func TestDAVListingDoesNotRecordBase(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "v1")
	body := f.get("mac", path) // the Mac holds v1
	f.tasks["T1"].Title = "v2"

	listing := `<?xml version="1.0"?><d:propfind xmlns:d="DAV:"><d:prop><d:getetag/></d:prop></d:propfind>`
	f.do("PROPFIND", "mac", davCalendarPath(thingsdav.InboxID), listing, map[string]string{"Depth": "1", "Content-Type": "application/xml"})

	f.put("mac", path, replaceLine(body, "SUMMARY:", "SUMMARY:mac edit"), nil)
	if len(f.writes) != 0 {
		t.Errorf("base must still be v1, so this is a conflict Things wins: %+v", f.writes)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestDAVListDeleteRefused(t *testing.T) {
	f := newFakeDAV(t)
	inboxTask(f, "T1", "x")
	if code, _ := f.do(http.MethodDelete, "mac", davCalendarPath(thingsdav.InboxID), "", nil); code != http.StatusForbidden {
		t.Errorf("DELETE of a whole list = %d, want 403", code)
	}
	if len(f.writes) != 0 {
		t.Errorf("list delete wrote: %+v", f.writes)
	}
}

func TestDAVStorePath(t *testing.T) {
	t.Setenv("DAV_DB_PATH", "")
	if got := davStorePath("/data/things.db"); got != "/data/things-dav.db" {
		t.Errorf("davStorePath = %s", got)
	}
	t.Setenv("DAV_DB_PATH", "/tmp/x.db")
	if got := davStorePath("/data/things.db"); got != "/tmp/x.db" {
		t.Errorf("DAV_DB_PATH override = %s", got)
	}
}
