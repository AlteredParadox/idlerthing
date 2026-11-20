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
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
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

// YABSDiskSpeed mirrors yabs_disk_speed. A speed of 0 means "not measured":
// it is stored as NULL (see InsertYABSSpeeds) so SQL aggregates skip it.
type YABSDiskSpeed struct {
	ID        int64
	YabsID    int64
	BlockSize string
	ReadMbps  float64
	WriteMbps float64
}

// YABSNetworkSpeed mirrors yabs_network_speed. A speed or latency of 0 means
// "not measured" — yabs.sh reports "busy" when an iperf endpoint refuses the
// run — and is stored as NULL (see InsertYABSSpeeds) so SQL aggregates skip it.
type YABSNetworkSpeed struct {
	ID       int64
	YabsID   int64
	Location string
	Provider string
	// Mode is the iperf address family ("IPv4"/"IPv6"), empty for rows
	// ingested before the split existed.
	Mode      string
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

// YABSSigWindow is how long an ingest signature stays valid and how long a
// consumed capability is retained. Retention starts when the capability is
// consumed, so requests already validated near expiry cannot outlive their
// replay marker.
const YABSSigWindow = 2 * time.Hour

// PruneCaps deletes capabilities consumed more than a signature window ago.
// Called periodically (the login sweep), NOT on the ingest hot path — the
// DELETE is an unindexed scan and the table stays tiny between logins.
func (st *YABSStore) PruneCaps(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-YABSSigWindow).Format(time.RFC3339)
	st.DB.ExecContext(ctx, "DELETE FROM yabs_caps WHERE datetime(consumed_at) < datetime(?)", cutoff)
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

// YABSDiskSpeedSelect and YABSNetworkSpeedSelect are the SELECT ... FROM
// clauses every reader of the speed tables uses (append a WHERE / ORDER BY).
// Unmeasured values are NULL in the database and 0 in the structs, so a
// reader that selected the bare columns would fail to scan the first "busy"
// row it met.
const (
	YABSDiskSpeedSelect = `SELECT id, yabs_id, block_size,
	COALESCE(read_mbps, 0), COALESCE(write_mbps, 0) FROM yabs_disk_speed`
	YABSNetworkSpeedSelect = `SELECT id, yabs_id, location, provider, COALESCE(mode, ''),
	COALESCE(send_mbps, 0), COALESCE(recv_mbps, 0), COALESCE(latency_ms, 0) FROM yabs_network_speed`
)

// ScanYABSDiskSpeed scans one YABSDiskSpeedSelect row.
func ScanYABSDiskSpeed(row interface{ Scan(...any) error }) (YABSDiskSpeed, error) {
	var d YABSDiskSpeed
	err := row.Scan(&d.ID, &d.YabsID, &d.BlockSize, &d.ReadMbps, &d.WriteMbps)
	return d, err
}

// ScanYABSNetworkSpeed scans one YABSNetworkSpeedSelect row.
func ScanYABSNetworkSpeed(row interface{ Scan(...any) error }) (YABSNetworkSpeed, error) {
	var n YABSNetworkSpeed
	err := row.Scan(&n.ID, &n.YabsID, &n.Location, &n.Provider, &n.Mode,
		&n.SendMbps, &n.RecvMbps, &n.LatencyMs)
	return n, err
}

// InsertYABSSpeeds writes a run's speed rows. A value of 0 is stored as NULL:
// it means the test produced no result, and NULL keeps it out of AVG()/MIN()
// where a literal 0 would count as a measurement. Shared by ingest and backup
// restore so both write the same shape.
func InsertYABSSpeeds(ctx context.Context, q Querier, yabsID int64, disks []YABSDiskSpeed, network []YABSNetworkSpeed) error {
	for _, d := range disks {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO yabs_disk_speed (yabs_id, block_size, read_mbps, write_mbps)
			VALUES (?, ?, NULLIF(?, 0), NULLIF(?, 0))`,
			yabsID, d.BlockSize, d.ReadMbps, d.WriteMbps); err != nil {
			return fmt.Errorf("insert disk speed: %w", err)
		}
	}
	for _, n := range network {
		if _, err := q.ExecContext(ctx, `
			INSERT INTO yabs_network_speed (yabs_id, location, provider, mode, send_mbps, recv_mbps, latency_ms)
			VALUES (?, ?, ?, ?, NULLIF(?, 0), NULLIF(?, 0), NULLIF(?, 0))`,
			yabsID, n.Location, n.Provider, n.Mode, n.SendMbps, n.RecvMbps, n.LatencyMs); err != nil {
			return fmt.Errorf("insert network speed: %w", err)
		}
	}
	return nil
}

// RetainableYABSPayload reports whether a raw body can be kept byte for byte.
// The payload lives in a TEXT column and travels through the JSON backup as a
// string, and both are exact only for valid UTF-8: encoding/json accepts
// invalid UTF-8 inside a quoted string when parsing, but replaces it with
// U+FFFD when writing, so such a body would come back from a backup different
// from the bytes that were hashed. A body that fails this is still parsed and
// stored as a run; only its raw copy is not kept.
func RetainableYABSPayload(body []byte) bool {
	return len(body) > 0 && len(body) <= YABSPayloadMax && utf8.Valid(body)
}

// YABSPayloadMax is the largest raw payload kept for a run. It equals the
// ingest body limit, so nothing ingest accepts is ever too large to keep.
const YABSPayloadMax = 64 << 10

// Create inserts a run with its speed rows and no raw payload. See
// CreateWithPayload.
func (st *YABSStore) Create(ctx context.Context, y *YABS, disks []YABSDiskSpeed, network []YABSNetworkSpeed) (int64, error) {
	return st.CreateWithPayload(ctx, y, disks, network, "")
}

// CreateWithPayload inserts a run with its speed rows in one transaction,
// keeps the raw payload it was parsed from (when non-empty; callers pass only
// valid UTF-8 — see RetainableYABSPayload), and flips
// servers.has_yabs on. A payload-hash/gb_url unique-index violation maps to
// ErrDuplicatePayload. Capability consumption is the caller's job (see
// ConsumeCap) so a rollback here can never un-consume it.
func (st *YABSStore) CreateWithPayload(ctx context.Context, y *YABS, disks []YABSDiskSpeed, network []YABSNetworkSpeed, payload string) (int64, error) {
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
		if IsUniqueViolation(err) {
			return 0, ErrDuplicatePayload
		}
		return 0, fmt.Errorf("insert yabs: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}

	if err := InsertYABSSpeeds(ctx, tx, id, disks, network); err != nil {
		return 0, err
	}
	if payload != "" {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO yabs_payload (yabs_id, body) VALUES (?, ?)", id, payload); err != nil {
			return 0, fmt.Errorf("insert yabs payload: %w", err)
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
		YABSDiskSpeedSelect+" WHERE yabs_id = ? ORDER BY id", yabsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []YABSDiskSpeed
	for rows.Next() {
		d, err := ScanYABSDiskSpeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func (st *YABSStore) networkSpeeds(ctx context.Context, yabsID int64) ([]YABSNetworkSpeed, error) {
	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx,
		YABSNetworkSpeedSelect+" WHERE yabs_id = ? ORDER BY id", yabsID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []YABSNetworkSpeed
	for rows.Next() {
		n, err := ScanYABSNetworkSpeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Payload returns the raw yabs.sh JSON kept for a run, scoped to its server.
// sql.ErrNoRows means the run does not exist under that server or has no
// payload (it was stored before payloads were kept).
func (st *YABSStore) Payload(ctx context.Context, serverID, id int64) (string, error) {
	var body string
	err := QuerierFrom(ctx, st.DB).QueryRowContext(ctx, `
		SELECT p.body FROM yabs_payload p JOIN yabs y ON y.id = p.yabs_id
		WHERE y.id = ? AND y.server_id = ?`, id, serverID).Scan(&body)
	return body, err
}

// HasPayload reports whether a raw payload is kept for the run.
func (st *YABSStore) HasPayload(ctx context.Context, id int64) (bool, error) {
	var n int
	err := QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT COUNT(*) FROM yabs_payload WHERE yabs_id = ?", id).Scan(&n)
	return n > 0, err
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
