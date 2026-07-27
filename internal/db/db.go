// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

// Package db provides SQLite database access and migrations.
package db

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// dsnPath percent-escapes the URI-reserved characters in a file path so a
// literal path containing %, ?, or # is not reinterpreted as a file: URI
// (which would silently redirect the database to a truncated path while we
// chmod the 0-byte decoy). Slashes and spaces stay literal.
func dsnPath(path string) string {
	r := strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")
	return r.Replace(path)
}

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

	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", dsnPath(path))
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
