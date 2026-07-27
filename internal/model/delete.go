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

// deleteServiceTx deletes one service row plus its polymorphic children
// (pricings, ips, notes, labels_assigned) in ONE transaction. Rows linked
// by real FKs (server_disks, yabs, ip-keyed notes) cascade on their own.
func deleteServiceTx(ctx context.Context, db *sql.DB, table string, serviceType int, id int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Table name is a compile-time constant from the store.
	res, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("delete %s: %w", table, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}

	for _, child := range []string{"pricings", "ips", "notes", "labels_assigned"} {
		if _, err := tx.ExecContext(ctx,
			"DELETE FROM "+child+" WHERE service_id = ? AND service_type = ?",
			id, serviceType); err != nil {
			return fmt.Errorf("delete %s children: %w", child, err)
		}
	}
	return tx.Commit()
}
