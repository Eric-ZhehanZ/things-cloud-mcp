package main

// davStore persists what CalDAV needs beyond Things itself:
//
//   - snapshots:      per device, the fields of each object it last
//                     downloaded — the base of that device's next edit
//   - aliases:        the UID and file name a client created a task under
//   - sidecars:       client properties Things can't hold (duration, time of
//                     day, priority, extra alarms), merged back on render
//   - recent_deletes: tasks trashed over CalDAV, to recognize list moves
//   - write_log:      every CalDAV write, with the conflicts Things won
//   - meta:           a generation counter bumped by alias/sidecar changes,
//                     so every node's render cache notices them
//   - leases:         short-lived locks, e.g. who takes the daily backup
//
// The SQL lives here once; where it runs is a davSQL backend:
//
//   - sqliteSQL: a local SQLite file (DAV_DB_PATH, default beside the Things
//                mirror) — single-node deployments, local dev and tests
//   - rqliteSQL: a shared rqlite cluster (DAV_RQLITE_URL), so every node sees
//                the same merge bases, aliases and recent deletes
//   - failoverSQL: rqlite, falling back to a local copy with a replay journal
//                when the cluster has no leader (see dav_store_failover.go)

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arthursoares/things-cloud-sdk/thingsdav"
)

const davStoreSchema = `
CREATE TABLE IF NOT EXISTS snapshots (
    device    TEXT NOT NULL,
    key       TEXT NOT NULL,
    fields    TEXT NOT NULL,
    served_at INTEGER NOT NULL,
    PRIMARY KEY (device, key)
);
CREATE TABLE IF NOT EXISTS aliases (
    task_uuid  TEXT PRIMARY KEY,
    client_uid TEXT NOT NULL,
    name       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sidecars (
    task_uuid TEXT PRIMARY KEY,
    ics       TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS recent_deletes (
    task_uuid  TEXT PRIMARY KEY,
    created    INTEGER NOT NULL,
    title      TEXT NOT NULL,
    deleted_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS write_log (
    at     INTEGER NOT NULL,
    device TEXT NOT NULL,
    key    TEXT NOT NULL,
    action TEXT NOT NULL,
    detail TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS meta (
    key   TEXT PRIMARY KEY,
    value INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS leases (
    name       TEXT PRIMARY KEY,
    holder     TEXT NOT NULL,
    expires_at INTEGER NOT NULL
);
`

// davStoreTables are the tables of a CalDAV store, in schema order.
var davStoreTables = []string{"snapshots", "aliases", "sidecars", "recent_deletes", "write_log", "meta", "leases"}

// davStoreSchemaStmts splits davStoreSchema into single statements, for
// backends that take one statement at a time.
func davStoreSchemaStmts() []davStmt {
	var out []davStmt
	for _, s := range strings.Split(davStoreSchema, ";") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, davStmt{SQL: s})
		}
	}
	return out
}

// errDAVStoreUnavailable marks store failures that mean "try again later"
// (no rqlite leader, cluster unreachable) rather than a bad statement.
var errDAVStoreUnavailable = errors.New("CalDAV store unavailable")

// davStmt is one parameterized SQL statement. Key names the row it writes
// ("table:primary key"), so a replay journal keeps only the latest write per
// row; append-only writes leave it empty.
type davStmt struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args,omitempty"`
	Key  string `json:"key,omitempty"`
}

func stmt(key, sql string, args ...any) davStmt { return davStmt{SQL: sql, Args: args, Key: key} }

// davSQL is where the store's SQL runs.
type davSQL interface {
	// exec runs stmts atomically and returns each one's rows affected.
	exec(stmts ...davStmt) ([]int64, error)
	// query returns the rows of one SELECT. strong asks for a read that
	// reflects every write acknowledged anywhere in the cluster.
	query(strong bool, s davStmt) ([][]any, error)
	// backupTo writes a consistent SQLite copy of the store to path.
	backupTo(path string) error
	// mode names the backend's current state for /healthz and logs.
	mode() string
	close() error
}

type davStore struct {
	sql davSQL
}

// davStorePath is DAV_DB_PATH, else "<mirror>-dav.db" beside the Things
// mirror (/data/things.db → /data/things-dav.db).
func davStorePath(thingsDBPath string) string {
	if p := os.Getenv("DAV_DB_PATH"); p != "" {
		return p
	}
	return strings.TrimSuffix(thingsDBPath, filepath.Ext(thingsDBPath)) + "-dav.db"
}

// openDAVStore opens a local SQLite store at path.
func openDAVStore(path string) (*davStore, error) {
	db, err := openSQLiteSQL(path)
	if err != nil {
		return nil, err
	}
	return &davStore{sql: db}, nil
}

// openConfiguredDAVStore opens the store the environment asks for: rqlite
// at DAV_RQLITE_URL (with a local fallback copy beside localPath, e.g.
// things-dav-fallback.db, unless DAV_LOCAL_FALLBACK=false), else a SQLite
// file at localPath.
func openConfiguredDAVStore(localPath string) (*davStore, error) {
	raw := os.Getenv("DAV_RQLITE_URL")
	if raw == "" {
		return openDAVStore(localPath)
	}
	rq, err := newRqliteSQL(raw, os.Getenv("DAV_RQLITE_READ_LEVEL"))
	if err != nil {
		return nil, err
	}
	if os.Getenv("DAV_LOCAL_FALLBACK") == "false" {
		if err := rq.ensureSchema(); err != nil {
			log.Printf("[DAV] rqlite schema: %v (will retry on first write)", err)
		}
		return &davStore{sql: rq}, nil
	}
	fo, err := newFailoverSQL(rq, strings.TrimSuffix(localPath, filepath.Ext(localPath))+"-fallback.db")
	if err != nil {
		return nil, err
	}
	return &davStore{sql: fo}, nil
}

func (s *davStore) Close() error { return s.sql.close() }

// backupTo writes a consistent copy of the store to path.
func (s *davStore) backupTo(path string) error { return s.sql.backupTo(path) }

// mode describes where the store currently reads and writes.
func (s *davStore) mode() string { return s.sql.mode() }

// recordServed stores each delivered object's fields as the device's base,
// in one batch (rqlite has no interactive transactions).
func (s *davStore) recordServed(device string, objs []*thingsdav.Object) error {
	if len(objs) == 0 {
		return nil
	}
	now := time.Now().Unix()
	stmts := make([]davStmt, 0, len(objs))
	for _, o := range objs {
		stmts = append(stmts, snapshotStmt(device, o.Key, o.Snapshot, now))
	}
	_, err := s.sql.exec(stmts...)
	return err
}

func snapshotStmt(device, key, fields string, at int64) davStmt {
	return stmt("snapshots:"+device+"\x00"+key,
		`INSERT OR REPLACE INTO snapshots (device, key, fields, served_at) VALUES (?, ?, ?, ?)`,
		device, key, fields, at)
}

func (s *davStore) setSnapshot(device, key string, fields any) error {
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = s.sql.exec(snapshotStmt(device, key, string(b), time.Now().Unix()))
	return err
}

// snapshot loads a device's base for key into dst; false when none exists.
// The read is strong: the device may have downloaded through another node.
func (s *davStore) snapshot(device, key string, dst any) (bool, error) {
	rows, err := s.sql.query(true, stmt("", `SELECT fields FROM snapshots WHERE device = ? AND key = ?`, device, key))
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	return true, json.Unmarshal([]byte(sqlString(rows[0][0])), dst)
}

type davAlias struct{ uid, name string }

func (s *davStore) aliases() (map[string]davAlias, error) {
	rows, err := s.sql.query(false, stmt("", `SELECT task_uuid, client_uid, name FROM aliases`))
	if err != nil {
		return nil, err
	}
	out := make(map[string]davAlias, len(rows))
	for _, r := range rows {
		out[sqlString(r[0])] = davAlias{uid: sqlString(r[1]), name: sqlString(r[2])}
	}
	return out, nil
}

// bumpGeneration invalidates every node's render cache.
var bumpGeneration = stmt("meta:generation",
	`INSERT INTO meta (key, value) VALUES ('generation', 1) ON CONFLICT(key) DO UPDATE SET value = value + 1`)

func (s *davStore) setAlias(taskUUID, clientUID, name string) error {
	_, err := s.sql.exec(
		stmt("aliases:"+taskUUID, `INSERT OR REPLACE INTO aliases (task_uuid, client_uid, name) VALUES (?, ?, ?)`,
			taskUUID, clientUID, name),
		bumpGeneration)
	return err
}

func (s *davStore) sidecars() (map[string]string, error) {
	rows, err := s.sql.query(false, stmt("", `SELECT task_uuid, ics FROM sidecars`))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[sqlString(r[0])] = sqlString(r[1])
	}
	return out, nil
}

// setSidecar replaces a task's sidecar; an empty ics removes it.
func (s *davStore) setSidecar(taskUUID, ics string) error {
	key := "sidecars:" + taskUUID
	write := stmt(key, `INSERT OR REPLACE INTO sidecars (task_uuid, ics) VALUES (?, ?)`, taskUUID, ics)
	if ics == "" {
		write = stmt(key, `DELETE FROM sidecars WHERE task_uuid = ?`, taskUUID)
	}
	_, err := s.sql.exec(write, bumpGeneration)
	return err
}

// generation is the shared render-cache counter; 0 when unknown.
func (s *davStore) generation() int64 {
	rows, err := s.sql.query(false, stmt("", `SELECT value FROM meta WHERE key = 'generation'`))
	if err != nil || len(rows) == 0 {
		return 0
	}
	return sqlInt(rows[0][0])
}

func (s *davStore) recordDelete(taskUUID string, created time.Time, title string) error {
	_, err := s.sql.exec(stmt("recent_deletes:"+taskUUID,
		`INSERT OR REPLACE INTO recent_deletes (task_uuid, created, title, deleted_at) VALUES (?, ?, ?, ?)`,
		taskUUID, created.Unix(), title, time.Now().Unix()))
	return err
}

// takeRecentDelete finds (and forgets) a task trashed over CalDAV since
// `since` whose CREATED and title match — a list move in progress. The
// delete is conditional: if another node claimed the row first it affects
// nothing, and the next candidate is tried.
func (s *davStore) takeRecentDelete(created time.Time, title string, since time.Time) (string, error) {
	for attempt := 0; attempt < 5; attempt++ {
		rows, err := s.sql.query(true, stmt("", `SELECT task_uuid, deleted_at FROM recent_deletes
			WHERE created = ? AND title = ? AND deleted_at >= ? ORDER BY deleted_at DESC LIMIT 1`,
			created.Unix(), title, since.Unix()))
		if err != nil || len(rows) == 0 {
			return "", err
		}
		uuid, deletedAt := sqlString(rows[0][0]), sqlInt(rows[0][1])
		n, err := s.sql.exec(stmt("recent_deletes:"+uuid,
			`DELETE FROM recent_deletes WHERE task_uuid = ? AND deleted_at = ?`, uuid, deletedAt))
		if err != nil {
			return "", err
		}
		if n[0] == 1 {
			return uuid, nil
		}
	}
	return "", nil
}

func (s *davStore) logWrite(device, key, action string, detail any) {
	b, _ := json.Marshal(detail)
	if _, err := s.sql.exec(stmt("", `INSERT INTO write_log (at, device, key, action, detail) VALUES (?, ?, ?, ?, ?)`,
		time.Now().Unix(), device, key, action, string(b))); err != nil {
		logDAV("write log failed: %v", err)
	}
}

// acquireLease takes or renews the named lease for holder until now+ttl;
// false when another holder's lease is still live. Writes go through the
// rqlite leader, so two nodes can't both win.
func (s *davStore) acquireLease(name, holder string, now time.Time, ttl time.Duration) (bool, error) {
	n, err := s.sql.exec(stmt("leases:"+name,
		`INSERT INTO leases (name, holder, expires_at) VALUES (?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET holder = excluded.holder, expires_at = excluded.expires_at
		 WHERE leases.expires_at < ? OR leases.holder = excluded.holder`,
		name, holder, now.Add(ttl).Unix(), now.Unix()))
	if err != nil {
		return false, err
	}
	return n[0] == 1, nil
}

// tableCounts returns the row count of each table, for migration checks.
func (s *davStore) tableCounts() (map[string]int64, error) {
	out := map[string]int64{}
	for _, t := range davStoreTables {
		rows, err := s.sql.query(true, stmt("", `SELECT COUNT(*) FROM `+t))
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = sqlInt(rows[0][0])
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Value helpers: SQLite scans and rqlite JSON give different Go types.
// ---------------------------------------------------------------------------

func sqlString(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case []byte:
		return string(x)
	default:
		return fmt.Sprint(x)
	}
}

func sqlInt(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	case float64:
		return int64(x)
	case json.Number:
		n, _ := x.Int64()
		return n
	case string:
		var n int64
		fmt.Sscan(x, &n)
		return n
	}
	return 0
}

// writable reports whether writes can be stored right now. Only a bare
// rqlite backend can refuse: the failover backend always has its local copy.
func (s *davStore) writable() error {
	if r, ok := s.sql.(*rqliteSQL); ok && !r.ready() {
		return fmt.Errorf("%w: rqlite has no leader", errDAVStoreUnavailable)
	}
	return nil
}
