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

// Seedbox mirrors the seedboxes table.
type Seedbox struct {
	ID            int64
	Title         sql.NullString
	Hostname      string
	SeedBoxType   sql.NullString
	ProviderID    sql.NullInt64
	LocationID    sql.NullInt64
	PortSpeed     sql.NullInt64 // mbps
	DiskAsMB      sql.NullInt64
	BandwidthAsMB sql.NullInt64
	Active        bool
	ShowPublic    bool
	WasPromo      bool
	OwnedSince    sql.NullString
	CreatedAt     string
	UpdatedAt     string
}

const seedboxColumns = `id, title, hostname, seed_box_type, provider_id, location_id,
	port_speed, disk_as_mb, bandwidth_as_mb, active, show_public, was_promo,
	owned_since, created_at, updated_at`

func scanSeedbox(row interface{ Scan(...any) error }) (*Seedbox, error) {
	var b Seedbox
	var active, showPublic, wasPromo int
	err := row.Scan(&b.ID, &b.Title, &b.Hostname, &b.SeedBoxType, &b.ProviderID,
		&b.LocationID, &b.PortSpeed, &b.DiskAsMB, &b.BandwidthAsMB, &active,
		&showPublic, &wasPromo, &b.OwnedSince, &b.CreatedAt, &b.UpdatedAt)
	if err != nil {
		return nil, err
	}
	b.Active = active != 0
	b.ShowPublic = showPublic != 0
	b.WasPromo = wasPromo != 0
	return &b, nil
}

// SeedboxStore wraps the DB for seedbox queries.
type SeedboxStore struct{ DB *sql.DB }

// Create inserts a seedbox plus optional pricing in one transaction.
func (st *SeedboxStore) Create(ctx context.Context, b *Seedbox, pricing *Pricing) (int64, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO seedboxes (title, hostname, seed_box_type, provider_id, location_id,
			port_speed, disk_as_mb, bandwidth_as_mb, active, show_public, was_promo, owned_since)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.Title, b.Hostname, b.SeedBoxType, b.ProviderID, b.LocationID,
		b.PortSpeed, b.DiskAsMB, b.BandwidthAsMB, boolToInt(b.Active),
		boolToInt(b.ShowPublic), boolToInt(b.WasPromo), b.OwnedSince)
	if err != nil {
		return 0, fmt.Errorf("insert seedbox: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := upsertPricingTx(ctx, tx, ServiceSeedbox, id, pricing); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Get returns one seedbox with its pricing.
func (st *SeedboxStore) Get(ctx context.Context, id int64) (*Seedbox, *Pricing, error) {
	b, err := scanSeedbox(QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT "+seedboxColumns+" FROM seedboxes WHERE id = ?", id))
	if err != nil {
		return nil, nil, err
	}
	pricing, err := (&PricingStore{DB: st.DB}).Get(ctx, ServiceSeedbox, id)
	if err != nil {
		return nil, nil, err
	}
	return b, pricing, nil
}

// Update replaces a seedbox's fields and pricing in one transaction.
func (st *SeedboxStore) Update(ctx context.Context, b *Seedbox, pricing *Pricing) error {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE seedboxes SET title = ?, hostname = ?, seed_box_type = ?,
			provider_id = ?, location_id = ?, port_speed = ?, disk_as_mb = ?,
			bandwidth_as_mb = ?, active = ?, show_public = ?, was_promo = ?,
			owned_since = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		b.Title, b.Hostname, b.SeedBoxType, b.ProviderID, b.LocationID,
		b.PortSpeed, b.DiskAsMB, b.BandwidthAsMB, boolToInt(b.Active),
		boolToInt(b.ShowPublic), boolToInt(b.WasPromo), b.OwnedSince, b.ID)
	if err != nil {
		return fmt.Errorf("update seedbox: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := upsertPricingTx(ctx, tx, ServiceSeedbox, b.ID, pricing); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete removes a seedbox plus its polymorphic children.
func (st *SeedboxStore) Delete(ctx context.Context, id int64) error {
	return deleteServiceTx(ctx, st.DB, "seedboxes", ServiceSeedbox, id)
}

// SeedboxListItem is one row of the seedbox list view.
type SeedboxListItem struct {
	Seedbox
	ProviderName string
	LocationName string
	Pricing      *Pricing
}

var seedboxSortColumns = map[string]string{
	"title":    "s.title COLLATE NOCASE",
	"hostname": "s.hostname COLLATE NOCASE",
	"type":     "s.seed_box_type COLLATE NOCASE",
	"port":     "s.port_speed",
	"disk":     "s.disk_as_mb",
	"bw":       "s.bandwidth_as_mb IS NULL, s.bandwidth_as_mb",
	"location": "loc_name COLLATE NOCASE",
	"provider": "prov_name COLLATE NOCASE",
	"price":    "pr.price",
	"due":      "pr.next_due_date",
}

// List returns filtered/sorted seedboxes.
func (st *SeedboxStore) List(ctx context.Context, opts ListOptions) ([]SeedboxListItem, error) {
	var where []string
	var args []any
	switch opts.Status {
	case "inactive":
		where = append(where, "s.active = 0")
	case "all":
	default:
		where = append(where, "s.active = 1")
	}
	if opts.Q != "" {
		like := likePattern(opts.Q)
		where = append(where, "(s.hostname LIKE ? ESCAPE '\\' OR s.title LIKE ? ESCAPE '\\' OR prov_name LIKE ? ESCAPE '\\')")
		args = append(args, like, like, like)
	}

	orderBy := seedboxSortColumns["hostname"]
	if col, ok := seedboxSortColumns[opts.Sort]; ok {
		orderBy = col
	}
	orderBy = orderClause(orderBy, opts.Dir)

	query := `
		SELECT ` + prefixedColumns("s", seedboxColumns) + `,
			COALESCE(prov_name, ''), COALESCE(loc_name, ''),
			pr.currency, pr.price, pr.term, pr.next_due_date
		FROM (
			SELECT s.*, p.name AS prov_name, l.name AS loc_name
			FROM seedboxes s
			LEFT JOIN providers p ON p.id = s.provider_id
			LEFT JOIN locations l ON l.id = s.location_id
		) s
		LEFT JOIN pricings pr ON pr.service_id = s.id AND pr.service_type = 6 AND pr.active = 1`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + orderBy

	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SeedboxListItem
	for rows.Next() {
		var it SeedboxListItem
		var active, showPublic, wasPromo int
		var currency sql.NullString
		var price sql.NullFloat64
		var term sql.NullInt64
		var due sql.NullString
		err := rows.Scan(&it.ID, &it.Title, &it.Hostname, &it.SeedBoxType,
			&it.ProviderID, &it.LocationID, &it.PortSpeed, &it.DiskAsMB,
			&it.BandwidthAsMB, &active, &showPublic, &wasPromo, &it.OwnedSince,
			&it.CreatedAt, &it.UpdatedAt,
			&it.ProviderName, &it.LocationName, &currency, &price, &term, &due)
		if err != nil {
			return nil, err
		}
		it.Active = active != 0
		it.ShowPublic = showPublic != 0
		it.WasPromo = wasPromo != 0
		if currency.Valid {
			it.Pricing = &Pricing{
				ServiceID:   it.ID,
				ServiceType: ServiceSeedbox,
				Currency:    currency.String,
				Price:       price.Float64,
				Term:        int(term.Int64),
				NextDueDate: due,
				Active:      true,
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// StatusCounts returns active and inactive seedbox counts.
func (st *SeedboxStore) StatusCounts(ctx context.Context) (active, inactive int, err error) {
	err = QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT COALESCE(SUM(active = 1), 0), COALESCE(SUM(active = 0), 0) FROM seedboxes").
		Scan(&active, &inactive)
	return active, inactive, err
}

// prefixedColumns prefixes a comma-separated column list with a table alias.
func prefixedColumns(prefix, cols string) string {
	parts := strings.Split(cols, ",")
	for i, c := range parts {
		parts[i] = prefix + "." + strings.TrimSpace(c)
	}
	return strings.Join(parts, ", ")
}
