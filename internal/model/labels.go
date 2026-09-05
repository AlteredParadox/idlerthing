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
)

// MaxLabelsPerService caps label assignments per service.
const MaxLabelsPerService = 4

// ErrTooManyLabels is returned when a service already has the max labels.
var ErrTooManyLabels = errors.New("maximum labels per service reached")

// LabelStore wraps the DB for label assignment queries. The labels catalog
// itself is managed by CatalogStore.
type LabelStore struct {
	DB *sql.DB
}

// LabelCount pairs a label with its usage count.
type LabelCount struct {
	CatalogItem
	Used int
}

// Assign attaches a label to a service, enforcing the max-per-service cap
// atomically (single INSERT...SELECT, no COUNT-then-INSERT race).
// Re-assigning an existing pair is a no-op.
func (s *LabelStore) Assign(ctx context.Context, labelID, serviceID int64, serviceType int) error {
	var already int
	if err := QuerierFrom(ctx, s.DB).QueryRowContext(ctx,
		"SELECT COUNT(*) FROM labels_assigned WHERE label_id = ? AND service_id = ? AND service_type = ?",
		labelID, serviceID, serviceType).Scan(&already); err != nil {
		return err
	}
	if already > 0 {
		return nil
	}
	table, ok := ServiceTable[serviceType]
	if !ok {
		return sql.ErrNoRows
	}
	res, err := s.DB.ExecContext(ctx, `
		INSERT INTO labels_assigned (label_id, service_id, service_type)
		SELECT ?, ?, ?
		WHERE (SELECT COUNT(*) FROM labels_assigned
			WHERE service_id = ? AND service_type = ?) < ?
		  AND EXISTS (SELECT 1 FROM `+table+` WHERE id = ?)`,
		labelID, serviceID, serviceType, serviceID, serviceType, MaxLabelsPerService,
		serviceID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Distinguish "target gone" from "cap reached".
		var exists int
		QuerierFrom(ctx, s.DB).QueryRowContext(ctx,
			"SELECT COUNT(*) FROM "+table+" WHERE id = ?", serviceID).Scan(&exists)
		if exists == 0 {
			return sql.ErrNoRows
		}
		return ErrTooManyLabels
	}
	return nil
}

// Unassign detaches a label from a service.
func (s *LabelStore) Unassign(ctx context.Context, labelID, serviceID int64, serviceType int) error {
	_, err := s.DB.ExecContext(ctx,
		"DELETE FROM labels_assigned WHERE label_id = ? AND service_id = ? AND service_type = ?",
		labelID, serviceID, serviceType)
	return err
}

// ListFor returns the labels assigned to one service.
func (s *LabelStore) ListFor(ctx context.Context, serviceID int64, serviceType int) ([]CatalogItem, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx, `
		SELECT l.id, l.label FROM labels l
		JOIN labels_assigned a ON a.label_id = l.id
		WHERE a.service_id = ? AND a.service_type = ?
		ORDER BY l.label COLLATE NOCASE`, serviceID, serviceType)
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

// AllWithCounts returns every label with its assignment count.
func (s *LabelStore) AllWithCounts(ctx context.Context) ([]LabelCount, error) {
	rows, err := QuerierFrom(ctx, s.DB).QueryContext(ctx, `
		SELECT l.id, l.label,
			(SELECT COUNT(*) FROM labels_assigned a WHERE a.label_id = l.id) AS used
		FROM labels l ORDER BY l.label COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LabelCount
	for rows.Next() {
		var lc LabelCount
		if err := rows.Scan(&lc.ID, &lc.Name, &lc.Used); err != nil {
			return nil, err
		}
		out = append(out, lc)
	}
	return out, rows.Err()
}

// FindOrCreate returns the ID of a label by name (case-insensitively),
// creating it if needed. A UNIQUE-conflict on insert — another creator won
// the race between lookup and insert — re-selects and returns the existing
// row instead of erroring.
func (s *LabelStore) FindOrCreate(ctx context.Context, name string) (int64, error) {
	var id int64
	err := QuerierFrom(ctx, s.DB).QueryRowContext(ctx,
		"SELECT id FROM labels WHERE label = ? COLLATE NOCASE", name).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	res, err := s.DB.ExecContext(ctx, "INSERT INTO labels (label) VALUES (?)", name)
	if err != nil {
		if IsUniqueViolation(err) &&
			QuerierFrom(ctx, s.DB).QueryRowContext(ctx,
				"SELECT id FROM labels WHERE label = ? COLLATE NOCASE", name).Scan(&id) == nil {
			return id, nil
		}
		return 0, fmt.Errorf("create label: %w", err)
	}
	return res.LastInsertId()
}
