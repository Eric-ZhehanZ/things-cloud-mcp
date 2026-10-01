package main

// Database backups on the persistent volume:
//
//   - manual:  a consistent snapshot of the live Things mirror and CalDAV
//              store, taken on demand (things_backup_create)
//   - rebuild: Things as it stood at a past moment, rebuilt by replaying
//              Things Cloud history up to then (things_backup_create as_of)
//   - auto:    a scheduled snapshot every BACKUP_INTERVAL_HOURS (default 24)
//
// Each backup is a zip of plain SQLite files plus manifest.json, stored in
// BACKUP_DIR (default <data dir>/backups) and pruned per kind. Downloads use
// short-lived signed links so tool output never carries the API key.

import (
	"archive/zip"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	dir := backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(dir, ".tmp-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	now := backupNow().UTC().Truncate(time.Second)
	info := &backupInfo{Kind: kind, CreatedAt: now, AsOf: now}
	info.ID = fmt.Sprintf("things-%s-%s.zip", kind, now.Format("20060102T150405Z"))
	if _, err := os.Stat(filepath.Join(dir, info.ID)); err == nil {
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

	path := filepath.Join(dir, info.ID)
	if err := writeBackupZip(path, files, info); err != nil {
		os.Remove(path)
		return nil, err
	}
	if err := finishBackupInfo(path, info); err != nil {
		return nil, err
	}
	pruneBackups(kind)
	log.Printf("[BACKUP] %s: %s (%d bytes)", kind, info.ID, info.SizeBytes)
	return info, nil
}

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
// next to it.
func finishBackupInfo(path string, info *backupInfo) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return err
	}
	info.SizeBytes, info.SHA256 = n, hex.EncodeToString(h.Sum(nil))
	b, _ := json.MarshalIndent(info, "", "  ")
	return os.WriteFile(strings.TrimSuffix(path, ".zip")+".json", b, 0o600)
}

// listBackups returns backups newest first.
func listBackups() ([]*backupInfo, error) {
	entries, err := os.ReadDir(backupDir())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*backupInfo
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(backupDir(), e.Name()))
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
		base := filepath.Join(backupDir(), strings.TrimSuffix(b.ID, ".zip"))
		os.Remove(base + ".zip")
		os.Remove(base + ".json")
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
// the interval. Fly stops idle machines, so this is checked on start and
// hourly rather than trusting one long timer.
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

func startBackupScheduler(ctx context.Context) {
	interval := backupInterval()
	if interval == 0 {
		log.Printf("[BACKUP] automatic backups disabled (BACKUP_INTERVAL_HOURS=0)")
		return
	}
	log.Printf("[BACKUP] automatic backups every %s into %s", interval, backupDir())
	go func() {
		check := func() {
			if autoBackupDue(time.Now(), interval) {
				if _, err := createBackup(backupKindAuto, nil); err != nil {
					log.Printf("[BACKUP] automatic backup failed: %v", err)
				}
			}
		}
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

func publicBaseURL() string {
	if u := os.Getenv("PUBLIC_URL"); u != "" {
		return strings.TrimSuffix(u, "/")
	}
	if app := os.Getenv("FLY_APP_NAME"); app != "" {
		return "https://" + app + ".fly.dev"
	}
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	return "http://localhost:" + port
}

func withDownloadURL(info *backupInfo, now time.Time) *backupInfo {
	exp := now.Add(backupLinkTTL).Truncate(time.Second)
	out := *info
	q := url.Values{"id": {info.ID}, "exp": {strconv.FormatInt(exp.Unix(), 10)}, "sig": {signBackup(info.ID, exp.Unix())}}
	out.DownloadURL = publicBaseURL() + "/api/backups/download?" + q.Encode()
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
	path := filepath.Join(backupDir(), id)
	f, err := os.Open(path)
	if err != nil {
		jsonError(w, "unknown backup", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+id+`"`)
	http.ServeContent(w, r, id, st.ModTime(), f)
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

func mcpBackupCreate(_ context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	return jsonToolResultWithIndent(withDownloadURL(info, time.Now()), true), nil
}

func mcpBackupList(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	all, err := listBackups()
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	now := time.Now()
	out := make([]*backupInfo, 0, len(all))
	for _, b := range all {
		out = append(out, withDownloadURL(b, now))
	}
	return jsonToolResultWithIndent(out, true), nil
}
