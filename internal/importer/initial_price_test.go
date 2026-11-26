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

package importer

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// Blank parts of an initial price are copied from the active pricing
// (inline or top-level), as saving does. A positive amount left with no
// currency is dropped with a warning instead of showing as invented USD.
// Free needs no currency and survives.
func TestImportClearsOrphanInitialPrice(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,
	"servers":[
		{"server":{"id":1,"hostname":"no-pricing","server_type":1,"active":true,"initial_price":20,"initial_term":4}},
		{"server":{"id":2,"hostname":"bad-currency","server_type":1,"active":true,"initial_price":20},
		 "pricing":{"currency":"US-D","price":5,"term":1}},
		{"server":{"hostname":"no-id","server_type":1,"active":true,"initial_price":20}},
		{"server":{"id":3,"hostname":"free","server_type":1,"active":true,"initial_price":0}},
		{"server":{"id":4,"hostname":"inline","server_type":1,"active":true,"initial_price":20},
		 "pricing":{"currency":"EUR","price":5,"term":1}},
		{"server":{"id":5,"hostname":"top-level","server_type":1,"active":true,"initial_price":20}},
		{"server":{"id":6,"hostname":"own-currency","server_type":1,"active":true,"initial_price":20,"initial_currency":"CAD"}},
		{"server":{"id":7,"hostname":"old-backup","server_type":1,"active":true},
		 "pricing":{"currency":"AUD","price":9,"term":2}}
	],
	"pricings":[{"service_id":5,"service_type":1,"currency":"GBP","price":5,"term":1,"active":true}]}`
	sum, err := Import(context.Background(), database, strings.NewReader(doc), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	want := map[string]sql.NullFloat64{
		"no-pricing":   {},
		"bad-currency": {},
		"no-id":        {},
		"free":         {Float64: 0, Valid: true},
		"inline":       {Float64: 20, Valid: true},
		"top-level":    {Float64: 20, Valid: true},
		"own-currency": {Float64: 20, Valid: true},
		"old-backup":   {Float64: 9, Valid: true},
	}
	wantCur := map[string]string{"inline": "EUR", "top-level": "GBP", "own-currency": "CAD", "old-backup": "AUD"}
	for host, w := range want {
		var got sql.NullFloat64
		var term sql.NullInt64
		var cur sql.NullString
		if err := database.QueryRow("SELECT initial_price, initial_term, initial_currency FROM servers WHERE hostname = ?", host).
			Scan(&got, &term, &cur); err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if cur.String != wantCur[host] {
			t.Errorf("%s: initial_currency = %q, want %q", host, cur.String, wantCur[host])
		}
		if got != w {
			t.Errorf("%s: initial_price = %+v, want %+v", host, got, w)
		}
		if !w.Valid && term.Valid {
			t.Errorf("%s: dropped initial price must drop its term too", host)
		}
	}
	dropped := 0
	for _, w := range sum.Warnings {
		if strings.Contains(w, "initial price dropped") {
			dropped++
		}
	}
	if dropped != 3 {
		t.Errorf("want 3 drop warnings, got %d: %v", dropped, sum.Warnings)
	}
}
