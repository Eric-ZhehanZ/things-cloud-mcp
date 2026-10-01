package thingsdav

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-ical"

	things "github.com/arthursoares/things-cloud-sdk"
)

var newYork = mustLoad("America/New_York")

func mustLoad(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

func loadFixture(t *testing.T, name string) *ical.Calendar {
	t.Helper()
	f, err := os.Open("testdata/busycal/" + name + ".ics")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cal, err := ical.NewDecoder(f).Decode()
	if err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	return cal
}

func parseFixture(t *testing.T, name string) *ParsedTodo {
	t.Helper()
	p, err := ParseTodo(loadFixture(t, name), "", newYork)
	if err != nil {
		t.Fatalf("ParseTodo(%s): %v", name, err)
	}
	return p
}

func extraTodo(p *ParsedTodo) *ical.Component {
	if p.Extras == nil {
		return nil
	}
	return firstChild(p.Extras.Component, ical.CompToDo)
}

// Fixtures are BusyCal's real PUT bodies captured in the spike.
func TestParseBusyCalFixtures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		fixture string
		want    TaskFields
	}{
		{"set-date-duration", TaskFields{Title: "Inbox task (no dates)", Status: StatusOpen, When: "2026-10-02", Reminder: NoReminder}},
		{"edit-tags", TaskFields{Title: "Tagged errand + home", Status: StatusOpen, Reminder: NoReminder, Tags: "errand\ntestadd"}},
		{"allday-alarm-9am", TaskFields{Title: "Deadline in 5 days (DUE)", Status: StatusOpen, When: "2026-10-06", Reminder: 9 * 3600}},
		// The timed to-do's alarm is BusyCal's automatic default: not a reminder.
		{"timed-default-alarm", TaskFields{Title: "When = today (DTSTART)", Status: StatusOpen, When: "2026-10-01", Reminder: NoReminder}},
		{"complete", TaskFields{Title: "Inbox task (no dates)", Status: StatusCompleted, Reminder: NoReminder}},
		{"uncomplete", TaskFields{Title: "Inbox task (no dates)", Status: StatusOpen, Reminder: NoReminder}},
		{"create", TaskFields{Title: "Try something new", Status: StatusOpen, Reminder: NoReminder}},
		{"priority", TaskFields{Title: "When tomorrow + deadline in 7 days", Status: StatusOpen, When: "2026-10-08", Reminder: NoReminder}},
	} {
		got := parseFixture(t, tc.fixture).Fields
		if got != tc.want {
			t.Errorf("%s:\n got  %+v\n want %+v", tc.fixture, got, tc.want)
		}
	}
}

func TestParseKeepsExtras(t *testing.T) {
	t.Parallel()
	if x := extraTodo(parseFixture(t, "set-date-duration")); x == nil || x.Props.Get("X-BUSYMAC-TASK-DURATION") == nil {
		t.Error("BusyCal duration must be kept as an extra")
	}
	if x := extraTodo(parseFixture(t, "priority")); x == nil || x.Props.Get(ical.PropPriority) == nil {
		t.Error("PRIORITY must be kept as an extra")
	}

	timed := parseFixture(t, "timed-default-alarm")
	x := extraTodo(timed)
	tp := x.Props.Get(propTime)
	if tp == nil || tp.Value != "090000" || tp.Params.Get(ical.PropTimezoneID) != "America/New_York" {
		t.Errorf("time of day not kept: %+v", tp)
	}
	if len(x.Children) != 1 || !isDefaultAlarm(x.Children[0]) {
		t.Errorf("default alarm should be kept as an extra, got %d children", len(x.Children))
	}
	if firstChild(timed.Extras.Component, ical.CompTimezone) == nil {
		t.Error("VTIMEZONE must be kept for the timed date")
	}
	for name := range x.Props {
		if name == ical.PropUID || name == ical.PropDateTimeStamp {
			continue // the encoder's required placeholders, checked below
		}
		if ownedTodoProps[name] {
			t.Errorf("owned property %s leaked into extras", name)
		}
	}
	if propText(x, ical.PropUID) != "sidecar" {
		t.Errorf("client UID leaked into extras: %q", propText(x, ical.PropUID))
	}

	if got := parseFixture(t, "complete").CompletedAt; got == nil || !got.Equal(time.Date(2026, 10, 1, 11, 33, 39, 0, time.UTC)) {
		t.Errorf("CompletedAt = %v", got)
	}
	if p := parseFixture(t, "move-create"); p.Created.IsZero() || p.UID != "5EDC938E-F73D-4772-A06D-D5188D1681CC" {
		t.Errorf("move create: UID %q, Created %v", p.UID, p.Created)
	}
}

func TestParseDeadlineFixture(t *testing.T) {
	t.Parallel()
	p, err := ParseDeadline(loadFixture(t, "deadline-drag"), newYork)
	if err != nil {
		t.Fatal(err)
	}
	if p.Fields.Date != "2026-10-05" || p.UID != "deadline-spike-deadline" {
		t.Errorf("deadline = %+v", p)
	}
}

func TestParseSomedayAndDateWins(t *testing.T) {
	t.Parallel()
	mk := func(body string) *ical.Calendar {
		raw := "BEGIN:VCALENDAR\r\nVERSION:2.0\r\nPRODID:t\r\nBEGIN:VTODO\r\nUID:x\r\nDTSTAMP:20261001T000000Z\r\n" +
			strings.ReplaceAll(body, "\n", "\r\n") + "\r\nEND:VTODO\r\nEND:VCALENDAR\r\n"
		cal, err := ical.NewDecoder(strings.NewReader(raw)).Decode()
		if err != nil {
			t.Fatal(err)
		}
		return cal
	}
	p, _ := ParseTodo(mk("SUMMARY:later\nCATEGORIES:home,Someday"), "", newYork)
	if !p.Fields.Someday || p.Fields.Tags != "home" {
		t.Errorf("undated Someday: %+v", p.Fields)
	}
	p, _ = ParseTodo(mk("SUMMARY:dated\nCATEGORIES:Someday\nDUE;VALUE=DATE:20261105"), "", newYork)
	if p.Fields.Someday || p.Fields.When != "2026-11-05" {
		t.Errorf("a date must end Someday: %+v", p.Fields)
	}
	// UTC date-times resolve to the local calendar day.
	p, _ = ParseTodo(mk("SUMMARY:late\nDUE:20261002T020000Z"), "", newYork)
	if p.Fields.When != "2026-10-01" {
		t.Errorf("UTC due should land on the New York day: %+v", p.Fields)
	}
}

func TestMergeFields(t *testing.T) {
	t.Parallel()
	base := TaskFields{Title: "t", Status: StatusOpen, When: "2026-10-02", Reminder: 9 * 3600, Tags: "a"}
	with := func(f func(*TaskFields)) TaskFields { c := base; f(&c); return c }

	for _, tc := range []struct {
		name          string
		client, thing TaskFields
		want          TaskFields
		conflicts     int
	}{
		{"client only", with(func(f *TaskFields) { f.Title = "new" }), base, with(func(f *TaskFields) { f.Title = "new" }), 0},
		{"things only", base, with(func(f *TaskFields) { f.Title = "things" }), with(func(f *TaskFields) { f.Title = "things" }), 0},
		{"different fields", with(func(f *TaskFields) { f.Title = "new" }), with(func(f *TaskFields) { f.Tags = "b" }),
			with(func(f *TaskFields) { f.Title = "new"; f.Tags = "b" }), 0},
		{"same change", with(func(f *TaskFields) { f.Title = "x" }), with(func(f *TaskFields) { f.Title = "x" }), with(func(f *TaskFields) { f.Title = "x" }), 0},
		{"conflict: Things wins", with(func(f *TaskFields) { f.Title = "client" }), with(func(f *TaskFields) { f.Title = "things" }),
			with(func(f *TaskFields) { f.Title = "things" }), 1},
		{"date vs someday: Things wins", with(func(f *TaskFields) { f.When = "2026-10-09" }),
			with(func(f *TaskFields) { f.When = ""; f.Someday = true; f.Reminder = NoReminder }),
			with(func(f *TaskFields) { f.When = ""; f.Someday = true; f.Reminder = NoReminder }), 1},
		{"client clears date: reminder goes too", with(func(f *TaskFields) { f.When = ""; f.Reminder = NoReminder }), base,
			with(func(f *TaskFields) { f.When = ""; f.Reminder = NoReminder }), 0},
	} {
		got, conflicts := MergeFields(base, tc.client, tc.thing)
		if got != tc.want || len(conflicts) != tc.conflicts {
			t.Errorf("%s:\n got  %+v (%d conflicts)\n want %+v (%d)", tc.name, got, len(conflicts), tc.want, tc.conflicts)
		}
	}
}

// The spike's two-device run: the iPhone re-sent a stale "completed" after
// the Mac reopened the task. Per-device bases must keep it open.
func TestTwoDeviceStaleEdit(t *testing.T) {
	t.Parallel()
	parse := func(name string) TaskFields { return parseFixture(t, name).Fields }
	things := TaskFields{Title: "When tomorrow + deadline in 7 days", Status: StatusOpen, When: "2026-10-08", Reminder: NoReminder}
	baseIPhone, baseMac := things, things

	apply := func(base *TaskFields, fixture string) {
		client := parse(fixture)
		things, _ = MergeFields(*base, client, things)
		*base = client // the device now holds what it sent
	}
	apply(&baseIPhone, "twodev-1-iphone-complete")
	if things.Status != StatusCompleted {
		t.Fatalf("iPhone completion lost: %+v", things)
	}
	baseMac = things // the Mac downloads the completed task
	apply(&baseMac, "twodev-2-mac-reopen")
	apply(&baseMac, "twodev-3-mac-1h")
	apply(&baseIPhone, "twodev-4-iphone-stale-30m")
	apply(&baseMac, "twodev-5-mac-2h")
	if things.Status != StatusOpen {
		t.Errorf("stale iPhone copy re-completed the task: %+v", things)
	}
}

// Whatever we render must parse back to the same fields, or every download
// would look like an edit.
func TestRenderParseRoundTrip(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.in.Location = newYork
	plain := f.add(task("Plain", "plain"))
	plain.Note = "notes; with, punctuation\nand lines"
	full := f.add(task("Full", "full"))
	full.ScheduledDate = day(2026, 10, 2)
	full.AlarmTimeOffset = intPtr(8*3600 + 15*60)
	full.TagIDs = []string{"TagB", "TagA"}
	someday := f.add(task("Someday", "later"))
	someday.Schedule = things.TaskScheduleSomeday
	done := f.add(task("Done", "done"))
	done.Status = things.TaskStatusCompleted
	done.CompletionDate = &modified

	// A timed BusyCal to-do keeps its time, and the Things reminder becomes
	// an alarm relative to it.
	timed := f.add(task("Timed", "timed"))
	timed.ScheduledDate = day(2026, 10, 1)
	timed.AlarmTimeOffset = intPtr(8 * 3600)
	extras := parseFixture(t, "timed-default-alarm").Extras
	f.in.Extras = func(uuid string) *ical.Calendar {
		if uuid == "Timed" {
			return extras
		}
		return nil
	}

	v := build(t, f)
	for _, tk := range []*things.Task{plain, full, someday, done, timed} {
		o := v.Calendar(NoProjectID).Object(tk.UUID + ".ics")
		p, err := ParseTodo(o.Data, "", newYork)
		if err != nil {
			t.Fatal(err)
		}
		want := FieldsFromTask(tk, f.in.Tags, "", now)
		if p.Fields != want {
			t.Errorf("%s round trip:\n got  %+v\n want %+v\n%s", tk.UUID, p.Fields, want, o.Raw)
		}
	}
	timedRaw := strings.ReplaceAll(string(v.Calendar(NoProjectID).Object("Timed.ics").Raw), "\r\n", "\n")
	for _, want := range []string{"DUE;TZID=America/New_York:20261001T090000", "BEGIN:VTIMEZONE", "X-BUSYMAC-DEFAULT-ALARM:TRUE", "TRIGGER:-PT1H"} {
		if !strings.Contains(timedRaw, want) {
			t.Errorf("timed render missing %q:\n%s", want, timedRaw)
		}
	}
	if strings.Contains(timedRaw, "X-THINGSDAV") {
		t.Errorf("sidecar markers leaked to the client:\n%s", timedRaw)
	}
}

func TestRenderReusesClientReminderAlarm(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.in.Location = newYork
	tk := f.add(task("T", "with alarm"))
	tk.ScheduledDate = day(2026, 10, 6)
	tk.AlarmTimeOffset = intPtr(9 * 3600)
	extras := parseFixture(t, "allday-alarm-9am").Extras
	f.in.Extras = func(string) *ical.Calendar { return extras }

	raw := string(build(t, f).Calendar(NoProjectID).Object("T.ics").Raw)
	if !strings.Contains(raw, ":Blow") || strings.Count(raw, "BEGIN:VALARM") != 1 {
		t.Errorf("client alarm should be reused while it matches:\n%s", raw)
	}
	tk.AlarmTimeOffset = intPtr(10 * 3600) // reminder moved in Things
	raw = string(build(t, f).Calendar(NoProjectID).Object("T.ics").Raw)
	if strings.Contains(raw, ":Blow") || !strings.Contains(raw, "TRIGGER:PT10H") {
		t.Errorf("stale client alarm must be replaced:\n%s", raw)
	}
}

func TestAliasRendersClientUID(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.add(task("T", "created in BusyCal"))
	f.in.Alias = func(uuid string) (string, string, bool) { return "CLIENT-UID", "CLIENT-UID.ics", uuid == "T" }
	o := build(t, f).Calendar(NoProjectID).Object("CLIENT-UID.ics")
	if o == nil || o.UID != "CLIENT-UID" || o.TaskUUID != "T" || o.Key != TaskKey("T") {
		t.Fatalf("alias object = %+v", o)
	}
	if !strings.Contains(string(o.Raw), "UID:CLIENT-UID") {
		t.Error("rendered UID must be the client's")
	}
}

func TestExtrasEncodeRoundTrip(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"set-date-duration", "timed-default-alarm", "allday-alarm-9am", "priority"} {
		p := parseFixture(t, name)
		raw, err := EncodeExtras(p.Extras)
		if err != nil || raw == "" {
			t.Fatalf("%s: encode: %q, %v", name, raw, err)
		}
		back, err := DecodeExtras(raw)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		again, _ := EncodeExtras(back)
		if again != raw {
			t.Errorf("%s: sidecar changed across a round trip", name)
		}
	}
}
