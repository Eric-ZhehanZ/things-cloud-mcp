package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// rqliteSQL runs the CalDAV store on an rqlite cluster over its HTTP API
// (https://rqlite.io/docs/api/). The app talks to its own node; rqlite
// forwards writes and strong reads to the leader.
type rqliteSQL struct {
	base       string // scheme://host:port, no credentials
	user, pass string
	level      string // consistency level for strong reads
	hc         *http.Client
}

func newRqliteSQL(raw, level string) (*rqliteSQL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("DAV_RQLITE_URL must look like http://host:4001, got %q", raw)
	}
	r := &rqliteSQL{level: level, hc: &http.Client{Timeout: 10 * time.Second}}
	if u.User != nil {
		r.user = u.User.Username()
		r.pass, _ = u.User.Password()
	}
	if r.user == "" {
		// Credentials may also come from their own variables, so the URL
		// can stay in plain config.
		r.user, r.pass = os.Getenv("DAV_RQLITE_USER"), os.Getenv("DAV_RQLITE_PASSWORD")
	}
	u.User = nil
	r.base = strings.TrimSuffix(u.String(), "/")
	if r.level == "" {
		r.level = "linearizable"
	}
	return r, nil
}

func (r *rqliteSQL) do(method, path string, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, r.base+path, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.user != "" {
		req.SetBasicAuth(r.user, r.pass)
	}
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: rqlite %s: %v", errDAVStoreUnavailable, path, err)
	}
	if resp.StatusCode >= 500 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("%w: rqlite %s: %s %s", errDAVStoreUnavailable, path, resp.Status, strings.TrimSpace(string(msg)))
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		return nil, fmt.Errorf("rqlite %s: %s %s", path, resp.Status, strings.TrimSpace(string(msg)))
	}
	return resp, nil
}

type rqliteResult struct {
	Error        string   `json:"error"`
	RowsAffected int64    `json:"rows_affected"`
	Columns      []string `json:"columns"`
	RawValues    [][]any  `json:"values"`
}

type rqliteResponse struct {
	Results []rqliteResult `json:"results"`
	Error   string         `json:"error"`
}

func rqliteBody(stmts []davStmt) [][]any {
	out := make([][]any, len(stmts))
	for i, s := range stmts {
		out[i] = append([]any{s.SQL}, s.Args...)
	}
	return out
}

func (r *rqliteSQL) post(path string, stmts []davStmt) ([]rqliteResult, error) {
	resp, err := r.do(http.MethodPost, path, rqliteBody(stmts))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	dec.UseNumber()
	var out rqliteResponse
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("rqlite %s: decode: %w", path, err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("rqlite %s: %s", path, out.Error)
	}
	for _, res := range out.Results {
		if res.Error != "" {
			return nil, fmt.Errorf("rqlite: %s", res.Error)
		}
	}
	if len(out.Results) != len(stmts) {
		return nil, fmt.Errorf("rqlite %s: %d results for %d statements", path, len(out.Results), len(stmts))
	}
	return out.Results, nil
}

func (r *rqliteSQL) exec(stmts ...davStmt) ([]int64, error) {
	res, err := r.post("/db/execute?transaction", stmts)
	if err != nil && isNoSuchTable(err) {
		// A store loaded from an older single-node file lacks newer tables.
		if serr := r.ensureSchema(); serr == nil {
			res, err = r.post("/db/execute?transaction", stmts)
		}
	}
	if err != nil {
		return nil, err
	}
	out := make([]int64, len(res))
	for i, x := range res {
		out[i] = x.RowsAffected
	}
	return out, nil
}

func (r *rqliteSQL) query(strong bool, s davStmt) ([][]any, error) {
	level := "weak"
	if strong {
		level = r.level
	}
	res, err := r.post("/db/query?level="+url.QueryEscape(level), []davStmt{s})
	if err != nil && !strong && isStoreUnavailable(err) {
		// Renders keep working without a leader, from this node's copy.
		return r.queryLocal(s)
	}
	if err != nil {
		return nil, err
	}
	return res[0].RawValues, nil
}

// queryLocal reads this node's own copy without asking the leader; it works
// while the cluster has no leader.
func (r *rqliteSQL) queryLocal(s davStmt) ([][]any, error) {
	res, err := r.post("/db/query?level=none", []davStmt{s})
	if err != nil {
		return nil, err
	}
	return res[0].RawValues, nil
}

func (r *rqliteSQL) ensureSchema() error {
	_, err := r.post("/db/execute?transaction", davStoreSchemaStmts())
	return err
}

// ready reports whether the node has a reachable leader.
func (r *rqliteSQL) ready() bool {
	resp, err := r.do(http.MethodGet, "/readyz", nil)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return true
}

// backupTo downloads a SQLite copy of the store from the leader.
func (r *rqliteSQL) backupTo(path string) error {
	resp, err := r.do(http.MethodGet, "/db/backup", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, resp.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && n < 100 {
		err = fmt.Errorf("rqlite backup is only %d bytes", n)
	}
	if err == nil {
		err = checkSQLiteHeader(path)
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func checkSQLiteHeader(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	hdr := make([]byte, 16)
	if _, err := io.ReadFull(f, hdr); err != nil || string(hdr) != "SQLite format 3\x00" {
		return fmt.Errorf("%s is not a SQLite database", path)
	}
	return nil
}

func (r *rqliteSQL) mode() string { return "rqlite" }

func (r *rqliteSQL) close() error { return nil }

func isNoSuchTable(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no such table")
}

// isStoreUnavailable reports errors that mean the cluster can't serve now.
func isStoreUnavailable(err error) bool {
	var ne net.Error
	return errors.Is(err, errDAVStoreUnavailable) || errors.As(err, &ne)
}
