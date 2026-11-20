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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"idlerthing/internal/model"
)

// fr describes one run of the fleet fixtures: statRun for a given server and
// CPU model.
type fr struct {
	id, server           int64
	at, cpu              string
	single, multi, gbVer int64
	disk4k, net          float64
}

func (f fr) run() model.YABSRun {
	r := statRun(f.id, f.at, f.single, f.multi, f.gbVer, f.disk4k, f.net)
	r.ServerID = f.server
	r.CPU = ns(f.cpu)
	r.CPUCores = sqlNi(4)
	return r
}

var overviewNow = time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)

// overviewFleet is a small fleet exercising every section:
//
//	1 alpha    Acme   $10  EPYC  steady, 3 runs
//	2 bravo    Acme   $5   EPYC  latest run halved on a different CPU (flagged)
//	3 charlie  Bolt   $20  EPYC  1 run, slower than Acme's EPYCs
//	4 delta    Bolt   —    Xeon  only Geekbench 5 (other version), no pricing
//	5 echo     ""     $4   Xeon  last run 300 days ago (stale)
//	6 foxtrot  Bolt   $9   —     active, never benchmarked
//	7 golf     Acme   $9   EPYC  inactive: out of the statistics, in the feed
func overviewFleet() ([]yabsFleetServer, []model.YABSRun) {
	servers := []yabsFleetServer{
		{ID: 1, Hostname: "alpha", Provider: "Acme", Active: true, Cost: 10},
		{ID: 2, Hostname: "bravo", Provider: "Acme", Active: true, Cost: 5},
		{ID: 3, Hostname: "charlie", Provider: "Bolt", Active: true, Cost: 20},
		{ID: 4, Hostname: "delta", Provider: "Bolt", Active: true},
		{ID: 5, Hostname: "echo", Active: true, Cost: 4},
		{ID: 6, Hostname: "foxtrot", Provider: "Bolt", Active: true, Cost: 9},
		{ID: 7, Hostname: "golf", Provider: "Acme", Active: false, Cost: 9},
	}
	const epyc, xeon, old = "AMD EPYC 7543", "Intel Xeon Gold 6152", "Intel Xeon E5-2680 v2"
	runs := []model.YABSRun{
		fr{1, 1, "2026-09-01 00:00:00", epyc, 1700, 6000, 6, 100, 800}.run(),
		fr{2, 1, "2026-10-01 00:00:00", epyc, 1720, 6050, 6, 101, 810}.run(),
		fr{3, 1, "2026-11-01 00:00:00", epyc, 1710, 6020, 6, 99, 790}.run(),
		fr{4, 2, "2026-08-01 00:00:00", epyc, 1650, 3100, 6, 80, 600}.run(),
		fr{5, 2, "2026-09-02 00:00:00", epyc, 1660, 3120, 6, 81, 610}.run(),
		fr{6, 2, "2026-10-02 00:00:00", epyc, 1640, 3090, 6, 79, 590}.run(),
		fr{7, 2, "2026-11-10 00:00:00", old, 760, 1420, 6, 40, 300}.run(),
		fr{8, 3, "2026-11-05 00:00:00", epyc, 1500, 5000, 6, 60, 700}.run(),
		fr{9, 4, "2026-11-12 00:00:00", xeon, 900, 2100, 5, 50, 500}.run(),
		fr{10, 5, "2026-01-24 00:00:00", xeon, 1100, 2900, 6, 70, 400}.run(),
		fr{11, 7, "2026-11-15 00:00:00", epyc, 1800, 6500, 6, 120, 900}.run(),
	}
	return servers, runs
}

func TestYABSOverviewCountsAndCoverage(t *testing.T) {
	servers, runs := overviewFleet()
	v := buildYABSOverview(servers, runs, overviewNow)

	if v.TotalRuns != 11 || v.ActiveServers != 6 || v.Benchmarked != 5 {
		t.Fatalf("runs=%d active=%d benchmarked=%d, want 11/6/5", v.TotalRuns, v.ActiveServers, v.Benchmarked)
	}
	if v.GBLabel != "GB6" || !v.HasCost {
		t.Errorf("GBLabel=%q HasCost=%v", v.GBLabel, v.HasCost)
	}
	// Coverage: the never-benchmarked server and the one whose last run is
	// 300 days old. The inactive server is not asked for a benchmark.
	if v.Uncovered != 2 || len(v.Coverage) != 2 {
		t.Fatalf("coverage = %+v", v.Coverage)
	}
	never, stale := v.Coverage[0], v.Coverage[1]
	if never.Name != "foxtrot" || never.LastRun != "never" || never.Age != "" || never.AddURL != "/servers/6/yabs/new" {
		t.Errorf("never row = %+v", never)
	}
	if stale.Name != "echo" || stale.LastRun != "2026-01-24" || stale.Age != "300 days ago" {
		t.Errorf("stale row = %+v", stale)
	}

	// Attention: only bravo — its newest run is far below its own median and
	// on a different CPU.
	if v.Flagged != 1 || len(v.Attention) != 1 || v.Attention[0].Name != "bravo" {
		t.Fatalf("attention = %+v", v.Attention)
	}
	reasons := strings.Join(v.Attention[0].Reasons, " | ")
	for _, want := range []string{"15% or more below its own median", "Geekbench 6 single", "CPU changed: AMD EPYC 7543 → Intel Xeon E5-2680 v2"} {
		if !strings.Contains(reasons, want) {
			t.Errorf("attention reasons %q missing %q", reasons, want)
		}
	}
}

func TestYABSOverviewProviders(t *testing.T) {
	servers, runs := overviewFleet()
	v := buildYABSOverview(servers, runs, overviewNow)

	if len(v.Providers) != 3 {
		t.Fatalf("providers = %+v", v.Providers)
	}
	// Ordered by single-core median, best first:
	//   Bolt   charlie 1500 (delta is on Geekbench 5 and does not count)
	//   Acme   median(alpha 1710, bravo 760) = 1235 — golf is inactive
	//   (none) echo 1100
	bolt, acme, none := v.Providers[0], v.Providers[1], v.Providers[2]
	if bolt.Name != "Bolt" || acme.Name != "Acme" || none.Name != "No provider set" {
		t.Fatalf("provider order = %q, %q, %q", bolt.Name, acme.Name, none.Name)
	}
	if acme.Servers != 2 || acme.Runs != 7 || acme.Values[ymSingle] != "1,235" || acme.Values[ymMulti] != "3,720" {
		t.Errorf("Acme = %+v", acme)
	}
	// Disk has no version, so both Bolt servers count: 4k totals 120 and 100.
	if bolt.Servers != 2 || bolt.Values[ymSingle] != "1,500" || bolt.Values[ymDisk4k] != "110" {
		t.Errorf("Bolt = %+v", bolt)
	}
	if bolt.BarPct != "100.0" || acme.BarPct != "82.3" {
		t.Errorf("bars = %q, %q", bolt.BarPct, acme.BarPct)
	}
	// Cost is the median of the servers that have one; value is the median
	// of each server's single-core points per unit of cost.
	if acme.Cost != "7.50" || acme.Value != "162" { // median(1710/10, 760/5) = median(171, 152)
		t.Errorf("Acme cost/value = %q / %q", acme.Cost, acme.Value)
	}
	if bolt.Cost != "20.00" || bolt.Value != "75.0" { // delta has no pricing
		t.Errorf("Bolt cost/value = %q / %q", bolt.Cost, bolt.Value)
	}
}

func TestYABSOverviewSameCPU(t *testing.T) {
	servers, runs := overviewFleet()
	v := buildYABSOverview(servers, runs, overviewNow)

	// EPYC runs at Acme (alpha 1710) and Bolt (charlie 1500): model median
	// 1605. bravo's newest run is on another CPU; the Xeon Gold is at one
	// provider only on Geekbench 6, so it has nobody to be compared with.
	if len(v.CPUs) != 2 {
		t.Fatalf("cpu rows = %+v", v.CPUs)
	}
	first, second := v.CPUs[0], v.CPUs[1]
	if first.CPU != "AMD EPYC 7543" || first.Provider != "Acme" || first.Single != "1,710" || first.Delta.Text != "▲ 6.5%" || first.Delta.Class != "delta-good" {
		t.Errorf("first = %+v %+v", first, first.Delta)
	}
	if second.Provider != "Bolt" || second.Single != "1,500" || second.Delta.Text != "▼ 6.5%" || second.Delta.Class != "delta-bad" || second.Multi != "5,000" {
		t.Errorf("second = %+v %+v", second, second.Delta)
	}
}

func TestYABSOverviewServersAndRecent(t *testing.T) {
	servers, runs := overviewFleet()
	v := buildYABSOverview(servers, runs, overviewNow)

	var names []string
	for _, r := range v.Servers {
		names = append(names, r.Name)
	}
	// Fastest comparable single-core first; delta (other Geekbench version)
	// has no comparable score and goes last. golf is inactive: not listed.
	if got := strings.Join(names, ","); got != "alpha,charlie,echo,bravo,delta" {
		t.Fatalf("server order = %s", got)
	}
	bravo, delta := v.Servers[3], v.Servers[4]
	if !bravo.Cells[ymSingle].Low || bravo.Cells[ymSingle].Value != "760" || bravo.Value != "152" || bravo.Runs != 4 {
		t.Errorf("bravo = %+v", bravo)
	}
	if delta.Cells[ymSingle].Value != "900 · v5" || delta.BarPct != "" || delta.Value != "—" || delta.Cost != "—" {
		t.Errorf("delta = %+v", delta)
	}
	if v.Servers[0].BarPct != "100.0" || v.Servers[0].LastRun != "2026-11-01" {
		t.Errorf("alpha = %+v", v.Servers[0])
	}

	// The feed is every run, newest first — including the inactive server's.
	if len(v.Recent) != yabsRecentRuns {
		t.Fatalf("recent = %d rows, want %d", len(v.Recent), yabsRecentRuns)
	}
	if v.Recent[0].Name != "golf" || v.Recent[0].RunURL != "/servers/7/yabs/11" || v.Recent[0].Date != "2026-11-15 00:00" {
		t.Errorf("newest = %+v", v.Recent[0])
	}
	// bravo's drop, against its own previous run (1640 → 760).
	var drop *yabsRecentRow
	for i := range v.Recent {
		if v.Recent[i].RunURL == "/servers/2/yabs/7" {
			drop = &v.Recent[i]
		}
	}
	if drop == nil || drop.Cells[0].Value != "760" || drop.Cells[0].Delta == nil || drop.Cells[0].Delta.Text != "▼ 54%" || !drop.Cells[0].Low {
		t.Fatalf("bravo's drop in the feed = %+v", drop)
	}
	// delta's score is Geekbench 5 under a "GB6" heading: it carries its
	// version, and it still has no delta (it is that server's first run).
	var gb5 *yabsRecentRow
	for i := range v.Recent {
		if v.Recent[i].Name == "delta" {
			gb5 = &v.Recent[i]
		}
	}
	if gb5 == nil || gb5.Cells[0].Value != "900 · v5" || gb5.Cells[1].Value != "2,100 · v5" {
		t.Errorf("a score on another Geekbench version must be marked in the feed: %+v", gb5)
	}
	// A server's first run has nothing to be compared with.
	for _, r := range v.Recent {
		if r.Name == "charlie" && r.Cells[0].Delta != nil {
			t.Errorf("a first run must have no delta: %+v", r.Cells[0])
		}
	}
}

// "Latest run" means the latest run: a metric that run did not measure is
// missing, not filled in from an older one. Here "moved" was benchmarked on
// CPU A (twice normally, then once badly), then moved to CPU B where the
// newest run skipped Geekbench and disk.
func TestYABSOverviewUsesTheLatestRunOnly(t *testing.T) {
	servers := []yabsFleetServer{
		{ID: 1, Hostname: "moved", Provider: "Acme", Active: true, Cost: 10},
		{ID: 2, Hostname: "peer-a", Provider: "Bolt", Active: true, Cost: 10},
		{ID: 3, Hostname: "peer-b", Provider: "Bolt", Active: true, Cost: 10},
	}
	runs := []model.YABSRun{
		fr{1, 1, "2026-06-01 00:00:00", "CPU A", 1600, 3000, 6, 80, 600}.run(),
		fr{2, 1, "2026-07-01 00:00:00", "CPU A", 1610, 3010, 6, 80, 600}.run(),
		fr{3, 1, "2026-08-01 00:00:00", "CPU A", 1590, 2990, 6, 80, 600}.run(),
		fr{4, 1, "2026-09-01 00:00:00", "CPU A", 700, 1300, 6, 30, 600}.run(), // a low run…
		fr{5, 1, "2026-10-01 00:00:00", "CPU B", 0, 0, 0, 0, 600}.run(),       // …then a partial one on new hardware
		fr{6, 2, "2026-10-01 00:00:00", "CPU B", 2000, 4000, 6, 90, 700}.run(),
		fr{7, 3, "2026-10-01 00:00:00", "CPU A", 1500, 2900, 6, 85, 650}.run(),
	}
	v := buildYABSOverview(servers, runs, overviewNow)

	// The servers table shows what the latest run measured: no Geekbench, no disk.
	var moved *yabsFleetRow
	for i := range v.Servers {
		if v.Servers[i].Name == "moved" {
			moved = &v.Servers[i]
		}
	}
	if moved == nil || moved.Cells[ymSingle].Value != "—" || moved.Cells[ymDisk4k].Value != "—" ||
		moved.Cells[ymNetSend].Value == "—" || moved.BarPct != "" || moved.Value != "—" {
		t.Fatalf("moved row must show only the latest run's measurements: %+v", moved)
	}
	for _, c := range moved.Cells {
		if c.Low {
			t.Errorf("an older low result must not mark the latest run: %+v", moved.Cells)
		}
	}
	// The old low Geekbench result is not an alert on the latest run. The
	// CPU change is — that is about the latest run.
	if len(v.Attention) != 1 || v.Attention[0].Name != "moved" {
		t.Fatalf("attention = %+v", v.Attention)
	}
	reasons := strings.Join(v.Attention[0].Reasons, " | ")
	if strings.Contains(reasons, "below its own median") || !strings.Contains(reasons, "CPU changed: CPU A → CPU B") {
		t.Errorf("attention reasons = %q", reasons)
	}
	// Acme's Geekbench median has no server to draw on; its network does.
	for _, p := range v.Providers {
		if p.Name == "Acme" && (p.Values[ymSingle] != "—" || p.Values[ymNetSend] == "—" || p.Value != "—") {
			t.Errorf("Acme = %+v", p)
		}
	}
	// CPU A's old score is not counted under CPU B: CPU B is then at one
	// provider only (peer-a) and CPU A at one (peer-b) — nothing to compare.
	if len(v.CPUs) != 0 {
		t.Errorf("a carried-forward score created a same-CPU comparison: %+v", v.CPUs)
	}
}

func TestYABSOverviewEmptyAndVersionTie(t *testing.T) {
	v := buildYABSOverview(nil, nil, overviewNow)
	if v.TotalRuns != 0 || len(v.Providers)+len(v.Servers)+len(v.Recent)+len(v.Coverage) != 0 || v.GBLabel != "GB" {
		t.Errorf("empty overview = %+v", v)
	}
	// Servers but no runs: all of them are coverage gaps.
	servers, _ := overviewFleet()
	v = buildYABSOverview(servers, nil, overviewNow)
	if v.Uncovered != 6 || v.Benchmarked != 0 {
		t.Errorf("no runs: uncovered=%d benchmarked=%d, want 6/0", v.Uncovered, v.Benchmarked)
	}

	// One server on each Geekbench version: follow the newer one.
	hosts := []yabsHostSummary{{GBVersion: 5}, {GBVersion: 6}, {GBVersion: 0}}
	if got := yabsFleetGBVersion(hosts); got != 6 {
		t.Errorf("version tie = %d, want 6", got)
	}
	if got := yabsFleetGBVersion([]yabsHostSummary{{GBVersion: 5}, {GBVersion: 5}, {GBVersion: 6}}); got != 5 {
		t.Errorf("majority version = %d, want 5", got)
	}
	// A server whose only score records no version: listed with the score
	// marked, and not counted in any Geekbench median.
	srvs := []yabsFleetServer{
		{ID: 1, Hostname: "known", Provider: "Acme", Active: true},
		{ID: 2, Hostname: "unknown", Provider: "Acme", Active: true},
	}
	rs := []model.YABSRun{
		fr{1, 1, "2026-11-01 00:00:00", "X", 1500, 4000, 6, 50, 400}.run(),
		fr{2, 2, "2026-11-02 00:00:00", "X", 1900, 5000, 0, 50, 400}.run(),
	}
	ov := buildYABSOverview(srvs, rs, overviewNow)
	if ov.GBLabel != "GB6" || ov.Providers[0].Values[ymSingle] != "1,500" {
		t.Errorf("unversioned score entered the fleet median: %+v", ov.Providers[0])
	}
	if last := ov.Servers[len(ov.Servers)-1]; last.Name != "unknown" || last.Cells[ymSingle].Value != "1,900 · v?" || last.BarPct != "" {
		t.Errorf("unversioned server row = %+v", last)
	}
	if got := yabsFleetGBVersion(nil); got != 0 {
		t.Errorf("no scores = %d, want 0", got)
	}
}

// The page itself, against a real database: providers and costs come from
// the inventory, and everything links through.
func TestYABSOverviewPage(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	srv.rates.BaseURL = "http://127.0.0.1:1/unreachable" // no network in tests; USD needs no rate
	client := authedClient(t, ts)

	// No runs: empty state, but servers still show up as coverage gaps.
	createServer(t, client, ts, "ov-alpha")
	code, page := getPage(t, client, ts.URL+"/yabs")
	if code != http.StatusOK || !strings.Contains(page, "No YABS runs yet") || !strings.Contains(page, `href="/yabs/new"`) {
		t.Fatalf("empty overview (%d)", code)
	}
	if !strings.Contains(page, "Not benchmarked lately") || !strings.Contains(page, `href="/servers/1/yabs/new"`) || !strings.Contains(page, "never") {
		t.Errorf("a server without runs should be listed as a coverage gap")
	}

	createServer(t, client, ts, "ov-bravo")
	if _, err := database.Exec("INSERT INTO providers (name) VALUES ('Acme Hosting')"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE servers SET provider_id = 1 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	at := func(daysAgo int) string { return now.AddDate(0, 0, -daysAgo).Format("2006-01-02 15:04:05") }
	seedHostRuns(t, database, 1,
		hostRun{at: at(90), cpu: cpuA, kernel: "6.1", single: 1650, multi: 3100, disk: 80, net: 600},
		hostRun{at: at(60), cpu: cpuA, kernel: "6.1", single: 1660, multi: 3120, disk: 81, net: 610},
		hostRun{at: at(30), cpu: cpuA, kernel: "6.1", single: 1640, multi: 3090, disk: 79, net: 590},
		hostRun{at: at(1), cpu: cpuB, kernel: "6.1", single: 760, multi: 1420, disk: 45, net: 400})
	seedHostRuns(t, database, 2, hostRun{at: at(400), cpu: cpuA, kernel: "6.1", single: 1500, multi: 3000, disk: 70, net: 500})

	code, page = getPage(t, client, ts.URL+"/yabs")
	if code != http.StatusOK {
		t.Fatalf("overview: %d", code)
	}
	for _, want := range []string{
		`<div class="stat-value">5</div>`, "2 <span class=\"ybs-unit\">of 2 active</span>",
		"Need a look", "15% or more below its own median", "CPU changed: " + cpuA,
		"Recent runs", `href="/servers/1/yabs/4"`, "▼ 54%",
		"Providers", "Acme Hosting", "No provider set",
		// createServer prices every server at $10/mo: 760 / 10 and 1500 / 10.
		"Cost/mo", "Single/$", ">10.00<", ">76.0<", ">150<",
		"Servers · latest run", `href="/servers/2/yabs"`, "<svg class=\"ybs-bar\"",
		"Not benchmarked lately", "400 days ago",
		`href="/yabs/runs"`, "All 5 runs",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("overview missing %q", want)
		}
	}
	// One CPU model at one provider each: nothing to compare.
	if strings.Contains(page, "Same CPU, different providers") {
		t.Errorf("the same-CPU section needs a model at two providers")
	}
	// Rates are down, but every price is USD: nothing is missing, no note.
	if strings.Contains(page, "no exchange rate available") {
		t.Errorf("no rates note is due when nothing needs converting")
	}

	// The complete list moved to /yabs/runs.
	code, list := getPage(t, client, ts.URL+"/yabs/runs")
	if code != http.StatusOK || strings.Count(list, `class="row-link"`) != 5 || !strings.Contains(list, `href="/yabs"`) {
		t.Fatalf("/yabs/runs (%d) should list all 5 runs", code)
	}
	for _, path := range []string{"/yabs", "/yabs/runs"} {
		if code, _ := getPage(t, newClient(t), ts.URL+path); code == http.StatusOK {
			t.Errorf("%s served without a session", path)
		}
	}
}

// Costs follow the dashboard currency when a rate is available, and a price
// in a currency without a rate is left out rather than guessed.
func TestYABSOverviewCurrency(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	ratesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base":"USD","rates":{"EUR":0.5,"USD":1}}`))
	}))
	t.Cleanup(ratesSrv.Close)
	srv.rates.BaseURL = ratesSrv.URL
	client := authedClient(t, ts)
	createServer(t, client, ts, "cur-usd") // $10/mo
	createServer(t, client, ts, "cur-xyz")
	if _, err := database.Exec("UPDATE pricings SET currency = 'XYZ' WHERE service_id = 2"); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec("UPDATE settings SET dashboard_currency = 'EUR' WHERE id = 1"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format("2006-01-02 15:04:05")
	seedHostRuns(t, database, 1, hostRun{at: now, cpu: cpuA, kernel: "6.1", single: 1000, multi: 3000, disk: 80, net: 600})
	seedHostRuns(t, database, 2, hostRun{at: now, cpu: cpuA, kernel: "6.1", single: 900, multi: 2900, disk: 80, net: 600})

	_, page := getPage(t, client, ts.URL+"/yabs")
	// $10 → €5.00; 1000 points / 5 = 200 per €.
	for _, want := range []string{"Single/€", ">5.00<", ">200<"} {
		if !strings.Contains(page, want) {
			t.Errorf("EUR overview missing %q", want)
		}
	}
	if !strings.Contains(page, "cur-xyz</a>") {
		t.Fatalf("cur-xyz row missing")
	}
	if !strings.Contains(page, "1 server is priced in a currency with no exchange rate available") {
		t.Errorf("a cost left out for want of a rate should be explained")
	}
	// The XYZ-priced server has no convertible cost: 900 points, no value.
	if strings.Contains(page, ">180<") || strings.Contains(page, ">90.0<") {
		t.Errorf("a price without a rate must not produce a value figure")
	}
}
