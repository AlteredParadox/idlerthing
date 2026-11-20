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

// yabs_overview.go — the fleet-wide YABS dashboard (GET /yabs): what was
// benchmarked recently, how providers and CPU models compare, which servers
// need a look, and which have never been benchmarked. Built from one
// HistoryAll() load plus one servers query, with the per-server maths shared
// with the server dashboard (yabs_stats.go).

import (
	"database/sql"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
	"idlerthing/internal/pricing"
)

const (
	// yabsStaleDays is how old a server's newest run can be before the
	// overview lists the server as due for another.
	yabsStaleDays = 180
	// yabsRecentRuns is how many runs the overview's feed shows.
	yabsRecentRuns = 10
	// routeYABSRuns is the complete run list the feed links to.
	routeYABSRuns = "/yabs/runs"
)

// yabsFleetServer is what the overview needs to know about a server.
type yabsFleetServer struct {
	ID       int64
	Hostname string
	Provider string // "" when none is set
	Active   bool
	Cost     float64 // per month in the display currency; 0 = unknown
}

// yabsHostSummary is one server's history reduced to what the fleet views
// compare: the newest measurement of each headline metric and its flags.
type yabsHostSummary struct {
	Srv       yabsFleetServer
	Runs      []model.YABSRun // oldest first
	GBVersion int64
	Series    []yabsSeries // one per headline metric
	Latest    []float64    // newest measurement per metric, 0 = none
	Low       []string     // labels of the metrics whose newest measurement is low
	Hardware  []yabsChange // newest run against the one before
	CPU       string       // newest run's CPU model
	Last      time.Time
}

// summarizeYABSHost reduces a server's runs (any order) to a summary, using
// the same series, baseline and low rule as the server's own dashboard.
func summarizeYABSHost(srv yabsFleetServer, runs []model.YABSRun) yabsHostSummary {
	sortYABSRuns(runs)
	h := yabsHostSummary{Srv: srv, Runs: runs, GBVersion: yabsGBVersion(runs)}
	latest := &runs[len(runs)-1]
	h.Last = yabsRunTime(&latest.YABS)
	h.CPU = strings.TrimSpace(latest.CPU.String)
	for _, m := range yabsMetrics(runs) {
		s := newYabsSeries(runs, m.Value)
		h.Series = append(h.Series, s)
		var cur float64
		if n := len(s.Points); n > 0 {
			cur = s.Points[n-1].V
			if s.low(cur) {
				h.Low = append(h.Low, m.Label)
			}
		}
		h.Latest = append(h.Latest, cur)
	}
	if len(runs) >= 2 {
		h.Hardware, _ = yabsSystemChanges(&runs[len(runs)-2].YABS, &latest.YABS)
	}
	return h
}

// Indexes of the headline metrics in yabsMetrics' fixed order.
const (
	ymSingle = iota
	ymMulti
	ymDisk4k
	ymDisk1m
	ymNetSend
	ymNetRecv
	ymCount
)

// yabsFleetGBVersion is the Geekbench major version most servers' newest
// scored run uses (the higher one on a tie), 0 when none has a score. Fleet
// statistics compare Geekbench only across servers on this version.
func yabsFleetGBVersion(hosts []yabsHostSummary) int64 {
	count := map[int64]int{}
	for _, h := range hosts {
		if h.GBVersion > 0 {
			count[h.GBVersion]++
		}
	}
	var best int64
	for v, n := range count {
		if n > count[best] || (n == count[best] && v > best) {
			best = v
		}
	}
	return best
}

// fleetValue is a server's newest measurement of metric i as the fleet
// statistics see it: Geekbench scores count only when the server is on the
// fleet's version.
func (h *yabsHostSummary) fleetValue(i int, fleetGB int64) float64 {
	if (i == ymSingle || i == ymMulti) && h.GBVersion != fleetGB {
		return 0
	}
	return h.Latest[i]
}

// yabsOverviewView is the payload of the fleet dashboard.
type yabsOverviewView struct {
	AddURL  string
	RunsURL string

	TotalRuns     int
	ActiveServers int
	Benchmarked   int // active servers with at least one run
	Flagged       int
	Uncovered     int // active servers never benchmarked, or not for yabsStaleDays

	GBLabel   string // "GB6"; "GB" when no server has a score
	Currency  string // symbol the cost columns are in
	HasCost   bool   // at least one server has a known monthly cost
	RatesNote string

	Attention []yabsAttentionRow
	Recent    []yabsRecentRow
	Providers []yabsGroupRow
	CPUs      []yabsCPURow
	Servers   []yabsFleetRow
	Coverage  []yabsCoverageRow
}

// yabsAttentionRow is a server whose newest run deserves a look.
type yabsAttentionRow struct {
	Name, URL string
	Provider  string
	LastRun   string
	Reasons   []string
}

// yabsRecentRow is one run in the feed.
type yabsRecentRow struct {
	Date      string
	RunURL    string
	Name      string
	ServerURL string
	CPU       string
	Cells     []yabsCell // single, multi, disk 4k, net send
}

// yabsGroupRow is one provider: the median, across its servers, of each
// server's newest measurement.
type yabsGroupRow struct {
	Name    string
	Servers int
	Runs    int
	Values  []string // one per headline metric
	BarPct  string   // single-core median relative to the best group, 0–100
	Cost    string   // median monthly cost
	Value   string   // median single-core points per unit of monthly cost
}

// yabsCPURow is one provider's servers on one CPU model, against every
// server on that model.
type yabsCPURow struct {
	CPU      string
	Provider string
	Servers  int
	Single   string
	Multi    string
	Delta    *yabsDelta // single-core median vs the model's median across providers
}

// yabsFleetRow is one benchmarked server.
type yabsFleetRow struct {
	Name, URL string
	Provider  string
	LastRun   string
	Runs      int
	Cells     []yabsCell // one per headline metric
	BarPct    string
	Cost      string
	Value     string
}

// yabsCoverageRow is an active server with no recent benchmark.
type yabsCoverageRow struct {
	Name, URL string
	Provider  string
	LastRun   string // "never" or a date
	Age       string // "", or "212 days ago"
	AddURL    string
}

// handleYABSIndex renders GET /yabs: the fleet dashboard.
func (s *Server) handleYABSIndex(w http.ResponseWriter, r *http.Request) {
	runs, err := (&model.YABSStore{DB: s.db}).HistoryAll(r.Context())
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	rates, _ := s.rates.Get(r.Context())
	currency := s.dashSettings(r).currency
	if _, ok := rates[currency]; !ok || currency == "" {
		currency = "USD" // no rate to convert into the dashboard currency
	}
	servers, unconverted, err := s.yabsFleetServers(r, rates, currency)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	view := buildYABSOverview(servers, runs, time.Now().UTC())
	view.Currency = strings.TrimSpace(currencySymbols[currency])
	if view.Currency == "" {
		view.Currency = currency
	}
	// Say so only when a cost is actually missing for want of a rate — with
	// every price in USD, rates being down changes nothing on this page.
	switch {
	case unconverted == 1:
		view.RatesNote = "1 server is priced in a currency with no exchange rate available; its cost is left out."
	case unconverted > 1:
		view.RatesNote = fmt.Sprintf("%d servers are priced in a currency with no exchange rate available; their costs are left out.", unconverted)
	}
	data := s.newPageData(w, r, "YABS", "yabs")
	data.Data = view
	s.render(w, r, "yabs", data)
}

// yabsFleetServers loads every server with its provider and its monthly cost
// (from its active recurring pricing) in the display currency. unconverted
// counts the active servers whose recurring price could not be converted
// because there is no rate for its currency.
func (s *Server) yabsFleetServers(r *http.Request, rates map[string]float64, currency string) (out []yabsFleetServer, unconverted int, err error) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT s.id, s.hostname, s.active, COALESCE(p.name, ''), pr.currency, pr.price, pr.term
		FROM servers s
		LEFT JOIN providers p ON p.id = s.provider_id
		LEFT JOIN pricings pr ON pr.id = (
			SELECT MAX(id) FROM pricings
			WHERE service_id = s.id AND service_type = ? AND active = 1)
		ORDER BY s.hostname COLLATE NOCASE, s.id`, model.ServiceServer)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var srv yabsFleetServer
		var cur sql.NullString
		var price sql.NullFloat64
		var term sql.NullInt64
		if err := rows.Scan(&srv.ID, &srv.Hostname, &srv.Active, &srv.Provider, &cur, &price, &term); err != nil {
			return nil, 0, err
		}
		recurring := price.Float64 > 0 && model.TermMonths(int(term.Int64)) > 0
		if usd, ok := pricing.MonthlyUSDRaw(&model.Pricing{
			Currency: cur.String, Price: price.Float64, Term: int(term.Int64)}, rates); ok {
			srv.Cost = usd
			if rate := rates[currency]; currency != "USD" && rate > 0 {
				srv.Cost = usd * rate
			}
		} else if recurring && srv.Active {
			unconverted++
		}
		out = append(out, srv)
	}
	return out, unconverted, rows.Err()
}

// buildYABSOverview derives the fleet dashboard. Statistics cover active
// servers; the feed lists every run.
func buildYABSOverview(servers []yabsFleetServer, runs []model.YABSRun, now time.Time) yabsOverviewView {
	v := yabsOverviewView{AddURL: routeYABSNew, RunsURL: routeYABSRuns, TotalRuns: len(runs), GBLabel: "GB"}

	byServer := map[int64][]model.YABSRun{}
	for _, run := range runs {
		byServer[run.ServerID] = append(byServer[run.ServerID], run)
	}
	var hosts []yabsHostSummary // active servers with runs
	byID := map[int64]*yabsHostSummary{}
	var all []yabsHostSummary // every server with runs (the feed needs them)
	for _, srv := range servers {
		if srv.Active {
			v.ActiveServers++
		}
		if len(byServer[srv.ID]) == 0 {
			if srv.Active {
				v.Coverage = append(v.Coverage, yabsCoverageRow{
					Name: srv.Hostname, URL: serverYABSURL(srv.ID), Provider: srv.Provider,
					LastRun: "never", AddURL: serverYABSURL(srv.ID) + "/new",
				})
			}
			continue
		}
		all = append(all, summarizeYABSHost(srv, byServer[srv.ID]))
	}
	for i := range all {
		byID[all[i].Srv.ID] = &all[i]
		if all[i].Srv.Active {
			hosts = append(hosts, all[i])
		}
	}
	v.Benchmarked = len(hosts)

	fleetGB := yabsFleetGBVersion(hosts)
	if fleetGB > 0 {
		v.GBLabel += strconv.FormatInt(fleetGB, 10)
	}

	for i := range hosts {
		h := &hosts[i]
		if h.Srv.Cost > 0 {
			v.HasCost = true
		}
		if !h.Last.IsZero() && now.Sub(h.Last) > yabsStaleDays*24*time.Hour {
			v.Coverage = append(v.Coverage, yabsCoverageRow{
				Name: h.Srv.Hostname, URL: serverYABSURL(h.Srv.ID), Provider: h.Srv.Provider,
				LastRun: h.Last.Format(time.DateOnly),
				Age:     fmt.Sprintf("%d days ago", int(now.Sub(h.Last).Hours()/24)),
				AddURL:  serverYABSURL(h.Srv.ID) + "/new",
			})
		}
		if row, ok := yabsAttention(h); ok {
			v.Attention = append(v.Attention, row)
		}
	}
	v.Flagged = len(v.Attention)
	v.Uncovered = len(v.Coverage)

	v.Recent = yabsRecent(runs, byID)
	v.Providers = yabsProviderRows(hosts, fleetGB)
	v.CPUs = yabsCPURows(hosts, fleetGB)
	v.Servers = yabsFleetRows(hosts, fleetGB)
	return v
}

// yabsAttention reports why a server's newest run needs a look, if it does.
func yabsAttention(h *yabsHostSummary) (yabsAttentionRow, bool) {
	row := yabsAttentionRow{
		Name: h.Srv.Hostname, URL: serverYABSURL(h.Srv.ID), Provider: h.Srv.Provider,
		LastRun: yabsDate(&h.Runs[len(h.Runs)-1].YABS, false),
	}
	if len(h.Low) > 0 {
		row.Reasons = append(row.Reasons, fmt.Sprintf("%.0f%% or more below its own median: %s",
			yabsLowPct, strings.Join(h.Low, ", ")))
	}
	for _, c := range h.Hardware {
		row.Reasons = append(row.Reasons, fmt.Sprintf("%s changed: %s → %s", c.Field, c.From, c.To))
	}
	return row, len(row.Reasons) > 0
}

// yabsRecent is the newest runs across all servers, each against the run
// before it on the same server.
func yabsRecent(runs []model.YABSRun, hosts map[int64]*yabsHostSummary) []yabsRecentRow {
	type ref struct {
		host *yabsHostSummary
		idx  int // position in host.Runs
	}
	var refs []ref
	for _, h := range hosts {
		for i := range h.Runs {
			refs = append(refs, ref{h, i})
		}
	}
	sort.Slice(refs, func(a, b int) bool {
		ra, rb := &refs[a].host.Runs[refs[a].idx], &refs[b].host.Runs[refs[b].idx]
		ta, tb := yabsRunTime(&ra.YABS), yabsRunTime(&rb.YABS)
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return ra.ID > rb.ID
	})
	if len(refs) > yabsRecentRuns {
		refs = refs[:yabsRecentRuns]
	}
	var out []yabsRecentRow
	for _, rf := range refs {
		h, run := rf.host, &rf.host.Runs[rf.idx]
		row := yabsRecentRow{
			Date:      yabsDate(&run.YABS, true),
			RunURL:    yabsRunURL(h.Srv.ID, run.ID),
			Name:      h.Srv.Hostname,
			ServerURL: serverYABSURL(h.Srv.ID),
			CPU:       strings.TrimSpace(run.CPU.String),
		}
		for _, mi := range []int{ymSingle, ymMulti, ymDisk4k, ymNetSend} {
			cell := yabsCell{Value: "—"}
			s := h.Series[mi]
			if pos := s.at(rf.idx); pos >= 0 {
				cell.Value = fmtYABS(s.Points[pos].V)
				if pos > 0 {
					cell.Delta = newYabsDelta(s.Points[pos].V, s.Points[pos-1].V, false)
				}
				cell.Low = s.low(s.Points[pos].V)
			} else if other := otherVersionScore(run, yabsMetricKeys[mi], h.GBVersion); other != "" {
				cell.Value = other
			}
			row.Cells = append(row.Cells, cell)
		}
		out = append(out, row)
	}
	return out
}

// yabsMetricKeys mirrors yabsMetrics' keys by index.
var yabsMetricKeys = [ymCount]string{"gb_single", "gb_multi", "disk_4k", "disk_1m", "net_send", "net_recv"}

// barPct scales v against the largest value in its column, for the inline
// bars ("" when there is nothing to draw).
func barPct(v, max float64) string {
	if v <= 0 || max <= 0 {
		return ""
	}
	return strconv.FormatFloat(v/max*100, 'f', 1, 64)
}

// fmtCost renders a monthly cost ("—" when unknown).
func fmtCost(v float64) string {
	if v <= 0 {
		return "—"
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// yabsProviderRows groups servers by provider. Each figure is the median
// across the provider's servers of each server's newest measurement, so one
// unusually fast or slow box does not stand for the provider.
func yabsProviderRows(hosts []yabsHostSummary, fleetGB int64) []yabsGroupRow {
	type group struct {
		name   string
		hosts  []*yabsHostSummary
		single float64
	}
	groups := map[string]*group{}
	var order []*group
	for i := range hosts {
		name := hosts[i].Srv.Provider
		g := groups[name]
		if g == nil {
			g = &group{name: name}
			groups[name] = g
			order = append(order, g)
		}
		g.hosts = append(g.hosts, &hosts[i])
	}
	col := func(g *group, mi int) float64 {
		var vals []float64
		for _, h := range g.hosts {
			if v := h.fleetValue(mi, fleetGB); v > 0 {
				vals = append(vals, v)
			}
		}
		return median(vals)
	}
	var best float64
	for _, g := range order {
		g.single = col(g, ymSingle)
		if g.single > best {
			best = g.single
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		if order[a].single != order[b].single {
			return order[a].single > order[b].single
		}
		return strings.ToLower(order[a].name) < strings.ToLower(order[b].name)
	})
	var out []yabsGroupRow
	for _, g := range order {
		row := yabsGroupRow{Name: g.name, Servers: len(g.hosts), BarPct: barPct(g.single, best)}
		if row.Name == "" {
			row.Name = "No provider set"
		}
		var costs, values []float64
		for _, h := range g.hosts {
			row.Runs += len(h.Runs)
			if h.Srv.Cost > 0 {
				costs = append(costs, h.Srv.Cost)
				if single := h.fleetValue(ymSingle, fleetGB); single > 0 {
					values = append(values, single/h.Srv.Cost)
				}
			}
		}
		for mi := 0; mi < ymCount; mi++ {
			row.Values = append(row.Values, fmtYABS(col(g, mi)))
		}
		row.Cost, row.Value = fmtCost(median(costs)), fmtYABS(median(values))
		out = append(out, row)
	}
	return out
}

// yabsCPURows compares providers on identical CPU models: the same chip
// scoring lower at one provider is the clearest sign of an oversold node.
// Only models found at two or more providers are listed — with one provider
// there is nobody to compare against.
func yabsCPURows(hosts []yabsHostSummary, fleetGB int64) []yabsCPURow {
	type key struct{ cpu, provider string }
	single, multi := map[key][]float64{}, map[key][]float64{}
	perCPU := map[string][]float64{}
	providers := map[string]int{} // CPU model → number of providers running it
	var keys []key
	for i := range hosts {
		h := &hosts[i]
		s := h.fleetValue(ymSingle, fleetGB)
		if h.CPU == "" || s <= 0 {
			continue
		}
		k := key{h.CPU, h.Srv.Provider}
		if _, seen := single[k]; !seen {
			keys = append(keys, k)
			providers[h.CPU]++
		}
		single[k] = append(single[k], s)
		if m := h.fleetValue(ymMulti, fleetGB); m > 0 {
			multi[k] = append(multi[k], m)
		}
		perCPU[h.CPU] = append(perCPU[h.CPU], s)
	}
	sort.SliceStable(keys, func(a, b int) bool {
		if keys[a].cpu != keys[b].cpu {
			return strings.ToLower(keys[a].cpu) < strings.ToLower(keys[b].cpu)
		}
		return median(single[keys[a]]) > median(single[keys[b]])
	})
	var out []yabsCPURow
	for _, k := range keys {
		if providers[k.cpu] < 2 {
			continue
		}
		row := yabsCPURow{
			CPU: k.cpu, Provider: k.provider, Servers: len(single[k]),
			Single: fmtYABS(median(single[k])), Multi: fmtYABS(median(multi[k])),
			Delta: newYabsDelta(median(single[k]), median(perCPU[k.cpu]), false),
		}
		if row.Provider == "" {
			row.Provider = "No provider set"
		}
		out = append(out, row)
	}
	return out
}

// yabsFleetRows lists every benchmarked active server, fastest single-core
// first; servers without a comparable score go last, by name.
func yabsFleetRows(hosts []yabsHostSummary, fleetGB int64) []yabsFleetRow {
	sorted := make([]*yabsHostSummary, len(hosts))
	var best float64
	for i := range hosts {
		sorted[i] = &hosts[i]
		if v := hosts[i].fleetValue(ymSingle, fleetGB); v > best {
			best = v
		}
	}
	sort.SliceStable(sorted, func(a, b int) bool {
		va, vb := sorted[a].fleetValue(ymSingle, fleetGB), sorted[b].fleetValue(ymSingle, fleetGB)
		if va != vb {
			return va > vb
		}
		return strings.ToLower(sorted[a].Srv.Hostname) < strings.ToLower(sorted[b].Srv.Hostname)
	})
	var out []yabsFleetRow
	for _, h := range sorted {
		latest := &h.Runs[len(h.Runs)-1]
		row := yabsFleetRow{
			Name: h.Srv.Hostname, URL: serverYABSURL(h.Srv.ID), Provider: h.Srv.Provider,
			LastRun: yabsDate(&latest.YABS, false), Runs: len(h.Runs),
			BarPct: barPct(h.fleetValue(ymSingle, fleetGB), best),
			Cost:   fmtCost(h.Srv.Cost), Value: "—",
		}
		low := map[string]bool{}
		for _, l := range h.Low {
			low[l] = true
		}
		metrics := yabsMetrics(h.Runs)
		for mi := 0; mi < ymCount; mi++ {
			cell := yabsCell{Value: fmtYABS(h.Latest[mi]), Low: low[metrics[mi].Label]}
			// A score the fleet statistics do not use — another Geekbench
			// version, or no recorded version — is shown, marked, and kept
			// out of the ranking and the group medians.
			if (mi == ymSingle || mi == ymMulti) && h.fleetValue(mi, fleetGB) == 0 {
				if raw := newestUncomparedScore(h.Runs, yabsMetricKeys[mi], fleetGB); raw != "" {
					cell.Value = raw
				}
			}
			row.Cells = append(row.Cells, cell)
		}
		if single := h.fleetValue(ymSingle, fleetGB); single > 0 && h.Srv.Cost > 0 {
			row.Value = fmtYABS(single / h.Srv.Cost)
		}
		out = append(out, row)
	}
	return out
}

// newestUncomparedScore is the newest Geekbench score in a history that is
// not on the given version, formatted with its version marker ("900 · v5",
// "1,900 · v?"); "" when there is none.
func newestUncomparedScore(runs []model.YABSRun, key string, version int64) string {
	for i := len(runs) - 1; i >= 0; i-- {
		if other := otherVersionScore(&runs[i], key, version); other != "" {
			return other
		}
	}
	return ""
}

// handleYABSRuns renders GET /yabs/runs: every run, newest first.
func (s *Server) handleYABSRuns(w http.ResponseWriter, r *http.Request) {
	items, err := (&model.YABSStore{DB: s.db}).ListAll(r.Context())
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, "YABS runs", "yabs")
	data.Data = yabsIndexView{Rows: yabsRows(items), AddURL: routeYABSNew}
	s.render(w, r, "yabs_runs", data)
}
