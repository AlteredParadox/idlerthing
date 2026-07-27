package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// YABS mirrors the yabs table.
type YABS struct {
	ID               int64
	ServerID         int64
	RunAt            sql.NullString
	CPU              sql.NullString
	CPUCores         sql.NullInt64
	RAM              sql.NullString
	Swap             sql.NullString
	Distro           sql.NullString
	Kernel           sql.NullString
	Uptime           sql.NullString
	GeekbenchVersion sql.NullInt64
	GbSingle         sql.NullInt64
	GbMulti          sql.NullInt64
	GbURL            sql.NullString
	PayloadHash      sql.NullString
	CreatedAt        string
	UpdatedAt        string
}

// YABSDiskSpeed mirrors yabs_disk_speed.
type YABSDiskSpeed struct {
	ID        int64
	YabsID    int64
	BlockSize string
	ReadMbps  float64
	WriteMbps float64
}

// YABSNetworkSpeed mirrors yabs_network_speed.
type YABSNetworkSpeed struct {
	ID        int64
	YabsID    int64
	Location  string
	Provider  string
	SendMbps  float64
	RecvMbps  float64
	LatencyMs float64
}

// YABSListItem is one row of a yabs list, with the server's hostname.
type YABSListItem struct {
	YABS
	ServerHostname string
}

// YABSStore wraps the DB for yabs queries.
type YABSStore struct {
	DB *sql.DB
}

const yabsColumns = `id, server_id, run_at, cpu, cpu_cores, ram, swap, distro,
	kernel, uptime, geekbench_version, gb_single, gb_multi, gb_url, payload_hash,
	created_at, updated_at`

func scanYABS(row interface{ Scan(...any) error }) (*YABS, error) {
	var y YABS
	err := row.Scan(&y.ID, &y.ServerID, &y.RunAt, &y.CPU, &y.CPUCores, &y.RAM,
		&y.Swap, &y.Distro, &y.Kernel, &y.Uptime, &y.GeekbenchVersion,
		&y.GbSingle, &y.GbMulti, &y.GbURL, &y.PayloadHash, &y.CreatedAt, &y.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &y, nil
}

// ErrDuplicatePayload means an identical payload already exists (race-safe).
var ErrDuplicatePayload = errors.New("duplicate payload")

// YABSSigWindow is how long an ingest signature stays valid — the single
// source for the web package's signature check AND for cap pruning (older
// cap rows can never affect an ingest decision again).
const YABSSigWindow = 2 * time.Hour

// PruneCaps deletes consumed capabilities past the signature window. Called
// periodically (the login sweep), NOT on the ingest hot path — the DELETE
// is an unindexed scan and the table stays tiny between logins.
func (st *YABSStore) PruneCaps(ctx context.Context) {
	st.DB.ExecContext(ctx, "DELETE FROM yabs_caps WHERE ts < ?", time.Now().Add(-YABSSigWindow).Unix())
}

// ConsumeCap atomically consumes the (server_id, ts) ingest capability in
// its OWN transaction (never rolled back by a later failure). Returns false
// when the capability was already consumed. Consuming BEFORE payload parsing
// means a stolen URL dies here regardless of what body it carries.
func (st *YABSStore) ConsumeCap(ctx context.Context, serverID, ts int64) (bool, error) {
	res, err := st.DB.ExecContext(ctx,
		"INSERT OR IGNORE INTO yabs_caps (server_id, ts, consumed_at) VALUES (?, ?, ?)",
		serverID, ts, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Create inserts a run with its speed rows in one transaction, and flips
// servers.has_yabs on. A payload-hash/gb_url unique-index violation maps to
// ErrDuplicatePayload. Capability consumption is the caller's job (see
// ConsumeCap) so a rollback here can never un-consume it.
func (st *YABSStore) Create(ctx context.Context, y *YABS, disks []YABSDiskSpeed, network []YABSNetworkSpeed) (int64, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO yabs (server_id, run_at, cpu, cpu_cores, ram, swap, distro,
			kernel, uptime, geekbench_version, gb_single, gb_multi, gb_url, payload_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		y.ServerID, y.RunAt, y.CPU, y.CPUCores, y.RAM, y.Swap, y.Distro,
		y.Kernel, y.Uptime, y.GeekbenchVersion, y.GbSingle, y.GbMulti, y.GbURL, y.PayloadHash)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, ErrDuplicatePayload
		}
		return 0, fmt.Errorf("insert yabs: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	for _, d := range disks {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO yabs_disk_speed (yabs_id, block_size, read_mbps, write_mbps) VALUES (?, ?, ?, ?)",
			id, d.BlockSize, d.ReadMbps, d.WriteMbps); err != nil {
			return 0, fmt.Errorf("insert disk speed: %w", err)
		}
	}
	for _, n := range network {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO yabs_network_speed (yabs_id, location, provider, send_mbps, recv_mbps, latency_ms) VALUES (?, ?, ?, ?, ?, ?)",
			id, n.Location, n.Provider, n.SendMbps, n.RecvMbps, n.LatencyMs); err != nil {
			return 0, fmt.Errorf("insert network speed: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE servers SET has_yabs = 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?", y.ServerID); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// IsDuplicate reports whether an identical submission already exists
// (same server + geekbench URL, or same payload hash).
func (st *YABSStore) IsDuplicate(ctx context.Context, serverID int64, gbURL, payloadHash string) (bool, error) {
	if gbURL != "" {
		var n int
		if err := QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
			"SELECT COUNT(*) FROM yabs WHERE server_id = ? AND gb_url = ?", serverID, gbURL).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	if payloadHash != "" {
		var n int
		if err := QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
			"SELECT COUNT(*) FROM yabs WHERE server_id = ? AND payload_hash = ?", serverID, payloadHash).Scan(&n); err != nil {
			return false, err
		}
		if n > 0 {
			return true, nil
		}
	}
	return false, nil
}

// Get returns one run with its speed rows.
func (st *YABSStore) Get(ctx context.Context, id int64) (*YABS, []YABSDiskSpeed, []YABSNetworkSpeed, error) {
	y, err := scanYABS(QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT "+yabsColumns+" FROM yabs WHERE id = ?", id))
	if err != nil {
		return nil, nil, nil, err
	}
	disks, err := st.diskSpeeds(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	network, err := st.networkSpeeds(ctx, id)
	if err != nil {
		return nil, nil, nil, err
	}
	return y, disks, network, nil
}

func (st *YABSStore) diskSpeeds(ctx context.Context, yabsID int64) ([]YABSDiskSpeed, error) {
	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx,
		"SELECT id, yabs_id, block_size, read_mbps, write_mbps FROM yabs_disk_speed WHERE yabs_id = ? ORDER BY id", yabsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []YABSDiskSpeed
	for rows.Next() {
		var d YABSDiskSpeed
		if err := rows.Scan(&d.ID, &d.YabsID, &d.BlockSize, &d.ReadMbps, &d.WriteMbps); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (st *YABSStore) networkSpeeds(ctx context.Context, yabsID int64) ([]YABSNetworkSpeed, error) {
	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx,
		"SELECT id, yabs_id, location, provider, send_mbps, recv_mbps, latency_ms FROM yabs_network_speed WHERE yabs_id = ? ORDER BY id", yabsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []YABSNetworkSpeed
	for rows.Next() {
		var n YABSNetworkSpeed
		if err := rows.Scan(&n.ID, &n.YabsID, &n.Location, &n.Provider, &n.SendMbps, &n.RecvMbps, &n.LatencyMs); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListFor returns runs for one server, newest first.
func (st *YABSStore) ListFor(ctx context.Context, serverID int64) ([]YABSListItem, error) {
	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx, `
		SELECT `+prefixedColumns("y", yabsColumns)+`, COALESCE(s.hostname, '')
		FROM yabs y JOIN servers s ON s.id = y.server_id
		WHERE y.server_id = ? ORDER BY y.id DESC`, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanYABSList(rows)
}

// ListAll returns all runs across servers, newest first.
func (st *YABSStore) ListAll(ctx context.Context) ([]YABSListItem, error) {
	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx, `
		SELECT `+prefixedColumns("y", yabsColumns)+`, COALESCE(s.hostname, '')
		FROM yabs y JOIN servers s ON s.id = y.server_id
		ORDER BY y.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanYABSList(rows)
}

func scanYABSList(rows *sql.Rows) ([]YABSListItem, error) {
	var out []YABSListItem
	for rows.Next() {
		var it YABSListItem
		err := rows.Scan(&it.ID, &it.ServerID, &it.RunAt, &it.CPU, &it.CPUCores,
			&it.RAM, &it.Swap, &it.Distro, &it.Kernel, &it.Uptime,
			&it.GeekbenchVersion, &it.GbSingle, &it.GbMulti, &it.GbURL,
			&it.PayloadHash, &it.CreatedAt, &it.UpdatedAt, &it.ServerHostname)
		if err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// Delete removes a run scoped to its server (speed rows cascade via FK)
// and recomputes servers.has_yabs in the same transaction.
func (st *YABSStore) Delete(ctx context.Context, serverID, id int64) error {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, "DELETE FROM yabs WHERE id = ? AND server_id = ?", id, serverID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE servers SET has_yabs = (SELECT COUNT(*) > 0 FROM yabs WHERE server_id = ?)
		WHERE id = ?`, serverID, serverID); err != nil {
		return err
	}
	return tx.Commit()
}
