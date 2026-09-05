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
	"strings"
	"testing"
)

// Batch W1 — entities with a blank key field (hostname/domain/name, dns
// hostname+address) are skipped with a warning, never inserted as
// unreachable empty rows. Names are stored trimmed.
func TestImportSkipsBlankNames(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,
	  "servers":[{"server":{"id":1,"hostname":"  "}}, {"server":{"id":2,"hostname":"  real-host  "}}],
	  "shared":[{"shared_hosting":{"id":1,"main_domain":""}}],
	  "reseller":[{"shared_hosting":{"id":1,"main_domain":" "}}],
	  "seedboxes":[{"seedbox":{"id":1,"hostname":""}}],
	  "domains":[{"domain":{"id":1,"domain":""}}],
	  "misc":[{"misc_service":{"id":1,"name":""}}],
	  "dns":[{"dns_record":{"hostname":"","address":"1.2.3.4"}},
	         {"dns_record":{"hostname":"a.example.com","address":""}}]}`
	sum, err := Import(context.Background(), database, strings.NewReader(doc), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	for table, want := range map[string]int{
		"servers": 1, "shared_hosting": 0, "reseller_hosting": 0,
		"seedboxes": 0, "domains": 0, "misc_services": 0, "dns": 0,
	} {
		var n int
		database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n)
		if n != want {
			t.Errorf("%s: %d rows, want %d", table, n, want)
		}
	}
	var host string
	database.QueryRow("SELECT hostname FROM servers").Scan(&host)
	if host != "real-host" {
		t.Errorf("hostname should be stored trimmed, got %q", host)
	}
	if len(sum.Warnings) < 8 {
		t.Fatalf("expected a warning per skipped entity, got %d: %v", len(sum.Warnings), sum.Warnings)
	}
}

// Batch W2 — exported timestamps are validated: garbage keeps the schema
// default (and warns), RFC 3339 is normalised to SQLite's shape.
func TestImportTimestampsValidated(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,"servers":[
	  {"server":{"id":1,"hostname":"ts-bad","created_at":"garbage","updated_at":"also garbage"}},
	  {"server":{"id":2,"hostname":"ts-rfc","created_at":"2025-01-02T03:04:05Z","updated_at":"2025-06-07 08:09:10"}}]}`
	sum, err := Import(context.Background(), database, strings.NewReader(doc), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var created, updated string
	database.QueryRow("SELECT created_at, updated_at FROM servers WHERE hostname = 'ts-bad'").Scan(&created, &updated)
	if created == "garbage" || updated == "also garbage" || len(created) != 19 {
		t.Errorf("garbage timestamps must not be stored: %q %q", created, updated)
	}
	database.QueryRow("SELECT created_at, updated_at FROM servers WHERE hostname = 'ts-rfc'").Scan(&created, &updated)
	if created != "2025-01-02 03:04:05" || updated != "2025-06-07 08:09:10" {
		t.Errorf("timestamps not normalised: %q %q", created, updated)
	}
	found := false
	for _, w := range sum.Warnings {
		if strings.Contains(w, "invalid created_at") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a timestamp warning, got %v", sum.Warnings)
	}
}

// Batch W3 — a pricing object without an "active" key imports ACTIVE (the
// schema default); an explicit false is honoured.
func TestImportPricingActiveDefaultsTrue(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,
	  "servers":[{"server":{"id":1,"hostname":"p-default"},"pricing":{"currency":"USD","price":5,"term":1}},
	             {"server":{"id":2,"hostname":"p-off"},"pricing":{"currency":"USD","price":5,"term":1,"active":false}},
	             {"server":{"id":3,"hostname":"p-top"}}],
	  "pricings":[{"service_id":3,"service_type":1,"currency":"EUR","price":7,"term":1}]}`
	if _, err := Import(context.Background(), database, strings.NewReader(doc), false); err != nil {
		t.Fatalf("import: %v", err)
	}
	for id, want := range map[int]int{1: 1, 2: 0, 3: 1} {
		var active int
		if err := database.QueryRow("SELECT active FROM pricings WHERE service_id = ? AND service_type = 1", id).Scan(&active); err != nil {
			t.Fatalf("pricing for service %d: %v", id, err)
		}
		if active != want {
			t.Errorf("service %d: active=%d, want %d", id, active, want)
		}
	}
}

// Batch W4 — my-idlers IPs are stored canonicalised, so case/zero-run
// variants of one IPv6 address collapse into a single row.
func TestMyIdlersIPsCanonical(t *testing.T) {
	database := testDB(t)
	recs, warnings, err := ParseMyJSON(strings.NewReader(`[{"hostname":"v6-host",
	  "ips":[{"address":"2001:DB8::1","is_ipv4":0},{"address":"2001:db8:0:0::1","is_ipv4":0},{"address":"203.0.113.001","is_ipv4":1}]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ImportMyIdlers(context.Background(), database, recs, warnings); err != nil {
		t.Fatal(err)
	}
	var n int
	database.QueryRow("SELECT COUNT(*) FROM ips").Scan(&n)
	if n != 1 {
		var addrs []string
		rows, _ := database.Query("SELECT address FROM ips")
		for rows.Next() {
			var a string
			rows.Scan(&a)
			addrs = append(addrs, a)
		}
		rows.Close()
		t.Fatalf("expected one canonical IPv6 row (leading-zero IPv4 is invalid), got %d: %v", n, addrs)
	}
	var addr string
	database.QueryRow("SELECT address FROM ips").Scan(&addr)
	if addr != "2001:db8::1" {
		t.Fatalf("stored %q, want canonical 2001:db8::1", addr)
	}
}

func TestNormTimestamp(t *testing.T) {
	cases := map[string]struct {
		want string
		ok   bool
	}{
		"":                          {"", true},
		"   ":                       {"", true},
		"2025-01-02 03:04:05":       {"2025-01-02 03:04:05", true},
		"2025-01-02T03:04:05Z":      {"2025-01-02 03:04:05", true},
		"2025-01-02T05:04:05+02:00": {"2025-01-02 03:04:05", true},
		"2025-01-02":                {"2025-01-02 00:00:00", true},
		"garbage":                   {"", false},
		"2025-13-45":                {"", false},
	}
	for in, c := range cases {
		got, ok := normTimestamp(in)
		if got != c.want || ok != c.ok {
			t.Errorf("normTimestamp(%q) = %q,%v; want %q,%v", in, got, ok, c.want, c.ok)
		}
	}
}
