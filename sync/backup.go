package sync

import (
	"encoding/json"
	"fmt"
	"time"

	things "github.com/arthursoares/things-cloud-sdk"
)

// BackupTo writes a consistent copy of the mirror to path (which must not
// exist) using SQLite's VACUUM INTO. It runs alongside reads and blocks
// syncs only for the duration of the copy.
func (s *Syncer) BackupTo(path string) error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, err := s.rawDB.Exec(`VACUUM INTO ?`, path)
	return err
}

// ItemTime estimates when a history item was written, from the timestamps
// its payload carries: md (modified), else cd (created, on creates), else
// dld (tombstones). It returns the zero time for items without one (index
// moves, Task7 bookkeeping), which inherit the previous item's time.
func ItemTime(item things.Item) time.Time {
	var p struct {
		MD  *float64 `json:"md"`
		CD  *float64 `json:"cd"`
		DLD *float64 `json:"dld"`
	}
	if len(item.P) == 0 || json.Unmarshal(item.P, &p) != nil {
		return time.Time{}
	}
	for _, ts := range []*float64{p.MD, p.CD, p.DLD} {
		if ts != nil && *ts > 0 {
			sec := int64(*ts)
			return time.Unix(sec, int64((*ts-float64(sec))*1e9)).UTC()
		}
	}
	return time.Time{}
}

// cutAt returns how many of items were written at or before asOf, given the
// time of the item preceding them, and the time of the last item kept.
// History is in server order, so replay stops at the first item past asOf.
func cutAt(items []things.Item, asOf, prev time.Time) (int, time.Time) {
	last := prev
	for i, item := range items {
		t := ItemTime(item)
		if t.IsZero() {
			t = last
		}
		if t.After(asOf) {
			return i, last
		}
		last = t
	}
	return len(items), last
}

// RebuildResult describes a point-in-time rebuild.
type RebuildResult struct {
	AsOf        time.Time // requested moment
	Items       int       // history items replayed
	ServerIndex int       // history index the rebuild stopped at
	FirstItem   time.Time // time of the earliest replayed item (history start)
	LastItem    time.Time // time of the last replayed item
	HistoryID   string
}

// RebuildAt replays the account's Things Cloud history into a fresh mirror
// at dbPath, stopping before the first item written after asOf. The result
// is Things as it stood at that moment, as far back as Things Cloud still
// keeps history. Item times come from payload timestamps (see ItemTime), so
// the cut is accurate to when devices wrote, not when the server received.
func RebuildAt(dbPath string, client *things.Client, asOf time.Time) (RebuildResult, error) {
	res := RebuildResult{AsOf: asOf}
	s, err := Open(dbPath, client)
	if err != nil {
		return res, err
	}
	defer s.Close()

	h, err := client.OwnHistory()
	if err != nil {
		return res, fmt.Errorf("resolve history: %w", err)
	}
	res.HistoryID = h.ID

	start := 0
	var last time.Time
	for {
		items, more, err := h.Items(things.ItemsOptions{StartIndex: start})
		if err != nil {
			return res, fmt.Errorf("fetch history from %d: %w", start, err)
		}
		if len(items) == 0 {
			break
		}
		if res.FirstItem.IsZero() {
			for _, item := range items {
				if t := ItemTime(item); !t.IsZero() {
					res.FirstItem = t
					break
				}
			}
		}
		cut, lastKept := cutAt(items, asOf, last)
		if _, err := s.processItems(items[:cut], start); err != nil {
			return res, err
		}
		res.Items += cut
		last = lastKept
		if cut < len(items) {
			res.ServerIndex = start + cut
			break
		}
		start = h.LoadedServerIndex
		res.ServerIndex = start
		if !more {
			break
		}
	}
	res.LastItem = last
	if err := s.saveSyncState(h.ID, res.ServerIndex); err != nil {
		return res, err
	}
	return res, s.purgeDeleted()
}
