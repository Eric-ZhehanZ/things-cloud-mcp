# Things over CalDAV — Design Plan

**Date:** 2026-10-01
**Status:** Proposal (replaces the Todoist bridge plan)
**Client:** BusyCal (macOS/iOS) only. Every mapping choice below is tuned to
how BusyCal displays to-dos.

## Goal

Expose Things 3 as a CalDAV task server (VTODO), so any CalDAV client can read
and edit Things directly, with no intermediary service.

- Sync areas, projects, the hierarchy, headings, tasks, notes, completion,
  start dates, deadlines, reminders and tags.
- Never add anything to task content.
- Run alongside the MCP in the same server.
- Resolve conflicts deterministically. Things is the source of truth.
- Keep it as simple as possible.

## Why this is simpler than the Todoist bridge

Things stays the **only** data store. CalDAV is just another view and write
API over it, like the MCP:

- Two stores never have to be reconciled, so there is no mirror, no
  background loop, no echo suppression, no duplicates and no outbox.
- Every CalDAV request reads from or writes to Things through the code paths
  the MCP already uses (`syncForRead`, `taskUpdate`, `writeToHistory` under
  `historyMu`).
- Conflicts can only happen inside a single write request, and ETags handle
  that (see below).
- Fly can keep `auto_stop_machines`, because clients wake the machine on
  request. No always-on loop.

## Architecture

```
CalDAV clients ──HTTPS + Basic auth──▶ /dav/ (go-webdav caldav.Handler)
                                          │
                                   davBackend (server/dav.go)
                                   │                 │
                     vtodo/ (pure: Things ⇄ iCal,   dav.db (uid map,
                     field merge)                   ETag snapshots,
                                   │                sidecar props)
                                   ▼
                  sync.State (reads)  /  writeToHistory (writes)
                                   ▼
                              Things Cloud
```

- **Library:** `github.com/emersion/go-webdav/caldav` (server `Backend`
  interface: ListCalendars, GetCalendarObject, QueryCalendarObjects,
  PutCalendarObject with If-Match/If-None-Match, DeleteCalendarObject,
  CreateCalendar), plus `github.com/emersion/go-ical`.
- **`vtodo/` package:** pure functions with no I/O. `Render(task, ctx) →
  *ical.Component`, `Parse(component) → Canon`, and
  `Merge(base, things, client) → result`. Covered by table tests.
- **`server/dav.go`:** the `Backend` implementation. It lives in `package
  main` so it can reuse the existing write builders, validation and timezone
  helpers.
- **`/data/dav.db`:** a separate SQLite file that a `things.db` reset never
  touches.
- **Discovery:** `/.well-known/caldav` → `/dav/`, plus the principal and
  calendar-home-set.
- **Auth:** HTTP Basic, because Apple, DAVx5 and Thunderbird expect it. Any
  username is accepted, and the password is `CALDAV_PASSWORD` (falling back
  to `API_KEY`).

## Mapping

### Collections (lists) and hierarchy

CalDAV has no nested calendars, so each container becomes its own task list:

| Things | CalDAV calendar | Display name |
|---|---|---|
| Inbox | `inbox` | Inbox |
| Area (tasks directly in the area) | `area-<uuid>` | `Area` |
| Project in an area | `project-<uuid>` | `Area › Project` |
| Project with no area | `project-<uuid>` | `Project` |

Only the calendar name carries the hierarchy, not task content. Calendar
order follows Things order, so each area's projects sit right after it.
Every calendar declares `supported-calendar-component-set = VTODO`.

### Inside a calendar

BusyCal has no subtasks or nested to-dos, so each calendar is a flat list of
tasks:

| Things | CalDAV |
|---|---|
| Task | VTODO, `UID` = Things UUID |
| Heading | **Hidden.** Not exported. A task's heading lives only in Things and is never touched by a CalDAV write. If a task moves to another list, its heading is cleared (it belonged to the old project). |
| Checklist item | **Hidden** for the same reason (they would otherwise show up as separate flat to-dos). Edit checklists in Things. |

### Task fields

| Things | iCalendar | Notes |
|---|---|---|
| Title | `SUMMARY` | verbatim |
| Notes | `DESCRIPTION` | verbatim |
| Open / Completed / Canceled | `STATUS:NEEDS-ACTION` / `COMPLETED` (+`COMPLETED` time) / `CANCELLED` | BusyCal hides `CANCELLED` to-dos (spike), so canceled tasks are sent with their true status and stay hidden by choice. A PUT with no `STATUS` line means NEEDS-ACTION (BusyCal drops `STATUS`, `COMPLETED` and `PERCENT-COMPLETE` when you uncheck a to-do). BusyCal's `COMPLETED` timestamp becomes the Things completion date. |
| When date (Today / Upcoming) | `DUE;VALUE=DATE` **and** `DTSTART;VALUE=DATE`, both on the same day | BusyCal has one date field, which is `DUE`. When BusyCal sets a date it writes the same day into both (seen in the spike), so the server does the same. On parse, `DUE` wins and `DTSTART` is the fallback. |
| — | BusyCal **time** (`DUE` as a date-time) | BusyCal-only: the date part sets the When date, and the time of day is kept in the sidecar. On render, the stored time is put back on the current When date. Clearing the When date drops it. It never changes the Things reminder. |
| Deadline | **Not on the task.** Shown in the separate **Deadlines** calendar (below) | BusyCal task edits never change a deadline |
| — | BusyCal **duration** (`X-BUSYMAC-TASK-DURATION`) | BusyCal's private property, captured in the spike. Kept in the sidecar and returned unchanged. |
| Reminder | One `VALARM` with `TRIGGER;VALUE=DURATION:PT<h>H<m>M`, relative to the all-day date | BusyCal's own form (an alert at 09:00 on an all-day to-do was written as `PT9H`). Floating local time, so no timezone conversion. Absolute UTC triggers are still accepted on PUT and converted with `THINGS_TIMEZONE`. |
| Tags | `CATEGORIES` | |
| Someday | `CATEGORIES:Someday`, no date | Same rules as before (see below). The spike confirmed it shows as a tag. |
| Created / modified | `CREATED` / `LAST-MODIFIED` / `DTSTAMP` | read-only |
| Repeating template | — | Phase 4. Until then only the generated instances appear, with no `RRULE`. |
| Trash | not listed | |
| Completed | listed if completed within the last `CALDAV_COMPLETED_DAYS` (default 30; `0` = all) | See "Completed history" below |

### Someday rules (carried over)

- Someday = `CATEGORIES` includes `Someday` and there is no date. The
  label name can be changed with `CALDAV_SOMEDAY_CATEGORY`.
- **A date always ends Someday membership.** If a client sets a date on
  a Someday task, it moves to Upcoming in Things and the category is
  dropped. If a client adds the category to a task that has a date, the
  date wins.
- Adding the category to an undated task moves it to Someday. Removing it
  moves the task to Anytime.
- A whole Someday project shows as the project's own list. Its Someday
  status stays Things-only.

### Deadlines calendar

A separate, special calendar named **Deadlines** marks every Things
deadline on the calendar grid:

- **Contents:** one all-day `VEVENT` per open Things task that has a
  deadline (from any list, including Inbox and Someday). It's a calendar of
  events, not to-dos, so it shows in BusyCal's calendar grid like other
  events.
  - `UID` = `deadline-<things uuid>`
  - `SUMMARY` = the task title, verbatim (the calendar itself says it's a
    deadline, so nothing is added to the title)
  - `DTSTART;VALUE=DATE` = the deadline, and `DTEND` = the next day
  - `DESCRIPTION` = the task's notes, verbatim
  - `URL` = `things:///show?id=<uuid>`, so clicking it opens the task in
    Things
- **What you can do in BusyCal:**
  - **Drag or edit the date:** moves the Things deadline. This is the only
    field that writes back.
  - **Delete the event:** clears the Things deadline. The task itself is
    untouched.
  - **Edit the title or notes:** ignored. Things wins, and the event is
    re-rendered from the task.
  - **Create a new event:** refused (403). A deadline needs a task.
- **When it changes:**
  - A deadline is set, changed or cleared in Things.
  - A task is completed, canceled or trashed: its event drops off the
    calendar. The deadline stays on the task in Things.
- **Conflicts:** the same ETag field merge as for to-dos, with only one
  mergeable field (the date). If BusyCal moves the event while Things also
  changed the deadline, Things wins.
- The Deadlines calendar is the only VEVENT calendar. Every other calendar
  is VTODO-only.

### Completed history

Things keeps every completed task in its Logbook **forever**. The CalDAV
window never deletes, trashes or modifies anything in Things.

The window only decides what BusyCal sees. When a completed task ages past
30 days, the server stops listing it. CalDAV clients usually treat an item
missing from the server as removed, so **BusyCal will probably drop its
local copy** (to confirm in the spike). The record still exists in Things.
It just won't be in BusyCal's history.

- The server sends nothing that Things would treat as a delete when a task
  ages out. Only an explicit client DELETE trashes anything, and that is
  reversible.
- If you want BusyCal to keep the full history, set
  `CALDAV_COMPLETED_DAYS=0` and the whole Logbook is exposed. The cost is a
  heavier first sync. The spike measures how BusyCal handles your actual
  Logbook size, so you can decide with real numbers.
- Changing the window later is safe in both directions. Raising it brings
  old tasks back into BusyCal.

### Data Things can't hold: the sidecar

Things is the only store, so anything a client sends that Things can't hold
would vanish on the next fetch. To prevent that, `dav.db` keeps a **sidecar**
per task with the unmapped properties: `PRIORITY`, extra `VALARM`s,
`URL`, `GEO`, client `X-` props and so on. It also stores the client's own
UID and href when they differ from the Things UUID. On GET the sidecar is
merged back in, so the client gets back exactly what it stored. Mapped
fields always come from Things.

Normalization (Things wins, visible after the next GET):

- Alarms: the first non-default alarm that lands on the When date becomes
  the Things reminder. The others stay in the sidecar.
- **BusyCal default alarms** (`X-BUSYMAC-DEFAULT-ALARM:TRUE`): BusyCal adds
  one automatically (`PT0S`, a sound at the task time) whenever you give a
  to-do a time. These belong to the BusyCal-only task time, so they stay in
  the sidecar and never become a Things reminder.
- A date-time `DUE`/`DTSTART` with a `TZID` (how BusyCal writes timed
  to-dos): the When date is the **local date in that TZID**, not the UTC
  date.
- `RRULE` (before Phase 4): kept in the sidecar, not applied.
- `RELATED-TO` from a client: kept in the sidecar and ignored (there are no
  nested tasks).
- A `DUE` with a time: the date part becomes the When date, and the time
  is kept in the sidecar.

## Conflict resolution

### ETag + three-way field merge on every PUT

- The **ETag** is a hash of the rendered VTODO. When a hash is served, its
  canonical field values are saved in `etag_snapshots`. They're tiny,
  deduplicated by hash, and pruned after 30 days.
- On **PUT with `If-Match: <etag>`**, the snapshot for that ETag is the base:

| Client vs base | Things now vs base | Result |
|---|---|---|
| changed | same | apply the client value |
| same | changed | keep Things (the client's stale value is ignored) |
| changed | changed, equal | nothing to do |
| changed | changed, different | **Things wins**; logged |

  Sidecar properties (BusyCal-only, such as duration, task time, priority
  and extra alarms) are merged the same way per property. Things has no
  value for them, so in a true conflict **the latest PUT wins**.

  Instead of a 412 that clients often handle badly, the server merges and
  returns the new ETag. Any client edit that doesn't conflict always lands.
- If the snapshot is unknown (pruned), the server returns a standard
  `412 Precondition Failed`. The client re-fetches and retries.
- **PUT without If-Match.** The spike showed that **BusyCal never sends
  If-Match** on updates. If the current Things state were the base, a stale
  BusyCal copy would silently revert newer Things edits (BusyCal always sends
  the whole to-do). So instead, the base is **the last version served to
  that client** for that resource, recorded per (href, client) in
  `served_snapshots`. The client is identified by its **Basic-auth
  username**: each device signs in with its own (`mac`, `iphone`) and the
  same password. The spike showed that User-Agent can't be used: BusyCal
  for iOS (`BusyCal-6.7.6`) and BusyCal for Mac (`BusyCal-2026.3.3`) both
  claim `Mac OS X/13.0.1`, and the version number changes with every app
  update. The same merge
  table then applies. If no served snapshot exists, the current Things state
  is used as the base.
- **PUT with `If-None-Match: *`** on an existing UID returns 412 (standard
  CalDAV).

### Structural cases

| Scenario | Resolution |
|---|---|
| Client DELETE | Things **trash** (reversible). The bridge never purges anything in Things. |
| Edited in Things while a client deletes | Deleted with a stale If-Match → 412. The client re-syncs and sees the edit. |
| Task moved between lists | The spike showed BusyCal sends a DELETE from the old list, then a create (`If-None-Match: *`) in the new list with a **new UID**, keeping the original `CREATED` and title. A create whose `CREATED` + `SUMMARY` match a task trashed over CalDAV in the last 10 min **untrashes and moves** that task instead of creating a duplicate. This keeps its notes, deadline, checklist and heading, and the new UID is recorded in `uid_map`. The server therefore always renders `CREATED` from the Things creation date. |
| Task moved in Things | Leaves one calendar's listing and joins another. The client picks it up on its next poll. |
| Client creates a task with its own UID | A Things UUID is generated, and the client UID and href are kept in `uid_map`. The client keeps seeing its own UID. |
| MCP write and CalDAV write at the same time | Serialized by `historyMu`. The second write merges against fresh state. |
| Things history-key reset or mirror rebuild | DAV returns 503 until the resync finishes. Things UUIDs are stable. |

### Safety rails

1. **The server never purges in Things**, and DELETE always means trash.
2. **DELETE on a calendar is refused (403).** Delete areas and projects in
   Things.
3. **Mass-delete breaker:** more than 20 DELETEs within 60 s turns CalDAV
   writes off (503) until `POST /api/dav/resume`. This guards against a
   misconfigured client that wipes a list.
4. **`CALDAV_READ_ONLY=true`** for connecting a new client safely.
5. **Write log:** each DAV write records its UID, the fields changed, and
   any conflicts Things won.

## Change detection for clients

The spike showed that BusyCal asks for both `getctag` and `sync-token` on
every calendar. go-webdav v0.7.0 serves neither (it returns 404 for them),
so BusyCal falls back to re-downloading every list.

- **Per-calendar CTag** (`getctag`), computed as a hash of the member ETags,
  so BusyCal only refetches a list when it changed. go-webdav has no hook
  for adding calendar properties, so this needs either a small wrapper that
  answers `getctag` in PROPFIND, or a vendored fork of `caldav/server.go`.
  Decide when building Phase 2.
- `sync-collection` / `sync-token` (RFC 6578): optional. CTag plus ETags are
  enough for BusyCal.
- Reads call `syncForRead()`, so client polling is throttled by
  `SYNC_MIN_INTERVAL`, as with the MCP.

## Storage (`/data/dav.db`)

```
uid_map(things_uuid PK, client_uid, href, calendar)
etag_snapshots(etag PK, canon_json, created_at)
sidecar(things_uuid PK, props_ics)
recent_deletes(things_uuid, uid, created, summary, deleted_at)
served_snapshots(href, client, canon_json, served_at)
write_log(at, uid, fields, conflicts_json)
```

## Phases

1. **Spike (½–1 day).** A read-only go-webdav server with two hard-coded
   lists, connected to BusyCal on Mac and iOS. Record the exact iCalendar
   that BusyCal writes when you:
   - ✅ set a **date**: BusyCal writes `DTSTART` = `DUE` (same day)
   - ✅ set a **duration**: written as `X-BUSYMAC-TASK-DURATION` (seconds)
   - ✅ edit **tags**: written as `CATEGORIES`
   - ✅ seeded alarm, notes, completed and Someday all display correctly
   - ✅ alert on an all-day to-do: relative `TRIGGER:PT9H`
   - ✅ time on a date: `DTSTART`/`DUE` as `TZID` date-times, plus an
     automatic default alarm (`X-BUSYMAC-DEFAULT-ALARM`)
   - ✅ priority: `PRIORITY` (kept in the sidecar)
   - ✅ Cancelled: hidden by BusyCal
   - ✅ **Deadlines**: dragging an event sends a PUT with the new
     `DTSTART`, and deleting one sends a DELETE
   - ✅ create: `If-None-Match: *` with BusyCal's own UID
   - ✅ move: DELETE + create with a new UID, `CREATED` preserved
   - ✅ updates never carry `If-Match`
   - ✅ complete: `STATUS:COMPLETED` + `COMPLETED:<utc>` +
     `PERCENT-COMPLETE:100`. Uncheck: all three lines are removed.
   - ✅ BusyCal on iOS (plain `http` on the LAN works): it writes the same
     iCalendar as the Mac app.
   - ✅ Two-device conflict, recorded in `run4`:
     1. The iPhone completed a to-do.
     2. The Mac fetched it, reopened it, and set a 1h duration.
     3. The iPhone, **without re-fetching**, set a 30m duration and sent its
        stale `STATUS:COMPLETED`.
     4. The Mac set 2h.

     The naive last-write-wins test server let the iPhone's stale copy
     re-complete the task. With the per-device base, the iPhone's status is
     unchanged from its base and is ignored (the task stays open). Duration
     is a real conflict on a sidecar property, and the latest PUT wins.
     These four PUTs become the main merge test fixture.

   Also check:
   - what BusyCal does with a completed to-do once it leaves the server
     listing (dropped or kept locally)
   - how BusyCal handles the full Logbook (`CALDAV_COMPLETED_DAYS=0`)
   - whether go-webdav serves `getctag` / `sync-collection` and supports
     `MOVE`
2. **Read-only CalDAV.** Calendars, VTODO rendering, ETags and CTags,
   Basic auth, well-known discovery. Deploy and connect your clients.
3. **Task writes.** PUT create/update with the ETag merge, DELETE → trash,
   status, dates, deadline, reminder, tags, the Someday rules, the sidecar, the move-detection window, safety rails.
4. **Structure and repeats.** MKCALENDAR → new Things project (a name of the
   form `Area › X` places it in that area), and simple
   `RRULE` ⇄ Things repeat rules via `buildRepeatRule`.

## Test plan

- `vtodo/` table tests using BusyCal's captured iCalendar from the spike as
  fixtures (in `tapes/`): a render/parse round trip for every mapped field,
  the Someday rules, BusyCal time and duration surviving in the sidecar,
  alarm conversion across timezones and DST, and sidecar round trips.
- Deadlines calendar tests: render, date move → deadline, delete → deadline
  cleared, title edit ignored, create refused, and completed tasks dropping
  off.
- `Merge` table tests for every row of the conflict table.
- Backend tests with the existing fake `writeToHistory` seam: PUT with
  fresh, stale and unknown If-Match; DELETE + PUT move detection; the
  mass-delete breaker; read-only mode; an MCP write between a GET and a PUT.
- Client smoke test: a scripted session using go-webdav's own CalDAV client.

## Decisions

1. **Client:** BusyCal only. BusyCal has a single date field (`DUE`).
2. **Dates:** the Things When date → BusyCal's date. Deadlines live in the
   separate **Deadlines** event calendar, where dragging an event moves the
   deadline. A BusyCal time and duration are BusyCal-only (kept in the
   sidecar). Things reminders ⇄ BusyCal alarms.
3. **Headings and checklists:** hidden. They stay in Things only.
4. **Completed window:** 30 days to start. Things keeps the full history no
   matter what. BusyCal's local history depends on the window
   (`CALDAV_COMPLETED_DAYS=0` exposes everything).
5. **Canceled tasks:** sent as `CANCELLED`, which BusyCal hides. They exist
   only in the Things Logbook.
