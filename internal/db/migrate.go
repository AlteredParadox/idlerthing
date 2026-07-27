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

package db

import (
	"database/sql"
	"embed"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// deleteTableRe finds delete targets in a migration body. Table names come
// from the trusted embedded SQL files, so COUNT interpolation is safe.
// (Statement-level RowsAffected isn't available from a multi-statement
// Exec, and splitting the body naively breaks on semicolons inside SQL
// comments — 0007 has them.)
var deleteTableRe = regexp.MustCompile(`(?i)DELETE\s+FROM\s+(\w+)`)

// migration is one embedded migration file.
type migration struct {
	version int
	name    string
}

// Migrate applies any pending migrations to db and returns the list of
// migration versions applied during this call. Migrations are tracked via
// PRAGMA user_version; each embedded migrations/NNNN_name.sql file whose
// numeric prefix exceeds the current version is applied in a transaction.
// Delete-bearing migrations log the affected row count per target table.
func Migrate(db *sql.DB) ([]int, error) {
	var current int
	if err := db.QueryRow("PRAGMA user_version").Scan(&current); err != nil {
		return nil, fmt.Errorf("read user_version: %w", err)
	}
	pending, err := pendingMigrations(current)
	if err != nil {
		return nil, err
	}
	var applied []int
	for _, m := range pending {
		if err := applyMigration(db, m); err != nil {
			return applied, err
		}
		applied = append(applied, m.version)
	}
	return applied, nil
}

// pendingMigrations lists the embedded migrations beyond current, sorted by
// version. A DB from a newer build is refused.
func pendingMigrations(current int) ([]migration, error) {
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	var pending []migration
	maxVersion := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad numeric prefix: %w", e.Name(), err)
		}
		if v > maxVersion {
			maxVersion = v
		}
		if v > current {
			pending = append(pending, migration{version: v, name: e.Name()})
		}
	}
	if current > maxVersion {
		return nil, fmt.Errorf("database was created by a newer version of idlerthing (user_version %d > %d)", current, maxVersion)
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })
	return pending, nil
}

// applyMigration runs one migration in a transaction, logging the rows each
// DELETE removes (counted before/after — see deleteTableRe).
func applyMigration(db *sql.DB, m migration) error {
	body, err := migrationsFS.ReadFile("migrations/" + m.name)
	if err != nil {
		return fmt.Errorf("read migration %s: %w", m.name, err)
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", m.name, err)
	}
	defer tx.Rollback()

	before := map[string]int{}
	for _, dm := range deleteTableRe.FindAllStringSubmatch(string(body), -1) {
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM " + dm[1]).Scan(&n); err == nil {
			before[dm[1]] = n
		}
	}
	if _, err := tx.Exec(string(body)); err != nil {
		return fmt.Errorf("apply migration %s: %w", m.name, err)
	}
	for table, n := range before {
		var after int
		if err := tx.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&after); err == nil && after != n {
			slog.Info("migration deleted rows", "migration", m.name, "table", table, "rows", n-after)
		}
	}
	// user_version cannot be set as a bound parameter; version comes from
	// the trusted embedded filename so interpolation is safe.
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.version)); err != nil {
		return fmt.Errorf("set user_version for %s: %w", m.name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit migration %s: %w", m.name, err)
	}
	return nil
}
