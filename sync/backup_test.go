package sync

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	things "github.com/arthursoares/things-cloud-sdk"
)

func payloadItem(p map[string]any) things.Item {
	b, _ := json.Marshal(p)
	return things.Item{UUID: "x", Kind: things.ItemKindTask, P: b}
}

func TestItemTime(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p    map[string]any
		want time.Time
	}{
		{"md wins", map[string]any{"md": 1790000000.5, "cd": 1700000000.0}, time.Unix(1790000000, 5e8).UTC()},
		{"create without md", map[string]any{"md": nil, "cd": 1790115264.0}, time.Unix(1790115264, 0).UTC()},
		{"tombstone", map[string]any{"dloid": "a", "dld": 1790202027.0}, time.Unix(1790202027, 0).UTC()},
		{"index move", map[string]any{"ix": -775}, time.Time{}},
		{"schedule dates are not write times", map[string]any{"icsd": 1790121600, "sr": 1790121600}, time.Time{}},
	} {
		if got := ItemTime(payloadItem(tc.p)); !got.Equal(tc.want) {
			t.Errorf("%s: ItemTime = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCutAt(t *testing.T) {
	t.Parallel()
	at := func(sec float64) things.Item { return payloadItem(map[string]any{"md": sec}) }
	untimed := payloadItem(map[string]any{"ix": 3})
	items := []things.Item{at(100), untimed, at(200), untimed, at(300)}

	for _, tc := range []struct {
		asOf     int64
		wantCut  int
		wantLast int64
	}{
		{50, 0, 0},     // before everything
		{100, 2, 100},  // the untimed item after t=100 inherits 100
		{250, 4, 200},  // stops before t=300
		{1000, 5, 300}, // everything
	} {
		cut, last := cutAt(items, time.Unix(tc.asOf, 0), time.Time{})
		var lastSec int64
		if !last.IsZero() {
			lastSec = last.Unix()
		}
		if cut != tc.wantCut || lastSec != tc.wantLast {
			t.Errorf("asOf %d: cut %d (last %d), want %d (last %d)", tc.asOf, cut, lastSec, tc.wantCut, tc.wantLast)
		}
	}
}

func TestBackupTo(t *testing.T) {
	t.Parallel()
	s := openTestSyncer(t)
	payload, _ := json.Marshal(map[string]any{"tt": "Work", "ix": 0})
	if _, err := s.processItems([]things.Item{{UUID: "area-1", Kind: things.ItemKindArea3, Action: things.ItemActionCreated, P: payload}}, 0); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copy.db")
	if err := s.BackupTo(path); err != nil {
		t.Fatalf("BackupTo: %v", err)
	}
	copy, err := Open(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer copy.Close()
	if a, err := copy.State().Area("area-1"); err != nil || a == nil || a.Title != "Work" {
		t.Errorf("backup missing data: %+v, %v", a, err)
	}
}
