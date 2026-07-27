// Package pricing handles currency conversion and due-date arithmetic.
package pricing

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	"idlerthing/internal/model"
)

// DefaultRatesURL is the free, keyless exchange-rate endpoint (base USD).
const DefaultRatesURL = "https://api.exchangerate-api.com/v4/latest/USD"

// ratesTTL is how long fetched rates are trusted.
const ratesTTL = 7 * 24 * time.Hour

// Rates is an in-memory exchange-rate cache (base USD). On fetch failure it
// keeps whatever it has (possibly nothing) and reports ok=false so the UI
// can degrade to native currencies. Failures back off for 60s (negative
// caching) and concurrent Gets singleflight onto one in-flight fetch.
type Rates struct {
	// BaseURL is injectable for tests.
	BaseURL string

	mu        sync.Mutex
	rates     map[string]float64
	fetchedAt time.Time
	lastTry   time.Time
	inFlight  chan struct{}
	client    *http.Client
}

// NewRates creates a cache with the production endpoint.
func NewRates() *Rates {
	return &Rates{BaseURL: DefaultRatesURL, client: &http.Client{Timeout: 5 * time.Second}}
}

// ExpireForTest backdates the cache past the TTL (test hook, like BaseURL).
func (r *Rates) ExpireForTest() {
	r.mu.Lock()
	r.fetchedAt = time.Now().Add(-(ratesTTL + time.Hour))
	r.mu.Unlock()
}

// usable reports whether the cached rates are within their TTL (call with
// r.mu held). Stale rates are still RETURNED for conversion, just flagged
// ok=false so the UI can show the "rates unavailable" note.
func (r *Rates) usable() bool {
	return r.rates != nil && time.Since(r.fetchedAt) < ratesTTL
}

// Get returns rates (units per 1 USD) and whether they are usable.
// Fresh cache hits answer immediately; a concurrent cold fetch is joined
// (singleflight) so followers receive the leader's result; otherwise one
// fetch runs. Failed fetches back off for 60s (negative caching).
func (r *Rates) Get(ctx context.Context) (map[string]float64, bool) {
	r.mu.Lock()
	if r.rates != nil && time.Since(r.fetchedAt) < ratesTTL {
		cached := r.rates
		r.mu.Unlock()
		return cached, true
	}
	// Join an in-flight fetch FIRST — followers of a cold fetch must get the
	// leader's fresh rates, not the pre-fetch stale/nil state.
	if r.inFlight != nil {
		ch := r.inFlight
		r.mu.Unlock()
		select {
		case <-ch:
			r.mu.Lock()
			defer r.mu.Unlock()
			return r.rates, r.usable()
		case <-ctx.Done():
			// Caller gave up waiting — fall through to whatever is cached.
			r.mu.Lock()
			stale, ok := r.rates, r.usable()
			r.mu.Unlock()
			return stale, ok
		}
	}
	// Negative cache: a failed fetch backs off for 60s.
	if time.Since(r.lastTry) < 60*time.Second && !r.lastTry.IsZero() {
		stale, ok := r.rates, r.usable()
		r.mu.Unlock()
		return stale, ok
	}
	r.inFlight = make(chan struct{})
	r.lastTry = time.Now()
	r.mu.Unlock()

	// Fetch on a DETACHED context: if the leader's request ctx dies
	// mid-fetch (client disconnect), followers still get the completed
	// result instead of (nil,false) plus the 60s negative-cache backoff.
	// lastTry was already stamped above, so failures keep their backoff.
	fetchCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	fresh, ok := r.fetch(fetchCtx)
	cancel()

	r.mu.Lock()
	if ok {
		r.rates = fresh
		r.fetchedAt = time.Now()
	}
	close(r.inFlight)
	r.inFlight = nil
	out, outOk := r.rates, r.usable()
	r.mu.Unlock()
	return out, outOk
}

// fetch retrieves the current rates from the endpoint.
func (r *Rates) fetch(ctx context.Context) (map[string]float64, bool) {
	if r.client == nil {
		r.client = &http.Client{Timeout: 5 * time.Second}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.BaseURL, nil)
	if err != nil {
		return nil, false
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var body struct {
		Rates map[string]float64 `json:"rates"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil || len(body.Rates) == 0 {
		return nil, false
	}
	return body.Rates, true
}

// MonthlyUSD normalizes a pricing to a per-month USD amount, rounded to
// cents — for PER-ROW displays. ok=false when the amount cannot be
// computed (one-time term, or no rate for the currency).
func MonthlyUSD(p *model.Pricing, rates map[string]float64) (float64, bool) {
	v, ok := MonthlyUSDRaw(p, rates)
	if !ok {
		return 0, false
	}
	return round2(v), true
}

// MonthlyUSDRaw is MonthlyUSD without the cent-rounding — for SUMS, where
// per-row rounding would drift (round at display time instead).
func MonthlyUSDRaw(p *model.Pricing, rates map[string]float64) (float64, bool) {
	months := model.TermMonths(p.Term)
	if months == 0 || p.Price <= 0 {
		return 0, false
	}
	monthly := p.Price / float64(months)
	if p.Currency == "USD" {
		return monthly, true
	}
	rate, ok := rates[p.Currency]
	if !ok || rate <= 0 {
		return 0, false
	}
	return monthly / rate, true
}

// ConvertUSD converts a USD amount into another currency using rates.
// ok=false when the target currency is unknown.
func ConvertUSD(amount float64, currency string, rates map[string]float64) (float64, bool) {
	if currency == "USD" || currency == "" {
		return round2(amount), true
	}
	rate, ok := rates[currency]
	if !ok || rate <= 0 {
		return 0, false
	}
	return round2(amount * rate), true
}

func round2(f float64) float64 {
	return math.Round(f*100) / 100
}
