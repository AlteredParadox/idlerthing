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

// A positive initial price takes its currency from the server's pricing, so
// an import that leaves a server without usable pricing drops the amount
// (with a warning) instead of letting it display as an invented USD price.
// Free needs no currency and survives; top-level pricings count.
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
		{"server":{"id":5,"hostname":"top-level","server_type":1,"active":true,"initial_price":20}}
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
	}
	for host, w := range want {
		var got sql.NullFloat64
		var term sql.NullInt64
		if err := database.QueryRow("SELECT initial_price, initial_term FROM servers WHERE hostname = ?", host).
			Scan(&got, &term); err != nil {
			t.Fatalf("%s: %v", host, err)
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
