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

package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// CatalogKind identifies one of the name-only catalog tables.
type CatalogKind struct {
	Table   string // catalog table name
	NameCol string // display-name column ("name", or "label" for labels)
	Title   string // display title
	// usageColumns maps tables to the FK column referencing this catalog,
	// used to refuse deleting in-use entries.
	usageColumns map[string]string
}

// Catalogs maps the URL kind to its catalog definition.
var Catalogs = map[string]CatalogKind{
	"providers": {
		Table: "providers", NameCol: "name", Title: "Providers",
		usageColumns: map[string]string{
			"servers":          "provider_id",
			"shared_hosting":   "provider_id",
			"reseller_hosting": "provider_id",
			"seedboxes":        "provider_id",
			"domains":          "provider_id",
		},
	},
	"locations": {
		Table: "locations", NameCol: "name", Title: "Locations",
		usageColumns: map[string]string{
			"servers":          "location_id",
			"shared_hosting":   "location_id",
			"reseller_hosting": "location_id",
			"seedboxes":        "location_id",
		},
	},
	"os": {
		Table: "os", NameCol: "name", Title: "OS",
		usageColumns: map[string]string{
			"servers": "os_id",
		},
	},
	"labels": {
		Table: "labels", NameCol: "label", Title: "Labels",
		usageColumns: map[string]string{
			"labels_assigned": "label_id",
		},
	},
}

// CatalogItem is one row of a name-only catalog.
type CatalogItem struct {
	ID   int64
	Name string
}

// CatalogStore wraps the DB for catalog queries.
type CatalogStore struct {
	DB *sql.DB
}

// List returns all items of a catalog, ordered by name.
func (s *CatalogStore) List(ctx context.Context, kind CatalogKind) ([]CatalogItem, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx,
		"SELECT id, "+kind.NameCol+" FROM "+kind.Table+" ORDER BY "+kind.NameCol+" COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CatalogItem
	for rows.Next() {
		var it CatalogItem
		if err := rows.Scan(&it.ID, &it.Name); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Exists reports whether a catalog row with id exists (used to validate
// references before an insert, so a dangling id is a 422, not an FK 500).
func (s *CatalogStore) Exists(ctx context.Context, kind CatalogKind, id int64) (bool, error) {
	var n int
	err := QuerierFrom(ctx, s.DB).QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+kind.Table+" WHERE id = ?", id).Scan(&n)
	return n > 0, err
}

// Create inserts a new catalog item and returns its ID.
func (s *CatalogStore) Create(ctx context.Context, kind CatalogKind, name string) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO "+kind.Table+" ("+kind.NameCol+") VALUES (?)", name)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Update renames a catalog item.
func (s *CatalogStore) Update(ctx context.Context, kind CatalogKind, id int64, name string) error {
	res, err := s.DB.ExecContext(ctx,
		"UPDATE "+kind.Table+" SET "+kind.NameCol+" = ? WHERE id = ?", name, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UsageCount returns how many service rows reference this catalog entry.
func (s *CatalogStore) UsageCount(ctx context.Context, kind CatalogKind, id int64) (int, error) {
	total := 0
	for table, column := range kind.usageColumns {
		var n int
		if err := QuerierFrom(ctx, s.DB).QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE "+column+" = ?", id).Scan(&n); err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// Delete removes a catalog item. It refuses (ErrInUse) when any service
// still references it.
var ErrInUse = fmt.Errorf("catalog entry in use")

func (s *CatalogStore) Delete(ctx context.Context, kind CatalogKind, id int64) error {
	// Usage check + delete in ONE transaction — no TOCTOU race — with the
	// FK constraint error mapped to a clean ErrInUse as a second line of
	// defense.
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	n := 0
	for table, column := range kind.usageColumns {
		var c int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE "+column+" = ?", id).Scan(&c); err != nil {
			return err
		}
		n += c
	}
	if n > 0 {
		return fmt.Errorf("%w by %d services", ErrInUse, n)
	}

	res, err := tx.ExecContext(ctx, "DELETE FROM "+kind.Table+" WHERE id = ?", id)
	if err != nil {
		if strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
			return fmt.Errorf("%w (concurrent assignment)", ErrInUse)
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}
