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

package pricing

import (
	"context"
	"database/sql"
	"time"

	"idlerthing/internal/model"
)

// AdvanceDueDates lazily rolls past-due recurring pricings forward: any
// active pricing whose next_due_date is before today (and whose term is not
// one-time) is advanced by its term until it lands on or after today. Month
// arithmetic is clamped (Jan 31 + 1 month → Feb 28/29), matching PHP's
// addMonthsNoOverflow. Returns the number of rows updated.
func AdvanceDueDates(ctx context.Context, db *sql.DB) (int, error) {
	today := time.Now().Format(time.DateOnly)

	// Select candidates and update INSIDE one transaction, with a
	// compare-and-swap WHERE on the old date — a concurrent pricing edit
	// between select and update is never overwritten.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT id, term, next_due_date FROM pricings
		WHERE active = 1 AND next_due_date IS NOT NULL AND next_due_date < ?
		  AND term != ?`, today, model.TermOneTime)
	if err != nil {
		return 0, err
	}
	type dueRow struct {
		id   int64
		term int
		due  string
	}
	var pending []dueRow
	for rows.Next() {
		var dr dueRow
		if err := rows.Scan(&dr.id, &dr.term, &dr.due); err != nil {
			rows.Close()
			return 0, err
		}
		pending = append(pending, dr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}

	updated := 0
	// Compare against the SAME local "today" the SELECT used, not UTC
	// midnight — otherwise early-morning rows in UTC+N zones get selected
	// but never advanced.
	todayStart, _ := time.Parse(time.DateOnly, today)
	for _, dr := range pending {
		due, err := time.Parse(time.DateOnly, dr.due)
		if err != nil {
			continue // unparseable stored value — leave it alone
		}
		months := model.TermMonths(dr.term)
		if months == 0 {
			continue
		}
		for due.Before(todayStart) {
			due = AddMonthsClamped(due, months)
		}
		res, err := tx.ExecContext(ctx,
			"UPDATE pricings SET next_due_date = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ? AND next_due_date = ?",
			due.Format(time.DateOnly), dr.id, dr.due)
		if err != nil {
			return updated, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // changed concurrently — leave it alone
		}
		updated++
	}
	return updated, tx.Commit()
}

// AddMonthsClamped adds months to t, clamping the day of month when the
// target month is shorter (Jan 31 + 1 month → Feb 28/29).
func AddMonthsClamped(t time.Time, months int) time.Time {
	y, m, d := t.Date()
	// First of the target month, then find its length.
	first := time.Date(y, m+time.Month(months), 1, t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
	lastDay := first.AddDate(0, 1, -1).Day()
	if d > lastDay {
		d = lastDay
	}
	return time.Date(first.Year(), first.Month(), d,
		t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), t.Location())
}
