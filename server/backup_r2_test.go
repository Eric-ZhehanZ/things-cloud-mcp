package main

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	gosync "sync"
	"testing"
	"time"

	"github.com/arthursoares/things-cloud-sdk/thingsdav"
)

// fakeS3 is a minimal path-style S3 endpoint: PUT, GET (with ranges), HEAD,
// DELETE and ListObjectsV2. Signatures aren't checked.
type fakeS3 struct {
	mu      gosync.Mutex
	objects map[string][]byte // "bucket/key"
	srv     *httptest.Server
}

func newFakeS3(t *testing.T) *fakeS3 {
	f := &fakeS3{objects: map[string][]byte{}}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if key == "" && r.Method == http.MethodGet && r.URL.Query().Get("list-type") == "2" {
		prefix := r.URL.Query().Get("prefix")
		type content struct {
			Key          string
			Size         int
			LastModified string
		}
		var res struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			Name        string
			Prefix      string
			KeyCount    int
			MaxKeys     int
			IsTruncated bool
			Contents    []content
		}
		res.Name, res.Prefix, res.MaxKeys = bucket, prefix, 1000
		var keys []string
		for k := range f.objects {
			if b, name, _ := strings.Cut(k, "/"); b == bucket && strings.HasPrefix(name, prefix) {
				keys = append(keys, name)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			res.Contents = append(res.Contents, content{Key: k, Size: len(f.objects[bucket+"/"+k]), LastModified: time.Now().UTC().Format(time.RFC3339)})
		}
		res.KeyCount = len(res.Contents)
		w.Header().Set("Content-Type", "application/xml")
		xml.NewEncoder(w).Encode(res)
		return
	}
	full := bucket + "/" + key
	switch r.Method {
	case http.MethodPut:
		b, _ := io.ReadAll(r.Body)
		f.objects[full] = b
		w.Header().Set("ETag", `"etag"`)
	case http.MethodGet, http.MethodHead:
		b, ok := f.objects[full]
		if !ok {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(http.StatusNotFound)
			if r.Method == http.MethodGet {
				io.WriteString(w, `<Error><Code>NoSuchKey</Code><Message>missing</Message></Error>`)
			}
			return
		}
		w.Header().Set("ETag", `"etag"`)
		http.ServeContent(w, r, key, time.Now(), bytes.NewReader(b))
	case http.MethodDelete:
		delete(f.objects, full)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (f *fakeS3) keys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func stubR2Backups(t *testing.T) *fakeS3 {
	t.Helper()
	stubBackups(t)
	s3 := newFakeS3(t)
	store, err := newS3BackupStorage(s3.srv.URL, "things-backups", "key-id", "secret", "backups/", s3.srv.Client().Transport)
	if err != nil {
		t.Fatal(err)
	}
	backupStorageOverride = store
	t.Cleanup(func() { backupStorageOverride = nil })
	return s3
}

func TestR2BackupRoundTrip(t *testing.T) {
	s3 := stubR2Backups(t)
	t.Setenv("NODE_NAME", "us2")
	info, err := createBackup(backupKindManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"things-backups/backups/" + info.ID, "things-backups/backups/" + backupIndexName(info.ID)}
	sort.Strings(want)
	if got := s3.keys(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("objects = %v, want %v", got, want)
	}
	list, err := listBackups()
	if err != nil || len(list) != 1 || list[0].ID != info.ID || list[0].Node != "us2" {
		t.Fatalf("listBackups = %+v, %v", list, err)
	}

	// The signed link streams the zip from R2 through this server.
	link := withDownloadURL(info, time.Now(), "https://ai.thingsapi.com")
	if !strings.HasPrefix(link.DownloadURL, "https://ai.thingsapi.com/api/backups/download?") {
		t.Errorf("link left the request's domain: %s", link.DownloadURL)
	}
	u, _ := url.Parse(link.DownloadURL)
	rec := httptest.NewRecorder()
	handleBackupDownload(rec, httptest.NewRequest(http.MethodGet, "/api/backups/download?"+u.RawQuery, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("download = %d %s", rec.Code, rec.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "manifest.json,things-dav.db,things.db" {
		t.Errorf("zip = %v", names)
	}

	// Unknown backups are a 404, not a proxy error.
	rec = httptest.NewRecorder()
	handleBackupDownload(rec, authedDownload("things-manual-20200101T000000Z.zip"))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown backup = %d", rec.Code)
	}
}

func authedDownload(id string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/backups/download?id="+id, nil)
	r.Header.Set("Authorization", "Bearer backup-test-key")
	return r
}

func TestR2BackupPruning(t *testing.T) {
	s3 := stubR2Backups(t)
	for i := 0; i < backupKeep[backupKindManual]+2; i++ {
		if _, err := createBackup(backupKindManual, nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(s3.keys()); n != 2*backupKeep[backupKindManual] {
		t.Errorf("%d objects after pruning, want %d", n, 2*backupKeep[backupKindManual])
	}
}

// Only one node takes each automatic backup: the others lose the lease.
func TestAutoBackupLease(t *testing.T) {
	stubR2Backups(t)
	now := time.Date(2026, 10, 5, 3, 0, 0, 0, time.UTC)
	backupNow = func() time.Time { return now }
	t.Setenv("NODE_NAME", "us3")
	if !davDB.mustLease(t, "auto-backup", "us1", now) {
		t.Fatal("setup: us1 lease")
	}
	if runAutoBackup(now, 24*time.Hour) {
		t.Fatal("us3 took the backup while us1 held the lease")
	}
	t.Setenv("NODE_NAME", "us1")
	if !runAutoBackup(now, 24*time.Hour) {
		t.Fatal("the lease holder must take the backup")
	}
	// Once one exists, nobody takes another until the interval passes.
	t.Setenv("NODE_NAME", "ro")
	if runAutoBackup(now.Add(time.Hour), 24*time.Hour) {
		t.Error("a second automatic backup was taken within the interval")
	}
	all, _ := listBackups()
	if len(all) != 1 || all[0].Node != "us1" {
		t.Errorf("backups = %+v", all)
	}
}

func (s *davStore) mustLease(t *testing.T, name, holder string, now time.Time) bool {
	t.Helper()
	ok, err := s.acquireLease(name, holder, now, autoBackupLeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	return ok
}

// A backup taken with the store on rqlite contains rqlite's copy of it.
func TestBackupFromRqliteStore(t *testing.T) {
	stubBackups(t)
	rq := newFakeRqlite(t)
	davDB = rqliteTestStore(t, rq.url())
	davDB.recordServed("mac", []*thingsdav.Object{{Key: "task:A", Snapshot: `{}`}})
	info, err := createBackup(backupKindManual, nil)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.OpenReader(filepath.Join(backupDir(), info.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != "things-dav.db" {
			continue
		}
		tmp := filepath.Join(t.TempDir(), "dav.db")
		rc, _ := f.Open()
		b, _ := io.ReadAll(rc)
		rc.Close()
		if err := os.WriteFile(tmp, b, 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := openDAVStore(tmp)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		if c, _ := s.tableCounts(); c["snapshots"] != 1 {
			t.Errorf("backed-up store = %v", c)
		}
		return
	}
	t.Fatal("no things-dav.db in the backup")
}
