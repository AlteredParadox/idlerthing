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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idlerthing/internal/model"
)

// #1b — single-query counts() matches per-table counts.
func TestCountsMatchPerTable(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "c1")
	createServer(t, client, ts, "c2")

	srv := &Server{db: database}
	req, _ := http.NewRequest("GET", "/", nil)
	c := srv.counts(req)

	var servers int
	database.QueryRow("SELECT COUNT(*) FROM servers").Scan(&servers)
	if c.Servers != servers || c.Servers != 2 {
		t.Fatalf("counts mismatch: %+v (db says %d)", c, servers)
	}
	var users int
	database.QueryRow("SELECT COUNT(*) FROM users").Scan(&users)
	if users != 1 {
		t.Fatal("seed user missing")
	}
	// Every field is independently checkable — spot-check a few more.
	for table, got := range map[string]int{
		"dns": c.DNS, "ips": c.IPs, "labels": c.Labels, "notes": c.Notes, "yabs": c.YABS,
	} {
		var want int
		database.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&want)
		if got != want {
			t.Fatalf("%s: counts=%d db=%d", table, got, want)
		}
	}
}

// #1a — htmx partial renders skip the sidebar counts (and still work).
func TestHtmxPartialSkipsCounts(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "hx-01")

	req, _ := http.NewRequest("GET", ts.URL+"/servers?status=all", nil)
	req.Header.Set("HX-Request", "true")
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "hx-01") {
		t.Fatal("htmx partial should render rows")
	}
	// Partials render the table only — no layout/sidebar markup.
	if strings.Contains(body, "sidebar") {
		t.Fatal("partial must not include the sidebar")
	}
}

// #2 — monthly and yearly cards derive from the same sum.
func TestCostPairUSDFor(t *testing.T) {
	_, database, s := newTestServerFull(t)

	// $10/mo fixture direct in the DB.
	if _, err := database.Exec("INSERT INTO servers (hostname) VALUES ('pair-01')"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("INSERT INTO pricings (service_id, service_type, currency, price, term) VALUES (1, 1, 'USD', 10, 1)"); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("GET", "/", nil)
	monthly, yearly := s.costPairUSDFor(req, model.ServiceServer)
	if monthly != "$10.00/mo" || yearly != "$120.00/yr" {
		t.Fatalf("pair: %q %q", monthly, yearly)
	}
}

// #5 — failing prometheus is negative-cached; recovery works after TTL.
func TestUptime30dNegativeCache(t *testing.T) {
	_, database, s := newTestServerFull(t)

	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"a:9100"},"value":[1719300000,"99.5"]}]}}`)
	}))
	defer promSrv.Close()
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)

	req, _ := http.NewRequest("GET", "/", nil)

	if got := s.uptime30d(req, "a:9100"); got != "—" {
		t.Fatalf("expected — on failure, got %q", got)
	}
	before := calls.Load()
	if got := s.uptime30d(req, "a:9100"); got != "—" {
		t.Fatalf("expected cached —, got %q", got)
	}
	if calls.Load() != before {
		t.Fatal("second failure should be served from the negative cache")
	}

	// After the failure TTL, it retries — and succeeds once prom recovers.
	fail.Store(false)
	s.uptime.mu.Lock()
	s.uptime.at["a:9100"] = time.Now().Add(-time.Minute)
	s.uptime.mu.Unlock()
	if got := s.uptime30d(req, "a:9100"); got != "99.50%" {
		t.Fatalf("expected recovery to 99.50%%, got %q", got)
	}
}

// #5 — liveMonEntry negative-caches the unavailable view.
func TestLiveMonNegativeCache(t *testing.T) {
	_, database, s := newTestServerFull(t)

	var rangeCalls atomic.Int32
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "query_range") {
			rangeCalls.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if strings.Contains(r.URL.Query().Get("query"), "node_filesystem") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		liveMonFakeProm(w, r)
	}))
	defer promSrv.Close()
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)

	req, _ := http.NewRequest("GET", "/", nil)

	v1 := s.liveMonEntry(req, "a:9100")
	if v1 == nil || !v1.Unavailable {
		t.Fatalf("expected unavailable view, got %+v", v1)
	}
	before := rangeCalls.Load()
	v2 := s.liveMonEntry(req, "a:9100")
	if v2 == nil || !v2.Unavailable {
		t.Fatalf("expected cached unavailable view, got %+v", v2)
	}
	if rangeCalls.Load() != before {
		t.Fatal("unavailable view should be negative-cached")
	}
}

// #2 — htmx table swaps render rows correctly (post skip-counts change).
func TestHtmxSwapRendersRows(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "swap-01")
	createServer(t, client, ts, "swap-02")

	get := func(path string) string {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("HX-Request", "true")
		for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
			req.AddCookie(c)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", path, resp.StatusCode)
		}
		return body
	}

	body := get("/servers?status=all")
	if !strings.Contains(body, "swap-01") || !strings.Contains(body, "swap-02") {
		t.Fatal("swap should render all rows")
	}
	body = get("/servers?status=all&q=swap-01")
	if !strings.Contains(body, "swap-01") || strings.Contains(body, "swap-02") {
		t.Fatal("search swap should filter rows")
	}
	// Sort still works through the partial.
	body = get("/servers?status=all&sort=hostname&dir=desc")
	if strings.Index(body, "swap-02") > strings.Index(body, "swap-01") {
		t.Fatal("sort should apply in the partial")
	}
	// No layout in partials.
	if strings.Contains(body, "sidebar") || strings.Contains(body, "stat-card") {
		t.Fatal("partial must not contain layout/stat cards")
	}
}

// #6 — settings changes reflect immediately (per-request memo, no staleness).
func TestMemoSettingsFreshness(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	// Change theme; next request must see it (memo is per-request).
	database.Exec("UPDATE settings SET theme = 'light' WHERE id = 1")
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `data-theme="light"`) {
		t.Fatal("theme change must reflect on the next request")
	}
	database.Exec("UPDATE settings SET theme = 'dark' WHERE id = 1")
	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `data-theme="dark"`) {
		t.Fatal("revert must reflect immediately")
	}
}
