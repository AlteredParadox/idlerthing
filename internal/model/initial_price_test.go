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
	"strings"
	"testing"
)

func nf(v float64) sql.NullFloat64 { return sql.NullFloat64{Float64: v, Valid: true} }
func ni(v int64) sql.NullInt64     { return sql.NullInt64{Int64: v, Valid: true} }
func ns(v string) sql.NullString   { return sql.NullString{String: v, Valid: true} }

func TestNormalizeInitial(t *testing.T) {
	eur := &Pricing{Currency: "EUR", Price: 9, Term: TermQuarterly}
	cases := []struct {
		name    string
		in      Server
		p       *Pricing
		want    Server
		wantErr bool
	}{
		{"free drops term and currency", Server{InitialPrice: nf(0), InitialTerm: ni(4), InitialCurrency: ns("USD")}, eur,
			Server{InitialPrice: nf(0)}, false},
		{"blank copies the whole price", Server{InitialTerm: ni(4), InitialCurrency: ns("USD")}, eur,
			Server{InitialPrice: nf(9), InitialTerm: ni(TermQuarterly), InitialCurrency: ns("EUR")}, false},
		{"blank parts filled", Server{InitialPrice: nf(20)}, eur,
			Server{InitialPrice: nf(20), InitialTerm: ni(TermQuarterly), InitialCurrency: ns("EUR")}, false},
		{"own parts kept", Server{InitialPrice: nf(20), InitialTerm: ni(4), InitialCurrency: ns("CHF")}, eur,
			Server{InitialPrice: nf(20), InitialTerm: ni(4), InitialCurrency: ns("CHF")}, false},
		{"never priced stays NULL", Server{}, nil, Server{}, false},
		{"own currency needs no pricing", Server{InitialPrice: nf(20), InitialCurrency: ns("GBP")}, nil,
			Server{InitialPrice: nf(20), InitialCurrency: ns("GBP")}, false},
		{"no currency anywhere", Server{InitialPrice: nf(20)}, nil, Server{InitialPrice: nf(20)}, true},
	}
	for _, c := range cases {
		s := c.in
		err := normalizeInitial(&s, c.p)
		if (err != nil) != c.wantErr || (err != nil && !errors.Is(err, ErrInitialNoCurrency)) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if s.InitialPrice != c.want.InitialPrice || s.InitialTerm != c.want.InitialTerm || s.InitialCurrency != c.want.InitialCurrency {
			t.Errorf("%s: got %v %v %v", c.name, s.InitialPrice, s.InitialTerm, s.InitialCurrency)
		}
	}
}

func TestCreateRejectsInitialWithoutCurrency(t *testing.T) {
	st := &ServerStore{DB: testDB(t)}
	_, err := st.Create(context.Background(), &Server{Hostname: "nc", ServerType: TypeKVM, InitialPrice: nf(5)}, nil, nil)
	if !errors.Is(err, ErrInitialNoCurrency) {
		t.Fatalf("err = %v", err)
	}
}

// UpdateFields keeps the stored value of every field not marked as set,
// read inside the write transaction; marked ones replace it.
func TestUpdateFieldsPartial(t *testing.T) {
	ctx := context.Background()
	db := testDB(t)
	st := &ServerStore{DB: db}
	usd := &Pricing{Currency: "USD", Price: 5, Term: TermMonthly}
	id, err := st.Create(ctx, &Server{Hostname: "pf", ServerType: TypeKVM, Keep: true,
		InitialPrice: nf(40), InitialTerm: ni(TermAnnual), InitialCurrency: ns("CHF")}, nil, usd)
	if err != nil {
		t.Fatal(err)
	}
	get := func() *Server {
		s, _, _, err := st.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}

	// Nothing set: the stale zero values are ignored.
	if err := st.UpdateFields(ctx, &Server{ID: id, Hostname: "pf2", ServerType: TypeKVM}, nil, usd, ServerFieldsSet{}); err != nil {
		t.Fatal(err)
	}
	if s := get(); s.Hostname != "pf2" || !s.Keep || s.InitialPrice != nf(40) || s.InitialTerm != ni(TermAnnual) || s.InitialCurrency != ns("CHF") {
		t.Fatalf("unset fields changed: %+v", s)
	}

	// Keep and term set; price and currency kept.
	if err := st.UpdateFields(ctx, &Server{ID: id, Hostname: "pf2", ServerType: TypeKVM, Keep: false, InitialTerm: ni(TermBiennial)},
		nil, usd, ServerFieldsSet{Keep: true, InitialTerm: true}); err != nil {
		t.Fatal(err)
	}
	if s := get(); s.Keep || s.InitialPrice != nf(40) || s.InitialTerm != ni(TermBiennial) || s.InitialCurrency != ns("CHF") {
		t.Fatalf("partial set: %+v", s)
	}

	// Explicit NULL currency, no pricing: nothing left to copy from.
	err = st.UpdateFields(ctx, &Server{ID: id, Hostname: "pf2", ServerType: TypeKVM}, nil, nil, ServerFieldsSet{InitialCurrency: true})
	if !errors.Is(err, ErrInitialNoCurrency) {
		t.Fatalf("err = %v", err)
	}
	if s := get(); s.InitialCurrency != ns("CHF") {
		t.Fatalf("failed update must roll back: %+v", s)
	}

	// Missing server.
	if err := st.UpdateFields(ctx, &Server{ID: 999, Hostname: "x", ServerType: TypeKVM}, nil, nil, ServerFieldsSet{}); err != sql.ErrNoRows {
		t.Fatalf("missing server: %v", err)
	}
}

// orderClause leaves commas inside function calls alone.
func TestOrderClauseFunctionArgs(t *testing.T) {
	got := orderClause("COALESCE(s.initial_price, pr.price), s.hostname", "desc")
	if want := "COALESCE(s.initial_price, pr.price) DESC, s.hostname DESC"; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	if got := orderClause(sortColumns["initial_price"], "asc"); strings.Contains(got, "DESC") {
		t.Fatalf("asc: %q", got)
	}
	st := &ServerStore{DB: testDB(t)}
	if _, err := st.List(context.Background(), ListOptions{Status: "all", Sort: "initial_price", Dir: "desc"}); err != nil {
		t.Fatalf("desc initial_price list: %v", err)
	}
}
