package main

// Database backups:
//
//   - manual:  a consistent snapshot of the Things mirror (this node's) and
//              the CalDAV store, taken on demand (things_backup_create)
//   - rebuild: Things as it stood at a past moment, rebuilt by replaying
//              Things Cloud history up to then (things_backup_create as_of)
//   - auto:    a scheduled snapshot every BACKUP_INTERVAL_HOURS (default 24),
//              taken by one node at a time under a lease in the CalDAV store
//
// Each backup is a zip of plain SQLite files plus manifest.json, stored in an
// R2 bucket shared by all nodes when R2_* is set, else in BACKUP_DIR
// (default <data dir>/backups), and pruned per kind (backup_storage.go).
// Downloads use short-lived signed links, keyed off API_KEY so any node can
// serve any link, so tool output never carries the API key.

import (
	"archive/zip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	gosync "sync"
	"time"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arthursoares/things-cloud-sdk/sync"
)

const (
	backupKindManual  = "manual"
	backupKindAuto    = "auto"
	backupKindRebuild = "rebuild"

	backupLinkTTL = 15 * time.Minute
)

// backupKeep is how many backups of each kind are kept.
var backupKeep = map[string]int{backupKindManual: 10, backupKindAuto: 14, backupKindRebuild: 10}

// thingsDBPath is the live mirror's path; backups default to a sibling dir.
var thingsDBPath = "/data/things.db"

var backupMu gosync.Mutex // one backup at a time

// backupNow is the backup clock. Overridable in tests.
var backupNow = time.Now

var backupIDPattern = regexp.MustCompile(`^things-(manual|auto|rebuild)-\d{8}T\d{6}Z\.zip$`)

type backupInfo struct {
	ID           string     `json:"id"`
	Kind         string     `json:"kind"`
	CreatedAt    time.Time  `json:"created_at"`
	AsOf         time.Time  `json:"as_of"`
	SizeBytes    int64      `json:"size_bytes"`
	SHA256       string     `json:"sha256"`
	Files        []string   `json:"files"`
	HistoryItems int        `json:"history_items,omitempty"`
	HistoryStart *time.Time `json:"history_starts_at,omitempty"`
	Note         string     `json:"note,omitempty"`
	Node         string     `json:"node,omitempty"`
	DownloadURL  string     `json:"download_url,omitempty"`
	URLExpiresAt *time.Time `json:"download_url_expires_at,omitempty"`
}

func backupDir() string {
	if d := os.Getenv("BACKUP_DIR"); d != "" {
		return d
	}
	return filepath.Join(filepath.Dir(thingsDBPath), "backups")
}

// createBackup takes a snapshot of the live databases, or, with asOf,
// rebuilds Things as it stood then from Things Cloud history.
func createBackup(kind string, asOf *time.Time) (*backupInfo, error) {
	backupMu.Lock()
	defer backupMu.Unlock()

	store, err := currentBackupStorage()
	if err != nil {
		return nil, err
	}
	tmp, err := backupTempDir(store)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	now := backupNow().UTC().Truncate(time.Second)
	info := &backupInfo{Kind: kind, CreatedAt: now, AsOf: now, Node: nodeName()}
	info.ID = fmt.Sprintf("things-%s-%s.zip", kind, now.Format("20060102T150405Z"))
	if taken, err := store.exists(info.ID); err != nil {
		return nil, fmt.Errorf("check backup storage: %w", err)
	} else if taken {
		return nil, fmt.Errorf("a %s backup was just taken; try again in a second", kind)
	}

	files := map[string]string{}
	if asOf == nil {
		thingsCopy := filepath.Join(tmp, "things.db")
		if err := syncer.BackupTo(thingsCopy); err != nil {
			return nil, fmt.Errorf("snapshot Things mirror: %w", err)
		}
		files["things.db"] = thingsCopy
		if davDB != nil {
			davCopy := filepath.Join(tmp, "things-dav.db")
			if err := davDB.backupTo(davCopy); err != nil {
				return nil, fmt.Errorf("snapshot CalDAV store: %w", err)
			}
			files["things-dav.db"] = davCopy
		}
	} else {
		if asOf.After(now) {
			return nil, invalidInputf("as_of %s is in the future", asOf.Format(time.RFC3339))
		}
		rebuilt := filepath.Join(tmp, "things.db")
		res, err := sync.RebuildAt(rebuilt, client, *asOf)
		if err != nil {
			return nil, fmt.Errorf("rebuild as of %s: %w", asOf.Format(time.RFC3339), err)
		}
		files["things.db"] = rebuilt
		info.AsOf = asOf.UTC()
		info.HistoryItems = res.Items
		if !res.FirstItem.IsZero() {
			first := res.FirstItem
			info.HistoryStart = &first
			if first.After(*asOf) {
				info.Note = fmt.Sprintf("Things Cloud history starts at %s, after as_of: the rebuild is empty", first.Format(time.RFC3339))
			}
		}
	}

	path := filepath.Join(tmp, info.ID)
	if err := writeBackupZip(path, files, info); err != nil {
		return nil, err
	}
	index, err := finishBackupInfo(path, info)
	if err != nil {
		return nil, err
	}
	// The zip first, then its index: listings only show complete backups.
	if err := store.put(info.ID, path); err != nil {
		return nil, fmt.Errorf("store %s: %w", info.ID, err)
	}
	if err := store.put(backupIndexName(info.ID), index); err != nil {
		store.remove(info.ID)
		return nil, fmt.Errorf("store %s: %w", backupIndexName(info.ID), err)
	}
	pruneBackups(kind)
	log.Printf("[BACKUP] %s: %s (%d bytes) in %s", kind, info.ID, info.SizeBytes, store)
	return info, nil
}

// backupTempDir is where a backup is assembled: inside the backup directory
// for local storage (so the final move is a rename), else the system temp.
func backupTempDir(store backupStorage) (string, error) {
	if l, ok := store.(localBackupStorage); ok {
		if err := os.MkdirAll(l.dir, 0o700); err != nil {
			return "", err
		}
		return os.MkdirTemp(l.dir, ".tmp-")
	}
	return os.MkdirTemp("", "things-backup-")
}

func backupIndexName(id string) string { return strings.TrimSuffix(id, ".zip") + ".json" }

func writeBackupZip(path string, files map[string]string, info *backupInfo) error {
	out, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	info.Files = append(names, "manifest.json")
	for _, name := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Deflate, Modified: info.CreatedAt})
		if err != nil {
			return err
		}
		f, err := os.Open(files[name])
		if err != nil {
			return err
		}
		_, err = io.Copy(w, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	manifest, _ := json.MarshalIndent(info, "", "  ")
	w, err := zw.CreateHeader(&zip.FileHeader{Name: "manifest.json", Method: zip.Deflate, Modified: info.CreatedAt})
	if err != nil {
		return err
	}
	if _, err := w.Write(manifest); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	return out.Sync()
}

// finishBackupInfo records the archive's size and hash in a JSON index file
// next to it and returns that file's path.
func finishBackupInfo(path string, info *backupInfo) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", err
	}
	info.SizeBytes, info.SHA256 = n, hex.EncodeToString(h.Sum(nil))
	b, _ := json.MarshalIndent(info, "", "  ")
	index := strings.TrimSuffix(path, ".zip") + ".json"
	return index, os.WriteFile(index, b, 0o600)
}

// listBackups returns backups newest first.
func listBackups() ([]*backupInfo, error) {
	store, err := currentBackupStorage()
	if err != nil {
		return nil, err
	}
	names, err := store.list(".json")
	if err != nil {
		return nil, err
	}
	var out []*backupInfo
	for _, name := range names {
		b, err := store.read(name)
		if err != nil {
			continue
		}
		var info backupInfo
		if json.Unmarshal(b, &info) == nil && backupIDPattern.MatchString(info.ID) {
			out = append(out, &info)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func pruneBackups(kind string) {
	all, err := listBackups()
	if err != nil {
		log.Printf("[BACKUP] prune: %v", err)
		return
	}
	kept := 0
	for _, b := range all {
		if b.Kind != kind {
			continue
		}
		kept++
		if kept <= backupKeep[kind] {
			continue
		}
		store, err := currentBackupStorage()
		if err != nil {
			return
		}
		// Index first, so a half-pruned backup never lists.
		if err := store.remove(backupIndexName(b.ID)); err != nil {
			log.Printf("[BACKUP] prune %s: %v", b.ID, err)
			continue
		}
		store.remove(b.ID)
		log.Printf("[BACKUP] pruned %s", b.ID)
	}
}

// ---------------------------------------------------------------------------
// Scheduled backups
// ---------------------------------------------------------------------------

func backupInterval() time.Duration {
	raw := os.Getenv("BACKUP_INTERVAL_HOURS")
	if raw == "" {
		return 24 * time.Hour
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 24 * time.Hour
	}
	return time.Duration(n) * time.Hour
}

// autoBackupDue reports whether the newest automatic backup is older than
// the interval. Nodes restart, so this is checked on start and hourly
// rather than trusting one long timer.
func autoBackupDue(now time.Time, interval time.Duration) bool {
	all, err := listBackups()
	if err != nil {
		return false
	}
	for _, b := range all {
		if b.Kind == backupKindAuto {
			return now.Sub(b.CreatedAt) >= interval
		}
	}
	return true
}

// autoBackupLeaseTTL bounds how long a node that died mid-backup blocks the
// others.
const autoBackupLeaseTTL = 30 * time.Minute

// runAutoBackup takes the automatic backup when it is due and this node wins
// the "auto-backup" lease, so only one node takes each snapshot. Without a
// shared store the lease is local and always won.
func runAutoBackup(now time.Time, interval time.Duration) bool {
	if !autoBackupDue(now, interval) {
		return false
	}
	if davDB != nil {
		holder := nodeName()
		if holder == "" {
			holder, _ = os.Hostname()
		}
		won, err := davDB.acquireLease("auto-backup", holder, now, autoBackupLeaseTTL)
		if err != nil {
			log.Printf("[BACKUP] automatic backup skipped: lease: %v", err)
			return false
		}
		if !won {
			return false
		}
		// Another node may have finished one between the check and the lease.
		if !autoBackupDue(now, interval) {
			return false
		}
	}
	if _, err := createBackup(backupKindAuto, nil); err != nil {
		log.Printf("[BACKUP] automatic backup failed: %v", err)
		return false
	}
	return true
}

func startBackupScheduler(ctx context.Context) {
	interval := backupInterval()
	if interval == 0 {
		log.Printf("[BACKUP] automatic backups disabled (BACKUP_INTERVAL_HOURS=0)")
		return
	}
	where := backupDir()
	if store, err := currentBackupStorage(); err == nil {
		where = store.String()
	}
	log.Printf("[BACKUP] automatic backups every %s into %s", interval, where)
	go func() {
		check := func() { runAutoBackup(time.Now(), interval) }
		check()
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				check()
			}
		}
	}()
}

// ---------------------------------------------------------------------------
// Signed download links
// ---------------------------------------------------------------------------

var (
	backupSigningKeyOnce gosync.Once
	backupSigningKey     []byte
)

// signingKey derives from API_KEY so links survive restarts; without one,
// a per-process random key is used.
func signingKey() []byte {
	if k := os.Getenv("API_KEY"); k != "" {
		sum := sha256.Sum256([]byte("things-backup-links\x00" + k))
		return sum[:]
	}
	backupSigningKeyOnce.Do(func() {
		backupSigningKey = make([]byte, 32)
		rand.Read(backupSigningKey)
	})
	return backupSigningKey
}

func signBackup(id string, exp int64) string {
	mac := hmac.New(sha256.New, signingKey())
	fmt.Fprintf(mac, "%s\n%d", id, exp)
	return hex.EncodeToString(mac.Sum(nil))
}

type backupCtxKey struct{}

var validHost = regexp.MustCompile(`^[A-Za-z0-9.-]+(:\d+)?$`)

// requestBaseURL is the origin a request reached the server on, so links
// point back at the same domain (e.g. ai.thingsapi.com behind a proxy).
func requestBaseURL(r *http.Request) string {
	host := r.Host
	if fh := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); fh != "" {
		host = fh
	}
	if !validHost.MatchString(host) {
		return ""
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if fp := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); fp == "https" || fp == "http" {
		scheme = fp
	}
	return scheme + "://" + host
}

// withRequestBaseURL stores the request's origin for MCP tool handlers.
func withRequestBaseURL(ctx context.Context, r *http.Request) context.Context {
	return context.WithValue(ctx, backupCtxKey{}, requestBaseURL(r))
}

// baseURLFrom returns PUBLIC_URL when set, else the origin of the request
// that carried ctx.
func baseURLFrom(ctx context.Context) string {
	if u := os.Getenv("PUBLIC_URL"); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	base, _ := ctx.Value(backupCtxKey{}).(string)
	return base
}

func withDownloadURL(info *backupInfo, now time.Time, base string) *backupInfo {
	exp := now.Add(backupLinkTTL).Truncate(time.Second)
	out := *info
	q := url.Values{"id": {info.ID}, "exp": {strconv.FormatInt(exp.Unix(), 10)}, "sig": {signBackup(info.ID, exp.Unix())}}
	out.DownloadURL = base + "/api/backups/download?" + q.Encode()
	out.URLExpiresAt = &exp
	return &out
}

// handleBackupDownload serves a backup to a valid signed link, or to a
// request carrying the API key (Authorization: Bearer).
func handleBackupDownload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := q.Get("id")
	if !backupIDPattern.MatchString(id) {
		jsonError(w, "unknown backup", http.StatusNotFound)
		return
	}
	authorized := false
	if sig := q.Get("sig"); sig != "" {
		exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
		authorized = err == nil && time.Now().Unix() <= exp &&
			hmac.Equal([]byte(sig), []byte(signBackup(id, exp)))
	} else if key := os.Getenv("API_KEY"); key == "" || r.Header.Get("Authorization") == "Bearer "+key {
		authorized = true
	}
	if !authorized {
		jsonError(w, "link expired or invalid; create a fresh one with things_backup_list", http.StatusUnauthorized)
		return
	}
	store, err := currentBackupStorage()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Streamed through this server, so the link stays on the request's
	// own domain.
	f, modTime, err := store.open(id)
	if errors.Is(err, errBackupNotFound) {
		jsonError(w, "unknown backup", http.StatusNotFound)
		return
	}
	if err != nil {
		jsonError(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`"`)
	http.ServeContent(w, r, id, modTime, f)
}

// ---------------------------------------------------------------------------
// MCP tools
// ---------------------------------------------------------------------------

// parseAsOf accepts RFC 3339 or YYYY-MM-DD; a bare date means the end of
// that day in the request's timezone.
func parseAsOf(raw, tz string) (*time.Time, error) {
	if raw == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return &t, nil
	}
	loc, err := resolveLocation(tz)
	if err != nil {
		return nil, err
	}
	d, err := time.ParseInLocation("2006-01-02", raw, loc)
	if err != nil {
		return nil, invalidInputf("as_of must be YYYY-MM-DD or RFC 3339, got %q", raw)
	}
	end := d.AddDate(0, 0, 1).Add(-time.Second)
	return &end, nil
}

func mcpBackupCreate(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	asOf, err := parseAsOf(req.GetString("as_of", ""), req.GetString("timezone", ""))
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	kind := backupKindManual
	if asOf != nil {
		kind = backupKindRebuild
	} else if syncErr := syncForMCPReadResult(); syncErr != nil {
		return syncErr, nil
	}
	info, err := createBackup(kind, asOf)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return jsonToolResultWithIndent(withDownloadURL(info, time.Now(), baseURLFrom(ctx)), true), nil
}

func mcpBackupList(ctx context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	all, err := listBackups()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	now := time.Now()
	out := make([]*backupInfo, 0, len(all))
	for _, b := range all {
		out = append(out, withDownloadURL(b, now, baseURLFrom(ctx)))
	}
	return jsonToolResultWithIndent(out, true), nil
}
