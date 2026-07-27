package web

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"idlerthing/internal/model"
	"idlerthing/internal/pricing"
)

// dashboardCache is a 60s in-memory cache, invalidated by generation bumps
// on any service/pricing/catalog write.
type dashboardCache struct {
	mu      sync.Mutex
	gen     uint64
	viewGen uint64
	at      time.Time
	view    *dashboardView
	// Sidebar counts cached by the same generation counter — every write to
	// a COUNTED table bumps gen via touchDashboard (audited: all six service
	// types, catalogs, label assign/unassign, dns, ips, notes, yabs ingest +
	// delete). A weak TTL backstops out-of-band writes (CLI import against
	// the running server's DB). Writes to users/sessions/settings/
	// user_prefs don't change any count and intentionally don't bump.
	countsGen uint64
	counts    Counts
	countsOK  bool
	countsAt  time.Time
}

// touchDashboard invalidates the dashboard cache.
func (s *Server) touchDashboard() {
	s.dash.mu.Lock()
	s.dash.gen++
	s.dash.mu.Unlock()
}

// dashboardView is the template payload for GET /.
type dashboardView struct {
	TotalServices    int
	ActiveServices   int
	MonthlyCost      string
	YearlyCost       string
	RatesNote        string
	DueSoonCount     int
	DueSoonDays      int
	DueSoon          []dueSoonRow
	Recent           []recentRow
	CPUCores         int64
	BandwidthDisplay string
	RAMDisplay       string
	DiskDisplay      string
	Providers        int
	Locations        int
}

// dueSoonRow is one row of the due-soon card.
type dueSoonRow struct {
	Name      string
	URL       string
	TypeLabel string
	Price     string
	Due       string
	DueClass  string
}

// recentRow is one row of the recently-added card.
type recentRow struct {
	Name      string
	URL       string
	TypeLabel string
	Created   string
}

// serviceTables enumerates the six service tables for dashboard totals.
var serviceTables = []struct {
	table string
	name  string
}{
	{"servers", "hostname"},
	{"shared_hosting", "main_domain"},
	{"reseller_hosting", "main_domain"},
	{"domains", "domain"},
	{"misc_services", "name"},
	{"seedboxes", "hostname"},
}

// handleDashboard renders the real dashboard.
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	// Note: two concurrent cache misses may both build (benign duplicate
	// work) — the last store wins, and the view is read-only thereafter.
	s.dash.mu.Lock()
	cached := s.dash.view
	fresh := cached != nil && s.dash.viewGen == s.dash.gen &&
		s.dash.at.After(time.Now().Add(-60*time.Second))
	s.dash.mu.Unlock()

	var view *dashboardView
	if fresh {
		view = cached
	} else {
		// Lazy due-date rollover on cache misses only (≤60s lag is harmless
		// at day granularity); any change invalidates the cache.
		if n, err := pricing.AdvanceDueDates(r.Context(), s.db); err == nil && n > 0 {
			s.touchDashboard()
		}
		// Capture the generation BEFORE computing: a mutation landing
		// during compute bumps gen, so the new view is stale on arrival
		// and gets rebuilt on the next request rather than served fresh
		// for 60s. (Benign residual window: a mutation between the
		// freshness check and this capture is covered by the 60s TTL.)
		s.dash.mu.Lock()
		gen := s.dash.gen
		s.dash.mu.Unlock()
		var err error
		view, err = s.computeDashboard(r)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		s.dash.mu.Lock()
		s.dash.view = view
		s.dash.viewGen = gen
		s.dash.at = time.Now()
		s.dash.mu.Unlock()
	}

	data := s.newPageData(w, r, "Dashboard", "dashboard")
	data.Data = view
	s.render(w, r, "dashboard", data)
}

// computeDashboard runs all dashboard queries.
func (s *Server) computeDashboard(r *http.Request) (*dashboardView, error) {
	ctx := r.Context()
	view := &dashboardView{}

	// Totals across all six service types.
	for _, st := range serviceTables {
		var total, active int
		err := s.db.QueryRowContext(ctx,
			"SELECT COUNT(*), COALESCE(SUM(active = 1), 0) FROM "+st.table).Scan(&total, &active)
		if err != nil {
			return nil, err
		}
		view.TotalServices += total
		view.ActiveServices += active
	}

	// Monthly cost in the dashboard currency. Only active services with
	// active, recurring pricings count.
	settings := s.dashSettings(r)
	view.DueSoonDays = settings.dueSoon
	rates, ratesOK := s.rates.Get(ctx)
	var monthlyUSD float64
	var selects []string
	for _, st := range model.OrderedServiceTypes {
		selects = append(selects, fmt.Sprintf(
			`SELECT p.currency, p.price, p.term FROM pricings p
			 JOIN %s svc ON svc.id = p.service_id AND svc.active = 1
			 WHERE p.service_type = %d AND p.active = 1 AND p.term != %d`,
			model.ServiceTable[st], st, model.TermOneTime))
	}
	rows, err := s.db.QueryContext(ctx, strings.Join(selects, " UNION ALL "))
	if err != nil {
		return nil, err
	}
	type priceRow struct {
		currency string
		price    float64
		term     int
	}
	var priceRows []priceRow
	for rows.Next() {
		var pr priceRow
		if err := rows.Scan(&pr.currency, &pr.price, &pr.term); err != nil {
			rows.Close()
			return nil, err
		}
		priceRows = append(priceRows, pr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, pr := range priceRows {
		v, ok := pricing.MonthlyUSDRaw(&model.Pricing{Currency: pr.currency, Price: pr.price, Term: pr.term}, rates)
		if ok {
			monthlyUSD += v
		}
	}
	if !ratesOK {
		view.RatesNote = "exchange rates unavailable — showing USD only"
	}
	if monthly, ok := pricing.ConvertUSD(monthlyUSD, settings.currency, rates); ok && settings.currency != "USD" {
		view.MonthlyCost = priceDisplay(settings.currency, monthly, model.TermMonthly)
		// Yearly converts the RAW usd sum × 12 once — multiplying the
		// already-rounded converted monthly would drift.
		yearly, _ := pricing.ConvertUSD(monthlyUSD*12, settings.currency, rates)
		view.YearlyCost = priceDisplay(settings.currency, yearly, model.TermAnnual)
	} else {
		view.MonthlyCost = fmt.Sprintf("$%.2f/mo", monthlyUSD)
		view.YearlyCost = fmt.Sprintf("$%.2f/yr", monthlyUSD*12)
	}

	// Due-soon list + count: active services with active pricings only.
	today := time.Now()
	windowEnd := today.AddDate(0, 0, settings.dueSoon).Format(time.DateOnly)
	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM pricings a
		WHERE a.active = 1 AND a.next_due_date IS NOT NULL AND a.next_due_date <= ?
		AND `+model.PricingActiveServiceSQL(), windowEnd).
		Scan(&view.DueSoonCount)
	if err != nil {
		return nil, err
	}

	dueRows, err := s.db.QueryContext(ctx, `
		SELECT a.service_id, a.service_type, a.currency, a.price, a.term, a.next_due_date,
			`+model.TargetNameSQL+` AS target
		FROM pricings a
		WHERE a.active = 1 AND a.next_due_date IS NOT NULL AND a.next_due_date <= ?
		AND `+model.PricingActiveServiceSQL()+`
		ORDER BY a.next_due_date ASC
		LIMIT 50`, windowEnd)
	if err != nil {
		return nil, err
	}
	for dueRows.Next() {
		var serviceID int64
		var serviceType int
		var currency string
		var price float64
		var term int
		var due, target string
		if err := dueRows.Scan(&serviceID, &serviceType, &currency, &price, &term, &due, &target); err != nil {
			dueRows.Close()
			return nil, err
		}
		_, class := dueDisplay(sql.NullString{String: due, Valid: true}, settings.dueSoon)
		view.DueSoon = append(view.DueSoon, dueSoonRow{
			Name:      target,
			URL:       fmt.Sprintf("%s/%d", model.ServiceBasePath(serviceType), serviceID),
			TypeLabel: model.ServiceTypeLabel(serviceType),
			Price:     priceDisplay(currency, price, term),
			Due:       due,
			DueClass:  class,
		})
	}
	dueRows.Close()
	if err := dueRows.Err(); err != nil {
		return nil, err
	}

	// Recently added across all types.
	recent, err := s.db.QueryContext(ctx, `
		SELECT id, t, name, created_at FROM (
			SELECT id, 1 AS t, hostname AS name, created_at FROM servers
			UNION ALL SELECT id, 2, main_domain, created_at FROM shared_hosting
			UNION ALL SELECT id, 3, main_domain, created_at FROM reseller_hosting
			UNION ALL SELECT id, 4, domain, created_at FROM domains
			UNION ALL SELECT id, 5, name, created_at FROM misc_services
			UNION ALL SELECT id, 6, hostname, created_at FROM seedboxes
		) ORDER BY created_at DESC, id DESC LIMIT ?`, settings.recentlyAdded)
	if err != nil {
		return nil, err
	}
	for recent.Next() {
		var id, t int64
		var name, created string
		if err := recent.Scan(&id, &t, &name, &created); err != nil {
			recent.Close()
			return nil, err
		}
		view.Recent = append(view.Recent, recentRow{
			Name:      name,
			URL:       fmt.Sprintf("%s/%d", model.ServiceBasePath(int(t)), id),
			TypeLabel: model.ServiceTypeLabel(int(t)),
			Created:   dateOnly(created),
		})
	}
	recent.Close()
	if err := recent.Err(); err != nil {
		return nil, err
	}

	// Spec summary across active servers.
	var ramMB, diskMB, bwMB int64
	err = s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(cpu), 0), COALESCE(SUM(ram_as_mb), 0), COALESCE(SUM(bandwidth_as_mb), 0)
		FROM servers WHERE active = 1`).
		Scan(&view.CPUCores, &ramMB, &bwMB)
	if err != nil {
		return nil, err
	}
	s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(d.size_as_mb), 0) FROM server_disks d
		JOIN servers s ON s.id = d.server_id WHERE s.active = 1`).Scan(&diskMB)
	s.db.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT provider_id) FROM servers WHERE provider_id IS NOT NULL").Scan(&view.Providers)
	s.db.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT location_id) FROM servers WHERE location_id IS NOT NULL").Scan(&view.Locations)

	view.RAMDisplay = fmtMB(ramMB)
	view.DiskDisplay = fmtMB(diskMB)
	view.BandwidthDisplay = fmtMB(bwMB) + " (excl. unlimited)"
	return view, nil
}

// dashSettings reads the settings row relevant to the dashboard.
type dashSettings struct {
	currency      string
	dueSoon       int
	recentlyAdded int
}

func (s *Server) dashSettings(r *http.Request) dashSettings {
	settings := s.memoSettings(r)
	out := dashSettings{
		currency:      settings.DashboardCurrency,
		dueSoon:       settings.DueSoon,
		recentlyAdded: settings.RecentlyAdded,
	}
	if out.dueSoon <= 0 {
		out.dueSoon = 14
	}
	if out.recentlyAdded <= 0 {
		out.recentlyAdded = 5
	}
	return out
}
