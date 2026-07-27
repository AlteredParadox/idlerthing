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
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"idlerthing/internal/importer"
	"idlerthing/internal/model"
)

// #2 — ip-keyed notes and pricing timestamps survive an export→import
// round-trip (export emits {"note": ...,"ip_id": ...}; import remaps ip_id).
func TestExportImportIPNotesAndPricingTimestamps(t *testing.T) {
	ts, dbA := newTestServer(t)
	ctx := context.Background()

	st := &model.ServerStore{DB: dbA}
	srvID, err := st.Create(ctx, &model.Server{
		Hostname: "ipn-01", ServerType: model.TypeKVM, Active: true,
	}, nil, &model.Pricing{Currency: "USD", Price: 12, Term: model.TermAnnual})
	if err != nil {
		t.Fatal(err)
	}
	ipID, err := (&model.IPStore{DB: dbA}).Create(ctx, &model.IP{
		ServiceID: srvID, ServiceType: model.ServiceServer, Address: "198.51.100.9", IsIPv4: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// No app write path for ip-keyed notes yet — insert like the schema allows.
	if _, err := dbA.Exec("INSERT INTO notes (ip_id, body) VALUES (?, ?)", ipID, "note on the ip"); err != nil {
		t.Fatal(err)
	}
	if _, err := dbA.Exec(`UPDATE pricings SET created_at = '2020-01-02 03:04:05',
		updated_at = '2020-02-03 04:05:06' WHERE service_id = ? AND service_type = 1`, srvID); err != nil {
		t.Fatal(err)
	}

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
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %d", resp.StatusCode)
	}
	exportJSON, _ := io.ReadAll(resp.Body)

	dbB := freshDB(t)
	summary, err := importer.Import(ctx, dbB, bytes.NewReader(exportJSON), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Servers != 1 || summary.IPs != 1 || summary.Notes != 1 || summary.Pricings != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	var body string
	var ipRef any
	if err := dbB.QueryRow("SELECT body, ip_id FROM notes").Scan(&body, &ipRef); err != nil {
		t.Fatal(err)
	}
	if body != "note on the ip" || ipRef == nil {
		t.Fatalf("ip note lost: body=%q ip_id=%v", body, ipRef)
	}

	var ca, ua string
	if err := dbB.QueryRow("SELECT created_at, updated_at FROM pricings").Scan(&ca, &ua); err != nil {
		t.Fatal(err)
	}
	if ca != "2020-01-02 03:04:05" || ua != "2020-02-03 04:05:06" {
		t.Fatalf("pricing timestamps not preserved: %q %q", ca, ua)
	}
}

// #4 — absurd yabs numbers collapse to 0/NULL and exports still encode.
func TestYABSAbsurdNumbersExportOK(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "yabs-host")

	payload := `{
	  "cpu": {"model": "X", "cores": 1000000000},
	  "disk": {"fio": [{"bs": "4k", "read": "99999999999999 MB/s", "write": "1 MB/s"}]},
	  "geekbench": {"version": 6, "single": 99999999999, "multi": 5, "url": "https://browser.geekbench.com/v6/cpu/abs1"}
	}`
	now := time.Now().Unix()
	resp, err := http.Post(fmt.Sprintf("%s/api/yabs/1?sig=%s&ts=%d",
		ts.URL, signYABSTest(srv.secret, 1, now), now),
		"application/json", strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest: %d %s", resp.StatusCode, body)
	}

	var cores any
	var readMbps float64
	database.QueryRow("SELECT cpu_cores FROM yabs WHERE id = 1").Scan(&cores)
	database.QueryRow("SELECT read_mbps FROM yabs_disk_speed WHERE yabs_id = 1").Scan(&readMbps)
	if cores != nil {
		t.Fatalf("absurd cores should be NULL, got %v", cores)
	}
	if readMbps != 0 {
		t.Fatalf("absurd read speed should be 0, got %v", readMbps)
	}

	// JSON export must still 200 (no non-finite floats leaked in).
	req, _ := http.NewRequest("GET", ts.URL+"/export/json", nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export after absurd ingest: %d", resp.StatusCode)
	}
}

// #5 — a different payload with an already-seen gb_url is a duplicate, and a
// replayed capability on a third, modified payload is consumed (403).
func TestYABSGbURLDuplicate(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "yabs-host")

	// Pin the clock ONCE: tsOff selects the capability, so two calls meant to
	// reuse the same (server_id, ts) must derive it from the same base. Reading
	// time.Now() per call made that hold only while both landed in the same
	// wall-clock second — straddling a second boundary minted a FRESH
	// capability and the "capability consumed" assertion below failed. Rare
	// normally, reproducible under `go test -race ./...`.
	base := time.Now().Unix()
	post := func(tsOff int64, body string) (int, string) {
		now := base + tsOff
		resp, err := http.Post(fmt.Sprintf("%s/api/yabs/1?sig=%s&ts=%d",
			ts.URL, signYABSTest(srv.secret, 1, now), now),
			"application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	gbA := `"url": "https://browser.geekbench.com/v6/cpu/dup1"`
	code, body := post(0, `{"cpu": {"model": "A", "cores": 2}, "geekbench": {`+gbA+`}}`)
	if code != http.StatusOK {
		t.Fatalf("first: %d %s", code, body)
	}
	// Same gb_url, different body (hash differs): duplicate via the unique index.
	code, body = post(1, `{"cpu": {"model": "B", "cores": 4}, "geekbench": {`+gbA+`}}`)
	if code != http.StatusOK || !strings.Contains(body, "duplicate") {
		t.Fatalf("gb_url duplicate: %d %s", code, body)
	}
	var runs int
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	if runs != 1 {
		t.Fatalf("duplicate gb_url inserted: %d runs", runs)
	}

	// Batch J #3 — the "duplicate" answer consumed the capability in its OWN
	// transaction: a novel payload on the same (stolen) URL is now dead.
	code, body = post(1, `{"cpu": {"model": "C", "cores": 8}, "geekbench": {"url": "https://browser.geekbench.com/v6/cpu/novel1"}}`)
	if code != http.StatusForbidden || !strings.Contains(body, "capability consumed") {
		t.Fatalf("novel payload on consumed URL: %d %s", code, body)
	}
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	if runs != 1 {
		t.Fatalf("consumed URL inserted a run: %d runs", runs)
	}

	// A consumed URL rejects WITHOUT parsing: invalid JSON → 403, not 400.
	code, body = post(1, `{not json`)
	if code != http.StatusForbidden {
		t.Fatalf("consumed URL must reject before parsing: %d %s", code, body)
	}

	// A FRESH capability with invalid JSON → 400 (cap consumed first, then parse).
	code, body = post(2, `{not json`)
	if code != http.StatusBadRequest {
		t.Fatalf("fresh cap with bad JSON: %d %s", code, body)
	}
}

// #10 — importer validation: bad currency/IP/numeric classes are skipped
// with warnings; a lowercase currency normalizes.
func TestImportValidationClasses(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{
		"format": 1,
		"servers": [
			{"server": {"id": 1, "hostname": "bad-cur", "server_type": 1, "active": true},
				"pricing": {"currency": "US$", "price": 5, "term": 1}},
			{"server": {"id": 2, "hostname": "lower-cur", "server_type": 1, "active": true},
				"pricing": {"currency": "eur", "price": 5, "term": 1}},
			{"server": {"id": 3, "hostname": "huge-ram", "server_type": 1, "active": true, "ram_as_mb": 1e300}}
		],
		"ips": [
			{"ip": {"service_id": 2, "service_type": 1, "address": "999.1.2.3", "is_ipv4": true}},
			{"ip": {"service_id": 2, "service_type": 1, "address": "203.0.113.44", "is_ipv4": true}}
		]
	}`
	summary, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Servers != 3 || summary.Pricings != 1 || summary.IPs != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	if len(summary.Warnings) < 2 {
		t.Fatalf("expected ≥2 warnings (currency + ip), got %v", summary.Warnings)
	}

	var badCur int
	dbB.QueryRow("SELECT COUNT(*) FROM pricings WHERE currency = 'US$'").Scan(&badCur)
	if badCur != 0 {
		t.Fatal("invalid currency must be skipped")
	}
	var normCur string
	dbB.QueryRow(`SELECT p.currency FROM pricings p
		JOIN servers s ON s.id = p.service_id AND p.service_type = 1
		WHERE s.hostname = 'lower-cur'`).Scan(&normCur)
	if normCur != "EUR" {
		t.Fatalf("lowercase currency should normalize to EUR, got %q", normCur)
	}
	var ram any
	dbB.QueryRow("SELECT ram_as_mb FROM servers WHERE hostname = 'huge-ram'").Scan(&ram)
	if ram != nil {
		t.Fatalf("out-of-range ram_as_mb should be NULL, got %v", ram)
	}
	var badIP int
	dbB.QueryRow("SELECT COUNT(*) FROM ips WHERE address = '999.1.2.3'").Scan(&badIP)
	if badIP != 0 {
		t.Fatal("invalid IP must be skipped")
	}
}

// #11 — $1/yr renders as $1.00/yr (raw monthly × 12, not cent-rounded).
func TestPriceYrDisplay(t *testing.T) {
	got := priceYrDisplay(&model.Pricing{Currency: "USD", Price: 1, Term: model.TermAnnual}, nil)
	if got != "$1.00/yr" {
		t.Fatalf("$1/yr: %q", got)
	}
	if got := priceYrDisplay(nil, nil); got != "—" {
		t.Fatalf("nil pricing: %q", got)
	}
	got = priceYrDisplay(&model.Pricing{Currency: "USD", Price: 50, Term: model.TermOneTime}, nil)
	if got != "—" {
		t.Fatalf("one-time: %q", got)
	}
}

// #12 — list endpoints slice BEFORE flattening: per=1 returns one
// flattened row with the true total.
func TestAPIListSliceBeforeFlatten(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	client := authedClient(t, ts)
	for _, h := range []string{"pg-01", "pg-02"} {
		resp := postForm(t, client, ts, "/servers", url.Values{
			"hostname": {h}, "server_type": {"1"}, "active": {"on"},
			"price": {"10"}, "currency": {"USD"}, "term": {"1"},
		})
		resp.Body.Close()
	}

	resp, body := apiGet(t, ts, "/api/servers?per=1&page=2", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("servers page 2: %d", resp.StatusCode)
	}
	if body["total"].(float64) != 2 || body["page"].(float64) != 2 {
		t.Fatalf("envelope: %v", body)
	}
	data := body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 row, got %d", len(data))
	}
	row := data[0].(map[string]any)
	// ServerListItem embeds Server — flatten nests it under "server".
	srv, _ := row["server"].(map[string]any)
	if srv["hostname"] != "pg-02" {
		t.Fatalf("page 2 row: %v", row)
	}

	resp, body = apiGet(t, ts, "/api/pricings?per=1", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pricings: %d", resp.StatusCode)
	}
	if body["total"].(float64) != 2 {
		t.Fatalf("pricings total: %v", body["total"])
	}
	data = body["data"].([]any)
	if len(data) != 1 {
		t.Fatalf("expected 1 pricing row, got %d", len(data))
	}
	prow := data[0].(map[string]any)
	if prow["service_name"] == nil || prow["created_at"] == nil || prow["currency"] != "USD" {
		t.Fatalf("flattened pricing row incomplete: %v", prow)
	}
}

// #13 — full-width formula starters are quoted too.
func TestCSVCellFullWidth(t *testing.T) {
	for _, v := range []string{"＝1+1", "＋1", "－1", "＠sum"} {
		if got := csvCell(v); !strings.HasPrefix(got, "'") {
			t.Fatalf("full-width formula %q should be quoted: %q", v, got)
		}
	}
	if got := csvCell("日本語"); got != "日本語" {
		t.Fatalf("ordinary unicode untouched: %q", got)
	}
}

// #14 — protocol-relative and otherwise unsafe Referers fall back.
func TestSafeRedirectTarget(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://app//evil.com", "/fb"},
		{"//evil.com", "/fb"},
		{"https://evil.com/x", "/x"}, // same-origin path extraction is fine
		{"/servers?status=all", "/servers?status=all"},
		{"/servers\\evil", "/fb"},
		{"", "/fb"},
		{"https://app", "/fb"}, // empty path
	}
	for _, c := range cases {
		if got := safeRedirectTarget(c.in, "/fb"); got != c.want {
			t.Errorf("safeRedirectTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	// Handler level: the theme toggle redirects to the fallback, not //evil.
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	csrf := sessionCSRF(t, client, ts)
	form := url.Values{"csrf_token": {csrf}}
	req, _ := http.NewRequest("POST", ts.URL+"/prefs/theme", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Referer", "https://app//evil.com")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("expected fallback redirect /, got %q", loc)
	}
}

// #15 — a successful htmx catalog mutation invalidates the dashboard cache.
func TestCatalogHtmxRenameBumpsGeneration(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, "/catalogs/providers", url.Values{"name": {"Hetzner"}})
	resp.Body.Close()

	genBefore := func() uint64 {
		srv.dash.mu.Lock()
		defer srv.dash.mu.Unlock()
		return srv.dash.gen
	}()
	// Drain the dashboard-touch from the create above (gen may already be 1).

	csrf := sessionCSRF(t, client, ts)
	form := url.Values{"csrf_token": {csrf}, "name": {"Hetzner-alt"}}
	req, _ := http.NewRequest("POST", ts.URL+"/catalogs/providers/1/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("htmx update: %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Hetzner-alt") {
		t.Fatal("partial should show the new name")
	}

	srv.dash.mu.Lock()
	genAfter := srv.dash.gen
	srv.dash.mu.Unlock()
	if genAfter <= genBefore {
		t.Fatalf("htmx rename must bump dashboard generation: %d → %d", genBefore, genAfter)
	}
}
