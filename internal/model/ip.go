package model

import (
	"context"
	"database/sql"
)

// IP mirrors the ips table.
type IP struct {
	ID          int64
	ServiceID   int64
	ServiceType int
	Address     string
	IsIPv4      bool
	Country     sql.NullString
	Region      sql.NullString
	City        sql.NullString
	Org         sql.NullString
	Isp         sql.NullString
	Asn         sql.NullString
	FetchedAt   sql.NullString
	CreatedAt   string
	UpdatedAt   string
}

// IPWithTarget is an IP plus its service's display name (index page).
type IPWithTarget struct {
	IP
	Target string
}

// WhoisData holds refreshed whois fields for one IP.
type WhoisData struct {
	Country   string
	Region    string
	City      string
	Org       string
	Isp       string
	Asn       string
	FetchedAt string
}

// IPStore wraps the DB for IP queries.
type IPStore struct {
	DB *sql.DB
}

const ipColumns = `id, service_id, service_type, address, is_ipv4,
	country, region, city, org, isp, asn, fetched_at, created_at, updated_at`

func scanIP(row interface{ Scan(...any) error }) (*IP, error) {
	var ip IP
	var v4 int
	err := row.Scan(&ip.ID, &ip.ServiceID, &ip.ServiceType, &ip.Address, &v4,
		&ip.Country, &ip.Region, &ip.City, &ip.Org, &ip.Isp, &ip.Asn,
		&ip.FetchedAt, &ip.CreatedAt, &ip.UpdatedAt)
	if err != nil {
		return nil, err
	}
	ip.IsIPv4 = v4 != 0
	return &ip, nil
}

// Create attaches an IP to a service. Duplicate (service, address) pairs
// fail with the UNIQUE constraint.
func (s *IPStore) Create(ctx context.Context, ip *IP) (int64, error) {
	res, err := s.DB.ExecContext(ctx,
		"INSERT INTO ips (service_id, service_type, address, is_ipv4) VALUES (?, ?, ?, ?)",
		ip.ServiceID, ip.ServiceType, ip.Address, boolToInt(ip.IsIPv4))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// Get returns one IP.
func (s *IPStore) Get(ctx context.Context, id int64) (*IP, error) {
	return scanIP(s.DB.QueryRowContext(ctx, "SELECT "+ipColumns+" FROM ips WHERE id = ?", id))
}

// ListFor returns IPs attached to one service.
func (s *IPStore) ListFor(ctx context.Context, serviceID int64, serviceType int) ([]IP, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT "+ipColumns+" FROM ips WHERE service_id = ? AND service_type = ? ORDER BY address",
		serviceID, serviceType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IP
	for rows.Next() {
		ip, err := scanIP(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *ip)
	}
	return out, rows.Err()
}

// ListAll returns every IP with its target's display name.
func (s *IPStore) ListAll(ctx context.Context) ([]IPWithTarget, error) {
	rows, err := s.DB.QueryContext(ctx, `
		SELECT `+prefixedColumns("a", ipColumns)+`, `+TargetNameSQL+` AS target
		FROM ips a ORDER BY a.address`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IPWithTarget
	for rows.Next() {
		var it IPWithTarget
		var v4 int
		err := rows.Scan(&it.ID, &it.ServiceID, &it.ServiceType, &it.Address, &v4,
			&it.Country, &it.Region, &it.City, &it.Org, &it.Isp, &it.Asn,
			&it.FetchedAt, &it.CreatedAt, &it.UpdatedAt, &it.Target)
		if err != nil {
			return nil, err
		}
		it.IsIPv4 = v4 != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// Delete removes an IP.
func (s *IPStore) Delete(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, "DELETE FROM ips WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateWhois replaces the whois fields of one IP.
func (s *IPStore) UpdateWhois(ctx context.Context, id int64, w *WhoisData) error {
	res, err := s.DB.ExecContext(ctx, `
		UPDATE ips SET country = ?, region = ?, city = ?, org = ?, isp = ?, asn = ?,
			fetched_at = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		nullStr(w.Country), nullStr(w.Region), nullStr(w.City),
		nullStr(w.Org), nullStr(w.Isp), nullStr(w.Asn), w.FetchedAt, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
