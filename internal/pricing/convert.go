// Package pricing handles currency conversion and due-date arithmetic.
package pricing

import (
	"context"
	"encoding/json"
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
// can degrade to native currencies.
type Rates struct {
	// BaseURL is injectable for tests.
	BaseURL string

	mu        sync.Mutex
	rates     map[string]float64
	fetchedAt time.Time
	client    *http.Client
}

// NewRates creates a cache with the production endpoint.
func NewRates() *Rates {
	return &Rates{BaseURL: DefaultRatesURL, client: &http.Client{Timeout: 5 * time.Second}}
}

// Get returns rates (units per 1 USD) and whether they are usable.
// The HTTP fetch happens WITHOUT the mutex held (a slow endpoint must not
// serialize unrelated page renders); a rare duplicate fetch is fine.
func (r *Rates) Get(ctx context.Context) (map[string]float64, bool) {
	r.mu.Lock()
	cached, fetchedAt := r.rates, r.fetchedAt
	r.mu.Unlock()
	if cached != nil && time.Since(fetchedAt) < ratesTTL {
		return cached, true
	}

	fresh, ok := r.fetch(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	if !ok {
		return r.rates, r.rates != nil // keep stale
	}
	r.rates = fresh
	r.fetchedAt = time.Now()
	return r.rates, true
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
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || len(body.Rates) == 0 {
		return nil, false
	}
	return body.Rates, true
}

// MonthlyUSD normalizes a pricing to a per-month USD amount. ok=false when
// the amount cannot be computed (one-time term, or no rate for the currency).
func MonthlyUSD(p *model.Pricing, rates map[string]float64) (float64, bool) {
	months := model.TermMonths(p.Term)
	if months == 0 || p.Price <= 0 {
		return 0, false
	}
	monthly := p.Price / float64(months)
	if p.Currency == "USD" {
		return round2(monthly), true
	}
	rate, ok := rates[p.Currency]
	if !ok || rate <= 0 {
		return 0, false
	}
	return round2(monthly / rate), true
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
