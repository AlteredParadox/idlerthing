package web

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"idlerthing/internal/prom"
)

// fakePromHandler returns canned metrics for "live-low" (up, green),
// "live-mid" (up, yellow), "live-high" (up, red).
func fakePromHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	vec := func(samples ...string) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Join(samples, ",")+`]}}`)
	}
	s := func(metric, value string) string {
		return fmt.Sprintf(`{"metric":{%s},"value":[1719300000,%q]}`, metric, value)
	}
	switch {
	case q == "up" && strings.Contains(q, "avg_over_time"):
		vec(s(`"instance":"a:9100"`, "99.5"))
	case strings.HasPrefix(q, "avg_over_time"):
		vec(s(`"instance":"a:9100"`, "99.5"), s(`"instance":"b:9100"`, "97.2"), s(`"instance":"c:9100"`, "88.8"))
	case q == `up`:
		vec(s(`"instance":"a:9100"`, "1"), s(`"instance":"b:9100"`, "1"), s(`"instance":"c:9100"`, "0"))
	case q == `node_uname_info`:
		vec(
			s(`"instance":"a:9100","nodename":"live-low"`, "1"),
			s(`"instance":"b:9100","nodename":"live-mid"`, "1"),
			s(`"instance":"c:9100","nodename":"live-high"`, "1"),
		)
	case strings.Contains(q, "node_cpu_seconds_total"):
		vec(s(`"instance":"a:9100"`, "12"), s(`"instance":"b:9100"`, "65"), s(`"instance":"c:9100"`, "91"))
	case strings.Contains(q, "node_memory_MemAvailable"):
		vec(s(`"instance":"a:9100"`, "25"), s(`"instance":"b:9100"`, "70"), s(`"instance":"c:9100"`, "93"))
	case strings.Contains(q, "node_filesystem_avail"):
		vec(s(`"instance":"a:9100"`, "40"), s(`"instance":"b:9100"`, "80"), s(`"instance":"c:9100"`, "88"))
	case strings.Contains(q, "node_network_receive"):
		vec(s(`"instance":"a:9100"`, "1500000"), s(`"instance":"b:9100"`, "500000"))
	case strings.Contains(q, "node_network_transmit"):
		vec(s(`"instance":"a:9100"`, "400000"), s(`"instance":"b:9100"`, "125000"))
	default:
		vec()
	}
}

// promTestServer spins the app with prometheus enabled against the fake.
func promTestServer(t *testing.T) (*httptest.Server, *httptest.Server) {
	t.Helper()
	ts, database := newTestServer(t)
	promSrv := httptest.NewServer(http.HandlerFunc(fakePromHandler))
	t.Cleanup(promSrv.Close)
	if _, err := database.Exec(
		"UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1",
		promSrv.URL); err != nil {
		t.Fatal(err)
	}
	return ts, promSrv
}

func seedLiveServers(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	client := authedClient(t, ts)
	createServer(t, client, ts, "live-low")
	createServer(t, client, ts, "live-mid")
	createServer(t, client, ts, "live-high")
	createServer(t, client, ts, "not-monitored")
	// Drain flash.
	resp, err := client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return client
}

func TestMatchLiveHostnameForms(t *testing.T) {
	h := &prom.HostMetrics{Instance: "de-sn-csv01.example.com", Found: true, Online: true}
	m := &prom.Metrics{
		ByNodename: map[string]*prom.HostMetrics{"de-sn-csv01": h},
		ByInstance: map[string]*prom.HostMetrics{"de-sn-csv01.example.com": h},
	}
	for _, hostname := range []string{
		"de-sn-csv01",             // short, matches nodename
		"de-sn-csv01.example.com", // FQDN, matches instance or nodename short form
		"DE-SN-CSV01.EXAMPLE.COM", // case-insensitive
		" de-sn-csv01 ",           // whitespace trimmed
	} {
		if got := matchLive(m, hostname); got != h {
			t.Errorf("matchLive(%q) = %v, want the host", hostname, got)
		}
	}
	// Instance labels carrying a scrape port still match the bare FQDN.
	h2 := &prom.HostMetrics{Instance: "web1.example.com:9100", Found: true, Online: true}
	m2 := &prom.Metrics{
		ByNodename: map[string]*prom.HostMetrics{},
		ByInstance: map[string]*prom.HostMetrics{"web1.example.com:9100": h2},
	}
	if got := matchLive(m2, "web1.example.com"); got != h2 {
		t.Errorf("matchLive port-stripped instance = %v, want the host", got)
	}
	if got := matchLive(m, "de-sn-csv02"); got != nil {
		t.Errorf("matchLive unknown host = %v, want nil", got)
	}
	if got := matchLive(nil, "de-sn-csv01"); got != nil {
		t.Errorf("matchLive nil metrics = %v, want nil", got)
	}
}

func TestServersListLiveMeters(t *testing.T) {
	ts, _ := promTestServer(t)
	client := seedLiveServers(t, ts)

	resp, err := client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()

	// Matched servers get meters + throughput + live dots.
	if !strings.Contains(body, "meter-fill") {
		t.Fatal("expected meter markup for matched servers")
	}
	if !strings.Contains(body, "c-err") || !strings.Contains(body, "c-warn") || !strings.Contains(body, "c-ok") {
		t.Fatal("expected all three meter colors")
	}
	if !strings.Contains(body, "91%") || !strings.Contains(body, "65%") || !strings.Contains(body, "12%") {
		t.Fatal("expected pct labels")
	}
	if !strings.Contains(body, "↓ 12 Mbps") || !strings.Contains(body, "↑ 3.2 Mbps") {
		t.Fatal("expected live throughput")
	}
	if !strings.Contains(body, `title="up"`) || !strings.Contains(body, `title="down"`) {
		t.Fatal("expected live status dots")
	}

	// Unmatched server renders as today: no meter near its row. Cheap check:
	// count meter fills = 3 servers × 3 meters × 2 variants (block + compact
	// inline) = 18.
	if n := strings.Count(body, "meter-fill"); n != 18 {
		t.Fatalf("expected 18 meter fills (3 servers × 3 meters × 2 variants), got %d", n)
	}
}

func TestServersListPromDisabledNoMeters(t *testing.T) {
	ts, database := newTestServer(t)
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = 'http://127.0.0.1:1/down' WHERE id = 1")
	client := seedLiveServers(t, ts)

	// Prometheus down → silent degrade, still 200, no meters.
	resp, err := client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with prom down, got %d", resp.StatusCode)
	}
	if strings.Contains(body, "meter-fill") {
		t.Fatal("no meters expected when prometheus is down")
	}

	// Disabled → no meters.
	database.Exec("UPDATE settings SET prometheus_enabled = 0 WHERE id = 1")
	resp, err = client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "meter-fill") {
		t.Fatal("no meters expected when prometheus is disabled")
	}
}

func TestServerDetailLiveCard(t *testing.T) {
	ts, _ := promTestServer(t)
	client := seedLiveServers(t, ts)

	// Server 1 = live-low.
	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	for _, want := range []string{"Live", "badge-ok", "12%", "99.50%", "↓ 12 Mbps"} {
		if !strings.Contains(body, want) {
			t.Fatalf("live card should contain %q", want)
		}
	}

	// Unmatched server: no live card.
	resp, err = client.Get(ts.URL + "/servers/4")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "live-card") {
		t.Fatal("unmatched server should not get a live card")
	}
}

// TestCompactCSSKeepsLiveCardMeters guards a regression: the compact-mode
// rule hiding block meters must be scoped to list tables, or the detail
// Live card loses its CPU/RAM/Disk meters whenever compact mode is on.
func TestCompactCSSKeepsLiveCardMeters(t *testing.T) {
	css, err := assetsFS.ReadFile("assets/static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(css), ".compact .meter { display: none; }") {
		t.Fatal("compact meter hiding must be scoped to .tbl, or the Live card meters disappear")
	}
	if !strings.Contains(string(css), ".compact .tbl .meter { display: none; }") {
		t.Fatal("expected compact table meter rule '.compact .tbl .meter { display: none; }'")
	}
}

func TestPrometheusSettingsAndTestConnection(t *testing.T) {
	ts, database := newTestServer(t)
	promSrv := httptest.NewServer(http.HandlerFunc(fakePromHandler))
	defer promSrv.Close()
	client := authedClient(t, ts)

	base := url.Values{
		"default_currency": {"USD"}, "dashboard_currency": {"USD"},
		"due_soon_amount": {"14"}, "recently_added_amount": {"5"}, "theme": {"dark"},
	}

	// Enable with valid URL → persists.
	vals := url.Values{}
	for k, v := range base {
		vals[k] = v
	}
	vals.Set("prometheus_enabled", "on")
	vals.Set("prometheus_url", promSrv.URL)
	resp := postForm(t, client, ts, "/settings", vals)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	var en int
	var u string
	database.QueryRow("SELECT prometheus_enabled, prometheus_url FROM settings WHERE id = 1").Scan(&en, &u)
	if en != 1 || u != promSrv.URL {
		t.Fatalf("prom settings not persisted: %d %q", en, u)
	}

	// Enabled with garbage URL → validation error.
	vals.Set("prometheus_url", "ftp://nope")
	resp = postForm(t, client, ts, "/settings", vals)
	if !strings.Contains(readBody(t, resp), "http(s) URL") {
		t.Fatal("expected URL validation error")
	}
	resp.Body.Close()

	// Test connection: success flash.
	resp = postForm(t, client, ts, "/settings/prometheus/test", url.Values{})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || loc != "/settings" {
		t.Fatalf("expected redirect to /settings, got %d %q", resp.StatusCode, loc)
	}
	resp, err := client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "targets up") {
		t.Fatal("expected success flash with target counts")
	}

	// Test connection: failure flash with error.
	database.Exec("UPDATE settings SET prometheus_url = 'http://127.0.0.1:1/down' WHERE id = 1")
	resp = postForm(t, client, ts, "/settings/prometheus/test", url.Values{})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Connection failed") {
		t.Fatal("expected failure flash")
	}
}
