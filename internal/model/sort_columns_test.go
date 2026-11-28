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
	"reflect"
	"testing"
)

// NULLS LAST survives a descending sort, so blanks stay last both ways.
func TestOrderClauseNullsLast(t *testing.T) {
	got := orderClause("NULLIF(s.owned_since, '') NULLS LAST, s.hostname", "desc")
	if want := "NULLIF(s.owned_since, '') DESC NULLS LAST, s.hostname DESC"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// Every newly sortable column runs, and blank Since / Link Speed / CPU
// Model values sort last in both directions.
func TestServerListNewSortColumns(t *testing.T) {
	ctx := context.Background()
	st := &ServerStore{DB: testDB(t)}
	mk := func(host, since, cpu string, link int64, promo bool) {
		s := &Server{Hostname: host, ServerType: TypeKVM, Active: true, WasPromo: promo,
			CPUModel: sql.NullString{String: cpu, Valid: cpu != ""}}
		if since != "" {
			s.OwnedSince = sql.NullString{String: since, Valid: true}
		}
		if link > 0 {
			s.LinkSpeed = sql.NullInt64{Int64: link, Valid: true}
		}
		if _, err := st.Create(ctx, s, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	mk("a-blank", "", "", 0, false)
	mk("b-old", "2020-01-01", "AMD EPYC", 1000, true)
	mk("c-new", "2024-06-01", "Intel Xeon", 10000, false)

	order := func(key, dir string) []string {
		items, err := st.List(ctx, ListOptions{Status: "all", Sort: key, Dir: dir})
		if err != nil {
			t.Fatalf("%s %s: %v", key, dir, err)
		}
		var out []string
		for _, it := range items {
			out = append(out, it.Hostname)
		}
		return out
	}
	for _, key := range []string{"since", "link_speed", "cpu_model"} {
		if got := order(key, "asc"); !reflect.DeepEqual(got, []string{"b-old", "c-new", "a-blank"}) {
			t.Fatalf("%s asc: %v", key, got)
		}
		if got := order(key, "desc"); !reflect.DeepEqual(got, []string{"c-new", "b-old", "a-blank"}) {
			t.Fatalf("%s desc: %v", key, got)
		}
	}
	if got := order("promo", "desc"); got[0] != "b-old" {
		t.Fatalf("promo desc: promo server should lead: %v", got)
	}
	for _, key := range []string{"net", "active", "public", "transferable"} {
		if _, err := st.List(ctx, ListOptions{Status: "all", Sort: key, Dir: "desc"}); err != nil {
			t.Fatalf("%s: %v", key, err)
		}
	}
}
