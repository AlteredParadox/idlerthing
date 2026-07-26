// Package db provides SQLite database access and migrations.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Open opens (creating if necessary) the SQLite database at path, creating the
// parent directory if needed, and applies connection pragmas.
func Open(path string) (*sql.DB, error) {
	if path != ":memory:" {
		// Sessions/CSRF tokens live in this file — keep it private.
		// Create with 0700, but NEVER chmod a directory that already
		// existed (IDLER_DB=/tmp/x.db must not tighten /tmp).
		dir := filepath.Dir(path)
		_, statErr := os.Stat(dir)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create db directory: %w", err)
		}
		if os.IsNotExist(statErr) {
			if err := os.Chmod(dir, 0o700); err != nil {
				return nil, fmt.Errorf("chmod db directory: %w", err)
			}
		}
		// Pre-create the DB file 0600 when absent so the process umask
		// can never leak it.
		if _, err := os.Stat(path); os.IsNotExist(err) {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
			if err != nil {
				return nil, fmt.Errorf("create db file: %w", err)
			}
			f.Close()
		}
	}

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}

	// SQLite allows a single writer; keep one connection to avoid SQLITE_BUSY.
	db.SetMaxOpenConns(1)

	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}

	if path != ":memory:" {
		// Tighten db/-wal/-shm; failure to enforce confidentiality is fatal.
		for _, f := range []string{path, path + "-wal", path + "-shm"} {
			if _, err := os.Stat(f); err == nil {
				if err := os.Chmod(f, 0o600); err != nil {
					db.Close()
					return nil, fmt.Errorf("chmod %s: %w", f, err)
				}
			}
		}
	}
	return db, nil
}
