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
