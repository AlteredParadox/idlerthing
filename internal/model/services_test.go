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
	"testing"
)

// TestServiceStoresCRUD exercises create/get/update/delete + pricing for all
// five Phase-3b service types.
func TestServiceStoresCRUD(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	pricing := &Pricing{Currency: "USD", Price: 24, Term: TermAnnual, NextDueDate: strPtr("2027-05-01")}

	t.Run("shared", func(t *testing.T) {
		st := &SharedStore{DB: database}
		id, err := st.Create(ctx, &SharedHosting{
			MainDomain: "blog.example.com", SharedType: strPtr("cPanel"),
			DomainsLimit: intPtr(10), Active: true,
		}, pricing)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		h, p, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if h.MainDomain != "blog.example.com" || p == nil || p.ServiceType != ServiceShared {
			t.Fatalf("unexpected: %+v / %+v", h, p)
		}
		h.MainDomain = "www.example.com"
		if err := st.Update(ctx, h, nil); err != nil {
			t.Fatalf("Update: %v", err)
		}
		_, p2, _ := st.Get(ctx, id)
		if p2 != nil {
			t.Fatal("pricing should be removed on update with nil")
		}
		if err := st.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if _, _, err := st.Get(ctx, id); err != sql.ErrNoRows {
			t.Fatalf("expected ErrNoRows, got %v", err)
		}
	})

	t.Run("reseller", func(t *testing.T) {
		st := &ResellerStore{DB: database}
		id, err := st.Create(ctx, &ResellerHosting{
			MainDomain: "reseller.example.com", SharedType: strPtr("WHM"), Active: true,
		}, pricing)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		h, p, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if h.MainDomain != "reseller.example.com" || p.ServiceType != ServiceReseller {
			t.Fatalf("unexpected: %+v / %+v", h, p)
		}
		if err := st.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	t.Run("seedbox", func(t *testing.T) {
		st := &SeedboxStore{DB: database}
		id, err := st.Create(ctx, &Seedbox{
			Hostname: "seed-01.example.com", Title: strPtr("Racing box"),
			PortSpeed: intPtr(10000), DiskAsMB: intPtr(2000 * 1024), Active: true,
		}, pricing)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		b, p, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if b.Hostname != "seed-01.example.com" || p.ServiceType != ServiceSeedbox {
			t.Fatalf("unexpected: %+v / %+v", b, p)
		}
		b.Title = strPtr("Renamed")
		if err := st.Update(ctx, b, pricing); err != nil {
			t.Fatalf("Update: %v", err)
		}
		if err := st.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	t.Run("domain", func(t *testing.T) {
		st := &DomainStore{DB: database}
		id, err := st.Create(ctx, &Domain{
			Domain: "example.com", Extension: strPtr(".com"), Ns1: strPtr("ns1.example.com"), Active: true,
		}, pricing)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		d, p, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if d.Domain != "example.com" || p.ServiceType != ServiceDomain {
			t.Fatalf("unexpected: %+v / %+v", d, p)
		}
		if err := st.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})

	t.Run("misc", func(t *testing.T) {
		st := &MiscStore{DB: database}
		id, err := st.Create(ctx, &MiscService{Name: "VPN subscription", Active: true}, pricing)
		if err != nil {
			t.Fatalf("Create: %v", err)
		}
		m, p, err := st.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if m.Name != "VPN subscription" || p.ServiceType != ServiceMisc {
			t.Fatalf("unexpected: %+v / %+v", m, p)
		}
		if err := st.Delete(ctx, id); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	})
}

// TestServiceListFilterSearch spot-checks list filtering on two types.
func TestServiceListFilterSearch(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	t.Run("shared", func(t *testing.T) {
		st := &SharedStore{DB: database}
		st.Create(ctx, &SharedHosting{MainDomain: "alpha.com", Active: true}, nil)
		st.Create(ctx, &SharedHosting{MainDomain: "beta.com", Active: false}, nil)

		items, _ := st.List(ctx, ListOptions{})
		if len(items) != 1 || items[0].MainDomain != "alpha.com" {
			t.Fatalf("active filter: %+v", items)
		}
		items, _ = st.List(ctx, ListOptions{Status: "all", Q: "beta"})
		if len(items) != 1 || items[0].MainDomain != "beta.com" {
			t.Fatalf("search: %+v", items)
		}
		active, inactive, _ := st.StatusCounts(ctx)
		if active != 1 || inactive != 1 {
			t.Fatalf("counts: %d/%d", active, inactive)
		}
	})

	t.Run("misc", func(t *testing.T) {
		st := &MiscStore{DB: database}
		st.Create(ctx, &MiscService{Name: "b-service", Active: true}, &Pricing{Currency: "EUR", Price: 5, Term: TermMonthly})
		st.Create(ctx, &MiscService{Name: "a-service", Active: true}, &Pricing{Currency: "EUR", Price: 50, Term: TermMonthly})

		items, err := st.List(ctx, ListOptions{Sort: "price", Dir: "desc"})
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(items) != 2 || items[0].Name != "a-service" {
			t.Fatalf("price desc sort: %+v", items)
		}
	})
}
