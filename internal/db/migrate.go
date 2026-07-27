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

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return nil, fmt.Errorf("read embedded migrations: %w", err)
	}

	// Never run against a DB from a newer build.
	var maxVersion int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "_")
		if v, err := strconv.Atoi(prefix); err == nil && v > maxVersion {
			maxVersion = v
		}
	}
	if current > maxVersion {
		return nil, fmt.Errorf("database was created by a newer version of idlerthing (user_version %d > %d)", current, maxVersion)
	}

	type migration struct {
		version int
		name    string
	}
	var pending []migration
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		prefix, _, _ := strings.Cut(e.Name(), "_")
		v, err := strconv.Atoi(prefix)
		if err != nil {
			return nil, fmt.Errorf("migration %s: bad numeric prefix: %w", e.Name(), err)
		}
		if v > current {
			pending = append(pending, migration{version: v, name: e.Name()})
		}
	}
	sort.Slice(pending, func(i, j int) bool { return pending[i].version < pending[j].version })

	var applied []int
	for _, m := range pending {
		body, err := migrationsFS.ReadFile("migrations/" + m.name)
		if err != nil {
			return applied, fmt.Errorf("read migration %s: %w", m.name, err)
		}

		tx, err := db.Begin()
		if err != nil {
			return applied, fmt.Errorf("begin migration %s: %w", m.name, err)
		}
		// Row counts before, for delete-logging after (see deleteTableRe).
		before := map[string]int{}
		for _, dm := range deleteTableRe.FindAllStringSubmatch(string(body), -1) {
			var n int
			if err := tx.QueryRow("SELECT COUNT(*) FROM " + dm[1]).Scan(&n); err == nil {
				before[dm[1]] = n
			}
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return applied, fmt.Errorf("apply migration %s: %w", m.name, err)
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
			tx.Rollback()
			return applied, fmt.Errorf("set user_version for %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return applied, fmt.Errorf("commit migration %s: %w", m.name, err)
		}
		applied = append(applied, m.version)
	}
	return applied, nil
}
