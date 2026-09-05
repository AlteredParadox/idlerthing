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
)

// DNSTypes are the record types offered in the UI.
var DNSTypes = []string{"A", "AAAA", "CNAME", "TXT", "MX", "NS", "SRV"}

// DNSRecord mirrors the dns table.
type DNSRecord struct {
	ID         int64
	Hostname   string
	DNSType    string
	Address    string
	ServerID   sql.NullInt64
	DomainID   sql.NullInt64
	SharedID   sql.NullInt64
	ResellerID sql.NullInt64
	CreatedAt  string
	UpdatedAt  string
}

// DNSListItem is one row of the DNS index, with linked entity names.
type DNSListItem struct {
	DNSRecord
	ServerName   string
	DomainName   string
	SharedName   string
	ResellerName string
}

// DNSStore wraps the DB for DNS record queries.
type DNSStore struct {
	DB *sql.DB
}

const dnsColumns = `id, hostname, dns_type, address,
	server_id, domain_id, shared_id, reseller_id, created_at, updated_at`

func scanDNS(row interface{ Scan(...any) error }) (*DNSRecord, error) {
	var d DNSRecord
	err := row.Scan(&d.ID, &d.Hostname, &d.DNSType, &d.Address,
		&d.ServerID, &d.DomainID, &d.SharedID, &d.ResellerID, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// Create inserts a DNS record — atomically, only when every set parent
// still exists (a parent deleted between the handler's validation and this
// insert yields sql.ErrNoRows, not an FK violation 500).
func (s *DNSStore) Create(ctx context.Context, d *DNSRecord) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO dns (hostname, dns_type, address, server_id, domain_id, shared_id, reseller_id)
		SELECT ?, ?, ?, ?, ?, ?, ?
		WHERE (? IS NULL OR EXISTS (SELECT 1 FROM servers WHERE id = ?))
		  AND (? IS NULL OR EXISTS (SELECT 1 FROM domains WHERE id = ?))
		  AND (? IS NULL OR EXISTS (SELECT 1 FROM shared_hosting WHERE id = ?))
		  AND (? IS NULL OR EXISTS (SELECT 1 FROM reseller_hosting WHERE id = ?))`,
		d.Hostname, d.DNSType, d.Address, d.ServerID, d.DomainID, d.SharedID, d.ResellerID,
		d.ServerID, d.ServerID, d.DomainID, d.DomainID, d.SharedID, d.SharedID, d.ResellerID, d.ResellerID)
	if err != nil {
		return 0, fmt.Errorf("insert dns: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, sql.ErrNoRows
	}
	return res.LastInsertId()
}

// Get returns one DNS record.
func (s *DNSStore) Get(ctx context.Context, id int64) (*DNSRecord, error) {
	return scanDNS(QuerierFrom(ctx, s.DB).QueryRowContext(ctx,
		"SELECT "+dnsColumns+" FROM dns WHERE id = ?", id))
}

// Update replaces a DNS record. The same parent-existence guards ride in
// the WHERE clause; RowsAffected==0 maps to sql.ErrNoRows whether the
// RECORD id or a parent vanished (the handler 404s either way — the FK
// protects integrity, and the residual is a clean 404, not a 500).
func (s *DNSStore) Update(ctx context.Context, d *DNSRecord) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE dns SET hostname = ?, dns_type = ?, address = ?,
			server_id = ?, domain_id = ?, shared_id = ?, reseller_id = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ?
		  AND (? IS NULL OR EXISTS (SELECT 1 FROM servers WHERE id = ?))
		  AND (? IS NULL OR EXISTS (SELECT 1 FROM domains WHERE id = ?))
		  AND (? IS NULL OR EXISTS (SELECT 1 FROM shared_hosting WHERE id = ?))
		  AND (? IS NULL OR EXISTS (SELECT 1 FROM reseller_hosting WHERE id = ?))`,
		d.Hostname, d.DNSType, d.Address, d.ServerID, d.DomainID,
		d.SharedID, d.ResellerID, d.ID,
		d.ServerID, d.ServerID, d.DomainID, d.DomainID, d.SharedID, d.SharedID, d.ResellerID, d.ResellerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Delete removes a DNS record.
func (s *DNSStore) Delete(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, "DELETE FROM dns WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// dnsListSelect is the shared SELECT for list queries with linked names.
var dnsListSelect = `
	SELECT ` + prefixedColumns("a", dnsColumns) + `,
		COALESCE(s.hostname, ''), COALESCE(d.domain, ''),
		COALESCE(sh.main_domain, ''), COALESCE(rh.main_domain, '')
	FROM dns a
	LEFT JOIN servers s ON s.id = a.server_id
	LEFT JOIN domains d ON d.id = a.domain_id
	LEFT JOIN shared_hosting sh ON sh.id = a.shared_id
	LEFT JOIN reseller_hosting rh ON rh.id = a.reseller_id`

func scanDNSList(rows *sql.Rows) ([]DNSListItem, error) {
	var out []DNSListItem
	for rows.Next() {
		var it DNSListItem
		err := rows.Scan(&it.ID, &it.Hostname, &it.DNSType, &it.Address,
			&it.ServerID, &it.DomainID, &it.SharedID, &it.ResellerID,
			&it.CreatedAt, &it.UpdatedAt,
			&it.ServerName, &it.DomainName, &it.SharedName, &it.ResellerName)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// List returns all DNS records, ordered by hostname.
func (s *DNSStore) List(ctx context.Context) ([]DNSListItem, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx, dnsListSelect+" ORDER BY a.hostname COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDNSList(rows)
}

// dnsParentColumn maps a service type to the dns column that links to it
// ("" for types DNS cannot link to). One registry for every parent-aware
// site — the detail card used to hard-code servers and domains only, so
// records linked to shared/reseller hosting never appeared on those pages.
func dnsParentColumn(serviceType int) string {
	switch serviceType {
	case ServiceServer:
		return "server_id"
	case ServiceDomain:
		return "domain_id"
	case ServiceShared:
		return "shared_id"
	case ServiceReseller:
		return "reseller_id"
	}
	return ""
}

// DNSLinkable reports whether DNS records can be linked to this service type.
func DNSLinkable(serviceType int) bool { return dnsParentColumn(serviceType) != "" }

// ListForService returns DNS records linked to one service of any linkable
// type (nil, nil for types that cannot carry DNS records).
func (s *DNSStore) ListForService(ctx context.Context, serviceType int, id int64) ([]DNSListItem, error) {
	col := dnsParentColumn(serviceType)
	if col == "" {
		return nil, nil
	}
	// Column name comes from the fixed switch above.
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx,
		dnsListSelect+" WHERE a."+col+" = ? ORDER BY a.hostname COLLATE NOCASE", id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDNSList(rows)
}
