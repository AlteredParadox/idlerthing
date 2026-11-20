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
// compare: what its latest run measured, and that run's flags.
type yabsHostSummary struct {
	Srv  yabsFleetServer
	Runs []model.YABSRun // oldest first
	// LatestGB is the Geekbench major version of the latest run's score, 0
	// when that run has no score or does not record its version. It is what
	// the server contributes to the fleet's Geekbench statistics — an older
	// run's version says nothing about a figure the latest run did not
	// produce.
	LatestGB int64
	Series   []yabsSeries // one per headline metric
	// Latest is what the LATEST RUN measured per metric; 0 when that run
	// did not measure it. An older run's value is never carried forward:
	// these feed "latest run" tables, medians and alerts, and the CPU they
	// are grouped under is the latest run's.
	Latest   []float64
	Low      []string     // labels of the metrics the latest run measured low
	Hardware []yabsChange // latest run against the one before
	CPU      string       // latest run's CPU model
	Last     time.Time
}

// summarizeYABSHost reduces a server's runs (any order) to a summary, using
// the same series, baseline and low rule as the server's own dashboard, so
// what is flagged here is exactly what that page flags on its latest row.
func summarizeYABSHost(srv yabsFleetServer, runs []model.YABSRun) yabsHostSummary {
	sortYABSRuns(runs)
	last := len(runs) - 1
	h := yabsHostSummary{Srv: srv, Runs: runs}
	if yabsHasGB(&runs[last].YABS) {
		h.LatestGB = runs[last].GeekbenchVersion.Int64
	}
	h.Last = yabsRunTime(&runs[last].YABS)
	h.CPU = strings.TrimSpace(runs[last].CPU.String)
	for _, m := range yabsMetrics(runs) {
		s := m.series(runs)
		h.Series = append(h.Series, s)
		var cur float64
		if pos := s.at(last); pos >= 0 {
			cur = s.Points[pos].V
			if s.lowAt(pos) {
				h.Low = append(h.Low, m.Label)
			}
		}
		h.Latest = append(h.Latest, cur)
	}
	if last >= 1 {
		h.Hardware, _ = yabsSystemChanges(&runs[last-1].YABS, &runs[last].YABS)
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

// yabsFleetGBVersion is the Geekbench major version most servers' latest
// scores use (the higher one on a tie), 0 when no latest run has a versioned
// score. Only a server whose latest run carries such a score gets a vote:
// those are the scores the latest-run tables can show, so a server with an
// old score and a newer run that skipped Geekbench must not pick a version
// nothing current is on.
func yabsFleetGBVersion(hosts []yabsHostSummary) int64 {
	count := map[int64]int{}
	for _, h := range hosts {
		if h.LatestGB > 0 {
			count[h.LatestGB]++
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

// fleetValue is what the server's latest run measured for metric i, as the
// fleet statistics see it: Geekbench scores count only when that run is on
// the fleet's version.
func (h *yabsHostSummary) fleetValue(i int, fleetGB int64) float64 {
	if (i == ymSingle || i == ymMulti) && (fleetGB <= 0 || h.LatestGB != fleetGB) {
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

	all := v.summarize(servers, runs, now)
	var hosts []yabsHostSummary // the active ones: what the statistics cover
	for _, h := range all {
		if h.Srv.Active {
			hosts = append(hosts, h)
		}
	}
	v.Benchmarked = len(hosts)

	fleetGB := yabsFleetGBVersion(hosts)
	if fleetGB > 0 {
		v.GBLabel += strconv.FormatInt(fleetGB, 10)
	}
	for i := range hosts {
		h := &hosts[i]
		v.HasCost = v.HasCost || h.Srv.Cost > 0
		if !h.Last.IsZero() && now.Sub(h.Last) > yabsStaleDays*24*time.Hour {
			v.Coverage = append(v.Coverage, yabsCoverage(h.Srv, h.Last, now))
		}
		if row, ok := yabsAttention(h); ok {
			v.Attention = append(v.Attention, row)
		}
	}
	v.Flagged = len(v.Attention)
	v.Uncovered = len(v.Coverage)

	v.Recent = yabsRecent(all, fleetGB)
	v.Providers = yabsProviderRows(hosts, fleetGB)
	v.CPUs = yabsCPURows(hosts, fleetGB)
	v.Servers = yabsFleetRows(hosts, fleetGB)
	return v
}

// summarize reduces every server that has runs to a summary (the feed needs
// inactive servers' runs too), counting the active servers and listing the
// active ones that have never been benchmarked.
func (v *yabsOverviewView) summarize(servers []yabsFleetServer, runs []model.YABSRun, now time.Time) []yabsHostSummary {
	byServer := map[int64][]model.YABSRun{}
	for _, run := range runs {
		byServer[run.ServerID] = append(byServer[run.ServerID], run)
	}
	var all []yabsHostSummary
	for _, srv := range servers {
		if srv.Active {
			v.ActiveServers++
		}
		switch {
		case len(byServer[srv.ID]) > 0:
			all = append(all, summarizeYABSHost(srv, byServer[srv.ID]))
		case srv.Active:
			v.Coverage = append(v.Coverage, yabsCoverage(srv, time.Time{}, now))
		}
	}
	return all
}

// yabsCoverage is the coverage-gap row of a server: never benchmarked when
// last is zero, otherwise not since last.
func yabsCoverage(srv yabsFleetServer, last, now time.Time) yabsCoverageRow {
	row := yabsCoverageRow{
		Name: srv.Hostname, URL: serverYABSURL(srv.ID), Provider: srv.Provider,
		LastRun: "never", AddURL: serverYABSURL(srv.ID) + "/new",
	}
	if !last.IsZero() {
		row.LastRun = last.Format(time.DateOnly)
		row.Age = fmt.Sprintf("%d days ago", int(now.Sub(last).Hours()/24))
	}
	return row
}

// yabsAttention reports why a server's latest run needs a look, if it does.
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

// yabsFeedRef points at one run of one server.
type yabsFeedRef struct {
	host *yabsHostSummary
	idx  int // position in host.Runs
}

func (rf yabsFeedRef) run() *model.YABSRun { return &rf.host.Runs[rf.idx] }

// yabsRecent is the newest runs across all servers, each against the run
// before it on the same server.
func yabsRecent(hosts []yabsHostSummary, fleetGB int64) []yabsRecentRow {
	var refs []yabsFeedRef
	for i := range hosts {
		for j := range hosts[i].Runs {
			refs = append(refs, yabsFeedRef{&hosts[i], j})
		}
	}
	sort.Slice(refs, func(a, b int) bool {
		ta, tb := yabsRunTime(&refs[a].run().YABS), yabsRunTime(&refs[b].run().YABS)
		if !ta.Equal(tb) {
			return ta.After(tb)
		}
		return refs[a].run().ID > refs[b].run().ID
	})
	if len(refs) > yabsRecentRuns {
		refs = refs[:yabsRecentRuns]
	}
	var out []yabsRecentRow
	for _, rf := range refs {
		h, run := rf.host, rf.run()
		row := yabsRecentRow{
			Date:      yabsDate(&run.YABS, true),
			RunURL:    yabsRunURL(h.Srv.ID, run.ID),
			Name:      h.Srv.Hostname,
			ServerURL: serverYABSURL(h.Srv.ID),
			CPU:       strings.TrimSpace(run.CPU.String),
		}
		for _, mi := range []int{ymSingle, ymMulti, ymDisk4k, ymNetSend} {
			row.Cells = append(row.Cells, yabsFeedCell(rf, mi, fleetGB))
		}
		out = append(out, row)
	}
	return out
}

// yabsFeedCell is one metric of one run in the feed. The change and the low
// mark come from the server's own history, as on its dashboard.
//
// A Geekbench cell always shows the score the run recorded, whether or not
// that run is part of the server's current series (a server that moved from
// GB5 to GB6 still has GB5 runs in the feed). The columns are headed with the
// fleet's version, so a score on any other version — or with none recorded —
// carries its own marker ("900 · v5", "1,900 · v?") rather than sitting
// unmarked under that heading.
func yabsFeedCell(rf yabsFeedRef, mi int, fleetGB int64) yabsCell {
	cell := yabsCell{Value: "—"}
	s := rf.host.Series[mi]
	if pos := s.at(rf.idx); pos >= 0 {
		cell.Value = fmtYABS(s.Points[pos].V)
		cell.Delta = s.deltaPrev(pos)
		cell.Low = s.lowAt(pos)
	}
	if mi != ymSingle && mi != ymMulti {
		return cell
	}
	run := rf.run()
	score := run.GbSingle.Int64
	if mi == ymMulti {
		score = run.GbMulti.Int64
	}
	if score <= 0 {
		return cell
	}
	cell.Value = commaInt(score)
	if marked := otherVersionScore(run, yabsMetricKeys[mi], fleetGB); marked != "" {
		cell.Value = marked
	}
	return cell
}

// yabsMetricKeys mirrors yabsMetrics' keys by index.
var yabsMetricKeys = [ymCount]string{"gb_single", "gb_multi", "disk_4k", "disk_1m", "net_send", "net_recv"}

// barPct scales v against the largest value in its column, for the inline
// bars ("" when there is nothing to draw).
func barPct(v, top float64) string {
	if v <= 0 || top <= 0 {
		return ""
	}
	return strconv.FormatFloat(v/top*100, 'f', 1, 64)
}

// fmtCost renders a monthly cost ("—" when unknown).
func fmtCost(v float64) string {
	if v <= 0 {
		return "—"
	}
	return strconv.FormatFloat(v, 'f', 2, 64)
}

// yabsProvider is the servers of one provider.
type yabsProvider struct {
	name  string
	hosts []*yabsHostSummary
}

// col is the provider's figure for metric mi: the median across its servers
// of what each server's latest run measured.
func (g *yabsProvider) col(mi int, fleetGB int64) float64 {
	var vals []float64
	for _, h := range g.hosts {
		if v := h.fleetValue(mi, fleetGB); v > 0 {
			vals = append(vals, v)
		}
	}
	return median(vals)
}

// row renders the provider. Cost is the median over the servers that have
// one; value is the median of each server's single-core points per unit of
// its own cost.
func (g *yabsProvider) row(fleetGB int64, best float64) yabsGroupRow {
	row := yabsGroupRow{Name: g.name, Servers: len(g.hosts), BarPct: barPct(g.col(ymSingle, fleetGB), best)}
	if row.Name == "" {
		row.Name = "No provider set"
	}
	var costs, values []float64
	for _, h := range g.hosts {
		row.Runs += len(h.Runs)
		if h.Srv.Cost <= 0 {
			continue
		}
		costs = append(costs, h.Srv.Cost)
		if single := h.fleetValue(ymSingle, fleetGB); single > 0 {
			values = append(values, single/h.Srv.Cost)
		}
	}
	for mi := 0; mi < ymCount; mi++ {
		row.Values = append(row.Values, fmtYABS(g.col(mi, fleetGB)))
	}
	row.Cost, row.Value = fmtCost(median(costs)), fmtYABS(median(values))
	return row
}

// yabsProviderRows groups servers by provider, best single-core median
// first. Medians, so one unusually fast or slow box does not stand for the
// provider.
func yabsProviderRows(hosts []yabsHostSummary, fleetGB int64) []yabsGroupRow {
	groups := map[string]*yabsProvider{}
	var order []*yabsProvider
	for i := range hosts {
		name := hosts[i].Srv.Provider
		if groups[name] == nil {
			groups[name] = &yabsProvider{name: name}
			order = append(order, groups[name])
		}
		groups[name].hosts = append(groups[name].hosts, &hosts[i])
	}
	var best float64
	for _, g := range order {
		if v := g.col(ymSingle, fleetGB); v > best {
			best = v
		}
	}
	sort.SliceStable(order, func(a, b int) bool {
		sa, sb := order[a].col(ymSingle, fleetGB), order[b].col(ymSingle, fleetGB)
		if sa != sb {
			return sa > sb
		}
		return strings.ToLower(order[a].name) < strings.ToLower(order[b].name)
	})
	var out []yabsGroupRow
	for _, g := range order {
		out = append(out, g.row(fleetGB, best))
	}
	return out
}

// yabsCPUKey is one CPU model at one provider.
type yabsCPUKey struct{ cpu, provider string }

// yabsCPURows compares providers on identical CPU models: the same chip
// scoring lower at one provider is the clearest sign of an oversold node.
// Only models found at two or more providers are listed — with one provider
// there is nobody to compare against. A server counts under the CPU its
// latest run reports, with the score that same run measured.
func yabsCPURows(hosts []yabsHostSummary, fleetGB int64) []yabsCPURow {
	single, multi := map[yabsCPUKey][]float64{}, map[yabsCPUKey][]float64{}
	perCPU := map[string][]float64{}
	providers := map[string]int{} // CPU model → number of providers running it
	var keys []yabsCPUKey
	for i := range hosts {
		h := &hosts[i]
		s := h.fleetValue(ymSingle, fleetGB)
		if h.CPU == "" || s <= 0 {
			continue
		}
		k := yabsCPUKey{h.CPU, h.Srv.Provider}
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

// yabsFleetRows lists every benchmarked active server by its latest run,
// fastest single-core first; servers without a comparable score go last, by
// name.
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
		out = append(out, h.fleetRow(fleetGB, best))
	}
	return out
}

// fleetRow renders the server's latest run. A metric that run did not
// measure reads "—" even when an earlier run has a value.
func (h *yabsHostSummary) fleetRow(fleetGB int64, best float64) yabsFleetRow {
	latest := &h.Runs[len(h.Runs)-1]
	single := h.fleetValue(ymSingle, fleetGB)
	row := yabsFleetRow{
		Name: h.Srv.Hostname, URL: serverYABSURL(h.Srv.ID), Provider: h.Srv.Provider,
		LastRun: yabsDate(&latest.YABS, false), Runs: len(h.Runs),
		BarPct: barPct(single, best), Cost: fmtCost(h.Srv.Cost), Value: "—",
	}
	if single > 0 && h.Srv.Cost > 0 {
		row.Value = fmtYABS(single / h.Srv.Cost)
	}
	low := map[string]bool{}
	for _, l := range h.Low {
		low[l] = true
	}
	for mi, m := range yabsMetrics(h.Runs) {
		cell := yabsCell{Value: fmtYABS(h.Latest[mi]), Low: low[m.Label]}
		// A score the fleet statistics do not use — another Geekbench
		// version, or no recorded version — is shown, marked, and kept out
		// of the ranking and the group medians.
		if mi == ymSingle || mi == ymMulti {
			if marked := otherVersionScore(latest, yabsMetricKeys[mi], fleetGB); marked != "" {
				cell.Value = marked
			}
		}
		row.Cells = append(row.Cells, cell)
	}
	return row
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
