package main

import (
	"database/sql"
	"fmt"

	_ "modernc.org/sqlite"
)

// sqliteSQL runs the CalDAV store in a local SQLite file.
type sqliteSQL struct {
	db   *sql.DB
	path string
}

func openSQLiteSQL(path string) (*sqliteSQL, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	for _, s := range []string{"PRAGMA journal_mode=WAL", "PRAGMA busy_timeout=5000", davStoreSchema} {
		if _, err := db.Exec(s); err != nil {
			db.Close()
			return nil, fmt.Errorf("dav store %s: %w", path, err)
		}
	}
	return &sqliteSQL{db: db, path: path}, nil
}

func (s *sqliteSQL) exec(stmts ...davStmt) ([]int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	out, err := execInTx(tx, stmts)
	if err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

func execInTx(tx *sql.Tx, stmts []davStmt) ([]int64, error) {
	out := make([]int64, len(stmts))
	for i, st := range stmts {
		res, err := tx.Exec(st.SQL, st.Args...)
		if err != nil {
			return nil, err
		}
		out[i], _ = res.RowsAffected()
	}
	return out, nil
}

func (s *sqliteSQL) query(_ bool, st davStmt) ([][]any, error) {
	rows, err := s.db.Query(st.SQL, st.Args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func (s *sqliteSQL) backupTo(path string) error {
	_, err := s.db.Exec(`VACUUM INTO ?`, path)
	return err
}

func (s *sqliteSQL) mode() string { return "sqlite" }

func (s *sqliteSQL) close() error { return s.db.Close() }
