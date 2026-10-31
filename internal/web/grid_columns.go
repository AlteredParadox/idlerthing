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

package web

// Initial sizes are keyed independently of column availability and visibility.
var serverColumnWidths = map[string]int{
	"hostname":     240,
	"type":         88,
	"os":           240,
	"cpu":          210,
	"cpu_model":    260,
	"ram":          130,
	"disk":         170,
	"bw":           130,
	"link_speed":   130,
	"net":          140,
	"location":     260,
	"provider":     190,
	"price":        130,
	"price_yr":     160,
	"due":          130,
	"since":        130,
	"uptime":       100,
	"active":       88,
	"promo":        88,
	"public":       88,
	"transferable": 140,
	"labels":       190,
	"notes":        320,
}

// Width supplies a stable initial size, including for optional columns.
func (c colDef) Width() int {
	if width, ok := serverColumnWidths[c.Key]; ok {
		return width
	}
	return 160
}

// GridColumns keeps widths tied to column keys when columns are hidden.
func (v serversListView) GridColumns() []colDef {
	var columns []colDef
	for _, column := range serverColumns {
		if v.Col(column.Key) {
			columns = append(columns, column)
		}
	}
	return columns
}
