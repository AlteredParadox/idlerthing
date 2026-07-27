package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Server types.
const (
	TypeKVM      = 1
	TypeOVZ      = 2
	TypeDedi     = 3
	TypeLXC      = 4
	TypeSemiDedi = 5
	TypeVMware   = 6
	TypeNAT      = 7
)

// ServerTypeLabel returns the badge label for a server type.
func ServerTypeLabel(t int) string {
	switch t {
	case TypeKVM:
		return "KVM"
	case TypeOVZ:
		return "OVZ"
	case TypeDedi:
		return "DEDI"
	case TypeLXC:
		return "LXC"
	case TypeSemiDedi:
		return "SEMI-DEDI"
	case TypeVMware:
		return "VMware"
	case TypeNAT:
		return "NAT"
	default:
		return "?"
	}
}

// Server mirrors the servers table.
type Server struct {
	ID            int64
	Hostname      string
	ServerType    int
	OsID          sql.NullInt64
	ProviderID    sql.NullInt64
	LocationID    sql.NullInt64
	RamAsMB       sql.NullInt64
	CPU           sql.NullInt64
	CPUModel      sql.NullString
	BandwidthAsMB sql.NullInt64 // MB; NULL = unlimited
	LinkSpeed     sql.NullInt64 // mbps
	NetworkType   sql.NullString
	Ns1           sql.NullString
	Ns2           sql.NullString
	SSHPort       sql.NullInt64
	Active        bool
	ShowPublic    bool
	HasYabs       bool
	WasPromo      bool
	Transferrable bool
	OwnedSince    sql.NullString
	CreatedAt     string
	UpdatedAt     string
}

// ServerDisk mirrors the server_disks table.
type ServerDisk struct {
	ID       int64
	ServerID int64
	SizeAsMB int64
	Media    string
}

// ServerStore wraps the DB for server queries.
type ServerStore struct {
	DB *sql.DB
}

const serverColumns = `id, hostname, server_type, os_id, provider_id, location_id,
	ram_as_mb, cpu, cpu_model, bandwidth_as_mb, link_speed, network_type, ns1, ns2,
	ssh_port, active, show_public, has_yabs, was_promo, transferrable,
	owned_since, created_at, updated_at`

// listColumns is serverColumns with each column prefixed "s." for the
// joined list query, where bare names would be ambiguous.
var listColumns = prefixedColumns("s", serverColumns)

func scanServer(row interface{ Scan(...any) error }) (*Server, error) {
	var s Server
	var active, showPublic, hasYabs, wasPromo, transferrable int
	err := row.Scan(&s.ID, &s.Hostname, &s.ServerType, &s.OsID, &s.ProviderID,
		&s.LocationID, &s.RamAsMB, &s.CPU, &s.CPUModel, &s.BandwidthAsMB, &s.LinkSpeed,
		&s.NetworkType, &s.Ns1, &s.Ns2, &s.SSHPort, &active, &showPublic, &hasYabs,
		&wasPromo, &transferrable, &s.OwnedSince, &s.CreatedAt, &s.UpdatedAt)
	if err != nil {
		return nil, err
	}
	s.Active = active != 0
	s.ShowPublic = showPublic != 0
	s.HasYabs = hasYabs != 0
	s.WasPromo = wasPromo != 0
	s.Transferrable = transferrable != 0
	return &s, nil
}

// Create inserts a server plus its disks and optional pricing in one transaction.
func (st *ServerStore) Create(ctx context.Context, s *Server, disks []ServerDisk, pricing *Pricing) (int64, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO servers (hostname, server_type, os_id, provider_id, location_id,
			ram_as_mb, cpu, cpu_model, bandwidth_as_mb, link_speed, network_type, ns1, ns2,
			ssh_port, active, show_public, was_promo, transferrable, owned_since)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.Hostname, s.ServerType, s.OsID, s.ProviderID, s.LocationID,
		s.RamAsMB, s.CPU, s.CPUModel, s.BandwidthAsMB, s.LinkSpeed, s.NetworkType,
		s.Ns1, s.Ns2, s.SSHPort, boolToInt(s.Active), boolToInt(s.ShowPublic),
		boolToInt(s.WasPromo), boolToInt(s.Transferrable), s.OwnedSince)
	if err != nil {
		return 0, fmt.Errorf("insert server: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if err := insertDisksTx(ctx, tx, id, disks); err != nil {
		return 0, err
	}
	if err := upsertPricingTx(ctx, tx, ServiceServer, id, pricing); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Get returns one server with its disks and pricing.
func (st *ServerStore) Get(ctx context.Context, id int64) (*Server, []ServerDisk, *Pricing, error) {
	s, err := scanServer(QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT "+serverColumns+" FROM servers WHERE id = ?", id))
	if err != nil {
		return nil, nil, nil, err
	}
	disks, err := st.Disks(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	pricing, err := (&PricingStore{DB: st.DB}).Get(ctx, ServiceServer, id)
	if err != nil {
		return nil, nil, nil, err
	}
	return s, disks, pricing, nil
}

// Disks returns the disks of one server.
func (st *ServerStore) Disks(ctx context.Context, serverID int64) ([]ServerDisk, error) {
	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx,
		"SELECT id, server_id, size_as_mb, media FROM server_disks WHERE server_id = ? ORDER BY id", serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ServerDisk
	for rows.Next() {
		var d ServerDisk
		if err := rows.Scan(&d.ID, &d.ServerID, &d.SizeAsMB, &d.Media); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Update replaces a server's fields, disks, and pricing in one transaction.
func (st *ServerStore) Update(ctx context.Context, s *Server, disks []ServerDisk, pricing *Pricing) error {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE servers SET hostname = ?, server_type = ?, os_id = ?, provider_id = ?,
			location_id = ?, ram_as_mb = ?, cpu = ?, cpu_model = ?, bandwidth_as_mb = ?,
			link_speed = ?, network_type = ?, ns1 = ?, ns2 = ?, ssh_port = ?,
			active = ?, show_public = ?, was_promo = ?, transferrable = ?,
			owned_since = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		s.Hostname, s.ServerType, s.OsID, s.ProviderID, s.LocationID,
		s.RamAsMB, s.CPU, s.CPUModel, s.BandwidthAsMB, s.LinkSpeed, s.NetworkType,
		s.Ns1, s.Ns2, s.SSHPort, boolToInt(s.Active), boolToInt(s.ShowPublic),
		boolToInt(s.WasPromo), boolToInt(s.Transferrable), s.OwnedSince, s.ID)
	if err != nil {
		return fmt.Errorf("update server: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}

	if _, err := tx.ExecContext(ctx, "DELETE FROM server_disks WHERE server_id = ?", s.ID); err != nil {
		return err
	}
	if err := insertDisksTx(ctx, tx, s.ID, disks); err != nil {
		return err
	}
	if err := upsertPricingTx(ctx, tx, ServiceServer, s.ID, pricing); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete removes a server plus its polymorphic children (pricings, ips,
// notes, labels); disks and yabs cascade via FK.
func (st *ServerStore) Delete(ctx context.Context, id int64) error {
	return deleteServiceTx(ctx, st.DB, "servers", ServiceServer, id)
}

func insertDisksTx(ctx context.Context, tx *sql.Tx, serverID int64, disks []ServerDisk) error {
	for _, d := range disks {
		if d.SizeAsMB <= 0 {
			continue
		}
		media := d.Media
		if media == "" {
			media = "SSD"
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO server_disks (server_id, size_as_mb, media) VALUES (?, ?, ?)",
			serverID, d.SizeAsMB, media); err != nil {
			return fmt.Errorf("insert disk: %w", err)
		}
	}
	return nil
}

// ListOptions controls filtering and sorting of the server list.
type ListOptions struct {
	Status string // "active" (default), "inactive", "all"
	Q      string // substring match on hostname/cpu_model/provider/os
	Sort   string // whitelisted key; default "hostname"
	Dir    string // "asc" (default) or "desc"
}

// likeEscaper escapes %, _, and the escape char itself so a literal "%" or
// "_" in the search box can't act as a wildcard.
var likeEscaper = strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)

// likePattern wraps a user search term for a LIKE ... ESCAPE '\' clause.
func likePattern(q string) string {
	return "%" + likeEscaper.Replace(q) + "%"
}

// ServerListItem is one row of the server list view.
type ServerListItem struct {
	Server
	OSName       string
	ProviderName string
	LocationName string
	DiskMB       int64
	DiskMedia    string // comma-joined distinct media types
	Pricing      *Pricing
}

// sortColumns whitelists sortable list columns to SQL expressions.
var sortColumns = map[string]string{"hostname": "s.hostname COLLATE NOCASE",
	"type":     "s.server_type",
	"os":       "os_name COLLATE NOCASE",
	"cpu":      "s.cpu",
	"ram":      "s.ram_as_mb",
	"disk":     "disk_mb",
	"bw":       "s.bandwidth_as_mb IS NULL, s.bandwidth_as_mb",
	"net":      "s.network_type",
	"location": "loc_name COLLATE NOCASE",
	"provider": "prov_name COLLATE NOCASE",
	"price":    "pr.price",
	"due":      "pr.next_due_date",
}

// List returns filtered/sorted servers with joined catalog names, disk
// totals, and pricing.
func (st *ServerStore) List(ctx context.Context, opts ListOptions) ([]ServerListItem, error) {
	var where []string
	var args []any

	switch opts.Status {
	case "inactive":
		where = append(where, "s.active = 0")
	case "all":
		// no filter
	default:
		where = append(where, "s.active = 1")
	}
	if opts.Q != "" {
		like := likePattern(opts.Q)
		where = append(where, "(s.hostname LIKE ? ESCAPE '\\' OR s.cpu_model LIKE ? ESCAPE '\\' OR prov_name LIKE ? ESCAPE '\\' OR os_name LIKE ? ESCAPE '\\')")
		args = append(args, like, like, like, like)
	}

	orderBy := sortColumns["hostname"]
	if col, ok := sortColumns[opts.Sort]; ok {
		orderBy = col
	}
	if opts.Dir == "desc" {
		orderBy += " DESC"
	}

	query := `
		SELECT ` + listColumns + `,
			COALESCE(os_name, ''), COALESCE(prov_name, ''), COALESCE(loc_name, ''),
			COALESCE(disk_mb, 0), COALESCE(disk_media, ''),
			pr.currency, pr.price, pr.term, pr.next_due_date
		FROM (
			SELECT s.*, os.name AS os_name, p.name AS prov_name, l.name AS loc_name,
				(SELECT SUM(size_as_mb) FROM server_disks d WHERE d.server_id = s.id) AS disk_mb,
				(SELECT GROUP_CONCAT(DISTINCT media) FROM server_disks d WHERE d.server_id = s.id) AS disk_media
			FROM servers s
			LEFT JOIN os ON os.id = s.os_id
			LEFT JOIN providers p ON p.id = s.provider_id
			LEFT JOIN locations l ON l.id = s.location_id
		) s
		LEFT JOIN pricings pr ON pr.service_id = s.id AND pr.service_type = 1 AND pr.active = 1`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + orderBy

	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ServerListItem
	for rows.Next() {
		var it ServerListItem
		var active, showPublic, hasYabs, wasPromo, transferrable int
		var currency sql.NullString
		var price sql.NullFloat64
		var term sql.NullInt64
		var due sql.NullString
		err := rows.Scan(&it.ID, &it.Hostname, &it.ServerType, &it.OsID, &it.ProviderID,
			&it.LocationID, &it.RamAsMB, &it.CPU, &it.CPUModel, &it.BandwidthAsMB, &it.LinkSpeed,
			&it.NetworkType, &it.Ns1, &it.Ns2, &it.SSHPort, &active, &showPublic, &hasYabs,
			&wasPromo, &transferrable, &it.OwnedSince, &it.CreatedAt, &it.UpdatedAt,
			&it.OSName, &it.ProviderName, &it.LocationName, &it.DiskMB, &it.DiskMedia,
			&currency, &price, &term, &due)
		if err != nil {
			return nil, err
		}
		it.Active = active != 0
		it.ShowPublic = showPublic != 0
		it.HasYabs = hasYabs != 0
		it.WasPromo = wasPromo != 0
		it.Transferrable = transferrable != 0
		if currency.Valid {
			it.Pricing = &Pricing{
				ServiceID:   it.ID,
				ServiceType: ServiceServer,
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

// StatusCounts returns active and inactive server counts.
func (st *ServerStore) StatusCounts(ctx context.Context) (active, inactive int, err error) {
	err = QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT COALESCE(SUM(active = 1), 0), COALESCE(SUM(active = 0), 0) FROM servers").
		Scan(&active, &inactive)
	return active, inactive, err
}

// DistinctLocations returns the number of distinct locations used by servers.
func (st *ServerStore) DistinctLocations(ctx context.Context) (int, error) {
	var n int
	err := QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT location_id) FROM servers WHERE location_id IS NOT NULL").Scan(&n)
	return n, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
