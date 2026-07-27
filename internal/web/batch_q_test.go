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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"idlerthing/internal/prom"
)

// Batch Q N2 — a disconnecting leader no longer cancels the singleflight
// batch: the fetch completes on a detached context and followers get data.
func TestLiveMetricsLeaderDetachCompletes(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	var calls int32
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(100 * time.Millisecond)
		q := r.URL.Query().Get("query")
		if q == "up" {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[{"metric":{"instance":"a:9100"},"value":[1719300000,"1"]}]}}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(promSrv.Close)
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)

	leaderCtx, cancel := context.WithCancel(context.Background())
	leaderReq := httptest.NewRequest("GET", "/", nil).WithContext(leaderCtx)
	followerReq := httptest.NewRequest("GET", "/", nil)

	var wg sync.WaitGroup
	var leaderM, followerM *prom.Metrics
	wg.Add(2)
	go func() {
		defer wg.Done()
		leaderM = srv.liveMetrics(leaderReq)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(30 * time.Millisecond) // arrive as a follower
		followerM = srv.liveMetrics(followerReq)
	}()
	time.Sleep(60 * time.Millisecond)
	cancel() // leader's client disconnects mid-batch
	wg.Wait()

	if followerM == nil || !followerM.Healthy {
		t.Fatalf("follower should get the completed batch: %+v", followerM)
	}
	if leaderM == nil || !leaderM.Healthy {
		t.Fatalf("leader should complete on the detached ctx: %+v", leaderM)
	}
	if n := atomic.LoadInt32(&calls); n != 8 {
		t.Fatalf("expected one 8-query batch, got %d queries", n)
	}
}

// Batch Q N3 — liveMon singleflights PER INSTANCE: two instances fetch
// concurrently; the same instance dedupes to one upstream batch.
func TestLiveMonPerInstanceSingleflight(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	var callsA, callsB int32
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		q := r.URL.Query().Get("query")
		if strings.Contains(q, `"a:9100"`) {
			atomic.AddInt32(&callsA, 1)
		}
		if strings.Contains(q, `"b:9100"`) {
			atomic.AddInt32(&callsB, 1)
		}
		if r.URL.Path == "/api/v1/query_range" {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[{"values":[[1719300000,"12"]]}]}}`)
			return
		}
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(promSrv.Close)
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)

	// A and B concurrently: wall time ≈ one batch, not two serial batches.
	start := time.Now()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); srv.liveMonEntry(httptest.NewRequest("GET", "/", nil), "a:9100") }()
	go func() { defer wg.Done(); srv.liveMonEntry(httptest.NewRequest("GET", "/", nil), "b:9100") }()
	wg.Wait()
	elapsed := time.Since(start)
	// HostDetail: 10 queries 3-wide ≈ 4 waves × 50ms = 200ms (+2 fs queries).
	// Serialized would be ≥2× that.
	if elapsed > 400*time.Millisecond {
		t.Fatalf("instances serialized on a global lock: %v", elapsed)
	}
	if atomic.LoadInt32(&callsA) == 0 || atomic.LoadInt32(&callsB) == 0 {
		t.Fatalf("both instances should be fetched: a=%d b=%d", callsA, callsB)
	}

	// Same instance, cold again (fresh server): deduped to one batch.
	_, database2, srv2 := newTestServerFull(t)
	database2.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)
	before := atomic.LoadInt32(&callsA)
	wg.Add(2)
	go func() { defer wg.Done(); srv2.liveMonEntry(httptest.NewRequest("GET", "/", nil), "a:9100") }()
	go func() { defer wg.Done(); srv2.liveMonEntry(httptest.NewRequest("GET", "/", nil), "a:9100") }()
	wg.Wait()
	got := atomic.LoadInt32(&callsA) - before
	// One batch = 10 range queries + 2 filesystem queries.
	if got != 12 {
		t.Fatalf("same-instance burst should dedupe to one batch (12 queries), got %d", got)
	}
}

// Batch Q N4 — a disconnecting WAITER returns its stale value immediately
// instead of parking until the leader finishes.
func TestLiveMetricsWaiterDetachReturnsStale(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(250 * time.Millisecond)
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[]}}`)
	}))
	t.Cleanup(promSrv.Close)
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)

	// Prime a stale cache entry (old timestamp forces a refetch).
	srv.prom.baseURL = promSrv.URL
	srv.prom.metrics = promMetricsStub
	srv.prom.at = time.Now().Add(-time.Minute)

	// Leader starts the slow fetch.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		srv.liveMetrics(httptest.NewRequest("GET", "/", nil))
	}()
	time.Sleep(50 * time.Millisecond) // leader is in flight

	// Waiter with a ctx that cancels quickly: returns the stale entry fast.
	waiterCtx, cancel := context.WithCancel(context.Background())
	waiterReq := httptest.NewRequest("GET", "/", nil).WithContext(waiterCtx)
	start := time.Now()
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	got := srv.liveMetrics(waiterReq)
	elapsed := time.Since(start)
	if got != promMetricsStub {
		t.Fatalf("waiter should get the stale entry, got %+v", got)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("waiter parked on the leader: %v", elapsed)
	}
	<-leaderDone
}
