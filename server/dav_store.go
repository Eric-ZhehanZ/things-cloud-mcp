package main

// davStore persists what CalDAV needs beyond Things itself, in its own
// SQLite file (DAV_DB_PATH, default next to the Things mirror) so a Things
// mirror rebuild never wipes it:
//
//   - snapshots:      per device, the fields of each object it last
//                     downloaded — the base of that device's next edit
//   - aliases:        the UID and file name a client created a task under
//   - sidecars:       client properties Things can't hold (duration, time of
//                     day, priority, extra alarms), merged back on render
//   - recent_deletes: tasks trashed over CalDAV, to recognize list moves
//   - write_log:      every CalDAV write, with the conflicts Things won

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

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
`

type davStore struct {
	db *sql.DB
}

// davStorePath is DAV_DB_PATH, else "<mirror>-dav.db" beside the Things
// mirror (/data/things.db → /data/things-dav.db).
func davStorePath(thingsDBPath string) string {
	if p := os.Getenv("DAV_DB_PATH"); p != "" {
		return p
	}
	return strings.TrimSuffix(thingsDBPath, filepath.Ext(thingsDBPath)) + "-dav.db"
}

func openDAVStore(path string) (*davStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, stmt := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", davStoreSchema} {
		if _, err := db.Exec(stmt); err != nil {
			db.Close()
			return nil, fmt.Errorf("dav store %s: %w", path, err)
		}
	}
	return &davStore{db: db}, nil
}

func (s *davStore) Close() error { return s.db.Close() }

// recordServed stores each delivered object's fields as the device's base.
func (s *davStore) recordServed(device string, objs []*thingsdav.Object) error {
	if len(objs) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Unix()
	for _, o := range objs {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO snapshots (device, key, fields, served_at) VALUES (?, ?, ?, ?)`,
			device, o.Key, o.Snapshot, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *davStore) setSnapshot(device, key string, fields any) error {
	b, err := json.Marshal(fields)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT OR REPLACE INTO snapshots (device, key, fields, served_at) VALUES (?, ?, ?, ?)`,
		device, key, string(b), time.Now().Unix())
	return err
}

// snapshot loads a device's base for key into dst; false when none exists.
func (s *davStore) snapshot(device, key string, dst any) (bool, error) {
	var raw string
	err := s.db.QueryRow(`SELECT fields FROM snapshots WHERE device = ? AND key = ?`, device, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, json.Unmarshal([]byte(raw), dst)
}

type davAlias struct{ uid, name string }

func (s *davStore) aliases() (map[string]davAlias, error) {
	rows, err := s.db.Query(`SELECT task_uuid, client_uid, name FROM aliases`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]davAlias{}
	for rows.Next() {
		var uuid string
		var a davAlias
		if err := rows.Scan(&uuid, &a.uid, &a.name); err != nil {
			return nil, err
		}
		out[uuid] = a
	}
	return out, rows.Err()
}

func (s *davStore) setAlias(taskUUID, clientUID, name string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO aliases (task_uuid, client_uid, name) VALUES (?, ?, ?)`,
		taskUUID, clientUID, name)
	return err
}

func (s *davStore) sidecars() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT task_uuid, ics FROM sidecars`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var uuid, ics string
		if err := rows.Scan(&uuid, &ics); err != nil {
			return nil, err
		}
		out[uuid] = ics
	}
	return out, rows.Err()
}

// setSidecar replaces a task's sidecar; an empty ics removes it.
func (s *davStore) setSidecar(taskUUID, ics string) error {
	if ics == "" {
		_, err := s.db.Exec(`DELETE FROM sidecars WHERE task_uuid = ?`, taskUUID)
		return err
	}
	_, err := s.db.Exec(`INSERT OR REPLACE INTO sidecars (task_uuid, ics) VALUES (?, ?)`, taskUUID, ics)
	return err
}

func (s *davStore) recordDelete(taskUUID string, created time.Time, title string) error {
	_, err := s.db.Exec(`INSERT OR REPLACE INTO recent_deletes (task_uuid, created, title, deleted_at) VALUES (?, ?, ?, ?)`,
		taskUUID, created.Unix(), title, time.Now().Unix())
	return err
}

// takeRecentDelete finds (and forgets) a task trashed over CalDAV since
// `since` whose CREATED and title match — a list move in progress.
func (s *davStore) takeRecentDelete(created time.Time, title string, since time.Time) (string, error) {
	var uuid string
	err := s.db.QueryRow(`SELECT task_uuid FROM recent_deletes WHERE created = ? AND title = ? AND deleted_at >= ?
		ORDER BY deleted_at DESC LIMIT 1`, created.Unix(), title, since.Unix()).Scan(&uuid)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`DELETE FROM recent_deletes WHERE task_uuid = ?`, uuid)
	return uuid, err
}

func (s *davStore) logWrite(device, key, action string, detail any) {
	b, _ := json.Marshal(detail)
	if _, err := s.db.Exec(`INSERT INTO write_log (at, device, key, action, detail) VALUES (?, ?, ?, ?, ?)`,
		time.Now().Unix(), device, key, action, string(b)); err != nil {
		logDAV("write log failed: %v", err)
	}
}
