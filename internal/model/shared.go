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

// SharedHosting mirrors the shared_hosting table.
type SharedHosting struct {
	ID              int64
	MainDomain      string
	SharedType      sql.NullString
	ProviderID      sql.NullInt64
	LocationID      sql.NullInt64
	DomainsLimit    sql.NullInt64
	SubdomainsLimit sql.NullInt64
	FtpLimit        sql.NullInt64
	EmailLimit      sql.NullInt64
	DbLimit         sql.NullInt64
	DiskAsMB        sql.NullInt64
	BandwidthAsMB   sql.NullInt64
	HasDedicatedIP  bool
	IP              sql.NullString
	Active          bool
	ShowPublic      bool
	WasPromo        bool
	OwnedSince      sql.NullString
	CreatedAt       string
	UpdatedAt       string
}

// hostingColumns are the columns shared by shared_hosting and reseller_hosting.
// typeCol differs ("shared_type" vs "reseller_type").
func hostingColumns(typeCol string) string {
	return `id, main_domain, ` + typeCol + `, provider_id, location_id,
		domains_limit, subdomains_limit, ftp_limit, email_limit, db_limit,
		disk_as_mb, bandwidth_as_mb, has_dedicated_ip, ip,
		active, show_public, was_promo, owned_since, created_at, updated_at`
}

// hostingListColumns prefixes each column with "s." for the joined list
// query, where bare names would be ambiguous.
func hostingListColumns(typeCol string) string {
	return prefixedColumns("s", hostingColumns(typeCol))
}

func scanHosting(row interface{ Scan(...any) error }, typeVal *sql.NullString, h *SharedHosting) error {
	var hasIP, active, showPublic, wasPromo int
	err := row.Scan(&h.ID, &h.MainDomain, typeVal, &h.ProviderID, &h.LocationID,
		&h.DomainsLimit, &h.SubdomainsLimit, &h.FtpLimit, &h.EmailLimit, &h.DbLimit,
		&h.DiskAsMB, &h.BandwidthAsMB, &hasIP, &h.IP,
		&active, &showPublic, &wasPromo, &h.OwnedSince, &h.CreatedAt, &h.UpdatedAt)
	if err != nil {
		return err
	}
	h.HasDedicatedIP = hasIP != 0
	h.Active = active != 0
	h.ShowPublic = showPublic != 0
	h.WasPromo = wasPromo != 0
	return nil
}

// hostingStore is the shared implementation behind SharedStore and ResellerStore.
type hostingStore struct {
	DB          *sql.DB
	table       string // "shared_hosting" or "reseller_hosting"
	typeCol     string // "shared_type" or "reseller_type"
	serviceType int    // pricings service_type
}

func (st *hostingStore) create(ctx context.Context, h *SharedHosting, pricing *Pricing) (int64, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO `+st.table+` (main_domain, `+st.typeCol+`, provider_id, location_id,
			domains_limit, subdomains_limit, ftp_limit, email_limit, db_limit,
			disk_as_mb, bandwidth_as_mb, has_dedicated_ip, ip,
			active, show_public, was_promo, owned_since)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		h.MainDomain, h.SharedType, h.ProviderID, h.LocationID,
		h.DomainsLimit, h.SubdomainsLimit, h.FtpLimit, h.EmailLimit, h.DbLimit,
		h.DiskAsMB, h.BandwidthAsMB, boolToInt(h.HasDedicatedIP), h.IP,
		boolToInt(h.Active), boolToInt(h.ShowPublic), boolToInt(h.WasPromo), h.OwnedSince)
	if err != nil {
		return 0, fmt.Errorf("insert %s: %w", st.table, err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := upsertPricingTx(ctx, tx, st.serviceType, id, pricing); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (st *hostingStore) get(ctx context.Context, id int64) (*SharedHosting, *Pricing, error) {
	h := &SharedHosting{}
	err := scanHosting(QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT "+hostingColumns(st.typeCol)+" FROM "+st.table+" WHERE id = ?", id),
		&h.SharedType, h)
	if err != nil {
		return nil, nil, err
	}
	pricing, err := (&PricingStore{DB: st.DB}).Get(ctx, st.serviceType, id)
	if err != nil {
		return nil, nil, err
	}
	return h, pricing, nil
}

func (st *hostingStore) update(ctx context.Context, h *SharedHosting, pricing *Pricing) error {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE `+st.table+` SET main_domain = ?, `+st.typeCol+` = ?, provider_id = ?,
			location_id = ?, domains_limit = ?, subdomains_limit = ?, ftp_limit = ?,
			email_limit = ?, db_limit = ?, disk_as_mb = ?, bandwidth_as_mb = ?,
			has_dedicated_ip = ?, ip = ?, active = ?, show_public = ?,
			was_promo = ?, owned_since = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		h.MainDomain, h.SharedType, h.ProviderID, h.LocationID,
		h.DomainsLimit, h.SubdomainsLimit, h.FtpLimit, h.EmailLimit, h.DbLimit,
		h.DiskAsMB, h.BandwidthAsMB, boolToInt(h.HasDedicatedIP), h.IP,
		boolToInt(h.Active), boolToInt(h.ShowPublic), boolToInt(h.WasPromo),
		h.OwnedSince, h.ID)
	if err != nil {
		return fmt.Errorf("update %s: %w", st.table, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := upsertPricingTx(ctx, tx, st.serviceType, h.ID, pricing); err != nil {
		return err
	}
	return tx.Commit()
}

func (st *hostingStore) delete(ctx context.Context, id int64) error {
	return deleteServiceTx(ctx, st.DB, st.table, st.serviceType, id)
}

// HostingListItem is one row of a hosting list view.
type HostingListItem struct {
	SharedHosting
	ProviderName string
	LocationName string
	Pricing      *Pricing
}

// hostingSortColumns whitelists sortable hosting list columns.
var hostingSortColumns = map[string]string{
	"domain":   "s.main_domain COLLATE NOCASE",
	"type":     "type_val COLLATE NOCASE",
	"disk":     "s.disk_as_mb",
	"bw":       "s.bandwidth_as_mb IS NULL, s.bandwidth_as_mb",
	"location": "loc_name COLLATE NOCASE",
	"provider": "prov_name COLLATE NOCASE",
	"price":    "pr.price",
	"due":      "pr.next_due_date",
}

func (st *hostingStore) list(ctx context.Context, opts ListOptions) ([]HostingListItem, error) {
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
		where = append(where, "(s.main_domain LIKE ? ESCAPE '\\' OR type_val LIKE ? ESCAPE '\\' OR prov_name LIKE ? ESCAPE '\\')")
		args = append(args, like, like, like)
	}

	orderBy := hostingSortColumns["domain"]
	if col, ok := hostingSortColumns[opts.Sort]; ok {
		orderBy = col
	}
	if opts.Dir == "desc" {
		orderBy += " DESC"
	}

	query := `
		SELECT ` + hostingListColumns(st.typeCol) + `,
			COALESCE(prov_name, ''), COALESCE(loc_name, ''),
			pr.currency, pr.price, pr.term, pr.next_due_date
		FROM (
			SELECT s.*, p.name AS prov_name, l.name AS loc_name,
				s.` + st.typeCol + ` AS type_val
			FROM ` + st.table + ` s
			LEFT JOIN providers p ON p.id = s.provider_id
			LEFT JOIN locations l ON l.id = s.location_id
		) s
		LEFT JOIN pricings pr ON pr.service_id = s.id AND pr.service_type = ` + fmt.Sprint(st.serviceType) + ` AND pr.active = 1`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + orderBy

	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []HostingListItem
	for rows.Next() {
		var it HostingListItem
		var hasIP, active, showPublic, wasPromo int
		var currency sql.NullString
		var price sql.NullFloat64
		var term sql.NullInt64
		var due sql.NullString
		err := rows.Scan(&it.ID, &it.MainDomain, &it.SharedType, &it.ProviderID,
			&it.LocationID, &it.DomainsLimit, &it.SubdomainsLimit, &it.FtpLimit,
			&it.EmailLimit, &it.DbLimit, &it.DiskAsMB, &it.BandwidthAsMB, &hasIP,
			&it.IP, &active, &showPublic, &wasPromo, &it.OwnedSince,
			&it.CreatedAt, &it.UpdatedAt,
			&it.ProviderName, &it.LocationName, &currency, &price, &term, &due)
		if err != nil {
			return nil, err
		}
		it.HasDedicatedIP = hasIP != 0
		it.Active = active != 0
		it.ShowPublic = showPublic != 0
		it.WasPromo = wasPromo != 0
		if currency.Valid {
			it.Pricing = &Pricing{
				ServiceID:   it.ID,
				ServiceType: st.serviceType,
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

func (st *hostingStore) statusCounts(ctx context.Context) (active, inactive int, err error) {
	err = QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT COALESCE(SUM(active = 1), 0), COALESCE(SUM(active = 0), 0) FROM "+st.table).
		Scan(&active, &inactive)
	return active, inactive, err
}

func (st *hostingStore) distinctProviders(ctx context.Context) (int, error) {
	var n int
	err := QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT provider_id) FROM "+st.table+" WHERE provider_id IS NOT NULL").Scan(&n)
	return n, err
}

// SharedStore wraps the DB for shared hosting queries.
type SharedStore struct{ DB *sql.DB }

func (s *SharedStore) impl() *hostingStore {
	return &hostingStore{DB: s.DB, table: "shared_hosting", typeCol: "shared_type", serviceType: ServiceShared}
}

func (s *SharedStore) Create(ctx context.Context, h *SharedHosting, p *Pricing) (int64, error) {
	return s.impl().create(ctx, h, p)
}
func (s *SharedStore) Get(ctx context.Context, id int64) (*SharedHosting, *Pricing, error) {
	return s.impl().get(ctx, id)
}
func (s *SharedStore) Update(ctx context.Context, h *SharedHosting, p *Pricing) error {
	return s.impl().update(ctx, h, p)
}
func (s *SharedStore) Delete(ctx context.Context, id int64) error { return s.impl().delete(ctx, id) }
func (s *SharedStore) List(ctx context.Context, opts ListOptions) ([]HostingListItem, error) {
	return s.impl().list(ctx, opts)
}
func (s *SharedStore) StatusCounts(ctx context.Context) (int, int, error) {
	return s.impl().statusCounts(ctx)
}
func (s *SharedStore) DistinctProviders(ctx context.Context) (int, error) {
	return s.impl().distinctProviders(ctx)
}
