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

// Batch J #10 — dashboard yearly converts the RAW monthly sum × 12 (not the
// cent-rounded monthly × 12) with a non-USD dashboard currency.
func TestDashboardYearlyRawConversion(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)

	// $1/yr pricing: raw monthly = 1/12. With EUR rate 0.5 the rounded
	// monthly (0.04) × 12 = 0.48 (old), raw × 12 = 1 × 0.5 = 0.50 (new).
	st := &model.ServerStore{DB: database}
	if _, err := st.Create(t.Context(), &model.Server{Hostname: "yr-01", ServerType: model.TypeKVM, Active: true},
		nil, &model.Pricing{Currency: "USD", Price: 1, Term: model.TermAnnual}); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE settings SET dashboard_currency = 'EUR' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	ratesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base":"USD","rates":{"EUR":0.5}}`))
	}))
	t.Cleanup(ratesSrv.Close)
	srv.rates.BaseURL = ratesSrv.URL

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "0.50/yr") {
		t.Fatalf("yearly should convert the raw sum (€0.50/yr), body snippet missing")
	}
	if strings.Contains(body, "0.48/yr") {
		t.Fatal("yearly must not multiply the cent-rounded monthly")
	}
}

// Batch J #13a — livemon fetches the host batch and filesystems
// CONCURRENTLY: total ≈ the slow part, not the sum.
func TestLiveMonConcurrentFetch(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(100 * time.Millisecond)
		if r.URL.Path == "/api/v1/query_range" {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1719300000,"12"]]}]}}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"mountpoint":"/","device":"/dev/sda1"},"value":[1719300000,"100"]}]}}`)
	}))
	t.Cleanup(promSrv.Close)
	if _, err := database.Exec(
		"UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL); err != nil {
		t.Fatal(err)
	}

	// Host batch: 10 range queries 3-wide = 4 waves ≈ 400ms; filesystems:
	// 2 serial instant queries ≈ 200ms. Sequential would be ≈600ms.
	req := httptest.NewRequest("GET", "/servers/1", nil)
	start := time.Now()
	view := srv.liveMonEntry(req, "a:9100")
	elapsed := time.Since(start)
	if view == nil || view.Unavailable {
		t.Fatalf("expected a live view: %+v", view)
	}
	if elapsed > 550*time.Millisecond {
		t.Fatalf("livemon looks sequential: %v", elapsed)
	}
}

// Batch J #13b — all prom caches are keyed by baseURL: pointing settings at
// a different Prometheus refetches instead of serving the old server's data.
func TestPromCachesKeyedByBaseURL(t *testing.T) {
	_, database, srv := newTestServerFull(t)

	var callsA, callsB int32
	fake := func(nodename, mount string, calls *int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(calls, 1)
			q := r.URL.Query().Get("query")
			switch {
			case r.URL.Path == "/api/v1/query_range":
				fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1719300000,"12"]]}]}}`)
			case strings.Contains(q, "node_filesystem"):
				fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"mountpoint":%q,"device":"/dev/sda1"},"value":[1719300000,"100"]}]}}`, mount)
			case q == "node_uname_info":
				fmt.Fprintf(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"a:9100","nodename":%q},"value":[1719300000,"1"]}]}}`, nodename)
			case q == "up":
				fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"a:9100"},"value":[1719300000,"1"]}]}}`)
			default:
				fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
			}
		}))
	}
	promA := fake("host-a", "/mnt/a", &callsA)
	t.Cleanup(promA.Close)
	promB := fake("host-b", "/mnt/b", &callsB)
	t.Cleanup(promB.Close)

	setURL := func(u string) {
		if _, err := database.Exec(
			"UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", u); err != nil {
			t.Fatal(err)
		}
	}

	setURL(promA.URL)
	req := httptest.NewRequest("GET", "/", nil)
	m := srv.liveMetrics(req)
	if m == nil || m.ByNodename["host-a"] == nil {
		t.Fatalf("expected metrics from A: %+v", m)
	}
	view := srv.liveMonEntry(req, "a:9100")
	if view == nil || len(view.Filesystems) != 1 || view.Filesystems[0].Mount != "/mnt/a" {
		t.Fatalf("expected livemon from A: %+v", view)
	}
	beforeA := atomic.LoadInt32(&callsA)

	// Switch to B: both caches must miss and refetch from B.
	setURL(promB.URL)
	m = srv.liveMetrics(req)
	if m == nil || m.ByNodename["host-b"] == nil {
		t.Fatalf("URL change must refetch from B: %+v", m)
	}
	view = srv.liveMonEntry(req, "a:9100")
	if view == nil || len(view.Filesystems) != 1 || view.Filesystems[0].Mount != "/mnt/b" {
		t.Fatalf("livemon URL change must refetch from B: %+v", view)
	}
	if atomic.LoadInt32(&callsB) == 0 {
		t.Fatal("B was never queried after the URL change")
	}
	if atomic.LoadInt32(&callsA) != beforeA {
		t.Fatal("old server A must not be queried after the switch")
	}
}
