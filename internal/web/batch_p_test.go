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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"idlerthing/internal/importer"
	"idlerthing/internal/model"
	"idlerthing/internal/prom"
)

// Batch P #3b — yabs runs export oldest-first, so a restore keeps id order
// and "latest run" displays the newest run.
func TestYABSExportImportOrderPreserved(t *testing.T) {
	ts, dbA := newTestServer(t)
	ctx := context.Background()
	st := &model.ServerStore{DB: dbA}
	srvID, err := st.Create(ctx, &model.Server{Hostname: "ord-01", ServerType: model.TypeKVM, Active: true}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	yabsSt := &model.YABSStore{DB: dbA}
	for _, runAt := range []string{"2026-01-01", "2026-06-01"} {
		if _, err := yabsSt.Create(ctx, &model.YABS{
			ServerID: srvID, RunAt: sqlNs(runAt), CPU: sqlNs("AMD"),
		}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	doc := exportJSONDoc(t, ts, authedClient(t, ts), "/export/json")
	runs := doc["yabs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("runs: %d", len(runs))
	}
	if runs[0].(map[string]any)["run_at"] != "2026-01-01" {
		t.Fatalf("export should be oldest-first: %v", runs[0])
	}

	raw, _ := json.Marshal(doc)
	dbB := freshDB(t)
	if _, err := importer.Import(ctx, dbB, strings.NewReader(string(raw)), false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	// Newest first by id (the list query) must be the newer run.
	var newest string
	if err := dbB.QueryRow("SELECT run_at FROM yabs ORDER BY id DESC LIMIT 1").Scan(&newest); err != nil {
		t.Fatal(err)
	}
	if newest != "2026-06-01" {
		t.Fatalf("restored id order wrong: newest=%q", newest)
	}
}

// Batch P #3c — a backup with duplicate payload_hash restores, one run kept.
func TestImportYABSDupHashKeptOnce(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{"format": 1,
		"servers": [{"server": {"id": 1, "hostname": "dup-yabs", "server_type": 1, "active": true}}],
		"yabs": [
			{"server_id": 1, "cpu": "AMD", "payload_hash": "abc"},
			{"server_id": 1, "cpu": "AMD", "payload_hash": "abc"}
		]}`
	summary, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.YABS != 1 {
		t.Fatalf("expected 1 kept run, got %+v", summary)
	}
	var n int
	dbB.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&n)
	if n != 1 {
		t.Fatalf("duplicate payload_hash must not abort or duplicate: %d runs", n)
	}
}

// Batch P #3d — case-distinct catalog names merge with a warning.
func TestImportCatalogCaseMergeWarning(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{"format": 1,
		"providers": [{"id": 1, "name": "OVH"}, {"id": 2, "name": "ovh"}]}`
	summary, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Providers != 1 {
		t.Fatalf("expected 1 created provider, got %+v", summary)
	}
	found := false
	for _, w := range summary.Warnings {
		if strings.Contains(w, `"ovh" merged into existing "OVH"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the merge warning, got %v", summary.Warnings)
	}
	var n int
	dbB.QueryRow("SELECT COUNT(*) FROM providers").Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 provider row, got %d", n)
	}
}

// Batch P #7a — native IP import stores the canonical address and derives
// is_ipv4 from it.
func TestImportIPCanonicalForm(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{"format": 1,
		"servers": [{"server": {"id": 1, "hostname": "canon-01", "server_type": 1, "active": true}}],
		"ips": [{"ip": {"service_id": 1, "service_type": 1, "address": "2001:0DB8:0000::0001", "is_ipv4": true}}]}`
	if _, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false); err != nil {
		t.Fatalf("Import: %v", err)
	}
	var addr string
	var v4 int
	if err := dbB.QueryRow("SELECT address, is_ipv4 FROM ips").Scan(&addr, &v4); err != nil {
		t.Fatal(err)
	}
	if addr != "2001:db8::1" || v4 != 0 {
		t.Fatalf("expected canonical 2001:db8::1 v6, got %q v4=%d", addr, v4)
	}
}

// Batch P #7c — the detail Live card marks status unknown when `up` failed.
func TestBuildLiveOnlineUnknown(t *testing.T) {
	_, _, srv := newTestServerFull(t)
	req := httptest.NewRequest("GET", "/servers/1", nil)
	h := &prom.HostMetrics{Instance: "a:9100", Found: true, Online: false, OnlineKnown: false, CPUPct: 12}
	m := &prom.Metrics{
		ByNodename: map[string]*prom.HostMetrics{"host-a": h},
		ByInstance: map[string]*prom.HostMetrics{"a:9100": h},
	}
	v := srv.buildLive(req, m, "host-a")
	if v == nil {
		t.Fatal("expected a live view")
	}
	if v.OnlineKnown || v.Online {
		t.Fatalf("status must be unknown, got %+v", v)
	}
	if v.CPU == nil {
		t.Fatal("cpu meter should still render")
	}
}

// Batch P #7d — authenticated HTML and the API are no-store.
func TestCacheControlNoStore(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("dashboard should be no-store, got %q", cc)
	}
	resp, err = client.Get(ts.URL + "/api/servers")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("API should be no-store, got %q", cc)
	}
}

// Batch P #7e — /healthz pings the DB (503 when it's broken).
func TestHealthzDBPing(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	resp, err := newClient(t).Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy: expected 200, got %d", resp.StatusCode)
	}
	database.Close()
	resp, err = newClient(t).Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("broken db: expected 503, got %d", resp.StatusCode)
	}
}

// Batch P #7i — a malformed form is a 400 even with a valid header token.
func TestCSRFMalformedForm400(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	var csrfToken string
	database.QueryRow("SELECT csrf_token FROM sessions LIMIT 1").Scan(&csrfToken)
	if csrfToken == "" {
		t.Fatal("expected a session csrf token")
	}
	req, _ := http.NewRequest("POST", ts.URL+"/prefs/theme?%zz", nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	req.Header.Set("X-CSRF-Token", csrfToken)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed form: expected 400, got %d", resp.StatusCode)
	}
}

// Batch P #6 — singleflight: a cold burst shares one upstream batch; a
// mid-fetch settings switch keeps the old endpoint's data out of the cache.
func TestLiveMetricsSingleflightAndStoreGuard(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	empty := `{"status":"success","data":{"resultType":"vector","result":[]}}`

	// Burst: 5 concurrent cold requests → exactly one 8-query batch.
	var callsA int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callsA, 1)
		time.Sleep(100 * time.Millisecond)
		fmt.Fprint(w, empty)
	}))
	t.Cleanup(slow.Close)
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", slow.URL)

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			srv.liveMetrics(httptest.NewRequest("GET", "/", nil))
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&callsA); n != 8 {
		t.Fatalf("expected 8 upstream queries (one batch), got %d", n)
	}

	// Mid-fetch switch: slow fetch to B finishing after the switch to C
	// must not be stored.
	var callsB, callsC int32
	slowB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callsB, 1)
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, empty)
	}))
	t.Cleanup(slowB.Close)
	fastC := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callsC, 1)
		fmt.Fprint(w, empty)
	}))
	t.Cleanup(fastC.Close)

	database.Exec("UPDATE settings SET prometheus_url = ? WHERE id = 1", slowB.URL)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.liveMetrics(httptest.NewRequest("GET", "/", nil))
	}()
	time.Sleep(50 * time.Millisecond) // B fetch in flight
	database.Exec("UPDATE settings SET prometheus_url = ? WHERE id = 1", fastC.URL)
	<-done

	srv.prom.mu.Lock()
	stored := srv.prom.metrics
	srv.prom.mu.Unlock()
	if stored != nil {
		t.Fatal("B's fetch finished after the switch — must not be stored")
	}

	// The next call fetches fresh from C.
	m := srv.liveMetrics(httptest.NewRequest("GET", "/", nil))
	if m == nil {
		t.Fatal("expected metrics from C after the switch")
	}
	if atomic.LoadInt32(&callsC) == 0 {
		t.Fatal("C was never queried after the switch")
	}
}
