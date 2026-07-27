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

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"idlerthing/internal/importer"
	"idlerthing/internal/model"
)

// Batch M R1 — DNS/IP/note writes bump the dashboard generation, so the
// cached sidebar counts refresh on the next render.
func TestRelationWritesRefreshSidebarCounts(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	req := httptest.NewRequest("GET", "/", nil)

	base := srv.counts(req)
	createServer(t, client, ts, "rel-host")
	afterServer := srv.counts(req)
	if afterServer.Servers != base.Servers+1 {
		t.Fatalf("server count should refresh: %d → %d", base.Servers, afterServer.Servers)
	}

	postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"r.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.7"},
	}).Body.Close()
	if got := srv.counts(req); got.DNS != afterServer.DNS+1 {
		t.Fatalf("dns count should refresh after create: %d → %d", afterServer.DNS, got.DNS)
	}

	postForm(t, client, ts, "/ips", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "address": {"203.0.113.8"},
	}).Body.Close()
	if got := srv.counts(req); got.IPs != afterServer.IPs+1 {
		t.Fatalf("ips count should refresh after create: %d → %d", afterServer.IPs, got.IPs)
	}

	postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "body": {"sidebar note"},
	}).Body.Close()
	if got := srv.counts(req); got.Notes != afterServer.Notes+1 {
		t.Fatalf("notes count should refresh after create: %d → %d", afterServer.Notes, got.Notes)
	}
}

// Batch M R2 — an imported inactive pricing keeps its flag (round-trip
// fidelity) but is invisible to Get and on the detail page.
func TestImportedInactivePricingHidden(t *testing.T) {
	database := freshDB(t)
	ctx := context.Background()
	fixture := `{
		"format": 1,
		"servers": [{"server": {"id": 1, "hostname": "inactive-priced", "server_type": 1, "active": true}}],
		"pricings": [{"service_id": 1, "service_type": 1, "currency": "USD", "price": 5, "term": 1, "active": false}]
	}`
	if _, err := importer.Import(ctx, database, strings.NewReader(fixture), false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	var active int
	if err := database.QueryRow("SELECT active FROM pricings").Scan(&active); err != nil {
		t.Fatal(err)
	}
	if active != 0 {
		t.Fatalf("inactive flag must survive the round-trip, got %d", active)
	}

	p, err := (&model.PricingStore{DB: database}).Get(ctx, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if p != nil {
		t.Fatalf("inactive pricing must be invisible to Get, got %+v", p)
	}

	// Detail page: visible while active, hidden once inactive.
	ts, _, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	resp := postForm(t, client, ts, "/servers", url.Values{
		"hostname": {"inactive-priced"}, "server_type": {"1"}, "active": {"on"},
		"price": {"5"}, "currency": {"USD"}, "term": {"1"},
	})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "$5.00") {
		t.Fatal("active pricing should show on the detail page")
	}
	if _, err := srv.db.Exec("UPDATE pricings SET active = 0 WHERE service_id = 1 AND service_type = 1"); err != nil {
		t.Fatal(err)
	}
	resp, err = client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "$5.00") {
		t.Fatal("detail page must not show imported-inactive pricing")
	}
}

// Batch M F4 — DNSStore parent guards: bogus parent → sql.ErrNoRows
// (atomic), never an FK 500; valid parent inserts fine.
func TestDNSAtomicParentGuard(t *testing.T) {
	_, database, _ := newTestServerFull(t)
	ctx := context.Background()
	st := &model.DNSStore{DB: database}

	bogus := &model.DNSRecord{
		Hostname: "x.example.com", DNSType: "A", Address: "203.0.113.10",
		ServerID: sql.NullInt64{Int64: 999, Valid: true},
	}
	if _, err := st.Create(ctx, bogus); err != sql.ErrNoRows {
		t.Fatalf("bogus parent should yield ErrNoRows, got %v", err)
	}

	// Valid parent inserts; re-pointing the update at a bogus parent fails
	// the same way.
	rec := &model.DNSRecord{
		Hostname: "y.example.com", DNSType: "A", Address: "203.0.113.11",
	}
	if _, err := database.Exec(
		"INSERT INTO servers (hostname, server_type, active) VALUES ('dns-guard', 1, 1)"); err != nil {
		t.Fatal(err)
	}
	rec.ServerID = sql.NullInt64{Int64: 1, Valid: true}
	id, err := st.Create(ctx, rec)
	if err != nil {
		t.Fatalf("valid parent: %v", err)
	}
	rec.ID = id
	rec.ServerID = sql.NullInt64{Int64: 999, Valid: true}
	if err := st.Update(ctx, rec); err != sql.ErrNoRows {
		t.Fatalf("update with bogus parent should yield ErrNoRows, got %v", err)
	}
}
