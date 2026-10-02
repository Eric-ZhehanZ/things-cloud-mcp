package main

import (
	"archive/zip"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/arthursoares/things-cloud-sdk/sync"
)

func stubBackups(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BACKUP_DIR", filepath.Join(dir, "backups"))
	t.Setenv("API_KEY", "backup-test-key")
	s, err := sync.Open(filepath.Join(dir, "things.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	store, err := openDAVStore(filepath.Join(dir, "things-dav.db"))
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	origSyncer, origDAV, origNow := syncer, davDB, backupNow
	syncer, davDB = s, store
	backupNow = func() time.Time { clock = clock.Add(time.Second); return clock }
	t.Cleanup(func() {
		s.Close()
		store.Close()
		syncer, davDB, backupNow = origSyncer, origDAV, origNow
	})
}

func TestCreateBackupSnapshot(t *testing.T) {
	stubBackups(t)
	info, err := createBackup(backupKindManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !backupIDPattern.MatchString(info.ID) || info.SizeBytes == 0 || len(info.SHA256) != 64 {
		t.Fatalf("info = %+v", info)
	}
	zr, err := zip.OpenReader(filepath.Join(backupDir(), info.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "manifest.json,things-dav.db,things.db" {
		t.Errorf("zip contents = %v", names)
	}
	list, err := listBackups()
	if err != nil || len(list) != 1 || list[0].ID != info.ID || list[0].SHA256 != info.SHA256 {
		t.Errorf("listBackups = %+v, %v", list, err)
	}
}

func TestBackupPruning(t *testing.T) {
	stubBackups(t)
	for i := 0; i < backupKeep[backupKindManual]+3; i++ {
		if _, err := createBackup(backupKindManual, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := createBackup(backupKindAuto, nil); err != nil {
		t.Fatal(err)
	}
	all, _ := listBackups()
	counts := map[string]int{}
	for _, b := range all {
		counts[b.Kind]++
	}
	if counts[backupKindManual] != backupKeep[backupKindManual] || counts[backupKindAuto] != 1 {
		t.Errorf("after pruning: %v", counts)
	}
	if all[0].Kind != backupKindAuto {
		t.Errorf("list must be newest first, got %s first", all[0].Kind)
	}
}

func TestBackupDownloadAuth(t *testing.T) {
	stubBackups(t)
	info, err := createBackup(backupKindManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	signed := withDownloadURL(info, time.Now(), "https://ai.thingsapi.com")
	u, _ := url.Parse(signed.DownloadURL)
	if u.Host != "ai.thingsapi.com" || u.Path != "/api/backups/download" || strings.Contains(signed.DownloadURL, "backup-test-key") {
		t.Fatalf("download URL = %s", signed.DownloadURL)
	}

	get := func(query string, bearer string) int {
		req := httptest.NewRequest(http.MethodGet, "/api/backups/download?"+query, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handleBackupDownload(rec, req)
		if rec.Code == http.StatusOK && rec.Header().Get("Content-Type") != "application/zip" {
			t.Errorf("content type %s", rec.Header().Get("Content-Type"))
		}
		return rec.Code
	}
	q := u.Query()
	if code := get(q.Encode(), ""); code != http.StatusOK {
		t.Errorf("signed link = %d", code)
	}
	tampered := url.Values{"id": {q.Get("id")}, "exp": {q.Get("exp")}, "sig": {strings.Repeat("0", 64)}}
	if code := get(tampered.Encode(), ""); code != http.StatusUnauthorized {
		t.Errorf("tampered link = %d", code)
	}
	past := time.Now().Add(-time.Minute).Unix()
	expired := url.Values{"id": {info.ID}, "exp": {strconv.FormatInt(past, 10)}, "sig": {signBackup(info.ID, past)}}
	if code := get(expired.Encode(), ""); code != http.StatusUnauthorized {
		t.Errorf("expired link = %d", code)
	}
	if code := get("id="+info.ID, "backup-test-key"); code != http.StatusOK {
		t.Errorf("API key download = %d", code)
	}
	if code := get("id="+info.ID, ""); code != http.StatusUnauthorized {
		t.Errorf("unauthenticated download = %d", code)
	}
	if code := get("id="+url.QueryEscape("../things.db"), "backup-test-key"); code != http.StatusNotFound {
		t.Errorf("path traversal = %d", code)
	}
}

func TestParseAsOf(t *testing.T) {
	t.Setenv("THINGS_TIMEZONE", "America/New_York")
	got, err := parseAsOf("2026-09-20", "")
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 21, 3, 59, 59, 0, time.UTC); !got.Equal(want) {
		t.Errorf("date as_of = %v, want end of day New York (%v)", got.UTC(), want)
	}
	got, err = parseAsOf("2026-09-20T10:00:00Z", "")
	if err != nil || !got.Equal(time.Date(2026, 9, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("RFC 3339 as_of = %v, %v", got, err)
	}
	if _, err := parseAsOf("last tuesday", ""); err == nil {
		t.Error("garbage as_of must be rejected")
	}
	if got, _ := parseAsOf("", ""); got != nil {
		t.Error("empty as_of means a snapshot")
	}
}

func TestAutoBackupDue(t *testing.T) {
	stubBackups(t)
	if !autoBackupDue(time.Now(), 24*time.Hour) {
		t.Error("no automatic backup yet: one is due")
	}
	info, err := createBackup(backupKindAuto, nil)
	if err != nil {
		t.Fatal(err)
	}
	if autoBackupDue(info.CreatedAt.Add(time.Hour), 24*time.Hour) {
		t.Error("a backup an hour old is not due")
	}
	if !autoBackupDue(info.CreatedAt.Add(25*time.Hour), 24*time.Hour) {
		t.Error("a backup 25 hours old is due")
	}
}

func TestRequestBaseURL(t *testing.T) {
	t.Setenv("PUBLIC_URL", "")
	for _, tc := range []struct {
		name    string
		host    string
		headers map[string]string
		want    string
	}{
		{"direct", "ai.thingsapi.com", nil, "http://ai.thingsapi.com"},
		{"behind a TLS proxy", "ai.thingsapi.com", map[string]string{"X-Forwarded-Proto": "https"}, "https://ai.thingsapi.com"},
		{"forwarded host wins", "internal:8080", map[string]string{"X-Forwarded-Host": "ai.thingsapi.com", "X-Forwarded-Proto": "https"}, "https://ai.thingsapi.com"},
		{"port kept", "localhost:8090", nil, "http://localhost:8090"},
		{"junk host rejected", "evil.com/x@", nil, ""},
		{"junk proto ignored", "ai.thingsapi.com", map[string]string{"X-Forwarded-Proto": "javascript"}, "http://ai.thingsapi.com"},
	} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Host = tc.host
		for k, v := range tc.headers {
			r.Header.Set(k, v)
		}
		if got := baseURLFrom(withRequestBaseURL(r.Context(), r)); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
	t.Setenv("PUBLIC_URL", "https://override.example/")
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	if got := baseURLFrom(withRequestBaseURL(r.Context(), r)); got != "https://override.example" {
		t.Errorf("PUBLIC_URL override = %q", got)
	}
}
