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
	"path/filepath"
	"testing"

	"idlerthing/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return database
}

func strPtr(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }
func intPtr(i int64) sql.NullInt64   { return sql.NullInt64{Int64: i, Valid: true} }

func TestServerCreateGetUpdateDelete(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &ServerStore{DB: database}

	srv := &Server{
		Hostname:      "web-01.example.com",
		ServerType:    TypeKVM,
		RamAsMB:       intPtr(4096),
		CPU:           intPtr(4),
		CPUModel:      strPtr("AMD EPYC 7443P"),
		BandwidthAsMB: intPtr(10 * 1024 * 1024),
		SSHPort:       intPtr(22),
		Active:        true,
		NetworkType:   strPtr("IPv4+IPv6"),
	}
	disks := []ServerDisk{
		{SizeAsMB: 80 * 1024, Media: "NVMe"},
		{SizeAsMB: 500 * 1024, Media: "HDD"},
		{SizeAsMB: 0, Media: "SSD"}, // ignored
	}
	pricing := &Pricing{
		Currency:    "USD",
		Price:       120,
		Term:        TermAnnual,
		NextDueDate: strPtr("2027-01-15"),
	}

	id, err := st.Create(ctx, srv, disks, pricing)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, gotDisks, gotPricing, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Hostname != srv.Hostname || !got.Active || got.RamAsMB.Int64 != 4096 {
		t.Fatalf("unexpected server: %+v", got)
	}
	if len(gotDisks) != 2 {
		t.Fatalf("expected 2 disks (empty row skipped), got %d", len(gotDisks))
	}
	if gotPricing == nil || gotPricing.Price != 120 || gotPricing.Term != TermAnnual {
		t.Fatalf("unexpected pricing: %+v", gotPricing)
	}
	if got := gotPricing.Price / float64(TermMonths(gotPricing.Term)); got != 10 {
		t.Fatalf("expected 10/mo, got %v", got)
	}

	// Update: rename, drop a disk, change pricing.
	got.Hostname = "web-02.example.com"
	got.Active = false
	err = st.Update(ctx, got, []ServerDisk{{SizeAsMB: 1024, Media: "NVMe"}}, &Pricing{
		Currency: "EUR", Price: 20, Term: TermMonthly,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	got2, gotDisks2, gotPricing2, err := st.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if got2.Hostname != "web-02.example.com" || got2.Active {
		t.Fatalf("update not applied: %+v", got2)
	}
	if len(gotDisks2) != 1 || gotDisks2[0].SizeAsMB != 1024 {
		t.Fatalf("disks not replaced: %+v", gotDisks2)
	}
	if gotPricing2.Currency != "EUR" || gotPricing2.Price != 20 {
		t.Fatalf("pricing not upserted: %+v", gotPricing2)
	}

	// Delete cascades disks and leaves no server.
	if err := st.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, _, _, err := st.Get(ctx, id); err != sql.ErrNoRows {
		t.Fatalf("expected ErrNoRows after delete, got %v", err)
	}
	var diskRows int
	database.QueryRow("SELECT COUNT(*) FROM server_disks WHERE server_id = ?", id).Scan(&diskRows)
	if diskRows != 0 {
		t.Fatalf("disks not cascaded: %d rows remain", diskRows)
	}
}

func TestServerListFilterSortSearch(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &ServerStore{DB: database}
	cat := &CatalogStore{DB: database}

	provID, _ := cat.Create(ctx, Catalogs["providers"], "Hetzner")
	_, _ = cat.Create(ctx, Catalogs["providers"], "OVH")

	mk := func(hostname string, ramMB int64, active bool, prov sql.NullInt64, price float64) {
		_, err := st.Create(ctx, &Server{
			Hostname: hostname, ServerType: TypeKVM, RamAsMB: intPtr(ramMB),
			Active: active, ProviderID: prov,
		}, nil, &Pricing{Currency: "USD", Price: price, Term: TermMonthly})
		if err != nil {
			t.Fatalf("create %s: %v", hostname, err)
		}
	}
	mk("alpha", 2048, true, sql.NullInt64{Int64: provID, Valid: true}, 5)
	mk("beta", 8192, true, sql.NullInt64{}, 10)
	mk("gamma", 1024, false, sql.NullInt64{}, 1)

	// Default: active only.
	items, err := st.List(ctx, ListOptions{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("expected 2 active, got %d", len(items))
	}

	// Inactive + all.
	if items, _ = st.List(ctx, ListOptions{Status: "inactive"}); len(items) != 1 {
		t.Fatalf("expected 1 inactive, got %d", len(items))
	}
	if items, _ = st.List(ctx, ListOptions{Status: "all"}); len(items) != 3 {
		t.Fatalf("expected 3 total, got %d", len(items))
	}

	// Search by provider name.
	items, _ = st.List(ctx, ListOptions{Status: "all", Q: "hetz"})
	if len(items) != 1 || items[0].Hostname != "alpha" {
		t.Fatalf("provider search failed: %+v", items)
	}

	// Sort by ram desc.
	items, _ = st.List(ctx, ListOptions{Status: "all", Sort: "ram", Dir: "desc"})
	if len(items) != 3 || items[0].Hostname != "beta" || items[2].Hostname != "gamma" {
		t.Fatalf("ram desc sort failed: %v", hostnames(items))
	}

	// Sort by price asc.
	items, _ = st.List(ctx, ListOptions{Status: "all", Sort: "price", Dir: "asc"})
	if items[0].Hostname != "gamma" {
		t.Fatalf("price asc sort failed: %v", hostnames(items))
	}

	// Bogus sort key falls back to hostname, no error.
	if _, err := st.List(ctx, ListOptions{Sort: "; DROP TABLE servers--"}); err != nil {
		t.Fatalf("bogus sort must not error: %v", err)
	}

	active, inactive, err := st.StatusCounts(ctx)
	if err != nil || active != 2 || inactive != 1 {
		t.Fatalf("StatusCounts: %d/%d err=%v", active, inactive, err)
	}
}

func hostnames(items []ServerListItem) []string {
	var out []string
	for _, it := range items {
		out = append(out, it.Hostname)
	}
	return out
}

func TestCatalogCRUDAndInUseRefusal(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	cat := &CatalogStore{DB: database}
	providers := Catalogs["providers"]

	id, err := cat.Create(ctx, providers, "Hetzner")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := cat.Create(ctx, providers, "Hetzner"); err == nil {
		t.Fatal("expected unique violation on duplicate name")
	}

	if err := cat.Update(ctx, providers, id, "Hetzner GmbH"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	items, _ := cat.List(ctx, providers)
	if len(items) != 1 || items[0].Name != "Hetzner GmbH" {
		t.Fatalf("unexpected list: %+v", items)
	}

	// Attach a server → delete must be refused with usage count.
	st := &ServerStore{DB: database}
	if _, err := st.Create(ctx, &Server{
		Hostname: "x", ServerType: TypeDedi,
		ProviderID: sql.NullInt64{Int64: id, Valid: true},
	}, nil, nil); err != nil {
		t.Fatalf("create server: %v", err)
	}
	err = cat.Delete(ctx, providers, id)
	if !errors.Is(err, ErrInUse) {
		t.Fatalf("expected ErrInUse, got %v", err)
	}
	if n, _ := cat.UsageCount(ctx, providers, id); n != 1 {
		t.Fatalf("expected usage 1, got %d", n)
	}

	// Detach → delete succeeds.
	var srvID int64
	database.QueryRow("SELECT id FROM servers WHERE hostname = 'x'").Scan(&srvID)
	if err := st.Delete(ctx, srvID); err != nil {
		t.Fatalf("delete server: %v", err)
	}
	if err := cat.Delete(ctx, providers, id); err != nil {
		t.Fatalf("Delete after detach: %v", err)
	}
}

// Batch V4 — sorting by bandwidth descending puts unlimited (NULL) FIRST;
// a single trailing DESC used to reverse only the value column and pin
// unlimited last in both directions.
func TestServerListBandwidthSortUnlimitedFirstDesc(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &ServerStore{DB: database}
	mk := func(hostname string, bw sql.NullInt64) {
		if _, err := st.Create(ctx, &Server{Hostname: hostname, ServerType: TypeKVM, Active: true, BandwidthAsMB: bw}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	mk("one-tb", intPtr(1<<20))
	mk("unlimited", sql.NullInt64{})
	mk("ten-tb", intPtr(10<<20))

	items, err := st.List(ctx, ListOptions{Sort: "bw", Dir: "desc"})
	if err != nil {
		t.Fatal(err)
	}
	if got := hostnames(items); len(got) != 3 || got[0] != "unlimited" || got[1] != "ten-tb" || got[2] != "one-tb" {
		t.Fatalf("bw desc: %v", got)
	}
	items, _ = st.List(ctx, ListOptions{Sort: "bw", Dir: "asc"})
	if got := hostnames(items); got[0] != "one-tb" || got[1] != "ten-tb" || got[2] != "unlimited" {
		t.Fatalf("bw asc: %v", got)
	}
}
