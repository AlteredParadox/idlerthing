package web

import (
	"database/sql"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"idlerthing/internal/prom"
)

// Batch O #1 — the counts cache re-queries past its TTL even without a gen
// bump (out-of-band writes), and a failed query is never cached.
func TestCountsTTLAndFailureNotCached(t *testing.T) {
	_, database, srv := newTestServerFull(t)
	req := httptest.NewRequest("GET", "/", nil)

	c1 := srv.counts(req)
	// Simulate an out-of-band write (CLI import): raw SQL, no touchDashboard.
	if _, err := database.Exec(
		"INSERT INTO servers (hostname, server_type, active) VALUES ('oob-01', 1, 1)"); err != nil {
		t.Fatal(err)
	}
	if got := srv.counts(req); got.Servers != c1.Servers {
		t.Fatal("within the TTL the cached counts are served")
	}
	// Backdate the cache past the TTL → re-query despite unchanged gen.
	srv.dash.mu.Lock()
	srv.dash.countsAt = time.Now().Add(-countsTTL - time.Minute)
	srv.dash.mu.Unlock()
	if got := srv.counts(req); got.Servers != c1.Servers+1 {
		t.Fatalf("expired cache must re-query: %d → %d", c1.Servers, got.Servers)
	}

	// A failed query returns zeros and is NOT cached (fresh Server, closed DB).
	database.Close()
	srv2 := &Server{db: database, dash: &dashboardCache{}}
	got := srv2.counts(req)
	if got.Servers != 0 {
		t.Fatalf("failed query should return zeros, got %+v", got)
	}
	srv2.dash.mu.Lock()
	cached := srv2.dash.countsOK
	srv2.dash.mu.Unlock()
	if cached {
		t.Fatal("failed query must not be cached")
	}
}

// Batch O #11 — with the `up` query failed, rows keep the neutral grey dot
// (Live 0) while meters still render.
func TestApplyLiveOnlineUnknownGreyDot(t *testing.T) {
	row := serverRow{}
	h := &prom.HostMetrics{Instance: "a:9100", Found: true, OnlineKnown: false, CPUPct: 12}
	applyLiveToRow(&row, h, sql.NullInt64{})
	if row.Live != 0 {
		t.Fatalf("unknown online status must keep Live=0 (grey), got %d", row.Live)
	}
	if row.CPUMeter == nil {
		t.Fatal("cpu meter should still render")
	}

	// Known-down stays red; known-up stays green.
	row = serverRow{}
	applyLiveToRow(&row, &prom.HostMetrics{Found: true, OnlineKnown: true, Online: false}, sql.NullInt64{})
	if row.Live != 2 {
		t.Fatalf("known down should be red, got %d", row.Live)
	}
	row = serverRow{}
	applyLiveToRow(&row, &prom.HostMetrics{Found: true, OnlineKnown: true, Online: true}, sql.NullInt64{})
	if row.Live != 1 {
		t.Fatalf("known up should be green, got %d", row.Live)
	}
}

// Batch O #2 — no token is revealed without the one-time cookie (the PRG
// flow itself is covered by TestAPITokenGeneration).
func TestSettingsNoTokenWithoutCookie(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	resp, err := client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "will not be shown again") {
		t.Fatal("no token should be revealed without the one-time cookie")
	}
	// Sanity: the page still shows (memoized) settings values.
	if !strings.Contains(body, "Default currency") {
		t.Fatal("settings page should render normally")
	}
}
