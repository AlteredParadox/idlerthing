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

// Create inserts a DNS record.
func (s *DNSStore) Create(ctx context.Context, d *DNSRecord) (int64, error) {
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO dns (hostname, dns_type, address, server_id, domain_id, shared_id, reseller_id)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		d.Hostname, d.DNSType, d.Address, d.ServerID, d.DomainID, d.SharedID, d.ResellerID)
	if err != nil {
		return 0, fmt.Errorf("insert dns: %w", err)
	}
	return res.LastInsertId()
}

// Get returns one DNS record.
func (s *DNSStore) Get(ctx context.Context, id int64) (*DNSRecord, error) {
	return scanDNS(QuerierFrom(ctx, s.DB).QueryRowContext(ctx,
		"SELECT "+dnsColumns+" FROM dns WHERE id = ?", id))
}

// Update replaces a DNS record.
func (s *DNSStore) Update(ctx context.Context, d *DNSRecord) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE dns SET hostname = ?, dns_type = ?, address = ?,
			server_id = ?, domain_id = ?, shared_id = ?, reseller_id = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		d.Hostname, d.DNSType, d.Address, d.ServerID, d.DomainID,
		d.SharedID, d.ResellerID, d.ID)
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

// ListForServer returns DNS records linked to a server.
func (s *DNSStore) ListForServer(ctx context.Context, serverID int64) ([]DNSListItem, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx,
		dnsListSelect+" WHERE a.server_id = ? ORDER BY a.hostname COLLATE NOCASE", serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDNSList(rows)
}

// ListForDomain returns DNS records linked to a domain.
func (s *DNSStore) ListForDomain(ctx context.Context, domainID int64) ([]DNSListItem, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx,
		dnsListSelect+" WHERE a.domain_id = ? ORDER BY a.hostname COLLATE NOCASE", domainID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanDNSList(rows)
}
