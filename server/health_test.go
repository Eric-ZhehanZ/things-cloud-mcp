package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHealthz(t *testing.T) {
	t.Setenv("NODE_NAME", "ro")
	serverReady.Store(false)
	t.Cleanup(func() { serverReady.Store(false) })

	rec := httptest.NewRecorder()
	handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("before the initial sync = %d", rec.Code)
	}

	serverReady.Store(true)
	rec = httptest.NewRecorder()
	handleHealthz(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body map[string]string
	json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusOK || body["node"] != "ro" || body["status"] != "ok" {
		t.Errorf("healthz = %d %v", rec.Code, body)
	}
}

// Every CalDAV write syncs from Things Cloud first, even inside the read
// throttle: another node may have just written.
func TestDAVWriteForcesFreshSync(t *testing.T) {
	f := newFakeDAV(t)
	t.Setenv("SYNC_MIN_INTERVAL", "3600")
	syncs := 0
	doSync = func() error { syncs++; return nil }
	path := inboxTask(f, "T1", "old")
	body := f.get("mac", path)
	before := syncs
	f.put("mac", path, replaceLine(body, "SUMMARY:", "SUMMARY:new"), nil)
	// One sync before the merge, one after the Things write.
	if syncs-before != 2 {
		t.Errorf("PUT ran %d syncs, want 2", syncs-before)
	}
	before = syncs
	if code, _ := f.do(http.MethodDelete, "mac", path, "", nil); code/100 != 2 {
		t.Fatalf("DELETE = %d", code)
	}
	if syncs-before != 2 {
		t.Errorf("DELETE ran %d syncs, want 2", syncs-before)
	}
}

func TestDAVWriteRefusedWhenThingsUnreachable(t *testing.T) {
	f := newFakeDAV(t)
	path := inboxTask(f, "T1", "old")
	body := f.get("mac", path)
	doSync = func() error { return errTest("things cloud down") }
	code, resp := f.do(http.MethodPut, "mac", path, replaceLine(body, "SUMMARY:", "SUMMARY:new"), nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(resp, "Things Cloud") {
		t.Errorf("PUT = %d %s", code, resp)
	}
	if len(f.writes) != 0 {
		t.Errorf("merged against stale state: %+v", f.writes)
	}
}

type errTest string

func (e errTest) Error() string { return string(e) }
