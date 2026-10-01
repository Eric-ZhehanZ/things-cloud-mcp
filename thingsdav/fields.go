package thingsdav

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/emersion/go-ical"

	things "github.com/arthursoares/things-cloud-sdk"
)

// TaskFields is the part of a Things task that CalDAV can see and edit. It
// is the unit of the three-way merge: base (what a device last downloaded),
// client (what it PUT) and current (Things now). All fields are comparable.
type TaskFields struct {
	Title  string `json:"title"`
	Notes  string `json:"notes"`
	Status string `json:"status"` // StatusOpen, StatusCompleted, StatusCanceled
	// When is the Things When date as YYYY-MM-DD, or "".
	When string `json:"when"`
	// Someday is set only for undated Someday tasks.
	Someday bool `json:"someday"`
	// Reminder is seconds after midnight of the When date, or NoReminder.
	Reminder int `json:"reminder"`
	// Tags holds sorted, de-duplicated tag titles joined by "\n".
	Tags string `json:"tags"`
}

// Task statuses as CalDAV sees them.
const (
	StatusOpen      = "open"
	StatusCompleted = "completed"
	StatusCanceled  = "canceled"
)

// NoReminder marks a task without a reminder.
const NoReminder = -1

// JoinTags normalizes a tag title list into TaskFields.Tags form.
func JoinTags(titles []string) string {
	seen := map[string]bool{}
	var out []string
	for _, t := range titles {
		t = strings.TrimSpace(t)
		if t != "" && !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

// TagList splits TaskFields.Tags back into titles.
func (f TaskFields) TagList() []string {
	if f.Tags == "" {
		return nil
	}
	return strings.Split(f.Tags, "\n")
}

// normalize enforces Things invariants: a date ends Someday, and a reminder
// needs a date.
func (f TaskFields) normalize() TaskFields {
	if f.When != "" {
		f.Someday = false
	} else {
		f.Reminder = NoReminder
	}
	return f
}

// DeadlineFields is the editable part of a Deadlines-calendar event.
type DeadlineFields struct {
	Date string `json:"date"` // YYYY-MM-DD, or "" when cleared
}

// ---------------------------------------------------------------------------
// Merge
// ---------------------------------------------------------------------------

// Conflict records a field both sides changed differently; Things won.
type Conflict struct {
	Field  string `json:"field"`
	Client string `json:"client"`
	Things string `json:"things"`
}

func merge3[T comparable](field string, base, client, current T, out *T, conflicts *[]Conflict) {
	switch {
	case client == base: // client didn't touch it
		*out = current
	case current == base || current == client: // only the client changed it
		*out = client
	default: // both changed it differently: Things wins
		*out = current
		*conflicts = append(*conflicts, Conflict{Field: field, Client: fmt.Sprint(client), Things: fmt.Sprint(current)})
	}
}

type schedule struct {
	when    string
	someday bool
}

// MergeFields three-way merges a client edit into the current Things state.
// Fields are independent except When+Someday, which merge as one schedule
// so a date set on one side and a Someday move on the other is a conflict
// Things wins rather than an accidental combination.
func MergeFields(base, client, current TaskFields) (TaskFields, []Conflict) {
	var out TaskFields
	var conflicts []Conflict
	merge3("title", base.Title, client.Title, current.Title, &out.Title, &conflicts)
	merge3("notes", base.Notes, client.Notes, current.Notes, &out.Notes, &conflicts)
	merge3("status", base.Status, client.Status, current.Status, &out.Status, &conflicts)
	var sched schedule
	merge3("schedule",
		schedule{base.When, base.Someday}, schedule{client.When, client.Someday}, schedule{current.When, current.Someday},
		&sched, &conflicts)
	out.When, out.Someday = sched.when, sched.someday
	merge3("reminder", base.Reminder, client.Reminder, current.Reminder, &out.Reminder, &conflicts)
	merge3("tags", base.Tags, client.Tags, current.Tags, &out.Tags, &conflicts)
	return out.normalize(), conflicts
}

// MergeDeadline three-way merges a Deadlines-event edit.
func MergeDeadline(base, client, current DeadlineFields) (DeadlineFields, []Conflict) {
	var out DeadlineFields
	var conflicts []Conflict
	merge3("deadline", base.Date, client.Date, current.Date, &out.Date, &conflicts)
	return out, conflicts
}

// ---------------------------------------------------------------------------
// Things → fields
// ---------------------------------------------------------------------------

// FieldsFromTask derives the CalDAV view of a task. tags maps tag UUID →
// title; now anchors tir-based Today.
func FieldsFromTask(t *things.Task, tags map[string]string, somedayCategory string, now time.Time) TaskFields {
	if somedayCategory == "" {
		somedayCategory = DefaultSomedayCategory
	}
	f := TaskFields{Title: t.Title, Notes: t.Note, Reminder: NoReminder}
	switch t.Status {
	case things.TaskStatusCompleted:
		f.Status = StatusCompleted
	case things.TaskStatusCanceled:
		f.Status = StatusCanceled
	default:
		f.Status = StatusOpen
	}
	if when := whenDate(t, now); when != nil {
		f.When = dateOnly(*when).Format(dateLayout)
		if t.AlarmTimeOffset != nil && *t.AlarmTimeOffset >= 0 {
			f.Reminder = *t.AlarmTimeOffset
		}
	} else if t.Schedule == things.TaskScheduleSomeday {
		f.Someday = true
	}
	var titles []string
	for _, id := range t.TagIDs {
		if title := tags[id]; title != "" && title != somedayCategory {
			titles = append(titles, title)
		}
	}
	f.Tags = JoinTags(titles)
	return f
}

// DeadlineFromTask derives the Deadlines-event view of a task or project.
func DeadlineFromTask(t *things.Task) DeadlineFields {
	if t.DeadlineDate == nil {
		return DeadlineFields{}
	}
	return DeadlineFields{Date: dateOnly(*t.DeadlineDate).Format(dateLayout)}
}

const dateLayout = "2006-01-02"

// whenDate is the Things When date: sr, or tir when it marks today.
func whenDate(t *things.Task, now time.Time) *time.Time {
	if t.ScheduledDate != nil {
		return t.ScheduledDate
	}
	if t.Schedule == things.TaskScheduleAnytime && t.TodayIndexReference != nil &&
		sameUTCDay(*t.TodayIndexReference, now) {
		return t.TodayIndexReference
	}
	return nil
}

// ---------------------------------------------------------------------------
// iCalendar → fields
// ---------------------------------------------------------------------------

// ParsedTodo is a client VTODO split into the fields Things stores and the
// extras it can't (kept in the sidecar and merged back on render).
type ParsedTodo struct {
	UID         string
	Fields      TaskFields
	Created     time.Time  // zero if absent; used to recognize list moves
	CompletedAt *time.Time // client COMPLETED timestamp
	Extras      *ical.Calendar
}

// ParsedDeadline is a client Deadlines event.
type ParsedDeadline struct {
	UID    string
	Fields DeadlineFields
}

// Properties the renderer owns. Everything else a client sends is extra.
var ownedTodoProps = map[string]bool{
	ical.PropUID: true, ical.PropSummary: true, ical.PropDescription: true,
	ical.PropStatus: true, ical.PropCompleted: true, ical.PropPercentComplete: true,
	ical.PropDateTimeStart: true, ical.PropDue: true, ical.PropDuration: true,
	ical.PropCategories: true, ical.PropDateTimeStamp: true, ical.PropCreated: true,
	ical.PropLastModified: true,
}

// Sidecar markers (never sent to clients).
const (
	propTime                = "X-THINGSDAV-TIME" // BusyCal time of day: VALUE HHMMSS, TZID param
	propAlarmRole           = "X-THINGSDAV-ROLE" // REMINDER marks the alarm that carries the Things reminder
	roleReminder            = "REMINDER"
	propBusyCalDefaultAlarm = "X-BUSYMAC-DEFAULT-ALARM"
)

// ParseTodo extracts TaskFields and extras from a client calendar object.
// loc resolves floating and UTC times to calendar days.
func ParseTodo(cal *ical.Calendar, somedayCategory string, loc *time.Location) (*ParsedTodo, error) {
	if somedayCategory == "" {
		somedayCategory = DefaultSomedayCategory
	}
	todo := firstChild(cal.Component, ical.CompToDo)
	if todo == nil {
		return nil, fmt.Errorf("no VTODO in calendar object")
	}
	p := &ParsedTodo{Fields: TaskFields{Status: StatusOpen, Reminder: NoReminder}}
	p.UID = propText(todo, ical.PropUID)
	p.Fields.Title = propText(todo, ical.PropSummary)
	p.Fields.Notes = propText(todo, ical.PropDescription)
	switch strings.ToUpper(propText(todo, ical.PropStatus)) {
	case "COMPLETED":
		p.Fields.Status = StatusCompleted
	case "CANCELLED":
		p.Fields.Status = StatusCanceled
	}
	if c := todo.Props.Get(ical.PropCompleted); c != nil {
		if t, err := c.DateTime(time.UTC); err == nil {
			p.CompletedAt = &t
		}
	}
	if p.CompletedAt != nil && p.Fields.Status == StatusOpen {
		p.Fields.Status = StatusCompleted // COMPLETED without STATUS still means done
	}
	if c := todo.Props.Get(ical.PropCreated); c != nil {
		if t, err := c.DateTime(time.UTC); err == nil {
			p.Created = t.UTC()
		}
	}

	extras := newExtras()
	extraTodo := firstChild(extras.Component, ical.CompToDo)

	// Date: DUE is BusyCal's date; DTSTART is the fallback.
	var start time.Time // the to-do's start instant, for relative alarms
	dateProp := todo.Props.Get(ical.PropDue)
	if dateProp == nil {
		dateProp = todo.Props.Get(ical.PropDateTimeStart)
	}
	if dateProp != nil {
		if dateProp.ValueType() == ical.ValueDate || len(dateProp.Value) == len("20060102") {
			if d, err := time.Parse("20060102", dateProp.Value); err == nil {
				p.Fields.When = d.Format(dateLayout)
				start = time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
			}
		} else if t, err := dateProp.DateTime(loc); err == nil {
			zone := t.Location()
			if zone == time.UTC && dateProp.Params.Get(ical.PropTimezoneID) == "" {
				zone = loc // UTC or floating: read the day in the server's zone
			}
			local := t.In(zone)
			p.Fields.When = local.Format(dateLayout)
			start = local
			tp := ical.NewProp(propTime)
			tp.Value = local.Format("150405")
			if tzid := dateProp.Params.Get(ical.PropTimezoneID); tzid != "" {
				tp.Params.Set(ical.PropTimezoneID, tzid)
			}
			extraTodo.Props.Set(tp)
		}
	}
	// Categories: tags plus the Someday marker.
	var titles []string
	someday := false
	for _, cp := range todo.Props.Values(ical.PropCategories) {
		list, err := cp.TextList()
		if err != nil {
			continue
		}
		for _, c := range list {
			if strings.EqualFold(strings.TrimSpace(c), somedayCategory) {
				someday = true
				continue
			}
			titles = append(titles, c)
		}
	}
	p.Fields.Tags = JoinTags(titles)
	p.Fields.Someday = someday && p.Fields.When == ""

	// Alarms: the first non-default alarm landing on the When date is the
	// Things reminder; every other alarm is an extra.
	for _, child := range todo.Children {
		if child.Name != ical.CompAlarm {
			extraTodo.Children = append(extraTodo.Children, child)
			continue
		}
		alarm := cloneComponent(child)
		if p.Fields.Reminder == NoReminder && p.Fields.When != "" && !isDefaultAlarm(alarm) {
			if secs, ok := alarmSecondsOnDay(alarm, start, p.Fields.When, loc); ok {
				p.Fields.Reminder = secs
				alarm.Props.SetText(propAlarmRole, roleReminder)
			}
		}
		extraTodo.Children = append(extraTodo.Children, alarm)
	}

	for name, props := range todo.Props {
		if ownedTodoProps[name] || strings.HasPrefix(name, "X-THINGSDAV-") {
			continue
		}
		extraTodo.Props[name] = append([]ical.Prop(nil), props...)
	}
	for _, child := range cal.Children {
		if child.Name == ical.CompTimezone {
			extras.Children = append(extras.Children, child)
		}
	}
	if len(extraTodo.Props) > 2 || len(extraTodo.Children) > 0 { // beyond the placeholders
		p.Extras = extras
	}
	return p, nil
}

// ParseDeadline extracts the deadline date from a client Deadlines event.
func ParseDeadline(cal *ical.Calendar, loc *time.Location) (*ParsedDeadline, error) {
	ev := firstChild(cal.Component, ical.CompEvent)
	if ev == nil {
		return nil, fmt.Errorf("no VEVENT in calendar object")
	}
	p := &ParsedDeadline{UID: propText(ev, ical.PropUID)}
	if ds := ev.Props.Get(ical.PropDateTimeStart); ds != nil {
		if len(ds.Value) == len("20060102") {
			if d, err := time.Parse("20060102", ds.Value); err == nil {
				p.Fields.Date = d.Format(dateLayout)
			}
		} else if t, err := ds.DateTime(loc); err == nil {
			p.Fields.Date = t.In(loc).Format(dateLayout)
		}
	}
	return p, nil
}

func isDefaultAlarm(alarm *ical.Component) bool {
	return strings.EqualFold(propText(alarm, propBusyCalDefaultAlarm), "TRUE")
}

// alarmSecondsOnDay resolves an alarm's trigger to seconds after midnight
// when it fires on the given day.
func alarmSecondsOnDay(alarm *ical.Component, start time.Time, day string, loc *time.Location) (int, bool) {
	trig := alarm.Props.Get(ical.PropTrigger)
	if trig == nil {
		return 0, false
	}
	var at time.Time
	if trig.ValueType() == ical.ValueDateTime {
		t, err := trig.DateTime(loc)
		if err != nil {
			return 0, false
		}
		at = t.In(loc)
		if start.Location() != loc && start.Location() != time.UTC {
			at = t.In(start.Location())
		}
	} else {
		d, err := trig.Duration()
		if err != nil || start.IsZero() {
			return 0, false
		}
		at = start.Add(d)
	}
	if at.Format(dateLayout) != day {
		return 0, false
	}
	return at.Hour()*3600 + at.Minute()*60 + at.Second(), true
}

// ---------------------------------------------------------------------------
// Extras (sidecar)
// ---------------------------------------------------------------------------

// newExtras returns an empty sidecar. Its VTODO carries placeholder UID and
// DTSTAMP only because the iCalendar encoder requires them; both are owned
// properties, so rendering never copies them to clients.
func newExtras() *ical.Calendar {
	cal := ical.NewCalendar()
	cal.Props.SetText(ical.PropVersion, "2.0")
	cal.Props.SetText(ical.PropProductID, prodID)
	todo := ical.NewComponent(ical.CompToDo)
	todo.Props.SetText(ical.PropUID, "sidecar")
	todo.Props.SetDateTime(ical.PropDateTimeStamp, time.Unix(0, 0).UTC())
	cal.Children = append(cal.Children, todo)
	return cal
}

// EncodeExtras serializes a sidecar for storage.
func EncodeExtras(cal *ical.Calendar) (string, error) {
	if cal == nil {
		return "", nil
	}
	var buf bytes.Buffer
	if err := ical.NewEncoder(&buf).Encode(cal); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// DecodeExtras parses a stored sidecar.
func DecodeExtras(s string) (*ical.Calendar, error) {
	if s == "" {
		return nil, nil
	}
	return ical.NewDecoder(strings.NewReader(s)).Decode()
}

func firstChild(c *ical.Component, name string) *ical.Component {
	for _, child := range c.Children {
		if child.Name == name {
			return child
		}
	}
	return nil
}

func propText(c *ical.Component, name string) string {
	p := c.Props.Get(name)
	if p == nil {
		return ""
	}
	if p.ValueType() == ical.ValueText {
		if s, err := p.Text(); err == nil {
			return s
		}
	}
	return p.Value
}

func cloneComponent(c *ical.Component) *ical.Component {
	out := ical.NewComponent(c.Name)
	for name, props := range c.Props {
		for _, p := range props {
			cp := p
			cp.Params = ical.Params{}
			for k, v := range p.Params {
				cp.Params[k] = append([]string(nil), v...)
			}
			out.Props[name] = append(out.Props[name], cp)
		}
	}
	for _, child := range c.Children {
		out.Children = append(out.Children, cloneComponent(child))
	}
	return out
}
