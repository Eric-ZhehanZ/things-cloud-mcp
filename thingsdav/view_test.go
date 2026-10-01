package thingsdav

import (
	"strings"
	"testing"
	"time"

	things "github.com/arthursoares/things-cloud-sdk"
)

func day(y int, m time.Month, d int) *time.Time {
	t := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	return &t
}

func intPtr(i int) *int { return &i }

var (
	created  = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	modified = time.Date(2026, 9, 20, 8, 30, 0, 0, time.UTC)
	now      = time.Date(2026, 10, 1, 15, 0, 0, 0, time.UTC)
)

func task(uuid, title string) *things.Task {
	return &things.Task{
		UUID:             uuid,
		Title:            title,
		Type:             things.TaskTypeTask,
		Status:           things.TaskStatusPending,
		Schedule:         things.TaskScheduleAnytime,
		CreationDate:     created,
		ModificationDate: &modified,
	}
}

type fixture struct {
	in      Input
	byUUID  map[string]*things.Task
	project *things.Task
	heading *things.Task
}

func newFixture() *fixture {
	f := &fixture{byUUID: map[string]*things.Task{}}
	f.project = task("Proj1", "Launch")
	f.project.Type = things.TaskTypeProject
	f.project.AreaIDs = []string{"Area1"}
	f.heading = task("Head1", "Phase 1")
	f.heading.Type = things.TaskTypeHeading
	f.heading.ParentTaskIDs = []string{"Proj1"}
	f.byUUID["Proj1"] = f.project
	f.byUUID["Head1"] = f.heading
	f.in = Input{
		Projects: []*things.Task{f.project},
		Areas:    []*things.Area{{UUID: "Area1", Title: "Work"}},
		Tags:     map[string]string{"TagA": "errand", "TagB": "home, garden"},
		Lookup:   func(id string) *things.Task { return f.byUUID[id] },
		Now:      now,
	}
	return f
}

func (f *fixture) add(t *things.Task) *things.Task {
	f.in.Tasks = append(f.in.Tasks, t)
	f.byUUID[t.UUID] = t
	return t
}

func build(t *testing.T, f *fixture) *View {
	t.Helper()
	v, err := Build(f.in)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return v
}

func raw(t *testing.T, v *View, calID, uid string) string {
	t.Helper()
	c := v.Calendar(calID)
	if c == nil {
		t.Fatalf("calendar %q missing", calID)
	}
	o := c.Object(uid + ".ics")
	if o == nil {
		t.Fatalf("object %s missing from %s", uid, calID)
	}
	return strings.ReplaceAll(string(o.Raw), "\r\n", "\n")
}

func TestRenderFullTodo(t *testing.T) {
	t.Parallel()
	f := newFixture()
	tk := task("Task1", "Write; report, draft")
	tk.Note = "line one\nline two"
	tk.ParentTaskIDs = []string{"Proj1"}
	tk.ScheduledDate = day(2026, 10, 2)
	tk.AlarmTimeOffset = intPtr(9*3600 + 30*60)
	tk.DeadlineDate = day(2026, 10, 9)
	tk.TagIDs = []string{"TagA", "TagB"}
	f.add(tk)

	got := raw(t, build(t, f), "project-Proj1", "Task1")
	want := `BEGIN:VCALENDAR
PRODID:-//things-cloud-mcp//thingsdav//EN
VERSION:2.0
BEGIN:VTODO
CATEGORIES:errand,home\, garden
CREATED:20260901T120000Z
DESCRIPTION:line one\nline two
DTSTAMP:20260920T083000Z
DTSTART;VALUE=DATE:20261002
DUE;VALUE=DATE:20261002
LAST-MODIFIED:20260920T083000Z
STATUS:NEEDS-ACTION
SUMMARY:Write\; report\, draft
UID:Task1
BEGIN:VALARM
ACTION:DISPLAY
DESCRIPTION:Write\; report\, draft
TRIGGER:PT9H30M
END:VALARM
END:VTODO
END:VCALENDAR
`
	if got != want {
		t.Errorf("rendered VTODO mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestRenderDeadlineEvent(t *testing.T) {
	t.Parallel()
	f := newFixture()
	tk := task("Task1", "File taxes")
	tk.Note = "bring receipts"
	tk.DeadlineDate = day(2026, 10, 15)
	f.add(tk)

	got := raw(t, build(t, f), DeadlinesID, "deadline-Task1")
	want := `BEGIN:VCALENDAR
PRODID:-//things-cloud-mcp//thingsdav//EN
VERSION:2.0
BEGIN:VEVENT
CREATED:20260901T120000Z
DESCRIPTION:bring receipts
DTEND;VALUE=DATE:20261016
DTSTAMP:20260920T083000Z
DTSTART;VALUE=DATE:20261015
LAST-MODIFIED:20260920T083000Z
SUMMARY:File taxes
TRANSP:TRANSPARENT
UID:deadline-Task1
URL:things:///show?id=Task1
END:VEVENT
END:VCALENDAR
`
	if got != want {
		t.Errorf("rendered VEVENT mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestContainers(t *testing.T) {
	t.Parallel()
	f := newFixture()
	inbox := f.add(task("InboxT", "inbox"))
	inbox.Schedule = things.TaskScheduleInbox
	f.add(task("LooseT", "loose"))
	areaT := f.add(task("AreaT", "area task"))
	areaT.AreaIDs = []string{"Area1"}
	projT := f.add(task("ProjT", "project task"))
	projT.ParentTaskIDs = []string{"Proj1"}
	headT := f.add(task("HeadT", "under heading"))
	headT.ActionGroupIDs = []string{"Head1"}
	sub := f.add(task("SubT", "subtask"))
	sub.ParentTaskIDs = []string{"ProjT"}
	orphan := f.add(task("OrphanT", "in trashed project"))
	orphan.ParentTaskIDs = []string{"GoneProj"}
	f.byUUID["GoneProj"] = &things.Task{UUID: "GoneProj", Type: things.TaskTypeProject}

	v := build(t, f)
	for calID, uids := range map[string][]string{
		InboxID:         {"InboxT"},
		NoProjectID:     {"LooseT"},
		"area-Area1":    {"AreaT"},
		"project-Proj1": {"HeadT", "ProjT", "SubT"},
	} {
		c := v.Calendar(calID)
		var got []string
		for _, o := range c.Objects {
			got = append(got, o.UID)
		}
		if strings.Join(got, ",") != strings.Join(uids, ",") {
			t.Errorf("%s: got %v, want %v", calID, got, uids)
		}
	}
	for _, c := range v.Calendars {
		if c.Object("OrphanT.ics") != nil {
			t.Errorf("task in hidden project rendered into %s", c.ID)
		}
	}
	if got := v.Calendar("project-Proj1").Name; got != "Work › Launch" {
		t.Errorf("project calendar name = %q", got)
	}
}

func TestStatusesAndSomeday(t *testing.T) {
	t.Parallel()
	f := newFixture()
	done := f.add(task("Done", "done"))
	done.Status = things.TaskStatusCompleted
	done.CompletionDate = &modified
	canceled := f.add(task("Canceled", "won't do"))
	canceled.Status = things.TaskStatusCanceled
	someday := f.add(task("Someday", "later"))
	someday.Schedule = things.TaskScheduleSomeday
	someday.TagIDs = []string{"TagA"}
	upcoming := f.add(task("Upcoming", "dated someday"))
	upcoming.Schedule = things.TaskScheduleSomeday
	upcoming.ScheduledDate = day(2026, 11, 1)

	v := build(t, f)
	checks := map[string][]string{
		"Done":     {"STATUS:COMPLETED", "COMPLETED:20260920T083000Z", "PERCENT-COMPLETE:100"},
		"Canceled": {"STATUS:CANCELLED"},
		"Someday":  {"CATEGORIES:errand,Someday"},
		"Upcoming": {"DUE;VALUE=DATE:20261101"},
	}
	for uid, wants := range checks {
		got := raw(t, v, NoProjectID, uid)
		for _, w := range wants {
			if !strings.Contains(got, w+"\n") {
				t.Errorf("%s: missing %q in\n%s", uid, w, got)
			}
		}
	}
	if strings.Contains(raw(t, v, NoProjectID, "Upcoming"), "Someday") {
		t.Error("dated Someday task must not carry the Someday category")
	}
	if strings.Contains(raw(t, v, NoProjectID, "Done"), "VALARM") {
		t.Error("undated task must not carry an alarm")
	}
}

func TestTodayViaTodayIndexReference(t *testing.T) {
	t.Parallel()
	f := newFixture()
	today := f.add(task("Today", "today via tir"))
	today.TodayIndexReference = day(2026, 10, 1)
	stale := f.add(task("Stale", "old tir"))
	stale.TodayIndexReference = day(2026, 9, 1)

	v := build(t, f)
	if !strings.Contains(raw(t, v, NoProjectID, "Today"), "DUE;VALUE=DATE:20261001") {
		t.Error("tir=today should render as today's date")
	}
	if strings.Contains(raw(t, v, NoProjectID, "Stale"), "DUE") {
		t.Error("stale tir must not render a date")
	}
}

func TestDeadlinesOnlyForOpenItems(t *testing.T) {
	t.Parallel()
	f := newFixture()
	open := f.add(task("Open", "open"))
	open.DeadlineDate = day(2026, 10, 5)
	done := f.add(task("Done", "done"))
	done.DeadlineDate = day(2026, 10, 5)
	done.Status = things.TaskStatusCompleted
	f.project.DeadlineDate = day(2026, 12, 1)

	c := build(t, f).Calendar(DeadlinesID)
	var got []string
	for _, o := range c.Objects {
		got = append(got, o.UID)
	}
	if want := "deadline-Open,deadline-Proj1"; strings.Join(got, ",") != want {
		t.Errorf("deadlines = %v, want %s", got, want)
	}
	if c.Kind != KindEvent {
		t.Errorf("deadlines kind = %s", c.Kind)
	}
}

func TestSkipsTemplatesTrashAndNonTasks(t *testing.T) {
	t.Parallel()
	f := newFixture()
	tmpl := f.add(task("Tmpl", "repeating template"))
	tmpl.Repeater = &things.RepeaterConfiguration{}
	trashed := f.add(task("Trashed", "trashed"))
	trashed.InTrash = true
	f.add(f.heading)

	for _, c := range build(t, f).Calendars {
		for _, o := range c.Objects {
			switch o.UID {
			case "Tmpl", "Trashed", "Head1":
				t.Errorf("%s should not be rendered (found in %s)", o.UID, c.ID)
			}
		}
	}
}

func TestCompletedProjectWindow(t *testing.T) {
	t.Parallel()
	f := newFixture()
	old := task("OldProj", "Old")
	old.Type = things.TaskTypeProject
	old.Status = things.TaskStatusCompleted
	old.CompletionDate = day(2026, 6, 1)
	recent := task("NewProj", "Recent")
	recent.Type = things.TaskTypeProject
	recent.Status = things.TaskStatusCompleted
	recent.CompletionDate = day(2026, 9, 25)
	f.in.Projects = append(f.in.Projects, old, recent)
	f.in.CompletedSince = now.AddDate(0, 0, -30)

	v := build(t, f)
	if v.Calendar("project-OldProj") != nil {
		t.Error("project completed before the window should be hidden")
	}
	if v.Calendar("project-NewProj") == nil {
		t.Error("project completed inside the window should be shown")
	}
}

func TestETagStableAndContentSensitive(t *testing.T) {
	t.Parallel()
	f := newFixture()
	tk := f.add(task("T", "title"))
	first := build(t, f).Calendar(NoProjectID).Object("T.ics").ETag
	second := build(t, f).Calendar(NoProjectID).Object("T.ics").ETag
	if first != second {
		t.Fatal("ETag must be stable across builds of the same state")
	}
	tk.Title = "changed"
	if third := build(t, f).Calendar(NoProjectID).Object("T.ics").ETag; third == first {
		t.Fatal("ETag must change when the task changes")
	}
}

func TestOffsetDuration(t *testing.T) {
	t.Parallel()
	for secs, want := range map[int]string{0: "PT0S", 32400: "PT9H", 34200: "PT9H30M", 45: "PT45S", 1800: "PT30M"} {
		if got := offsetDuration(secs); got != want {
			t.Errorf("offsetDuration(%d) = %s, want %s", secs, got, want)
		}
	}
}

func TestCalendarColors(t *testing.T) {
	t.Parallel()
	f := newFixture()
	f.in.Areas = append(f.in.Areas, &things.Area{UUID: "Area2", Title: "Home"})
	loose := task("LooseProj", "Side project")
	loose.Type = things.TaskTypeProject
	f.in.Projects = append(f.in.Projects, loose)

	v := build(t, f)
	for id, want := range map[string]string{
		InboxID:             InboxColor,
		NoProjectID:         ThingsColor,
		DeadlinesID:         DeadlinesColor,
		"area-Area1":        AreaColor,
		"project-Proj1":     AreaColor,
		"area-Area2":        AreaColor,
		"project-LooseProj": AreaColor,
	} {
		if got := v.Calendar(id).Color; got != want {
			t.Errorf("%s color = %s, want %s", id, got, want)
		}
	}
	if got := v.Calendar(NoProjectID).Name; got != "Things" {
		t.Errorf("loose-task calendar name = %q, want Things", got)
	}
}

func TestCTagTracksContents(t *testing.T) {
	t.Parallel()
	f := newFixture()
	tk := f.add(task("T", "title"))
	before := build(t, f)
	again := build(t, f)
	if before.Calendar(NoProjectID).CTag != again.Calendar(NoProjectID).CTag {
		t.Fatal("CTag must be stable for unchanged state")
	}
	tk.Title = "changed"
	after := build(t, f)
	if after.Calendar(NoProjectID).CTag == before.Calendar(NoProjectID).CTag {
		t.Error("CTag must change when an object changes")
	}
	if after.Calendar(InboxID).CTag != before.Calendar(InboxID).CTag {
		t.Error("CTag of an untouched calendar must not change")
	}
}
