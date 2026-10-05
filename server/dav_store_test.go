package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/arthursoares/things-cloud-sdk/thingsdav"
)

// fakeRqlite serves the parts of rqlite's HTTP API the store uses, backed by
// a SQLite file. noLeader makes it answer like a node without a leader:
// writes, strong reads, backups and /readyz fail with 503, level=none works.
type fakeRqlite struct {
	t        *testing.T
	db       *sqliteSQL
	srv      *httptest.Server
	noLeader atomic.Bool
	down     atomic.Bool // the node itself unreachable (500s everywhere)
	mu       gosync.Mutex
	execs    int
	user     string
	pass     string
}

func newFakeRqlite(t *testing.T) *fakeRqlite {
	t.Helper()
	db, err := openSQLiteSQL(filepath.Join(t.TempDir(), "rqlite.db"))
	if err != nil {
		t.Fatal(err)
	}
	// rqlite starts empty; the store creates its schema.
	for _, tbl := range davStoreTables {
		db.db.Exec(`DROP TABLE ` + tbl)
	}
	f := &fakeRqlite{t: t, db: db}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(func() { f.srv.Close(); db.close() })
	return f
}

func (f *fakeRqlite) url() string { return f.srv.URL }

func (f *fakeRqlite) serve(w http.ResponseWriter, r *http.Request) {
	if f.down.Load() {
		http.Error(w, "node down", http.StatusBadGateway)
		return
	}
	if f.user != "" {
		if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}
	noLeader := f.noLeader.Load()
	unavailable := func() { http.Error(w, "leader not found", http.StatusServiceUnavailable) }
	switch r.URL.Path {
	case "/readyz":
		if noLeader {
			unavailable()
			return
		}
		io.WriteString(w, "[+]node ok\n[+]leader ok\n[+]store ok")
	case "/db/backup":
		if noLeader {
			unavailable()
			return
		}
		tmp := filepath.Join(f.t.TempDir(), "backup.db")
		if err := f.db.backupTo(tmp); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		http.ServeFile(w, r, tmp)
	case "/db/execute", "/db/query":
		if noLeader && (r.URL.Path == "/db/execute" || r.URL.Query().Get("level") != "none") {
			unavailable()
			return
		}
		var body [][]any
		dec := json.NewDecoder(r.Body)
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		stmts := make([]davStmt, len(body))
		for i, b := range body {
			stmts[i] = davStmt{SQL: b[0].(string), Args: sqliteArgs(b[1:])}
		}
		var results []map[string]any
		if r.URL.Path == "/db/execute" {
			f.mu.Lock()
			f.execs++
			f.mu.Unlock()
			n, err := f.db.exec(stmts...)
			if err != nil {
				results = append(results, map[string]any{"error": err.Error()})
			}
			for _, x := range n {
				results = append(results, map[string]any{"rows_affected": x})
			}
		} else {
			for _, s := range stmts {
				rows, err := f.db.query(true, s)
				if err != nil {
					results = append(results, map[string]any{"error": err.Error()})
					continue
				}
				res := map[string]any{"columns": []string{}}
				if len(rows) > 0 {
					res["values"] = rows
				}
				results = append(results, res)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeRqlite) count(table string) int64 {
	rows, err := f.db.query(true, stmt("", `SELECT COUNT(*) FROM `+table))
	if err != nil {
		f.t.Fatalf("count %s: %v", table, err)
	}
	return sqlInt(rows[0][0])
}

// newTestDAVStore opens the store the DAV tests run against: SQLite by
// default, the fake rqlite with TEST_DAV_STORE=fake-rqlite, or a real
// rqlite node with TEST_DAV_STORE=http://host:4001.
func newTestDAVStore(t *testing.T) *davStore {
	t.Helper()
	var s *davStore
	var err error
	switch mode := os.Getenv("TEST_DAV_STORE"); {
	case mode == "" || mode == "sqlite":
		s, err = openDAVStore(filepath.Join(t.TempDir(), "dav.db"))
	case mode == "fake-rqlite":
		s = rqliteTestStore(t, newFakeRqlite(t).url())
	default:
		s = rqliteTestStore(t, mode)
		for _, tbl := range davStoreTables {
			s.sql.exec(stmt("", `DELETE FROM `+tbl))
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func rqliteTestStore(t *testing.T, url string) *davStore {
	t.Helper()
	rq, err := newRqliteSQL(url, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := rq.ensureSchema(); err != nil {
		t.Fatal(err)
	}
	return &davStore{sql: rq}
}

// storeBackends runs fn against every store backend.
func storeBackends(t *testing.T, fn func(t *testing.T, s *davStore)) {
	t.Run("sqlite", func(t *testing.T) {
		s, err := openDAVStore(filepath.Join(t.TempDir(), "dav.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		fn(t, s)
	})
	t.Run("rqlite", func(t *testing.T) {
		fn(t, rqliteTestStore(t, newFakeRqlite(t).url()))
	})
	t.Run("failover", func(t *testing.T) {
		fo, err := newFailoverSQL(mustRqlite(t, newFakeRqlite(t).url()), filepath.Join(t.TempDir(), "fallback.db"))
		if err != nil {
			t.Fatal(err)
		}
		s := &davStore{sql: fo}
		defer s.Close()
		fn(t, s)
	})
	if u := os.Getenv("TEST_RQLITE_URL"); u != "" {
		t.Run("real-rqlite", func(t *testing.T) {
			s := rqliteTestStore(t, u)
			for _, tbl := range davStoreTables {
				s.sql.exec(stmt("", `DELETE FROM `+tbl))
			}
			fn(t, s)
		})
	}
}

func mustRqlite(t *testing.T, url string) *rqliteSQL {
	t.Helper()
	rq, err := newRqliteSQL(url, "")
	if err != nil {
		t.Fatal(err)
	}
	return rq
}

func TestDAVStoreOperations(t *testing.T) {
	storeBackends(t, func(t *testing.T, s *davStore) {
		objs := []*thingsdav.Object{{Key: "task:A", Snapshot: `{"title":"a"}`}, {Key: "task:B", Snapshot: `{"title":"b"}`}}
		if err := s.recordServed("mac", objs); err != nil {
			t.Fatal(err)
		}
		var got map[string]string
		if ok, err := s.snapshot("mac", "task:B", &got); !ok || err != nil || got["title"] != "b" {
			t.Fatalf("snapshot = %v %v %v", got, ok, err)
		}
		if ok, _ := s.snapshot("iphone", "task:B", &got); ok {
			t.Error("bases are per device")
		}
		if err := s.setSnapshot("mac", "task:B", map[string]string{"title": "b2"}); err != nil {
			t.Fatal(err)
		}
		s.snapshot("mac", "task:B", &got)
		if got["title"] != "b2" {
			t.Errorf("setSnapshot not stored: %v", got)
		}

		gen := s.generation()
		if err := s.setAlias("A", "client-uid", "client.ics"); err != nil {
			t.Fatal(err)
		}
		if err := s.setSidecar("A", "BEGIN:VCALENDAR"); err != nil {
			t.Fatal(err)
		}
		if s.generation() != gen+2 {
			t.Errorf("generation %d → %d, want +2", gen, s.generation())
		}
		al, err := s.aliases()
		if err != nil || al["A"] != (davAlias{uid: "client-uid", name: "client.ics"}) {
			t.Errorf("aliases = %v, %v", al, err)
		}
		sc, _ := s.sidecars()
		if sc["A"] != "BEGIN:VCALENDAR" {
			t.Errorf("sidecars = %v", sc)
		}
		s.setSidecar("A", "")
		if sc, _ := s.sidecars(); len(sc) != 0 {
			t.Errorf("empty sidecar not removed: %v", sc)
		}

		created := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
		if err := s.recordDelete("A", created, "title"); err != nil {
			t.Fatal(err)
		}
		since := time.Now().Add(-time.Minute)
		if uuid, err := s.takeRecentDelete(created, "title", since); uuid != "A" || err != nil {
			t.Fatalf("takeRecentDelete = %q, %v", uuid, err)
		}
		if uuid, _ := s.takeRecentDelete(created, "title", since); uuid != "" {
			t.Error("a recent delete must be taken only once")
		}

		s.logWrite("mac", "task:A", "update", map[string]int{"n": 1})
		counts, err := s.tableCounts()
		if err != nil || counts["write_log"] != 1 || counts["snapshots"] != 2 || counts["aliases"] != 1 {
			t.Errorf("counts = %v, %v", counts, err)
		}

		now := time.Now()
		if ok, err := s.acquireLease("auto-backup", "us1", now, time.Minute); !ok || err != nil {
			t.Fatalf("first lease = %v, %v", ok, err)
		}
		if ok, _ := s.acquireLease("auto-backup", "us2", now, time.Minute); ok {
			t.Error("a live lease must not be taken by another holder")
		}
		if ok, _ := s.acquireLease("auto-backup", "us1", now, time.Minute); !ok {
			t.Error("the holder may renew its lease")
		}
		if ok, _ := s.acquireLease("auto-backup", "us2", now.Add(2*time.Minute), time.Minute); !ok {
			t.Error("an expired lease must be takeable")
		}

		path := filepath.Join(t.TempDir(), "copy.db")
		if err := s.backupTo(path); err != nil {
			t.Fatal(err)
		}
		copy, err := openDAVStore(path)
		if err != nil {
			t.Fatal(err)
		}
		defer copy.Close()
		if c, _ := copy.tableCounts(); c["snapshots"] != 2 {
			t.Errorf("backup has %v", c)
		}
	})
}

// A device can download through one node and upload through another: the
// merge base must be visible to every node right away.
func TestDAVStoreSharedAcrossNodes(t *testing.T) {
	rq := newFakeRqlite(t)
	us1, us2 := rqliteTestStore(t, rq.url()), rqliteTestStore(t, rq.url())
	us1.recordServed("iphone", []*thingsdav.Object{{Key: "task:A", Snapshot: `{"title":"a"}`}})
	var got map[string]string
	if ok, err := us2.snapshot("iphone", "task:A", &got); !ok || err != nil {
		t.Fatalf("base written via us1 not visible via us2: %v %v", ok, err)
	}
	us1.recordDelete("A", time.Unix(1000, 0), "moved")
	if uuid, _ := us2.takeRecentDelete(time.Unix(1000, 0), "moved", time.Now().Add(-time.Minute)); uuid != "A" {
		t.Error("recent delete via us1 not visible via us2")
	}
	if uuid, _ := us1.takeRecentDelete(time.Unix(1000, 0), "moved", time.Now().Add(-time.Minute)); uuid != "" {
		t.Error("both nodes took the same delete")
	}
}

func TestRqliteCredentials(t *testing.T) {
	rq := newFakeRqlite(t)
	rq.user, rq.pass = "app", "s3cret"
	if _, err := rqliteTestStoreErr(rq.url()); err == nil {
		t.Error("expected 401 without credentials")
	}
	withCreds := strings.Replace(rq.url(), "http://", "http://app:s3cret@", 1)
	if _, err := rqliteTestStoreErr(withCreds); err != nil {
		t.Errorf("credentials in URL: %v", err)
	}
	t.Setenv("DAV_RQLITE_USER", "app")
	t.Setenv("DAV_RQLITE_PASSWORD", "s3cret")
	if _, err := rqliteTestStoreErr(rq.url()); err != nil {
		t.Errorf("credentials from env: %v", err)
	}
}

func rqliteTestStoreErr(url string) (*rqliteSQL, error) {
	rq, err := newRqliteSQL(url, "")
	if err != nil {
		return nil, err
	}
	return rq, rq.ensureSchema()
}

// Writes made through rqlite before the outage, plus those made while it
// had no leader, must all be readable during the outage and land in rqlite
// once it recovers.
func setProbeEvery(t *testing.T, d time.Duration) {
	orig := davFailoverProbeEvery
	davFailoverProbeEvery = d
	t.Cleanup(func() { davFailoverProbeEvery = orig })
}

func TestFailoverServesAndReplays(t *testing.T) {
	rq := newFakeRqlite(t)
	setProbeEvery(t, 20*time.Millisecond)
	fo, err := newFailoverSQL(mustRqlite(t, rq.url()), filepath.Join(t.TempDir(), "fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &davStore{sql: fo}
	defer s.Close()

	s.setAlias("Before", "uid-before", "before.ics")
	s.recordServed("mac", []*thingsdav.Object{{Key: "task:Before", Snapshot: `{"title":"before"}`}})

	rq.noLeader.Store(true) // every US node down: no quorum
	if err := s.writable(); err != nil {
		t.Fatalf("failover store must stay writable: %v", err)
	}
	if err := s.setAlias("During", "uid-during", "during.ics"); err != nil {
		t.Fatalf("write during outage: %v", err)
	}
	s.setSnapshot("mac", "task:Before", map[string]string{"title": "edited during outage"})
	s.logWrite("mac", "task:During", "create", nil)
	if !fo.isDegraded() || !strings.HasPrefix(s.mode(), "local-fallback") {
		t.Fatalf("mode = %s", s.mode())
	}
	al, err := s.aliases()
	if err != nil || al["Before"].uid != "uid-before" || al["During"].uid != "uid-during" {
		t.Fatalf("aliases during outage = %v, %v", al, err)
	}
	var base map[string]string
	if ok, _ := s.snapshot("mac", "task:Before", &base); !ok || base["title"] != "edited during outage" {
		t.Errorf("snapshot during outage = %v", base)
	}
	if rq.count("aliases") != 1 {
		t.Fatal("nothing may reach rqlite while it has no leader")
	}
	path := filepath.Join(t.TempDir(), "outage-backup.db")
	if err := s.backupTo(path); err != nil {
		t.Errorf("backup during outage: %v", err)
	}

	rq.noLeader.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for fo.isDegraded() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fo.isDegraded() {
		t.Fatal("did not switch back to rqlite")
	}
	if rq.count("aliases") != 2 || rq.count("write_log") != 1 {
		t.Errorf("journal not replayed: aliases=%d write_log=%d", rq.count("aliases"), rq.count("write_log"))
	}
	other := rqliteTestStore(t, rq.url())
	if ok, _ := other.snapshot("mac", "task:Before", &base); !ok || base["title"] != "edited during outage" {
		t.Errorf("replayed snapshot = %v", base)
	}
	if fo.pending() != 0 {
		t.Errorf("journal not emptied: %d", fo.pending())
	}
}

// The journal keeps one entry per row, so a long outage of BusyCal polls
// doesn't grow it without bound.
func TestFailoverJournalKeepsLatestPerRow(t *testing.T) {
	rq := newFakeRqlite(t)
	rq.noLeader.Store(true)
	setProbeEvery(t, time.Hour)
	fo, err := newFailoverSQL(mustRqlite(t, rq.url()), filepath.Join(t.TempDir(), "fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	s := &davStore{sql: fo}
	defer s.Close()
	for i := 0; i < 50; i++ {
		s.recordServed("mac", []*thingsdav.Object{{Key: "task:A", Snapshot: `{}`}, {Key: "task:B", Snapshot: `{}`}})
	}
	if n := fo.pending(); n != 2 {
		t.Errorf("journal has %d entries, want 2", n)
	}
}

// A node restarted mid-outage still has its journal and keeps serving the
// local copy until the replay succeeds.
func TestFailoverJournalSurvivesRestart(t *testing.T) {
	rq := newFakeRqlite(t)
	local := filepath.Join(t.TempDir(), "fallback.db")
	setProbeEvery(t, time.Hour)
	fo, _ := newFailoverSQL(mustRqlite(t, rq.url()), local)
	rq.noLeader.Store(true)
	(&davStore{sql: fo}).setAlias("X", "uid", "x.ics")
	fo.close()

	davFailoverProbeEvery = 20 * time.Millisecond
	fo2, err := newFailoverSQL(mustRqlite(t, rq.url()), local)
	if err != nil {
		t.Fatal(err)
	}
	defer fo2.close()
	if !fo2.isDegraded() {
		t.Fatal("restart with a pending journal must start on the local copy")
	}
	rq.noLeader.Store(false)
	deadline := time.Now().Add(5 * time.Second)
	for fo2.isDegraded() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rq.count("aliases") != 1 {
		t.Error("journal from before the restart was not replayed")
	}
}

// When the local rqlite node itself is gone, the copy refreshed earlier is
// what the node serves.
func TestFailoverWithLocalNodeDown(t *testing.T) {
	rq := newFakeRqlite(t)
	setProbeEvery(t, time.Hour)
	fo, _ := newFailoverSQL(mustRqlite(t, rq.url()), filepath.Join(t.TempDir(), "fallback.db"))
	s := &davStore{sql: fo}
	defer s.Close()
	s.setAlias("A", "uid-a", "a.ics")
	// Seed as the periodic refresh would.
	fo.mu.Lock()
	fo.seedLocked(fo.primary.queryLocal)
	fo.mu.Unlock()

	rq.down.Store(true)
	al, err := s.aliases()
	if err != nil || al["A"].uid != "uid-a" {
		t.Errorf("aliases with the local node down = %v, %v", al, err)
	}
}

// Without the fallback, a store with no leader refuses CalDAV writes with a
// 503 before anything is written to Things.
func TestDAVWriteUnavailableWithoutLeader(t *testing.T) {
	f := newFakeDAV(t)
	rq := newFakeRqlite(t)
	davDB = rqliteTestStore(t, rq.url())
	path := inboxTask(f, "T1", "old")
	body := f.get("mac", path)
	rq.noLeader.Store(true)
	code, resp := f.do(http.MethodPut, "mac", path, replaceLine(body, "SUMMARY:", "SUMMARY:new"), nil)
	if code != http.StatusServiceUnavailable || !strings.Contains(resp, "no leader") {
		t.Errorf("PUT without a leader = %d %s", code, resp)
	}
	if len(f.writes) != 0 {
		t.Errorf("nothing may reach Things: %+v", f.writes)
	}
	// Reads keep working.
	f.get("mac", path)
}

// The whole BusyCal round trip through the failover store: download via
// rqlite, outage, edit, recovery.
func TestDAVEditDuringOutage(t *testing.T) {
	f := newFakeDAV(t)
	rq := newFakeRqlite(t)
	setProbeEvery(t, time.Hour)
	fo, err := newFailoverSQL(mustRqlite(t, rq.url()), filepath.Join(t.TempDir(), "fallback.db"))
	if err != nil {
		t.Fatal(err)
	}
	davDB = &davStore{sql: fo}
	defer davDB.Close()

	path := inboxTask(f, "T1", "original")
	body := f.get("mac", path)
	f.tasks["T1"].Title = "renamed in Things"
	rq.noLeader.Store(true)
	// The stale Mac copy sets a date; its old title must not win, which
	// needs the base recorded before the outage.
	f.put("mac", path, addLine(addLine(body, "DUE;VALUE=DATE:20300105"), "DTSTART;VALUE=DATE:20300105"), nil)
	u := f.onlyUpdate()
	if _, ok := u["tt"]; ok {
		t.Errorf("stale title overwrote Things during the outage: %+v", u)
	}
	if fo.pending() == 0 {
		t.Error("the edit's new base was not journaled")
	}
}
