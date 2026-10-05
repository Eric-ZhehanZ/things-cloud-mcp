package main

// failoverSQL keeps CalDAV fully working on a node that loses the rqlite
// leader — in particular Romania when every US node is down, since a lone
// non-voter (or a lone voter of three) can never form a Raft quorum.
//
// Normally every statement goes to rqlite and a local SQLite copy is
// refreshed from the cluster every few minutes. When rqlite can't serve
// (no leader, node unreachable) the node switches to the local copy: it is
// re-seeded from the local rqlite node's own data (level=none needs no
// leader), reads and writes run locally, and each write is also recorded in
// a journal, keeping only the latest write per row. Once rqlite has a
// leader again the journal is replayed into the cluster and the node goes
// back to rqlite.
//
// Cloudflare only sends traffic to Romania once every US connector is gone,
// so during an outage one node writes and the replay can't clash with
// anything. If two partitioned nodes both take writes, the later replay
// wins per row; Things itself stays consistent either way, since only
// merge bases, aliases and sidecars live here.

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	gosync "sync"
	"time"
)

const failoverJournalSchema = `
CREATE TABLE IF NOT EXISTS journal (
    seq  INTEGER PRIMARY KEY AUTOINCREMENT,
    key  TEXT NOT NULL,
    stmt TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS journal_key ON journal (key);
`

// davMirrorTables are copied from rqlite into the local fallback; the
// write log is only appended to.
var davMirrorTables = []string{"snapshots", "aliases", "sidecars", "recent_deletes", "meta", "leases"}

// davFailoverProbeEvery is how often a degraded node checks for a leader.
// Overridable in tests.
var davFailoverProbeEvery = 5 * time.Second

type failoverSQL struct {
	primary *rqliteSQL
	local   *sqliteSQL

	mu       gosync.RWMutex
	degraded bool
	probing  bool
	since    time.Time

	probeEvery   time.Duration
	refreshEvery time.Duration
	done         chan struct{}
	closeOnce    gosync.Once
}

func newFailoverSQL(primary *rqliteSQL, localPath string) (*failoverSQL, error) {
	local, err := openSQLiteSQL(localPath)
	if err != nil {
		return nil, err
	}
	if _, err := local.db.Exec(failoverJournalSchema); err != nil {
		local.close()
		return nil, fmt.Errorf("fallback journal %s: %w", localPath, err)
	}
	f := &failoverSQL{
		primary:      primary,
		local:        local,
		probeEvery:   davFailoverProbeEvery,
		refreshEvery: 5 * time.Minute,
		done:         make(chan struct{}),
	}
	if n := f.pending(); n > 0 {
		// Writes from an earlier outage haven't reached the cluster yet:
		// keep serving the local copy until they're replayed.
		log.Printf("[DAV] %d journaled writes from a previous outage; serving the local copy until rqlite has a leader", n)
		f.degraded, f.since = true, time.Now()
		f.startProbe()
	} else if err := primary.ensureSchema(); err != nil {
		log.Printf("[DAV] rqlite not ready at start: %v", err)
	}
	go f.refreshLoop()
	return f, nil
}

func (f *failoverSQL) pending() int64 {
	var n int64
	f.local.db.QueryRow(`SELECT COUNT(*) FROM journal`).Scan(&n)
	return n
}

func (f *failoverSQL) exec(stmts ...davStmt) ([]int64, error) {
	f.mu.RLock()
	if !f.degraded {
		n, err := f.primary.exec(stmts...)
		f.mu.RUnlock()
		if err == nil || !isStoreUnavailable(err) {
			return n, err
		}
		f.degrade(err)
	} else {
		f.mu.RUnlock()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.degraded { // recovered meanwhile
		return f.primary.exec(stmts...)
	}
	return f.execJournaled(stmts)
}

// execJournaled applies stmts locally and journals them, in one transaction.
func (f *failoverSQL) execJournaled(stmts []davStmt) ([]int64, error) {
	tx, err := f.local.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := execInTx(tx, stmts)
	if err != nil {
		return nil, err
	}
	for _, s := range stmts {
		if s.Key != "" {
			if _, err := tx.Exec(`DELETE FROM journal WHERE key = ?`, s.Key); err != nil {
				return nil, err
			}
		}
		b, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO journal (key, stmt) VALUES (?, ?)`, s.Key, string(b)); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

func (f *failoverSQL) query(strong bool, s davStmt) ([][]any, error) {
	f.mu.RLock()
	if !f.degraded {
		rows, err := f.primary.query(strong, s)
		f.mu.RUnlock()
		if err == nil || !isStoreUnavailable(err) {
			return rows, err
		}
		f.degrade(err)
	} else {
		f.mu.RUnlock()
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	if !f.degraded {
		return f.primary.query(strong, s)
	}
	return f.local.query(strong, s)
}

// degrade switches to the local copy, first re-seeding it from this node's
// own rqlite data when that is reachable.
func (f *failoverSQL) degrade(cause error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.degraded {
		return
	}
	if err := f.seedLocked(f.primary.queryLocal); err != nil {
		log.Printf("[DAV] couldn't re-seed the local copy (%v); using it as last refreshed", err)
	}
	f.degraded, f.since = true, time.Now()
	log.Printf("[DAV] rqlite unavailable (%v); CalDAV store now runs on the local copy and journals writes", cause)
	f.startProbe()
}

// seedLocked replaces the local mirror tables with rows read by read.
// Callers hold f.mu (or own f exclusively).
func (f *failoverSQL) seedLocked(read func(davStmt) ([][]any, error)) error {
	data := map[string][][]any{}
	for _, t := range davMirrorTables {
		rows, err := read(stmt("", `SELECT * FROM `+t))
		if err != nil {
			if isNoSuchTable(err) {
				continue
			}
			return err
		}
		data[t] = rows
	}
	tx, err := f.local.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, t := range davMirrorTables {
		if _, err := tx.Exec(`DELETE FROM ` + t); err != nil {
			return err
		}
		for _, row := range data[t] {
			ph := strings.TrimSuffix(strings.Repeat("?, ", len(row)), ", ")
			if _, err := tx.Exec(`INSERT INTO `+t+` VALUES (`+ph+`)`, sqliteArgs(row)...); err != nil {
				return fmt.Errorf("seed %s: %w", t, err)
			}
		}
	}
	return tx.Commit()
}

// sqliteArgs turns rqlite JSON values into driver values.
func sqliteArgs(row []any) []any {
	out := make([]any, len(row))
	for i, v := range row {
		if n, ok := v.(json.Number); ok {
			if x, err := n.Int64(); err == nil {
				out[i] = x
			} else {
				out[i], _ = n.Float64()
			}
			continue
		}
		out[i] = v
	}
	return out
}

func (f *failoverSQL) startProbe() {
	if f.probing {
		return
	}
	f.probing = true
	go func() {
		t := time.NewTicker(f.probeEvery)
		defer t.Stop()
		for {
			select {
			case <-f.done:
				return
			case <-t.C:
				if f.primary.ready() && f.recover() {
					return
				}
			}
		}
	}()
}

// recover replays the journal into rqlite and switches back to it.
func (f *failoverSQL) recover() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	replayed := 0
	for {
		rows, err := f.local.db.Query(`SELECT seq, stmt FROM journal ORDER BY seq LIMIT 200`)
		if err != nil {
			log.Printf("[DAV] read journal: %v", err)
			return false
		}
		var seqs []int64
		var stmts []davStmt
		for rows.Next() {
			var seq int64
			var raw string
			if err := rows.Scan(&seq, &raw); err != nil {
				rows.Close()
				return false
			}
			dec := json.NewDecoder(strings.NewReader(raw))
			dec.UseNumber()
			var s davStmt
			if err := dec.Decode(&s); err != nil {
				log.Printf("[DAV] dropping unreadable journal entry %d: %v", seq, err)
				f.local.db.Exec(`DELETE FROM journal WHERE seq = ?`, seq)
				continue
			}
			seqs, stmts = append(seqs, seq), append(stmts, s)
		}
		rows.Close()
		if len(stmts) == 0 {
			break
		}
		if _, err := f.primary.exec(stmts...); err != nil {
			log.Printf("[DAV] journal replay paused after %d writes: %v", replayed, err)
			return false
		}
		last := seqs[len(seqs)-1]
		if _, err := f.local.db.Exec(`DELETE FROM journal WHERE seq <= ?`, last); err != nil {
			log.Printf("[DAV] trim journal: %v", err)
			return false
		}
		replayed += len(stmts)
	}
	f.degraded, f.probing = false, false
	log.Printf("[DAV] rqlite has a leader again; replayed %d journaled writes after %s on the local copy",
		replayed, time.Since(f.since).Round(time.Second))
	return true
}

// refreshLoop keeps the local copy close to the cluster, so it is useful
// even when the local rqlite node itself is down at failover time.
func (f *failoverSQL) refreshLoop() {
	refresh := func() {
		f.mu.RLock()
		degraded := f.degraded
		f.mu.RUnlock()
		if degraded {
			return
		}
		data := map[string][][]any{}
		for _, t := range davMirrorTables {
			rows, err := f.primary.query(false, stmt("", `SELECT * FROM `+t))
			if err != nil {
				return
			}
			data[t] = rows
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.degraded {
			return
		}
		if err := f.seedLocked(func(s davStmt) ([][]any, error) {
			return data[strings.TrimPrefix(s.SQL, "SELECT * FROM ")], nil
		}); err != nil {
			log.Printf("[DAV] refresh local copy: %v", err)
		}
	}
	select {
	case <-f.done:
		return
	case <-time.After(10 * time.Second):
	}
	refresh()
	t := time.NewTicker(f.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-f.done:
			return
		case <-t.C:
			refresh()
		}
	}
}

func (f *failoverSQL) backupTo(path string) error {
	f.mu.RLock()
	degraded := f.degraded
	f.mu.RUnlock()
	if !degraded {
		err := f.primary.backupTo(path)
		if err == nil || !isStoreUnavailable(err) {
			return err
		}
	}
	// Backups during an outage come from this node's copy.
	return f.local.backupTo(path)
}

func (f *failoverSQL) mode() string {
	f.mu.RLock()
	defer f.mu.RUnlock()
	if f.degraded {
		return fmt.Sprintf("local-fallback since %s (%d writes to replay)", f.since.UTC().Format(time.RFC3339), f.pending())
	}
	return "rqlite"
}

func (f *failoverSQL) isDegraded() bool {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.degraded
}

func (f *failoverSQL) close() error {
	f.closeOnce.Do(func() { close(f.done) })
	return f.local.close()
}
