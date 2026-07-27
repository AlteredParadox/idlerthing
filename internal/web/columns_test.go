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
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// createFullServer posts a server with all optional-column fields set.
func createFullServer(t *testing.T, client *http.Client, ts *httptest.Server, hostname string, extra url.Values) {
	t.Helper()
	vals := url.Values{
		"hostname":             {hostname},
		"server_type":          {"1"},
		"active":               {"on"},
		"ram_as_mb":            {"4096"},
		"ram_as_mb_unit":       {"MB"},
		"cpu":                  {"4"},
		"cpu_model":            {"AMD EPYC 7543P"},
		"link_speed":           {"1000"},
		"owned_since":          {"2024-03-15"},
		"price":                {"10"},
		"currency":             {"USD"},
		"term":                 {"1"},
		"bandwidth_as_mb":      {"20"},
		"bandwidth_as_mb_unit": {"TB"},
		"network_type":         {"IPv4+IPv6"},
	}
	for k, v := range extra {
		vals[k] = v
	}
	resp := postForm(t, client, ts, "/servers", vals)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create %s: expected 303, got %d", hostname, resp.StatusCode)
	}
}

// applyCols posts the chooser form with the given visible column keys.
func applyCols(t *testing.T, client *http.Client, ts *httptest.Server, visible []string) {
	t.Helper()
	vals := url.Values{}
	for _, k := range visible {
		vals.Add("col_"+k, "on")
	}
	resp := postForm(t, client, ts, "/prefs/servers-cols", vals)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("apply cols: expected 303, got %d", resp.StatusCode)
	}
}

// allColKeys returns every choosable column key.
func allColKeys() []string {
	var out []string
	for _, c := range serverColumns {
		out = append(out, c.Key)
	}
	return out
}

func TestColumnsDefaultHiddenAndChooser(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createFullServer(t, client, ts, "cols-01", nil)

	get := func() string {
		resp, err := client.Get(ts.URL + "/servers")
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		return body
	}

	// Default: new columns absent, default ones present.
	body := get()
	if !strings.Contains(body, "Columns 12/17") {
		t.Fatal("chooser should read 12/17 by default")
	}
	if strings.Contains(body, ">CPU Model<") || strings.Contains(body, ">Link Speed<") ||
		strings.Contains(body, ">Price/YR (USD)<") || strings.Contains(body, ">Since<") ||
		strings.Contains(body, ">Uptime<") {
		t.Fatal("new columns must be hidden by default")
	}
	if strings.Contains(body, "AMD EPYC 7543P") {
		// cpu_model appears only as cell-sub in the CPU cell — allowed; header must be absent
		if strings.Contains(body, ">CPU Model<") {
			t.Fatal("cpu_model column must be hidden by default")
		}
	}

	// Enable cpu_model + link_speed via Apply (visible = defaults + these two).
	visible := []string{"hostname", "type", "os", "cpu", "cpu_model", "ram", "disk", "bw",
		"link_speed", "net", "location", "provider", "price", "due"}
	applyCols(t, client, ts, visible)
	body = get()
	if !strings.Contains(body, ">CPU Model<") {
		t.Fatal("cpu_model column should appear after enabling")
	}
	if !strings.Contains(body, ">Link Speed<") || !strings.Contains(body, "1 Gbps") {
		t.Fatal("link_speed column should appear with 1 Gbps")
	}
	if !strings.Contains(body, "Columns 14/17") {
		t.Fatal("chooser count should update to 14/17")
	}

	// Persisted across requests (fresh GET already proved it; also hide os now).
	visible = []string{"hostname", "type", "cpu", "cpu_model", "ram", "disk", "bw",
		"link_speed", "net", "location", "provider", "price", "due"}
	applyCols(t, client, ts, visible)
	body = get()
	if strings.Contains(body, ">OS <span class=\"arrow\"") {
		t.Fatal("os column should disappear when hidden")
	}

	// Unhide everything sticks.
	applyCols(t, client, ts, allColKeys())
	body = get()
	if !strings.Contains(body, "Columns 17/17") {
		t.Fatal("unhide-all should stick (17/17)")
	}
	for _, h := range []string{">Since<", ">Uptime<", ">Price/YR (USD)<"} {
		if !strings.Contains(body, h) {
			t.Fatalf("all columns enabled: missing %s", h)
		}
	}
	if !strings.Contains(body, "2024-03-15") {
		t.Fatal("since column should show owned_since")
	}
}

func TestPriceYrColumn(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	// $10/mo → $120.00/yr; one-time → —.
	createFullServer(t, client, ts, "monthly-01", url.Values{
		"next_due_date": {"2030-05-05"},
	})
	createFullServer(t, client, ts, "onetime-01", url.Values{
		"price": {"50"}, "term": {"7"},
	})
	applyCols(t, client, ts, allColKeys())

	resp, err := client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "$120.00/yr") {
		t.Fatal("expected $120.00/yr for $10/mo")
	}
	// One-time row shows — (appears at least once in the price_yr column).
	if !strings.Contains(body, "$50.00 once") {
		t.Fatal("one-time pricing should render in Price column")
	}

	// Regression: price_yr cell must sit between Price and Due in the row,
	// matching the header order (bug: body had due before price_yr).
	rowStart := strings.Index(body, "monthly-01")
	if rowStart < 0 {
		t.Fatal("monthly-01 row missing")
	}
	row := body[rowStart : rowStart+3000]
	iy := strings.Index(row, "$120.00/yr")
	id := strings.Index(row, "2030-05-05")
	if iy < 0 || id < 0 || iy > id {
		t.Fatalf("price_yr cell should precede due cell in row (price_yr@%d, due@%d)", iy, id)
	}
}

func TestUptimeColumn(t *testing.T) {
	ts, _ := promTestServer(t)
	client := seedLiveServers(t, ts)
	applyCols(t, client, ts, allColKeys())

	resp, err := client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	// live-low → 99.5%, live-mid → 97.2% from the fake.
	if !strings.Contains(body, "99.5%") || !strings.Contains(body, "97.2%") {
		t.Fatal("expected batched uptime percentages")
	}
}

func TestLinkSpeedMeter(t *testing.T) {
	ts, database := newTestServer(t)
	promSrv := httptest.NewServer(http.HandlerFunc(fakePromHandler))
	defer promSrv.Close()
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)
	client := authedClient(t, ts)

	// Matched host WITH link speed.
	createFullServer(t, client, ts, "live-low", nil)
	// Matched host WITHOUT link speed.
	createFullServer(t, client, ts, "live-mid", url.Values{"link_speed": {""}})
	applyCols(t, client, ts, allColKeys())
	database.Exec("UPDATE servers SET link_speed = NULL WHERE hostname = 'live-mid'")

	resp, err := client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()

	// live-low: rx 1.5e6 + tx 4e5 Bps = 15.2 Mbps of 1000 Mbps ≈ 2% util → meter present.
	if !strings.Contains(body, "1 Gbps") {
		t.Fatal("link speed value should render")
	}
	// Exactly one link-speed value: live-mid (no link_speed) shows — instead.
	if strings.Count(body, "1 Gbps") != 1 {
		t.Fatal("expected exactly one 1 Gbps cell")
	}
	row := rowHTML(t, body, "live-mid")
	if !strings.Contains(row, "<div>—</div>") {
		t.Fatal("live-mid link speed cell should show —")
	}
	if strings.Contains(row, `pct-inline">2%<`) {
		t.Fatal("live-mid should have no utilization pct without link_speed")
	}
}

// rowHTML extracts the <tr> containing marker from a table body.
func rowHTML(t *testing.T, body, marker string) string {
	t.Helper()
	i := strings.Index(body, marker)
	if i < 0 {
		t.Fatalf("marker %q not found", marker)
	}
	start := strings.LastIndex(body[:i], "<tr")
	end := strings.Index(body[i:], "</tr>")
	if start < 0 || end < 0 {
		t.Fatalf("row for %q not found", marker)
	}
	return body[start : i+end]
}

func TestServerColsPrefCSRF(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	resp, err := client.PostForm(ts.URL+"/prefs/servers-cols", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF, got %d", resp.StatusCode)
	}
}
