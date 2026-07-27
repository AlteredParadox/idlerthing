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
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"idlerthing/internal/importer"
	"idlerthing/internal/model"
)

// #1d — export/import/export equivalence by natural keys.
func TestExportImportEquivalence(t *testing.T) {
	tsA, dbA := newTestServer(t)
	ctx := context.Background()

	// Rich seed: catalogs, server with everything, shared with label,
	// domain with dns, note, ip, yabs, an INACTIVE pricing.
	cat := &model.CatalogStore{DB: dbA}
	provID, _ := cat.Create(ctx, model.Catalogs["providers"], "Hetzner")
	locID, _ := cat.Create(ctx, model.Catalogs["locations"], "Falkenstein")
	osID, _ := cat.Create(ctx, model.Catalogs["os"], "Debian 12")
	ref := func(i int64) sql.NullInt64 { return sql.NullInt64{Int64: i, Valid: true} }
	str := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

	srvSt := &model.ServerStore{DB: dbA}
	srvID, err := srvSt.Create(ctx, &model.Server{
		Hostname: "eq-01", ServerType: model.TypeDedi, OsID: ref(osID),
		ProviderID: ref(provID), LocationID: ref(locID),
		RamAsMB: ref(8192), CPU: ref(4), CPUModel: str("AMD EPYC"),
		BandwidthAsMB: ref(20971520), NetworkType: str("IPv4+IPv6"),
		SSHPort: ref(2222), Active: true, OwnedSince: str("2024-05-01"),
	}, []model.ServerDisk{{SizeAsMB: 512000, Media: "NVMe"}},
		&model.Pricing{Currency: "EUR", Price: 25, Term: model.TermMonthly, NextDueDate: str("2026-12-01")})
	if err != nil {
		t.Fatal(err)
	}
	ips := &model.IPStore{DB: dbA}
	ips.Create(ctx, &model.IP{ServiceID: srvID, ServiceType: model.ServiceServer, Address: "203.0.113.10", IsIPv4: true})
	notes := &model.NoteStore{DB: dbA}
	notes.Create(ctx, &model.Note{ServiceID: ref(srvID), ServiceType: ref(model.ServiceServer), Body: "eq note"})
	labels := &model.LabelStore{DB: dbA}
	labelID, _ := labels.FindOrCreate(ctx, "production")
	labels.Assign(ctx, labelID, srvID, model.ServiceServer)
	yabsSt := &model.YABSStore{DB: dbA}
	yabsSt.Create(ctx, &model.YABS{
		ServerID: srvID, RunAt: str("2026-07-01"), CPU: str("AMD EPYC"),
		GbSingle: ref(1600), GbMulti: ref(4500),
	}, []model.YABSDiskSpeed{{BlockSize: "4k", ReadMbps: 88, WriteMbps: 90}},
		[]model.YABSNetworkSpeed{{Location: "FRA", SendMbps: 900, RecvMbps: 950, LatencyMs: 12}})

	sharedSt := &model.SharedStore{DB: dbA}
	sharedID, _ := sharedSt.Create(ctx, &model.SharedHosting{
		MainDomain: "eq.example.com", ProviderID: ref(provID), LocationID: ref(locID),
		DiskAsMB: ref(51200), Active: true,
	}, &model.Pricing{Currency: "USD", Price: 9, Term: model.TermAnnual, NextDueDate: str("2027-02-01")})
	labels.Assign(ctx, labelID, sharedID, model.ServiceShared)

	domSt := &model.DomainStore{DB: dbA}
	domID, _ := domSt.Create(ctx, &model.Domain{Domain: "eq-domain.com", ProviderID: ref(provID), Active: true}, nil)
	dnsSt := &model.DNSStore{DB: dbA}
	dnsSt.Create(ctx, &model.DNSRecord{Hostname: "www.eq-domain.com", DNSType: "CNAME", Address: "eq-domain.com", DomainID: ref(domID)})

	// Inactive pricing (must survive via the top-level pricings table).
	dbA.Exec("UPDATE pricings SET active = 0 WHERE service_id = ? AND service_type = 2", sharedID)

	exportA := fetchExport(t, tsA)

	// Import into fresh B, then export B.
	dbB := freshDB(t)
	if _, err := importer.Import(ctx, dbB, bytes.NewReader(exportA), false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	srvB, err := New(dbB)
	if err != nil {
		t.Fatal(err)
	}
	exportB := fetchExportFrom(t, srvB)

	a := naturalize(t, exportA)
	b := naturalize(t, exportB)
	if a != b {
		t.Fatalf("exports diverge:\nA: %s\nB: %s", a, b)
	}
}

// naturalize reduces an export document to a comparable canonical form:
// ids/timestamps/volatile fields stripped, keyed by natural keys.
func naturalize(t *testing.T, raw []byte) string {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var lines []string
	add := func(prefix string, item any) {
		b, _ := json.Marshal(item)
		lines = append(lines, prefix+string(b))
	}

	strip := func(m map[string]any, keys ...string) map[string]any {
		out := map[string]any{}
		for _, k := range keys {
			if v, ok := m[k]; ok {
				out[k] = v
			}
		}
		return out
	}

	for _, key := range []string{"providers", "locations", "os", "labels"} {
		for _, item := range arrOf(doc[key]) {
			add(key+":", strip(item.(map[string]any), "name"))
		}
	}
	for _, item := range arrOf(doc["servers"]) {
		m := item.(map[string]any)
		s := m["server"].(map[string]any)
		add("server:", strip(s, "hostname", "server_type", "ram_as_mb", "bandwidth_as_mb", "owned_since", "network_type", "ssh_port"))
		if p, ok := m["pricing"].(map[string]any); ok {
			add("pricing:", strip(p, "currency", "price", "term", "next_due_date", "active"))
		}
		for _, d := range arrOf(m["disks"]) {
			add("disk:", strip(d.(map[string]any), "size_as_mb", "media"))
		}
		for _, l := range arrOf(m["labels"]) {
			add("label-on-server:", strip(l.(map[string]any), "name"))
		}
		for _, ip := range arrOf(m["ips"]) {
			add("ip:", strip(ip.(map[string]any), "address", "is_ipv4"))
		}
	}
	for _, item := range arrOf(doc["shared"]) {
		m := item.(map[string]any)
		h := m["shared_hosting"].(map[string]any)
		add("shared:", strip(h, "main_domain", "active", "disk_as_mb"))
		if p, ok := m["pricing"].(map[string]any); ok {
			add("shared-pricing:", strip(p, "currency", "price", "term", "active"))
		}
	}
	for _, item := range arrOf(doc["pricings"]) {
		p := item.(map[string]any)
		// service ids differ between A and B — compare shape only.
		add("toplevel-pricing:", strip(p, "currency", "price", "term", "active"))
	}
	for _, item := range arrOf(doc["yabs"]) {
		y := item.(map[string]any)
		add("yabs:", strip(y, "gb_single", "gb_multi", "cpu"))
		for _, d := range arrOf(y["disk_speed"]) {
			add("yabs-disk:", strip(d.(map[string]any), "block_size", "read_mbps", "write_mbps"))
		}
		for _, n := range arrOf(y["network_speed"]) {
			add("yabs-net:", strip(n.(map[string]any), "location", "send_mbps", "recv_mbps", "latency_ms"))
		}
	}
	for _, item := range arrOf(doc["labels_assigned"]) {
		a := item.(map[string]any)
		add("labels_assigned:", strip(a, "service_type"))
	}
	for _, item := range arrOf(doc["ips"]) {
		add("ip-all:", strip(item.(map[string]any)["ip"].(map[string]any), "address"))
	}
	for _, item := range arrOf(doc["dns"]) {
		d := item.(map[string]any)["dns_record"].(map[string]any)
		add("dns:", strip(d, "hostname", "dns_type", "address"))
	}
	for _, item := range arrOf(doc["notes"]) {
		n := item.(map[string]any)["note"].(map[string]any)
		add("note:", strip(n, "service_type", "body"))
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func arrOf(v any) []any {
	a, _ := v.([]any)
	return a
}

// fetchExport gets the full export via an authed request on a live server.
func fetchExport(t *testing.T, ts *httptest.Server) []byte {
	t.Helper()
	client := authedClient(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/export/json", nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body
}

// fetchExportFrom builds the export by calling the handler directly.
func fetchExportFrom(t *testing.T, srv *Server) []byte {
	t.Helper()
	recorder := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/export/json", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxMemo, &reqMemo{}))
	srv.handleExportJSON(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("export from B: %d", recorder.Code)
	}
	return recorder.Body.Bytes()
}

// #2 — myidlers: parse-time IP dedupe means the first row imports (warning
// attached, counts exact); the second same-hostname row is the skipped
// duplicate. (Batch N M1 made parse-driven row failures unreachable — the
// savepoint machinery remains as a safety net.)
func TestMyIdlersFailedRowDoesntSuppress(t *testing.T) {
	dbB := freshDB(t)
	ctx := context.Background()
	records, parseWarnings, err := importer.ParseMyJSON(strings.NewReader(`[
		{"hostname": "retry-01", "server_type": 1,
		 "os": {"name": "Debian 12"},
		 "ips": [{"address": "203.0.113.5", "is_ipv4": 1},
		         {"address": "203.0.113.5", "is_ipv4": 1}]},
		{"hostname": "retry-01", "server_type": 1,
		 "os": {"name": "Debian 12"},
		 "ips": [{"address": "203.0.113.5", "is_ipv4": 1}]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := importer.ImportMyIdlers(ctx, dbB, records, parseWarnings)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Imported != 1 || sum.SkippedDup != 1 {
		t.Fatalf("first row imports, second is the skipped dup: %+v", sum)
	}
	if len(sum.Warnings) != 1 || !strings.Contains(sum.Warnings[0], "duplicate IP") {
		t.Fatalf("expected the duplicate-IP warning: %+v", sum)
	}
	// Counters must match the DB exactly — the dup IP inflated nothing.
	var servers, ips, osCount int
	dbB.QueryRow("SELECT COUNT(*) FROM servers").Scan(&servers)
	dbB.QueryRow("SELECT COUNT(*) FROM ips").Scan(&ips)
	dbB.QueryRow("SELECT COUNT(*) FROM os").Scan(&osCount)
	if servers != 1 || ips != 1 || osCount != 1 {
		t.Fatalf("db counts: %d %d %d", servers, ips, osCount)
	}
	if sum.OS != osCount || sum.IPs != ips || sum.Disks != 0 {
		t.Fatalf("summary drift: %+v vs db (%d %d %d)", sum, servers, ips, osCount)
	}
	var hostname string
	dbB.QueryRow("SELECT hostname FROM servers").Scan(&hostname)
	if hostname != "retry-01" {
		t.Fatalf("wrong row imported: %s", hostname)
	}
}

// #4 — happy paths still work after atomic EXISTS.
func TestExtrasHappyPathsStillWork(t *testing.T) {
	ts, _ := newTestServer(t)
	client := seedOneServer(t, ts)

	resp := postForm(t, client, ts, "/ips", url.Values{
		"service_id": {"1"}, "service_type": {"1"},
		"address": {"203.0.113.77"}, "back": {"/servers/1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("ip create: %d", resp.StatusCode)
	}
	resp = postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "body": {"still works"}, "back": {"/servers/1"},
	})
	resp.Body.Close()
	resp = postForm(t, client, ts, "/labels/assign", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "new_label": {"happy"}, "back": {"/servers/1"},
	})
	resp.Body.Close()

	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	for _, want := range []string{"203.0.113.77", "still works", "happy"} {
		if !strings.Contains(body, want) {
			t.Fatalf("detail missing %q", want)
		}
	}
}

// #6 — pagination overflow can't panic; envelope stays correct.
func TestAPIPaginationOverflow(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	client := authedClient(t, ts)
	createServer(t, client, ts, "pg-01")

	resp, body := apiGet(t, ts, "/api/servers?page=9223372036854775807&per=50", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("huge page: %d", resp.StatusCode)
	}
	if body["total"] != 1.0 {
		t.Fatalf("total wrong: %v", body["total"])
	}
	if data := body["data"].([]any); len(data) != 0 {
		t.Fatalf("huge page should be empty: %v", data)
	}

	// Normal page still works.
	_, body = apiGet(t, ts, "/api/servers?per=1&page=1", token)
	if len(body["data"].([]any)) != 1 {
		t.Fatal("page 1 should have the row")
	}
}

// #7 — unpublishing reflects on /public immediately (generation invalidation).
func TestPublicCacheInvalidation(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	// Enable public page + one public server.
	resp := postForm(t, client, ts, "/settings", url.Values{
		"default_currency": {"USD"}, "dashboard_currency": {"USD"},
		"due_soon_amount": {"14"}, "recently_added_amount": {"5"},
		"theme": {"dark"}, "servers_public": {"on"},
	})
	resp.Body.Close()
	createServer(t, client, ts, "pub-01")

	// Make it public via update.
	resp = postForm(t, client, ts, "/servers/1/update", url.Values{
		"hostname": {"pub-01"}, "server_type": {"1"}, "active": {"on"}, "show_public": {"on"},
	})
	resp.Body.Close()

	public := func() string {
		resp, err := newClient(t).Get(ts.URL + "/public")
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		return body
	}

	if !strings.Contains(public(), "pub-01") {
		t.Fatal("public server should be listed")
	}

	// Unpublish (touchDashboard bumps the generation on update).
	resp = postForm(t, client, ts, "/servers/1/update", url.Values{
		"hostname": {"pub-01"}, "server_type": {"1"}, "active": {"on"},
	})
	resp.Body.Close()
	if strings.Contains(public(), "pub-01") {
		t.Fatal("unpublished server must disappear immediately")
	}
}

// #9 — whois throttle bounces instead of sleeping.
func TestWhoisThrottleBounces(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	client := seedOneServer(t, ts)
	postForm(t, client, ts, "/ips", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "address": {"203.0.113.10"}, "back": {"/servers/1"},
	}).Body.Close()

	srv.whoisURL = "http://127.0.0.1:1/unreachable" // fast failure path
	csrf := sessionCSRF(t, client, ts)

	start := time.Now()
	for i := 0; i < 2; i++ {
		resp, err := client.PostForm(ts.URL+"/ips/1/whois", url.Values{"csrf_token": {csrf}, "back": {"/ips"}})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	if elapsed := time.Since(start); elapsed > 900*time.Millisecond {
		t.Fatalf("throttle should bounce, not sleep: %v", elapsed)
	}
	resp, err := client.Get(ts.URL + "/ips")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Slow down") {
		t.Fatal("expected throttle message after rapid refresh")
	}
}
