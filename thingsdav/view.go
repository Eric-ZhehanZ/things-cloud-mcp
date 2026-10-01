// Package thingsdav renders Things state as CalDAV calendars: one VTODO list
// per container (Inbox, loose tasks, each area, each project) plus a VEVENT
// "Deadlines" calendar. It is pure — no I/O — so the CalDAV backend can cache
// a View per Things sync index and tests can drive it with plain structs.
//
// Mapping (see docs/plans/2026-10-01-caldav-server-design.md):
//   - When date            → DUE and DTSTART, both VALUE=DATE (BusyCal's single date)
//   - Reminder             → VALARM, TRIGGER relative to the date (PT9H30M)
//   - Tags                 → CATEGORIES, plus the Someday category for undated Someday tasks
//   - Completed / Canceled → STATUS:COMPLETED (+COMPLETED) / STATUS:CANCELLED
//   - Deadline             → an all-day event in the Deadlines calendar
//
// Headings and checklist items are not exported; titles and notes are passed
// through verbatim.
package thingsdav

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	things "github.com/arthursoares/things-cloud-sdk"
)

// Fixed calendar IDs. Area and project calendars use "area-<uuid>" and
// "project-<uuid>".
const (
	InboxID     = "inbox"
	NoProjectID = "no-project"
	DeadlinesID = "deadlines"
)

// DefaultSomedayCategory is the CATEGORIES value that marks Someday tasks.
const DefaultSomedayCategory = "Someday"

const prodID = "-//things-cloud-mcp//thingsdav//EN"

// Kind is the iCalendar component a calendar holds.
type Kind string

const (
	KindTodo  Kind = ical.CompToDo
	KindEvent Kind = ical.CompEvent
)

// Input is everything Build needs. Tasks and Projects must already exclude
// trashed entities; Tasks should hold open tasks plus the completed/canceled
// tasks that fall inside the completed window.
type Input struct {
	Tasks    []*things.Task
	Projects []*things.Task
	Areas    []*things.Area
	// Tags maps tag UUID → title.
	Tags map[string]string
	// Lookup resolves a parent (project, heading or parent task) by UUID.
	// It may return nil for unknown UUIDs.
	Lookup func(uuid string) *things.Task
	// Now anchors "today" for tasks placed in Today via tir only, and the
	// completed window for projects.
	Now time.Time
	// CompletedSince hides completed projects that finished earlier. Zero
	// means no limit.
	CompletedSince  time.Time
	SomedayCategory string
}

// Object is one rendered calendar resource.
type Object struct {
	Name    string // file name within the calendar, e.g. "<uuid>.ics"
	UID     string
	Data    *ical.Calendar
	Raw     []byte
	ETag    string
	ModTime time.Time
}

// Calendar colors (Apple calendar-color, #RRGGBBAA): Inbox and loose tasks
// are blue, every area and project shares one gray, Deadlines is red.
const (
	InboxColor     = "#007AFFFF" // blue
	ThingsColor    = InboxColor  // loose tasks
	AreaColor      = "#A5A5AAFF" // gray: all areas and projects
	DeadlinesColor = "#FF3B30FF" // red
)

// Calendar is one CalDAV collection.
type Calendar struct {
	ID    string
	Name  string
	Kind  Kind
	Color string
	// CTag changes whenever the calendar or any of its objects changes, so
	// clients can skip re-listing unchanged calendars.
	CTag    string
	Objects []*Object
	byName  map[string]*Object
}

// Object returns the object with the given file name, or nil.
func (c *Calendar) Object(name string) *Object { return c.byName[name] }

// View is a rendered snapshot of all calendars.
type View struct {
	Calendars []*Calendar
	byID      map[string]*Calendar
}

// Calendar returns the calendar with the given ID, or nil.
func (v *View) Calendar(id string) *Calendar { return v.byID[id] }

// Build renders the full calendar view.
func Build(in Input) (*View, error) {
	if in.SomedayCategory == "" {
		in.SomedayCategory = DefaultSomedayCategory
	}
	if in.Lookup == nil {
		in.Lookup = func(string) *things.Task { return nil }
	}
	b := &builder{in: in, view: &View{byID: map[string]*Calendar{}}}

	// Calendar order: Inbox, loose tasks, each area followed by its
	// projects, area-less projects, Deadlines.
	b.addCalendar(InboxID, "Inbox", KindTodo, InboxColor)
	b.addCalendar(NoProjectID, "Things", KindTodo, ThingsColor)
	areaNames := map[string]string{}
	projectsByArea := map[string][]*things.Task{}
	for _, p := range in.Projects {
		if !b.projectVisible(p) {
			continue
		}
		area := ""
		if len(p.AreaIDs) > 0 {
			area = p.AreaIDs[0]
		}
		projectsByArea[area] = append(projectsByArea[area], p)
	}
	for _, a := range in.Areas {
		areaNames[a.UUID] = a.Title
		b.addCalendar("area-"+a.UUID, a.Title, KindTodo, AreaColor)
		for _, p := range projectsByArea[a.UUID] {
			b.addCalendar("project-"+p.UUID, a.Title+" › "+p.Title, KindTodo, AreaColor)
		}
	}
	for area, projects := range projectsByArea {
		if _, ok := areaNames[area]; ok {
			continue
		}
		for _, p := range projects {
			b.addCalendar("project-"+p.UUID, p.Title, KindTodo, AreaColor)
		}
	}
	deadlines := b.addCalendar(DeadlinesID, "Deadlines", KindEvent, DeadlinesColor)

	for _, t := range in.Tasks {
		if t.InTrash || t.Type != things.TaskTypeTask || t.Repeater != nil {
			continue
		}
		cal := b.view.byID[b.containerID(t)]
		if cal == nil {
			continue // parent project hidden (trashed or completed long ago)
		}
		if err := b.addObject(cal, t.UUID, b.renderTodo(t), modTime(t)); err != nil {
			return nil, err
		}
		if t.Status == things.TaskStatusPending && t.DeadlineDate != nil {
			if err := b.addObject(deadlines, "deadline-"+t.UUID, b.renderDeadline(t), modTime(t)); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range in.Projects {
		if p.Status == things.TaskStatusPending && !p.InTrash && p.DeadlineDate != nil {
			if err := b.addObject(deadlines, "deadline-"+p.UUID, b.renderDeadline(p), modTime(p)); err != nil {
				return nil, err
			}
		}
	}
	for _, c := range b.view.Calendars {
		sort.Slice(c.Objects, func(i, j int) bool { return c.Objects[i].Name < c.Objects[j].Name })
		h := sha256.New()
		fmt.Fprintf(h, "%s\x00%s\x00", c.Name, c.Color)
		for _, o := range c.Objects {
			fmt.Fprintf(h, "%s\x00%s\x00", o.Name, o.ETag)
		}
		c.CTag = hex.EncodeToString(h.Sum(nil)[:16])
	}
	return b.view, nil
}

type builder struct {
	in   Input
	view *View
}

func (b *builder) addCalendar(id, name string, kind Kind, color string) *Calendar {
	c := &Calendar{ID: id, Name: name, Kind: kind, Color: color, byName: map[string]*Object{}}
	b.view.Calendars = append(b.view.Calendars, c)
	b.view.byID[id] = c
	return c
}

func (b *builder) addObject(c *Calendar, uid string, comp *ical.Component, mod time.Time) error {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, prodID)
	cal.Children = append(cal.Children, comp)
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return fmt.Errorf("render %s: %w", uid, err)
	}
	sum := sha256.Sum256(buf.Bytes())
	o := &Object{
		Name:    uid + ".ics",
		UID:     uid,
		Data:    cal,
		Raw:     buf.Bytes(),
		ETag:    hex.EncodeToString(sum[:16]),
		ModTime: mod,
	}
	c.Objects = append(c.Objects, o)
	c.byName[o.Name] = o
	return nil
}

// projectVisible reports whether a project gets a calendar: open projects
// always, finished ones only inside the completed window.
func (b *builder) projectVisible(p *things.Task) bool {
	if p.InTrash || p.Type != things.TaskTypeProject {
		return false
	}
	if p.Status == things.TaskStatusPending {
		return true
	}
	if b.in.CompletedSince.IsZero() {
		return true
	}
	return p.CompletionDate != nil && !p.CompletionDate.Before(b.in.CompletedSince)
}

// containerID walks a task up through parent tasks and headings to the
// calendar it belongs in.
func (b *builder) containerID(t *things.Task) string {
	cur := t
	for depth := 0; depth < 8; depth++ {
		if len(cur.ParentTaskIDs) > 0 && cur.ParentTaskIDs[0] != "" {
			if p := b.in.Lookup(cur.ParentTaskIDs[0]); p != nil {
				if p.Type == things.TaskTypeProject {
					return "project-" + p.UUID
				}
				cur = p // heading or parent task
				continue
			}
		}
		if len(cur.ActionGroupIDs) > 0 && cur.ActionGroupIDs[0] != "" {
			if h := b.in.Lookup(cur.ActionGroupIDs[0]); h != nil {
				cur = h
				continue
			}
		}
		break
	}
	if len(cur.AreaIDs) > 0 && cur.AreaIDs[0] != "" {
		return "area-" + cur.AreaIDs[0]
	}
	if cur.Schedule == things.TaskScheduleInbox {
		return InboxID
	}
	return NoProjectID
}

// whenDate is the Things When date: sr, or tir when it marks today.
func (b *builder) whenDate(t *things.Task) *time.Time {
	if t.ScheduledDate != nil {
		return t.ScheduledDate
	}
	if t.Schedule == things.TaskScheduleAnytime && t.TodayIndexReference != nil &&
		sameUTCDay(*t.TodayIndexReference, b.in.Now) {
		return t.TodayIndexReference
	}
	return nil
}

func (b *builder) renderTodo(t *things.Task) *ical.Component {
	c := ical.NewComponent(ical.CompToDo)
	c.Props.SetText(ical.PropUID, t.UUID)
	c.Props.SetText(ical.PropSummary, t.Title)
	if t.Note != "" {
		c.Props.SetText(ical.PropDescription, t.Note)
	}
	setTimestamps(c, t)

	when := b.whenDate(t)
	if when != nil {
		c.Props.SetDate(ical.PropDateTimeStart, dateOnly(*when))
		c.Props.SetDate(ical.PropDue, dateOnly(*when))
	}

	switch t.Status {
	case things.TaskStatusCompleted:
		c.Props.SetText(ical.PropStatus, "COMPLETED")
		if t.CompletionDate != nil {
			c.Props.SetDateTime(ical.PropCompleted, t.CompletionDate.UTC())
		}
		pct := ical.NewProp(ical.PropPercentComplete)
		pct.SetValueType(ical.ValueInt)
		pct.Value = "100"
		c.Props.Set(pct)
	case things.TaskStatusCanceled:
		c.Props.SetText(ical.PropStatus, "CANCELLED")
	default:
		c.Props.SetText(ical.PropStatus, "NEEDS-ACTION")
	}

	cats := b.tagTitles(t.TagIDs)
	if when == nil && t.Schedule == things.TaskScheduleSomeday {
		cats = append(cats, b.in.SomedayCategory)
	}
	if len(cats) > 0 {
		p := ical.NewProp(ical.PropCategories)
		p.SetTextList(cats)
		c.Props.Set(p)
	}

	if when != nil && t.AlarmTimeOffset != nil && *t.AlarmTimeOffset >= 0 {
		alarm := ical.NewComponent(ical.CompAlarm)
		alarm.Props.SetText(ical.PropAction, "DISPLAY")
		alarm.Props.SetText(ical.PropDescription, t.Title)
		trigger := ical.NewProp(ical.PropTrigger)
		trigger.SetValueType(ical.ValueDuration)
		trigger.Value = offsetDuration(*t.AlarmTimeOffset)
		alarm.Props.Set(trigger)
		c.Children = append(c.Children, alarm)
	}
	return c
}

func (b *builder) renderDeadline(t *things.Task) *ical.Component {
	c := ical.NewComponent(ical.CompEvent)
	c.Props.SetText(ical.PropUID, "deadline-"+t.UUID)
	c.Props.SetText(ical.PropSummary, t.Title)
	if t.Note != "" {
		c.Props.SetText(ical.PropDescription, t.Note)
	}
	setTimestamps(c, t)
	day := dateOnly(*t.DeadlineDate)
	c.Props.SetDate(ical.PropDateTimeStart, day)
	c.Props.SetDate(ical.PropDateTimeEnd, day.AddDate(0, 0, 1))
	c.Props.SetText(ical.PropTransparency, "TRANSPARENT")
	url := ical.NewProp(ical.PropURL)
	url.SetValueType(ical.ValueURI)
	url.Value = "things:///show?id=" + t.UUID
	c.Props.Set(url)
	return c
}

func (b *builder) tagTitles(ids []string) []string {
	var out []string
	for _, id := range ids {
		if title := b.in.Tags[id]; title != "" && title != b.in.SomedayCategory {
			out = append(out, title)
		}
	}
	return out
}

// setTimestamps writes DTSTAMP/CREATED/LAST-MODIFIED from Things dates so
// the rendering — and therefore the ETag — only changes when the task does.
func setTimestamps(c *ical.Component, t *things.Task) {
	mod := modTime(t)
	c.Props.SetDateTime(ical.PropDateTimeStamp, mod.UTC())
	if !t.CreationDate.IsZero() {
		c.Props.SetDateTime(ical.PropCreated, t.CreationDate.UTC())
	}
	if t.ModificationDate != nil {
		c.Props.SetDateTime(ical.PropLastModified, t.ModificationDate.UTC())
	}
}

func modTime(t *things.Task) time.Time {
	if t.ModificationDate != nil {
		return t.ModificationDate.UTC().Truncate(time.Second)
	}
	return t.CreationDate.UTC().Truncate(time.Second)
}

// dateOnly converts a Things day timestamp (midnight UTC of the calendar
// day) into a floating date.
func dateOnly(t time.Time) time.Time {
	u := t.UTC()
	return time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
}

func sameUTCDay(a, b time.Time) bool {
	ay, am, ad := a.UTC().Date()
	by, bm, bd := b.UTC().Date()
	return ay == by && am == bm && ad == bd
}

// offsetDuration formats seconds-after-midnight the way BusyCal writes
// alarm triggers on all-day to-dos: PT9H, PT9H30M, PT0S.
func offsetDuration(seconds int) string {
	if seconds == 0 {
		return "PT0S"
	}
	var sb strings.Builder
	sb.WriteString("PT")
	if h := seconds / 3600; h > 0 {
		fmt.Fprintf(&sb, "%dH", h)
	}
	if m := seconds % 3600 / 60; m > 0 {
		fmt.Fprintf(&sb, "%dM", m)
	}
	if s := seconds % 60; s > 0 {
		fmt.Fprintf(&sb, "%dS", s)
	}
	return sb.String()
}
