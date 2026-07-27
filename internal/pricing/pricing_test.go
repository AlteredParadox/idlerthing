package pricing

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"idlerthing/internal/db"
	"idlerthing/internal/model"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return database
}

func TestMonthlyUSD(t *testing.T) {
	rates := map[string]float64{"EUR": 0.92, "GBP": 0.79}

	// USD passes through, normalized by term.
	v, ok := MonthlyUSD(&model.Pricing{Currency: "USD", Price: 120, Term: model.TermAnnual}, rates)
	if !ok || v != 10 {
		t.Fatalf("USD annual: %v %v", v, ok)
	}

	// EUR converts via rate.
	v, ok = MonthlyUSD(&model.Pricing{Currency: "EUR", Price: 9.20, Term: model.TermMonthly}, rates)
	if !ok || v != 10 {
		t.Fatalf("EUR monthly: %v %v", v, ok)
	}

	// One-time term has no monthly amount.
	if _, ok := MonthlyUSD(&model.Pricing{Currency: "USD", Price: 50, Term: model.TermOneTime}, rates); ok {
		t.Fatal("one-time should not convert")
	}

	// Unknown currency fails gracefully.
	if _, ok := MonthlyUSD(&model.Pricing{Currency: "XYZ", Price: 10, Term: model.TermMonthly}, rates); ok {
		t.Fatal("unknown currency should fail")
	}

	// Nil rates map: USD still works, others don't.
	if _, ok := MonthlyUSD(&model.Pricing{Currency: "USD", Price: 5, Term: model.TermMonthly}, nil); !ok {
		t.Fatal("USD should work without rates")
	}
	if _, ok := MonthlyUSD(&model.Pricing{Currency: "EUR", Price: 5, Term: model.TermMonthly}, nil); ok {
		t.Fatal("EUR should fail without rates")
	}
}

func TestRatesFetchAndFailure(t *testing.T) {
	// Healthy endpoint.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base":"USD","rates":{"EUR":0.9,"GBP":0.8}}`))
	}))
	defer ts.Close()

	r := &Rates{BaseURL: ts.URL}
	rates, ok := r.Get(context.Background())
	if !ok || rates["EUR"] != 0.9 {
		t.Fatalf("fetch: %v %v", rates, ok)
	}

	// Broken endpoint: ok=false, no panic.
	bad := &Rates{BaseURL: "http://127.0.0.1:1/unreachable"}
	if _, ok := bad.Get(context.Background()); ok {
		t.Fatal("unreachable endpoint should report ok=false")
	}

	// Broken endpoint with stale cache PAST the TTL: data kept for
	// conversion, but flagged unusable (Batch R #4).
	r.BaseURL = "http://127.0.0.1:1/unreachable"
	r.fetchedAt = time.Now().Add(-8 * 24 * time.Hour) // force refetch attempt, past ratesTTL
	stale, ok := r.Get(context.Background())
	if stale["EUR"] != 0.9 {
		t.Fatalf("stale cache should be kept: %v", stale)
	}
	if ok {
		t.Fatal("stale-past-TTL rates must report ok=false")
	}
}

func TestAddMonthsClamped(t *testing.T) {
	cases := []struct {
		in     string
		months int
		want   string
	}{
		{"2023-01-31", 1, "2023-02-28"},  // non-leap February
		{"2024-01-31", 1, "2024-02-29"},  // leap year
		{"2023-01-31", 12, "2024-01-31"}, // full year keeps day
		{"2023-03-31", 1, "2023-04-30"},  // April shorter
		{"2023-01-15", 3, "2023-04-15"},  // mid-month quarterly
		{"2023-12-31", 2, "2024-02-29"},  // across year boundary into Feb
	}
	for _, c := range cases {
		in, _ := time.Parse(time.DateOnly, c.in)
		got := AddMonthsClamped(in, c.months).Format(time.DateOnly)
		if got != c.want {
			t.Errorf("%s +%dm = %s, want %s", c.in, c.months, got, c.want)
		}
	}
}

func TestAdvanceDueDates(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	var seq int64
	insert := func(term int, due any) int64 {
		seq++
		res, err := database.Exec(`
			INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date)
			VALUES (?, 1, 'USD', 10, ?, ?)`, seq, term, due)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}

	pastMonthly := insert(model.TermMonthly, "2020-01-15") // advances many months
	jan31 := insert(model.TermMonthly, "2020-01-31")       // clamped advances
	oneTime := insert(model.TermOneTime, "2020-01-01")     // skipped (one-time)
	nullDue := insert(model.TermMonthly, nil)              // skipped (NULL)
	future := insert(model.TermAnnual, "2099-01-01")       // skipped (future)

	n, err := AdvanceDueDates(ctx, database)
	if err != nil {
		t.Fatalf("AdvanceDueDates: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 updates, got %d", n)
	}

	dueOf := func(id int64) string {
		var d string
		database.QueryRow("SELECT next_due_date FROM pricings WHERE id = ?", id).Scan(&d)
		return d
	}
	// Assert against the same local "today" the advance logic uses.
	todayStart, _ := time.Parse(time.DateOnly, time.Now().Format(time.DateOnly))

	// Past monthly must land on/after today, same day-of-month.
	d, _ := time.Parse(time.DateOnly, dueOf(pastMonthly))
	if d.Before(todayStart) || d.Day() != 15 {
		t.Fatalf("past monthly advanced to %v", d)
	}
	// Jan 31 chain must land on/after today, clamped to month end each hop,
	// and never overflow into the next month.
	d, _ = time.Parse(time.DateOnly, dueOf(jan31))
	if d.Before(todayStart) || d.Day() > 31 {
		t.Fatalf("jan31 advanced to %v", d)
	}

	// A yesterday-due row must advance to >= today.
	yesterday := time.Now().AddDate(0, 0, -1).Format(time.DateOnly)
	yid := insert(model.TermMonthly, yesterday)
	if n, err := AdvanceDueDates(ctx, database); err != nil || n != 1 {
		t.Fatalf("yesterday advance: n=%d err=%v", n, err)
	}
	d, _ = time.Parse(time.DateOnly, dueOf(yid))
	if d.Before(todayStart) {
		t.Fatalf("yesterday-due row stuck at %v", d)
	}
	// Skipped rows unchanged.
	if dueOf(oneTime) != "2020-01-01" {
		t.Fatal("one-time should not advance")
	}
	var nullCheck any
	database.QueryRow("SELECT next_due_date FROM pricings WHERE id = ?", nullDue).Scan(&nullCheck)
	if nullCheck != nil {
		t.Fatal("NULL due should stay NULL")
	}
	if dueOf(future) != "2099-01-01" {
		t.Fatal("future due should not advance")
	}
}

// Batch I #7 — a failing endpoint is fetched ONCE across concurrent Gets
// (singleflight) and not retried for 60s (negative caching).
func TestRatesSingleflightAndBackoff(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()

	r := &Rates{BaseURL: ts.URL}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := r.Get(context.Background()); ok {
				t.Error("failing endpoint should report ok=false")
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expected 1 fetch across 3 concurrent Gets, got %d", n)
	}

	// Within the 60s backoff window another Get must NOT refetch.
	if _, ok := r.Get(context.Background()); ok {
		t.Fatal("still failing: ok should be false")
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("backoff window refetched: %d calls", n)
	}
}

// Batch M R2 — inactive (import-preserved) pricing rows are invisible to
// PricingStore.Get everywhere; cost queries exclude them too.
func TestPricingStoreInactiveInvisible(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &model.PricingStore{DB: database}

	if _, err := database.Exec(`
		INSERT INTO pricings (service_id, service_type, currency, price, term, active)
		VALUES (1, 1, 'USD', 10, 1, 1)`); err != nil {
		t.Fatal(err)
	}
	p, err := st.Get(ctx, 1, 1)
	if err != nil || p == nil || p.Price != 10 {
		t.Fatalf("active pricing should be visible: %v %v", p, err)
	}

	// Imported inactive row: Get returns nil (uniformly invisible), and the
	// dashboard cost union (p.active = 1) excludes it too.
	if _, err := database.Exec("UPDATE pricings SET active = 0 WHERE service_id = 1"); err != nil {
		t.Fatal(err)
	}
	p, err = st.Get(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p != nil {
		t.Fatalf("inactive pricing must be invisible, got %+v", p)
	}
	var cost int
	if err := database.QueryRow(`
		SELECT COUNT(*) FROM pricings p
		JOIN servers svc ON svc.id = p.service_id AND svc.active = 1
		WHERE p.service_type = 1 AND p.active = 1 AND p.term != 7`).Scan(&cost); err != nil {
		t.Fatal(err)
	}
	if cost != 0 {
		t.Fatal("cost queries must exclude inactive rows")
	}
}

// Batch I #16 — the due-date advance CAS bumps updated_at.
func TestAdvanceDueDatesBumpsUpdatedAt(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	res, err := database.Exec(`
		INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date, created_at, updated_at)
		VALUES (1, 1, 'USD', 10, 1, '2020-01-15', '2020-01-01 00:00:00', '2020-01-01 00:00:00')`)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	if n, err := AdvanceDueDates(ctx, database); err != nil || n != 1 {
		t.Fatalf("advance: n=%d err=%v", n, err)
	}
	var updatedAt string
	if err := database.QueryRow("SELECT updated_at FROM pricings WHERE id = ?", id).Scan(&updatedAt); err != nil {
		t.Fatal(err)
	}
	if updatedAt == "2020-01-01 00:00:00" {
		t.Fatalf("updated_at not bumped: %q", updatedAt)
	}
}

// Batch J #7 — followers of a COLD fetch receive the leader's successful
// result (not the pre-fetch nil/stale state).
func TestRatesColdFetchSharedWithFollowers(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(100 * time.Millisecond) // slow cold fetch
		w.Write([]byte(`{"base":"USD","rates":{"EUR":0.9}}`))
	}))
	defer ts.Close()

	r := &Rates{BaseURL: ts.URL}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rates, ok := r.Get(context.Background())
			if !ok || rates["EUR"] != 0.9 {
				t.Errorf("follower should see the leader's rates: %v %v", rates, ok)
			}
		}()
	}
	wg.Wait()
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expected 1 fetch, got %d", n)
	}
}

// Batch O #12 — a leader whose request ctx dies mid-fetch still completes
// on the detached context; followers get the rates, not a poisoned backoff.
func TestRatesLeaderCtxCancelDoesNotPoisonFollowers(t *testing.T) {
	var calls int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		time.Sleep(150 * time.Millisecond)
		w.Write([]byte(`{"base":"USD","rates":{"EUR":0.9}}`))
	}))
	defer ts.Close()

	r := &Rates{BaseURL: ts.URL}
	leaderCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	var leaderRates, followerRates map[string]float64
	var leaderOK, followerOK bool
	go func() {
		defer wg.Done()
		leaderRates, leaderOK = r.Get(leaderCtx)
	}()
	go func() {
		defer wg.Done()
		time.Sleep(30 * time.Millisecond) // arrive as a follower
		followerRates, followerOK = r.Get(context.Background())
	}()
	time.Sleep(60 * time.Millisecond)
	cancel() // leader's client disconnects mid-fetch
	wg.Wait()

	if !followerOK || followerRates["EUR"] != 0.9 {
		t.Fatalf("follower poisoned by leader cancel: %v %v", followerRates, followerOK)
	}
	if !leaderOK || leaderRates["EUR"] != 0.9 {
		t.Fatalf("leader should complete on the detached ctx: %v %v", leaderRates, leaderOK)
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("expected 1 fetch, got %d", n)
	}
}

// Batch R #4 — rates past their TTL are returned for conversion but
// flagged ok=false (the "rates unavailable" note must appear).
func TestRatesStaleReportedUnusable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base":"USD","rates":{"EUR":0.9}}`))
	}))
	defer ts.Close()

	r := &Rates{BaseURL: ts.URL}
	if _, ok := r.Get(context.Background()); !ok {
		t.Fatal("fresh fetch should be ok")
	}

	// Expire past the TTL and break the endpoint: stale data, ok=false.
	r.fetchedAt = time.Now().Add(-(ratesTTL + time.Hour))
	r.BaseURL = "http://127.0.0.1:1/down"
	stale, ok := r.Get(context.Background())
	if ok {
		t.Fatal("stale-past-TTL rates must report ok=false")
	}
	if stale["EUR"] != 0.9 {
		t.Fatalf("stale data should still be returned: %v", stale)
	}
}
