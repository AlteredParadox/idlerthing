package db

import (
	"database/sql"
	"embed"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate applies any pending migrations to db and returns the list of
// migration versions applied during this call. Migrations are tracked via
// PRAGMA user_version; each embedded migrations/NNNN_name.sql file whose
// numeric prefix exceeds the current version is applied in a transaction.
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
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return applied, fmt.Errorf("apply migration %s: %w", m.name, err)
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
