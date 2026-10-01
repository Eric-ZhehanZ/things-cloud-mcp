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
	"encoding/json"
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
	// Location resolves client times without a TZID and anchors relative
	// alarms on all-day dates. Defaults to UTC.
	Location *time.Location
	// Extras returns a task's sidecar (client properties Things can't hold),
	// or nil.
	Extras func(uuid string) *ical.Calendar
	// Alias returns the UID and file name a client created a task under,
	// when they differ from the Things UUID.
	Alias func(uuid string) (uid, name string, ok bool)
}

// Object is one rendered calendar resource.
type Object struct {
	Name     string // file name within the calendar, e.g. "<uuid>.ics"
	UID      string
	Key      string // TaskKey or DeadlineKey: identifies merge snapshots
	TaskUUID string // the Things task or project behind the object
	Title    string
	Created  time.Time // CREATED as rendered (second precision)
	// Snapshot is the JSON of the object's TaskFields or DeadlineFields:
	// recorded per device when the object is downloaded, it becomes the base
	// of that device's next edit.
	Snapshot string
	Data     *ical.Calendar
	Raw      []byte
	ETag     string
	ModTime  time.Time
}

// TaskKey and DeadlineKey name an object's merge snapshots.
func TaskKey(uuid string) string     { return "task:" + uuid }
func DeadlineKey(uuid string) string { return "deadline:" + uuid }

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
	if in.Location == nil {
		in.Location = time.UTC
	}
	if in.Extras == nil {
		in.Extras = func(string) *ical.Calendar { return nil }
	}
	if in.Alias == nil {
		in.Alias = func(string) (string, string, bool) { return "", "", false }
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
		f := FieldsFromTask(t, in.Tags, in.SomedayCategory, in.Now)
		uid, name := t.UUID, t.UUID+".ics"
		if u, n, ok := in.Alias(t.UUID); ok {
			uid, name = u, n
		}
		o := &Object{Name: name, UID: uid, Key: TaskKey(t.UUID), TaskUUID: t.UUID, Title: t.Title,
			Created: t.CreationDate.UTC().Truncate(time.Second), ModTime: modTime(t)}
		if err := b.addObject(cal, o, b.renderTodo(t, f, uid), f); err != nil {
			return nil, err
		}
		if t.Status == things.TaskStatusPending && t.DeadlineDate != nil {
			if err := b.addDeadline(deadlines, t); err != nil {
				return nil, err
			}
		}
	}
	for _, p := range in.Projects {
		if p.Status == things.TaskStatusPending && !p.InTrash && p.DeadlineDate != nil {
			if err := b.addDeadline(deadlines, p); err != nil {
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

func (b *builder) addObject(c *Calendar, o *Object, cal *ical.Calendar, snapshot any) error {
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return fmt.Errorf("render %s: %w", o.UID, err)
	}
	snap, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(buf.Bytes())
	o.Data, o.Raw, o.ETag, o.Snapshot = cal, buf.Bytes(), hex.EncodeToString(sum[:16]), string(snap)
	c.Objects = append(c.Objects, o)
	c.byName[o.Name] = o
	return nil
}

func (b *builder) addDeadline(c *Calendar, t *things.Task) error {
	uid := "deadline-" + t.UUID
	o := &Object{Name: uid + ".ics", UID: uid, Key: DeadlineKey(t.UUID), TaskUUID: t.UUID, Title: t.Title,
		Created: t.CreationDate.UTC().Truncate(time.Second), ModTime: modTime(t)}
	return b.addObject(c, o, wrapCalendar(b.renderDeadline(t, uid)), DeadlineFromTask(t))
}

func wrapCalendar(comps ...*ical.Component) *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, prodID)
	cal.Children = append(cal.Children, comps...)
	return cal
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

func (b *builder) renderTodo(t *things.Task, f TaskFields, uid string) *ical.Calendar {
	c := ical.NewComponent(ical.CompToDo)
	c.Props.SetText(ical.PropUID, uid)
	c.Props.SetText(ical.PropSummary, f.Title)
	if f.Notes != "" {
		c.Props.SetText(ical.PropDescription, f.Notes)
	}
	setTimestamps(c, t)

	var xtodo *ical.Component
	var timezones []*ical.Component
	if extras := b.in.Extras(t.UUID); extras != nil {
		xtodo = firstChild(extras.Component, ical.CompToDo)
		for _, child := range extras.Children {
			if child.Name == ical.CompTimezone {
				timezones = append(timezones, child)
			}
		}
	}

	// Date, at the client's time of day when it set one.
	var start time.Time
	timed := false
	if f.When != "" {
		d, _ := time.Parse(dateLayout, f.When)
		start = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, b.in.Location)
		if xtodo != nil {
			if tp := xtodo.Props.Get(propTime); tp != nil {
				if clock, err := time.Parse("150405", tp.Value); err == nil {
					zone := b.in.Location
					if tzid := tp.Params.Get(ical.PropTimezoneID); tzid != "" {
						if l, err := time.LoadLocation(tzid); err == nil {
							zone = l
						}
					}
					start = time.Date(d.Year(), d.Month(), d.Day(), clock.Hour(), clock.Minute(), clock.Second(), 0, zone)
					timed = true
				}
			}
		}
		if timed {
			c.Props.SetDateTime(ical.PropDateTimeStart, start)
			c.Props.SetDateTime(ical.PropDue, start)
		} else {
			c.Props.SetDate(ical.PropDateTimeStart, d)
			c.Props.SetDate(ical.PropDue, d)
		}
	}

	switch f.Status {
	case StatusCompleted:
		c.Props.SetText(ical.PropStatus, "COMPLETED")
		if t.CompletionDate != nil {
			c.Props.SetDateTime(ical.PropCompleted, t.CompletionDate.UTC())
		}
		pct := ical.NewProp(ical.PropPercentComplete)
		pct.SetValueType(ical.ValueInt)
		pct.Value = "100"
		c.Props.Set(pct)
	case StatusCanceled:
		c.Props.SetText(ical.PropStatus, "CANCELLED")
	default:
		c.Props.SetText(ical.PropStatus, "NEEDS-ACTION")
	}

	cats := f.TagList()
	if f.Someday {
		cats = append(cats, b.in.SomedayCategory)
	}
	if len(cats) > 0 {
		p := ical.NewProp(ical.PropCategories)
		p.SetTextList(cats)
		c.Props.Set(p)
	}

	// Client extras: unmapped properties, sub-components and alarms. The
	// alarm that carried the reminder is reused verbatim while the Things
	// reminder still matches it, so the client's sound and settings survive.
	var reminderAlarm *ical.Component
	if xtodo != nil {
		for name, props := range xtodo.Props {
			if ownedTodoProps[name] || strings.HasPrefix(name, "X-THINGSDAV-") {
				continue
			}
			c.Props[name] = append([]ical.Prop(nil), props...)
		}
		for _, child := range xtodo.Children {
			if child.Name != ical.CompAlarm {
				c.Children = append(c.Children, cloneComponent(child))
				continue
			}
			alarm := cloneComponent(child)
			if propText(alarm, propAlarmRole) == roleReminder {
				alarm.Props.Del(propAlarmRole)
				reminderAlarm = alarm
				continue
			}
			trig := alarm.Props.Get(ical.PropTrigger)
			if f.When == "" && (trig == nil || trig.ValueType() != ical.ValueDateTime) {
				continue // a relative alarm needs a date to hang off
			}
			c.Children = append(c.Children, alarm)
		}
	}
	if f.When != "" && f.Reminder != NoReminder {
		c.Children = append(c.Children, b.reminderAlarm(f, start, reminderAlarm))
	}
	return wrapCalendar(append(timezonesIf(timed, timezones), c)...)
}

// reminderAlarm returns the client's own reminder alarm while it still
// matches the Things reminder, else a fresh one relative to start.
func (b *builder) reminderAlarm(f TaskFields, start time.Time, clientAlarm *ical.Component) *ical.Component {
	if clientAlarm != nil {
		if secs, ok := alarmSecondsOnDay(clientAlarm, start, f.When, b.in.Location); ok && secs == f.Reminder {
			return clientAlarm
		}
	}
	alarm := ical.NewComponent(ical.CompAlarm)
	alarm.Props.SetText(ical.PropAction, "DISPLAY")
	alarm.Props.SetText(ical.PropDescription, f.Title)
	trigger := ical.NewProp(ical.PropTrigger)
	trigger.SetValueType(ical.ValueDuration)
	trigger.Value = offsetDuration(f.Reminder - (start.Hour()*3600 + start.Minute()*60 + start.Second()))
	alarm.Props.Set(trigger)
	return alarm
}

func timezonesIf(timed bool, tzs []*ical.Component) []*ical.Component {
	if !timed {
		return nil
	}
	return append([]*ical.Component(nil), tzs...)
}

func (b *builder) renderDeadline(t *things.Task, uid string) *ical.Component {
	c := ical.NewComponent(ical.CompEvent)
	c.Props.SetText(ical.PropUID, uid)
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

// offsetDuration formats an alarm offset the way BusyCal writes triggers:
// PT9H, PT9H30M, PT0S, -PT15M.
func offsetDuration(seconds int) string {
	if seconds == 0 {
		return "PT0S"
	}
	var sb strings.Builder
	if seconds < 0 {
		sb.WriteByte('-')
		seconds = -seconds
	}
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
