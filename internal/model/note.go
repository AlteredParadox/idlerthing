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
)

// Note mirrors the notes table. A note attaches to a service OR an IP:
// service_id+service_type for service notes (written by the notes UI),
// ip_id for IP notes (schema feature — no write path in the app yet;
// reserved for a future IP notes UI; FK cascade on ips keeps it clean).
type Note struct {
	ID          int64
	ServiceID   sql.NullInt64
	ServiceType sql.NullInt64
	IPID        sql.NullInt64
	Body        string
	CreatedAt   string
	UpdatedAt   string
}

// NoteWithTarget is a note plus its service's display name (index page).
type NoteWithTarget struct {
	Note
	Target string
}

// NoteStore wraps the DB for note queries.
type NoteStore struct {
	DB *sql.DB
}

// Create inserts a SERVICE note — atomically, only when the target
// service exists (a concurrent delete can't leave an orphan). A note
// targets exactly one thing: (service_id AND service_type) XOR ip_id —
// SQLite can't enforce that as a constraint, so the write paths do (the
// importer included; migration 0012 cleans history). IP notes have no app
// write path; they arrive via import only.
func (s *NoteStore) Create(ctx context.Context, n *Note) (int64, error) {
	if n.IPID.Valid || !n.ServiceID.Valid || !n.ServiceType.Valid {
		return 0, sql.ErrNoRows
	}
	serviceType := int(n.ServiceType.Int64)
	table, ok := ServiceTable[serviceType]
	if !ok {
		return 0, sql.ErrNoRows
	}
	res, err := s.DB.ExecContext(ctx,
		`INSERT INTO notes (service_id, service_type, body)
		 SELECT ?, ?, ? WHERE EXISTS (SELECT 1 FROM `+table+` WHERE id = ?)`,
		n.ServiceID, n.ServiceType, n.Body, n.ServiceID.Int64)
	if err != nil {
		return 0, err
	}
	if r, _ := res.RowsAffected(); r == 0 {
		return 0, sql.ErrNoRows
	}
	return res.LastInsertId()
}

// ListFor returns notes attached to one service, newest first.
func (s *NoteStore) ListFor(ctx context.Context, serviceID int64, serviceType int) ([]Note, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx, `
		SELECT id, service_id, service_type, ip_id, body, created_at, updated_at
		FROM notes WHERE service_id = ? AND service_type = ?
		ORDER BY created_at DESC, id DESC`, serviceID, serviceType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNotes(rows)
}

// ListAll returns every service note with its target's display name.
func (s *NoteStore) ListAll(ctx context.Context) ([]NoteWithTarget, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx, `
		SELECT a.id, a.service_id, a.service_type, a.ip_id, a.body, a.created_at, a.updated_at,
			`+TargetNameSQL+` AS target
		FROM notes a
		WHERE a.service_id IS NOT NULL
		ORDER BY a.created_at DESC, a.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NoteWithTarget
	for rows.Next() {
		var n NoteWithTarget
		if err := rows.Scan(&n.ID, &n.ServiceID, &n.ServiceType, &n.IPID, &n.Body,
			&n.CreatedAt, &n.UpdatedAt, &n.Target); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ListIPNotes returns notes attached to IPs (ip_id IS NOT NULL).
func (s *NoteStore) ListIPNotes(ctx context.Context) ([]Note, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx, `
		SELECT id, service_id, service_type, ip_id, body, created_at, updated_at
		FROM notes WHERE ip_id IS NOT NULL ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanNotes(rows)
}

// Delete removes a note.
func (s *NoteStore) Delete(ctx context.Context, id int64) error {
	res, err := s.DB.ExecContext(ctx, "DELETE FROM notes WHERE id = ?", id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func scanNotes(rows *sql.Rows) ([]Note, error) {
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.ServiceID, &n.ServiceType, &n.IPID, &n.Body,
			&n.CreatedAt, &n.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
