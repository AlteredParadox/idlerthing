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

	// Broken endpoint with stale cache: keeps stale data, ok=true.
	r.BaseURL = "http://127.0.0.1:1/unreachable"
	r.fetchedAt = time.Now().Add(-8 * 24 * time.Hour) // force refetch attempt
	stale, ok := r.Get(context.Background())
	if !ok || stale["EUR"] != 0.9 {
		t.Fatalf("stale cache should be kept: %v %v", stale, ok)
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
		in, _ := time.Parse("2006-01-02", c.in)
		got := AddMonthsClamped(in, c.months).Format("2006-01-02")
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
	todayStart, _ := time.Parse("2006-01-02", time.Now().Format("2006-01-02"))

	// Past monthly must land on/after today, same day-of-month.
	d, _ := time.Parse("2006-01-02", dueOf(pastMonthly))
	if d.Before(todayStart) || d.Day() != 15 {
		t.Fatalf("past monthly advanced to %v", d)
	}
	// Jan 31 chain must land on/after today, clamped to month end each hop,
	// and never overflow into the next month.
	d, _ = time.Parse("2006-01-02", dueOf(jan31))
	if d.Before(todayStart) || d.Day() > 31 {
		t.Fatalf("jan31 advanced to %v", d)
	}

	// A yesterday-due row must advance to >= today.
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	yid := insert(model.TermMonthly, yesterday)
	if n, err := AdvanceDueDates(ctx, database); err != nil || n != 1 {
		t.Fatalf("yesterday advance: n=%d err=%v", n, err)
	}
	d, _ = time.Parse("2006-01-02", dueOf(yid))
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

// Batch I #8 — archived (active=0) pricings are invisible to PricingStore.Get.
func TestPricingStoreArchivedInvisible(t *testing.T) {
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

	if _, err := database.Exec("UPDATE pricings SET active = 0 WHERE service_id = 1"); err != nil {
		t.Fatal(err)
	}
	p, err = st.Get(ctx, 1, 1)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if p != nil {
		t.Fatalf("archived pricing must be invisible, got %+v", p)
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
