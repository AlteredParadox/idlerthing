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
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// Batch V1 — a month-end due date stays anchored to month end when the
// catch-up passes through a shorter month (Jan 31 → … → Apr 30, not Apr 28).
func TestCatchUpKeepsMonthEndAnchor(t *testing.T) {
	cases := []struct {
		due, today, want string
		months           int
	}{
		{"2026-01-31", "2026-04-05", "2026-04-30", 1}, // through Feb: 28 would be the drift
		{"2026-01-31", "2026-02-10", "2026-02-28", 1}, // lands in Feb itself: clamped
		{"2026-01-31", "2026-03-01", "2026-03-31", 1}, // Feb 28 is before Mar 1 → Mar 31
		{"2024-01-31", "2024-03-15", "2024-03-31", 1}, // leap-year Feb 29 must not stick either
		{"2025-11-30", "2026-06-01", "2026-08-30", 3}, // quarterly: Feb 28 → May 30 → Aug 30
		{"2026-05-15", "2026-05-15", "2026-05-15", 1}, // already on/after today: untouched
		{"2026-01-31", "2026-01-31", "2026-01-31", 1},
	}
	for _, c := range cases {
		got := catchUp(day(c.due), c.months, day(c.today))
		if got.Format(time.DateOnly) != c.want {
			t.Errorf("catchUp(%s, %d, today %s) = %s, want %s",
				c.due, c.months, c.today, got.Format(time.DateOnly), c.want)
		}
	}
}
