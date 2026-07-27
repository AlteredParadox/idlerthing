package web

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"io"

	"idlerthing/internal/importer"
	"idlerthing/internal/model"
	"idlerthing/internal/prom"
)

// #1 — full round-trip now preserves yabs + non-server labels.
func TestImportRoundTripPreservesYabsAndLabels(t *testing.T) {
	ts, dbA := newTestServer(t)
	ctx := context.Background()

	// Seed A: one server with a yabs run, one shared service with a label.
	createServer(t, authedClient(t, ts), ts, "rt-yabs-01")
	yabsSt := &model.YABSStore{DB: dbA}
	if _, err := yabsSt.Create(ctx, &model.YABS{
		ServerID: 1, RunAt: sqlNs("2026-07-01"), CPU: sqlNs("AMD EPYC"),
		GbSingle: sqlNi(1500), GbMulti: sqlNi(4000),
	}, []model.YABSDiskSpeed{{BlockSize: "4k", ReadMbps: 88, WriteMbps: 90}},
		[]model.YABSNetworkSpeed{{Location: "FRA", Provider: "Hetzner", SendMbps: 900, RecvMbps: 950, LatencyMs: 12.5}}); err != nil {
		t.Fatal(err)
	}

	sharedSt := &model.SharedStore{DB: dbA}
	sharedID, err := sharedSt.Create(ctx, &model.SharedHosting{MainDomain: "rt-label.example.com", Active: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	labels := &model.LabelStore{DB: dbA}
	labelID, _ := labels.FindOrCreate(ctx, "production")
	if err := labels.Assign(ctx, labelID, sharedID, model.ServiceShared); err != nil {
		t.Fatal(err)
	}

	// Export A via the real endpoint.
	client := authedClient(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/export/json", nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	exportJSON, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// Import into fresh B.
	dbB := freshDB(t)
	if _, err := importer.Import(ctx, dbB, bytes.NewReader(exportJSON), false); err != nil {
		t.Fatalf("Import: %v", err)
	}

	// YABS run + speeds + has_yabs survived.
	var hasYabs, gbSingle, diskRows, netRows int
	dbB.QueryRow(`SELECT s.has_yabs, y.gb_single,
		(SELECT COUNT(*) FROM yabs_disk_speed d WHERE d.yabs_id = y.id),
		(SELECT COUNT(*) FROM yabs_network_speed n WHERE n.yabs_id = y.id)
		FROM yabs y JOIN servers s ON s.id = y.server_id`).Scan(&hasYabs, &gbSingle, &diskRows, &netRows)
	if hasYabs != 1 || gbSingle != 1500 || diskRows != 1 || netRows != 1 {
		t.Fatalf("yabs lost: flag=%d single=%d disks=%d net=%d", hasYabs, gbSingle, diskRows, netRows)
	}

	// Label on the SHARED service survived (not just server labels).
	var label string
	dbB.QueryRow(`SELECT l.label FROM labels l
		JOIN labels_assigned a ON a.label_id = l.id
		JOIN shared_hosting h ON h.id = a.service_id AND a.service_type = 2
		WHERE h.main_domain = 'rt-label.example.com'`).Scan(&label)
	if label != "production" {
		t.Fatalf("shared-service label lost: %q", label)
	}
}

// #1c — partial export warns about unresolvable catalog refs.
func TestImportPartialCatalogWarning(t *testing.T) {
	dbB := freshDB(t)
	// Partial doc: a server referencing provider 7, but no "providers" key.
	fixture := `{"format": 1, "servers": [{"server": {"id": 1, "hostname": "partial-01", "server_type": 1,
		"provider_id": 7, "location_id": 3, "os_id": 2, "active": true}}]}`
	sum, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range sum.Warnings {
		if strings.Contains(w, "catalog associations could not be restored") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected partial-export warning, got %v", sum.Warnings)
	}
}

// #2 — NaN and overflow rejected by sizeFormValue.
func TestSizeFormValueNaNOverflow(t *testing.T) {
	for _, raw := range []string{"NaN", "1e30", "-5"} {
		errs := map[string]string{}
		r := formReq(url.Values{"size": {raw}, "size_unit": {"GB"}})
		got := sizeFormValue(r, errs, "size", 1<<30)
		if len(errs) == 0 || got.Valid {
			t.Fatalf("%q should be rejected, got %v (errs %v)", raw, got, errs)
		}
	}
	errs := map[string]string{}
	r := formReq(url.Values{"size": {"2"}, "size_unit": {"GB"}})
	if got := sizeFormValue(r, errs, "size", 1<<30); !got.Valid || got.Int64 != 2048 {
		t.Fatalf("valid value broke: %v %v", got, errs)
	}
}

// #3 — liveMetrics backs off after failure; healthy-empty replaces stale.
func TestLiveMetricsBackoffAndHealthyEmpty(t *testing.T) {
	_, database, s := newTestServerFull(t)

	// Counters are atomic: prom.queryBatch fires 3 queries concurrently.
	var calls int32
	var fail int32 = 1
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		if atomic.LoadInt32(&fail) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		// Healthy but ZERO targets.
		w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer promSrv.Close()
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)

	// Prime STALE data (old timestamp) so the first request must refetch.
	s.prom.baseURL = promSrv.URL
	s.prom.metrics = promMetricsStub
	s.prom.at = time.Now().Add(-time.Minute)

	req, _ := http.NewRequest("GET", "/", nil)
	_ = s.liveMetrics(req)
	first := atomic.LoadInt32(&calls)
	_ = s.liveMetrics(req) // within backoff → no second fetch
	if got := atomic.LoadInt32(&calls); got != first {
		t.Fatalf("expected backoff (no refetch within 30s), got %d extra queries", got-first)
	}
	if first == 0 {
		t.Fatal("first request should have fetched")
	}

	// After the backoff window, a healthy-but-empty fetch replaces stale.
	s.prom.mu.Lock()
	s.prom.lastTry = time.Now().Add(-time.Minute)
	s.prom.mu.Unlock()
	atomic.StoreInt32(&fail, 0)
	m := s.liveMetrics(req)
	if m == promMetricsStub {
		t.Fatal("healthy-empty result should replace stale data")
	}
	if m == nil || len(m.ByInstance) != 0 {
		t.Fatalf("expected stored empty result, got %+v", m)
	}
}

// promMetricsStub is a non-empty stale marker.
var promMetricsStub = &prom.Metrics{
	ByNodename: map[string]*prom.HostMetrics{"stale:9100": {Found: true}},
	ByInstance: map[string]*prom.HostMetrics{"stale:9100": {Found: true}},
}

// #4 — extras endpoints reject bogus service ids.
func TestExtrasBogusServiceID(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	for path, vals := range map[string]url.Values{
		"/ips":           {"service_id": {"999"}, "service_type": {"1"}, "address": {"203.0.113.1"}, "back": {"/ips"}},
		"/labels/assign": {"service_id": {"999"}, "service_type": {"1"}, "new_label": {"x"}, "back": {"/servers"}},
		"/notes":         {"service_id": {"999"}, "service_type": {"1"}, "body": {"x"}, "back": {"/notes"}},
	} {
		resp := postForm(t, client, ts, path, vals)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: expected 404, got %d", path, resp.StatusCode)
		}
	}
	var n int
	database.QueryRow("SELECT COUNT(*) FROM ips").Scan(&n)
	if n != 0 {
		t.Fatal("nothing should be inserted for bogus targets")
	}
	database.QueryRow("SELECT COUNT(*) FROM notes").Scan(&n)
	if n != 0 {
		t.Fatal("nothing should be inserted for bogus targets")
	}
}

// #5 — native importer skips out-of-range pricing terms with a warning.
func TestImportBadPricingTerm(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{"format": 1, "servers": [{"server": {"id": 1, "hostname": "term-01", "server_type": 1, "active": true},
		"pricing": {"currency": "USD", "price": 10, "term": 99}}]}`
	sum, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pricings != 0 || len(sum.Warnings) != 1 {
		t.Fatalf("term 99 should be skipped with warning: %+v", sum)
	}
	var n int
	dbB.QueryRow("SELECT COUNT(*) FROM pricings").Scan(&n)
	if n != 0 {
		t.Fatal("no pricing row expected")
	}
}

// #9 — matchLive first-label fallback + ambiguity guard.
func TestMatchLiveFallback(t *testing.T) {
	m := &prom.Metrics{
		ByNodename: map[string]*prom.HostMetrics{},
		ByInstance: map[string]*prom.HostMetrics{},
	}
	m.ByNodename["web1.example.com"] = &prom.HostMetrics{Found: true, Online: true}
	m.ByInstance["db1:9100"] = &prom.HostMetrics{Found: true}

	// Exact match wins.
	if h := matchLive(m, "web1.example.com"); h == nil || !h.Online {
		t.Fatal("exact nodename match")
	}
	// First-label fallback (inventory short name ↔ FQDN nodename).
	if h := matchLive(m, "web1"); h == nil || !h.Online {
		t.Fatal("first-label fallback should match single candidate")
	}
	// Instance match.
	if h := matchLive(m, "db1"); h == nil {
		t.Fatal("instance first-label fallback")
	}
	if matchLive(m, "nope") != nil {
		t.Fatal("no match expected")
	}

	// Ambiguous: two scraped candidates share the first label → nil.
	m.ByNodename["web1.other.com"] = &prom.HostMetrics{Found: true}
	if h := matchLive(m, "web1"); h != nil {
		t.Fatal("ambiguous first labels must not cross-match")
	}
	// Exact still wins over ambiguity.
	if h := matchLive(m, "web1.example.com"); h == nil || !h.Online {
		t.Fatal("exact match keeps priority over ambiguity")
	}
}

// #10 — filesystem rows survive when all metric cards are no-data.
func TestLiveMonKeepsFilesystemsWhenCardsEmpty(t *testing.T) {
	d := &prom.Detail{} // all series Found=false
	fs := []prom.Filesystem{{Mount: "/", SizeBytes: 100, AvailBytes: 50, UsedPct: 50}}
	v := buildLiveMonView(d, fs)
	if v.Unavailable {
		t.Fatal("should not be unavailable with filesystem data present")
	}
	if len(v.Filesystems) != 1 || len(v.Cards) != 10 {
		t.Fatalf("view wrong: %+v", v)
	}
	// Nothing at all → unavailable.
	if v := buildLiveMonView(d, nil); !v.Unavailable {
		t.Fatal("should be unavailable when truly empty")
	}
}
