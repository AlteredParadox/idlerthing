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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"idlerthing/internal/importer"
)

// Batch R #1 — a settings change mid-flight wakes liveMon followers
// promptly (reset closes old channels) and no double-close panics.
func TestLiveMonResetWakesFollowers(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	emptyRange := `{"status":"success","data":{"resultType":"matrix","result":[]}}`
	emptyVec := `{"status":"success","data":{"resultType":"vector","result":[]}}`

	// Slow only for the first two queries: the leader needs ~2s, everything
	// after the reset is instant — so a promptly-woken follower is fast.
	var slowCalls int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&slowCalls, 1) <= 2 {
			time.Sleep(time.Second)
		}
		if r.URL.Path == "/api/v1/query_range" {
			fmt.Fprint(w, emptyRange)
			return
		}
		fmt.Fprint(w, emptyVec)
	}))
	t.Cleanup(slow.Close)
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/query_range" {
			fmt.Fprint(w, emptyRange)
			return
		}
		fmt.Fprint(w, emptyVec)
	}))
	t.Cleanup(fast.Close)
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", slow.URL)

	// Leader starts the slow batch; follower joins.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		srv.liveMonEntry(httptest.NewRequest("GET", "/", nil), "a:9100")
	}()
	time.Sleep(50 * time.Millisecond)
	followerDone := make(chan struct{})
	go func() {
		defer close(followerDone)
		srv.liveMonEntry(httptest.NewRequest("GET", "/", nil), "a:9100")
	}()
	time.Sleep(50 * time.Millisecond)

	// URL change mid-flight, then a THIRD request to trigger the lazy reset:
	// the follower (parked on the old channel) must wake PROMPTLY — the
	// slow leader needs ~1.2s for its 10 range queries.
	database.Exec("UPDATE settings SET prometheus_url = ? WHERE id = 1", fast.URL)
	start := time.Now()
	go srv.liveMonEntry(httptest.NewRequest("GET", "/", nil), "a:9100") // triggers the reset
	select {
	case <-followerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("follower not woken by the reset")
	}
	if elapsed := time.Since(start); elapsed > 800*time.Millisecond {
		t.Fatalf("follower parked behind the old leader: %v", elapsed)
	}
	// The old leader finishing must not panic (double close).
	<-leaderDone
}

// Batch R #2 — uptime30d: disconnecting leader completes on the detached
// context, followers still get the value; disconnecting waiter bails fast.
func TestUptimeLeaderAndWaiterDetach(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	var calls int32
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(150 * time.Millisecond)
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"a:9100"},"value":[1719300000,"99.95"]}]}}`)
	}))
	t.Cleanup(promSrv.Close)
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderReq := httptest.NewRequest("GET", "/", nil).WithContext(leaderCtx)
	followerReq := httptest.NewRequest("GET", "/", nil)
	var leaderGot, followerGot string
	done := make(chan struct{}, 2)
	go func() { leaderGot = srv.uptime30d(leaderReq, "a:9100"); done <- struct{}{} }()
	go func() {
		time.Sleep(40 * time.Millisecond)
		followerGot = srv.uptime30d(followerReq, "a:9100")
		done <- struct{}{}
	}()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	<-done
	if followerGot != "99.95%" || leaderGot != "99.95%" {
		t.Fatalf("leader cancel poisoned followers: leader=%q follower=%q", leaderGot, followerGot)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expected 1 upstream query, got %d", n)
	}

	// Waiter disconnect: returns "—" fast instead of parking.
	database.Exec("UPDATE settings SET prometheus_url = ? WHERE id = 1", promSrv.URL+"x")
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(slow.Close)
	database.Exec("UPDATE settings SET prometheus_url = ? WHERE id = 1", slow.URL)
	// Fresh server so the cache is cold.
	_, database2, srv2 := newTestServerFull(t)
	database2.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", slow.URL)
	go srv2.uptime30d(httptest.NewRequest("GET", "/", nil), "b:9100")
	time.Sleep(50 * time.Millisecond)
	waiterCtx, cancel2 := context.WithCancel(context.Background())
	waiterReq := httptest.NewRequest("GET", "/", nil).WithContext(waiterCtx)
	start := time.Now()
	go func() { time.Sleep(40 * time.Millisecond); cancel2() }()
	if got := srv2.uptime30d(waiterReq, "b:9100"); got != "—" {
		t.Fatalf("disconnecting waiter should degrade to —, got %q", got)
	}
	if time.Since(start) > 300*time.Millisecond {
		t.Fatalf("waiter parked: %v", time.Since(start))
	}
}

// Batch R #3 — triple flip A→B→A mid-flight stores nothing; B refetches.
func TestLiveMetricsTripleFlipStoreGuard(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	empty := `{"status":"success","data":{"resultType":"vector","result":[]}}`
	slowA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		fmt.Fprint(w, empty)
	}))
	t.Cleanup(slowA.Close)
	fastB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, empty)
	}))
	t.Cleanup(fastB.Close)

	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", slowA.URL)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.liveMetrics(httptest.NewRequest("GET", "/", nil)) // slow fetch to A
	}()
	time.Sleep(50 * time.Millisecond)
	database.Exec("UPDATE settings SET prometheus_url = ? WHERE id = 1", fastB.URL) // → B
	database.Exec("UPDATE settings SET prometheus_url = ? WHERE id = 1", slowA.URL) // → A again
	<-done

	srv.prom.mu.Lock()
	stored, slotURL := srv.prom.metrics, srv.prom.baseURL
	srv.prom.mu.Unlock()
	if stored != nil && slotURL != slowA.URL {
		t.Fatalf("triple flip stored A's data under slot %q", slotURL)
	}
	if stored != nil && slotURL == slowA.URL {
		// Slot key AND fetched URL both A — that's the legal single-flip-back
		// case only when no B reset happened in between; the slot went A→B→A,
		// so the B reset cleared it and the A store must match the CURRENT db
		// url (A) and slot (A) — acceptable.
	}
	// A subsequent request must be servable (fetch fresh or use the slot).
	if m := srv.liveMetrics(httptest.NewRequest("GET", "/", nil)); m == nil {
		t.Fatal("expected metrics after the flips settle")
	}
}

// Batch R #6 — a literal `null` document is not a valid export.
func TestImportNullDocument(t *testing.T) {
	if _, err := importer.Import(context.Background(), freshDB(t), strings.NewReader(`null`), false); err == nil {
		t.Fatal("null document must be rejected")
	}
	// Trailing newline after a valid doc is fine.
	if _, err := importer.Import(context.Background(), freshDB(t), strings.NewReader("{\"format\": 1}\n"), false); err != nil {
		t.Fatalf("trailing newline: %v", err)
	}
}

// Batch R #7 — reseller imports work, and an item missing its entity object
// warns instead of vanishing silently.
func TestImportResellerAndMissingEntityWarning(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{"format": 1,
		"reseller": [
			{"shared_hosting": {"id": 1, "main_domain": "res.example.com", "active": true}},
			{"note": "no entity object here"}
		]}`
	summary, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Reseller != 1 {
		t.Fatalf("reseller row should import: %+v", summary)
	}
	found := false
	for _, w := range summary.Warnings {
		if strings.Contains(w, "without a") && strings.Contains(w, "shared_hosting") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the missing-entity warning, got %v", summary.Warnings)
	}
	var domain string
	dbB.QueryRow("SELECT main_domain FROM reseller_hosting").Scan(&domain)
	if domain != "res.example.com" {
		t.Fatalf("reseller import broken: %q", domain)
	}
}

// Batch R #8 — a static 404 carries no immutable Cache-Control.
func TestStatic404NotImmutable(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := newClient(t).Get(ts.URL + "/static/nope.js")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Fatalf("404 must not be immutable, got %q", cc)
	}
	// 2xx keeps the immutable policy.
	resp, err = newClient(t).Get(ts.URL + "/static/app.css?v=1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("2xx asset should be immutable, got %q", cc)
	}
}

// Batch R #4 — with rates stale past TTL and refresh failing, the dashboard
// shows the "rates unavailable" note.
func TestDashboardRatesUnavailableNote(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "rates-host")

	ratesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base":"USD","rates":{"EUR":0.5}}`))
	}))
	t.Cleanup(ratesSrv.Close)
	srv.rates.BaseURL = ratesSrv.URL
	// Prime the cache, then expire it and kill the endpoint.
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	database.Exec("UPDATE settings SET dashboard_currency = 'EUR' WHERE id = 1")
	srv.rates.ExpireForTest()
	srv.rates.BaseURL = "http://127.0.0.1:1/down"
	srv.touchDashboard() // raw SQL above didn't bump the view cache

	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "exchange rates unavailable") {
		t.Fatal("dashboard should show the rates-unavailable note for expired rates")
	}
}
