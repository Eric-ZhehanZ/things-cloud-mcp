package main

// Where backups live: a local directory (BACKUP_DIR, default beside the
// Things mirror), or an S3-compatible bucket shared by every node —
// Cloudflare R2 in production — when R2_ENDPOINT, R2_BUCKET,
// R2_ACCESS_KEY_ID and R2_SECRET_ACCESS_KEY are set. Each backup is
// "<id>" (the zip) plus "<id minus .zip>.json" (its index entry), written
// in that order so a listing never shows a half-uploaded backup.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	gosync "sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type backupStorage interface {
	// put stores the local file src under name.
	put(name, src string) error
	// open returns the object for streaming, with its modification time.
	open(name string) (io.ReadSeekCloser, time.Time, error)
	read(name string) ([]byte, error)
	exists(name string) (bool, error)
	// list returns the names ending in suffix.
	list(suffix string) ([]string, error)
	remove(name string) error
	String() string
}

var errBackupNotFound = errors.New("backup not found")

var (
	backupStorageMu       gosync.Mutex
	backupStorageOverride backupStorage // tests
	backupStorageCached   backupStorage
	backupStorageKey      string
)

// currentBackupStorage returns the configured storage, rebuilt when the
// environment changes (tests vary it).
func currentBackupStorage() (backupStorage, error) {
	backupStorageMu.Lock()
	defer backupStorageMu.Unlock()
	if backupStorageOverride != nil {
		return backupStorageOverride, nil
	}
	endpoint, bucket := os.Getenv("R2_ENDPOINT"), os.Getenv("R2_BUCKET")
	if endpoint == "" && bucket == "" {
		return localBackupStorage{dir: backupDir()}, nil
	}
	key := strings.Join([]string{endpoint, bucket, os.Getenv("R2_ACCESS_KEY_ID"), os.Getenv("R2_PREFIX")}, "\x00")
	if backupStorageCached != nil && key == backupStorageKey {
		return backupStorageCached, nil
	}
	s, err := newS3BackupStorage(endpoint, bucket, os.Getenv("R2_ACCESS_KEY_ID"), os.Getenv("R2_SECRET_ACCESS_KEY"), r2Prefix(), nil)
	if err != nil {
		return nil, err
	}
	backupStorageCached, backupStorageKey = s, key
	return s, nil
}

// r2Prefix is R2_PREFIX (default "backups/"), the key prefix in the bucket.
func r2Prefix() string {
	p, ok := os.LookupEnv("R2_PREFIX")
	if !ok {
		return "backups/"
	}
	if p != "" && !strings.HasSuffix(p, "/") {
		p += "/"
	}
	return p
}

// ---------------------------------------------------------------------------
// Local directory
// ---------------------------------------------------------------------------

type localBackupStorage struct{ dir string }

func (l localBackupStorage) String() string { return l.dir }

func (l localBackupStorage) put(name, src string) error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	dst := filepath.Join(l.dir, name)
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	// Different filesystems: copy.
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func (l localBackupStorage) open(name string) (io.ReadSeekCloser, time.Time, error) {
	f, err := os.Open(filepath.Join(l.dir, name))
	if os.IsNotExist(err) {
		return nil, time.Time{}, errBackupNotFound
	}
	if err != nil {
		return nil, time.Time{}, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, time.Time{}, err
	}
	return f, st.ModTime(), nil
}

func (l localBackupStorage) read(name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(l.dir, name))
}

func (l localBackupStorage) exists(name string) (bool, error) {
	_, err := os.Stat(filepath.Join(l.dir, name))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

func (l localBackupStorage) list(suffix string) ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), suffix) {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

func (l localBackupStorage) remove(name string) error {
	err := os.Remove(filepath.Join(l.dir, name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// ---------------------------------------------------------------------------
// S3-compatible bucket (Cloudflare R2)
// ---------------------------------------------------------------------------

type s3BackupStorage struct {
	mc     *minio.Client
	bucket string
	prefix string
	desc   string
}

const backupOpTimeout = 5 * time.Minute

// newS3BackupStorage connects to endpoint (e.g.
// https://<account>.r2.cloudflarestorage.com). transport is for tests.
func newS3BackupStorage(endpoint, bucket, keyID, secret, prefix string, transport http.RoundTripper) (*s3BackupStorage, error) {
	if endpoint == "" || bucket == "" || keyID == "" || secret == "" {
		return nil, fmt.Errorf("R2 backups need R2_ENDPOINT, R2_BUCKET, R2_ACCESS_KEY_ID and R2_SECRET_ACCESS_KEY")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("R2_ENDPOINT must be a URL like https://<account>.r2.cloudflarestorage.com, got %q", endpoint)
	}
	mc, err := minio.New(u.Host, &minio.Options{
		Creds:        credentials.NewStaticV4(keyID, secret, ""),
		Secure:       u.Scheme != "http",
		Region:       "auto",
		BucketLookup: minio.BucketLookupPath,
		Transport:    transport,
	})
	if err != nil {
		return nil, err
	}
	return &s3BackupStorage{mc: mc, bucket: bucket, prefix: prefix, desc: fmt.Sprintf("bucket %s/%s at %s", bucket, prefix, u.Host)}, nil
}

func (s *s3BackupStorage) String() string { return s.desc }

func (s *s3BackupStorage) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), backupOpTimeout)
}

func (s *s3BackupStorage) put(name, src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	ctype := "application/json"
	if strings.HasSuffix(name, ".zip") {
		ctype = "application/zip"
	}
	ctx, cancel := s.ctx()
	defer cancel()
	_, err = s.mc.PutObject(ctx, s.bucket, s.prefix+name, f, st.Size(), minio.PutObjectOptions{
		ContentType: ctype,
		PartSize:    256 << 20, // one PUT for any realistic backup
	})
	if err == nil {
		os.Remove(src)
	}
	return err
}

func isS3NotFound(err error) bool {
	code := minio.ToErrorResponse(err).Code
	return code == "NoSuchKey" || code == "NotFound"
}

type s3Object struct {
	*minio.Object
	cancel context.CancelFunc
}

func (o s3Object) Close() error {
	err := o.Object.Close()
	o.cancel()
	return err
}

func (s *s3BackupStorage) open(name string) (io.ReadSeekCloser, time.Time, error) {
	ctx, cancel := s.ctx()
	obj, err := s.mc.GetObject(ctx, s.bucket, s.prefix+name, minio.GetObjectOptions{})
	if err != nil {
		cancel()
		return nil, time.Time{}, err
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		cancel()
		if isS3NotFound(err) {
			return nil, time.Time{}, errBackupNotFound
		}
		return nil, time.Time{}, err
	}
	return s3Object{Object: obj, cancel: cancel}, st.LastModified, nil
}

func (s *s3BackupStorage) read(name string) ([]byte, error) {
	r, _, err := s.open(name)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(io.LimitReader(r, 1<<20))
}

func (s *s3BackupStorage) exists(name string) (bool, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	_, err := s.mc.StatObject(ctx, s.bucket, s.prefix+name, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if isS3NotFound(err) {
		return false, nil
	}
	return false, err
}

func (s *s3BackupStorage) list(suffix string) ([]string, error) {
	ctx, cancel := s.ctx()
	defer cancel()
	var out []string
	for obj := range s.mc.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix}) {
		if obj.Err != nil {
			return nil, obj.Err
		}
		name := strings.TrimPrefix(obj.Key, s.prefix)
		if !strings.Contains(name, "/") && strings.HasSuffix(name, suffix) {
			out = append(out, name)
		}
	}
	return out, nil
}

func (s *s3BackupStorage) remove(name string) error {
	ctx, cancel := s.ctx()
	defer cancel()
	return s.mc.RemoveObject(ctx, s.bucket, s.prefix+name, minio.RemoveObjectOptions{})
}
