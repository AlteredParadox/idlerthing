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

// yabs_host.go — the per-server YABS dashboard (GET /servers/{id}/yabs) and
// the two-run comparison (GET /servers/{id}/yabs/compare). Everything shown
// is derived from one History() load by the pure helpers in yabs_stats.go.

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// yabsHostView is the payload of the per-server dashboard.
type yabsHostView struct {
	ServerID   int64
	ServerName string
	ServerURL  string
	AddURL     string
	CompareURL string
	RunCount   int
	FirstDate  string
	LatestDate string
	LatestURL  string
	Tiles      []yabsTile
	Notices    []yabsNotice // about the latest run
	Rows       []yabsHistoryRow
	Columns    []string // history column headings, one per tile metric
	Disk       []yabsDiskRow
	Network    []yabsNetRow
	// Baseline is true once any breakdown row has enough runs for a median;
	// the median columns are hidden until then.
	DiskBaseline bool
	NetBaseline  bool
}

// yabsTile is one headline metric: the latest value, how it compares, and
// its trend.
type yabsTile struct {
	Label    string
	Unit     string
	Value    string
	Note     string     // why the value is not from the latest run, or what it sums
	VsPrev   *yabsDelta // against the previous measurement
	VsMedian *yabsDelta // against the host's median (needs a baseline)
	Stats    string     // "median 1,102 · range 1,050–1,150 · 5 runs"
	Spark    *yabsSpark
	Low      bool
}

// yabsNotice is one line about the latest run that deserves attention.
type yabsNotice struct {
	Kind string // "warn" | "info"
	Text string
}

// yabsHistoryRow is one run in the history table (newest first).
type yabsHistoryRow struct {
	ID       int64
	URL      string
	Date     string
	Cells    []yabsCell
	Low      []string // labels of the metrics flagged low in this run
	Hardware []yabsChange
	Software []yabsChange
}

// yabsCell is one metric of one run.
type yabsCell struct {
	Value string
	Delta *yabsDelta // against the previous run that measured it
	Low   bool
}

// yabsDiskRow is one fio block size of the latest run against the host's
// history.
type yabsDiskRow struct {
	BlockSize   string
	Read, Write yabsCell
	MedianRead  string
	MedianWrite string
}

// yabsNetRow is one iperf location of the latest run against the host's
// history.
type yabsNetRow struct {
	Location, Provider, Mode string
	Send, Recv               yabsCell
	MedianSend, MedianRecv   string
	Latency                  string
	MedianLatency            string
}

// yabsDate renders a run's time for the views; "—" when unknown.
func yabsDate(y *model.YABS, withTime bool) string {
	t := yabsRunTime(y)
	if t.IsZero() {
		return "—"
	}
	if withTime {
		return t.Format("2006-01-02 15:04")
	}
	return t.Format("2006-01-02")
}

// yabsRunURL is the detail page of one run.
func yabsRunURL(serverID, runID int64) string {
	return serverYABSURL(serverID) + "/" + strconv.FormatInt(runID, 10)
}

// handleServerYABS renders GET /servers/{id}/yabs: the server's YABS
// dashboard.
func (s *Server) handleServerYABS(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.yabsServer(w, r)
	if !ok {
		return
	}
	runs, err := (&model.YABSStore{DB: s.db}).History(r.Context(), srv.ID)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, "YABS · "+srv.Hostname, "yabs")
	data.Data = buildYABSHostView(srv, runs)
	s.render(w, r, "yabs_host", data)
}

// buildYABSHostView derives the whole dashboard from a server's runs.
func buildYABSHostView(srv *model.Server, runs []model.YABSRun) yabsHostView {
	base := serverYABSURL(srv.ID)
	v := yabsHostView{
		ServerID:   srv.ID,
		ServerName: srv.Hostname,
		ServerURL:  routeServers + "/" + strconv.FormatInt(srv.ID, 10),
		AddURL:     base + "/new",
		CompareURL: base + "/compare",
		RunCount:   len(runs),
	}
	if len(runs) == 0 {
		return v
	}
	sortYABSRuns(runs)
	latest := len(runs) - 1
	v.FirstDate = yabsDate(&runs[0].YABS, false)
	v.LatestDate = yabsDate(&runs[latest].YABS, false)
	v.LatestURL = yabsRunURL(srv.ID, runs[latest].ID)

	metrics := yabsMetrics(runs)
	gbVersion := yabsGBVersion(runs)
	series := make([]yabsSeries, len(metrics))
	for i, m := range metrics {
		series[i] = m.series(runs)
		v.Columns = append(v.Columns, m.Short)
		tile := buildYABSTile(m, series[i], runs)
		if len(series[i].Points) == 0 {
			uncomparedTile(&tile, m.Key, runs, gbVersion)
		}
		v.Tiles = append(v.Tiles, tile)
	}

	for i := latest; i >= 0; i-- {
		v.Rows = append(v.Rows, buildYABSHistoryRow(srv.ID, runs, i, metrics, series, gbVersion))
	}
	v.Notices = yabsLatestNotices(v.Rows[0])
	v.Disk, v.DiskBaseline = buildYABSDiskRows(runs)
	v.Network, v.NetBaseline = buildYABSNetRows(runs)
	return v
}

// buildYABSHistoryRow is run i of a time-ordered history as a table row:
// each metric against the previous run that measured it, the flags, and what
// changed in the system since the run before.
func buildYABSHistoryRow(serverID int64, runs []model.YABSRun, i int, metrics []yabsMetric, series []yabsSeries, gbVersion int64) yabsHistoryRow {
	run := &runs[i]
	row := yabsHistoryRow{ID: run.ID, URL: yabsRunURL(serverID, run.ID), Date: yabsDate(&run.YABS, true)}
	for mi, m := range metrics {
		cell := yabsCell{Value: "—"}
		if pos := series[mi].at(i); pos >= 0 {
			cell.Value = fmtYABS(series[mi].Points[pos].V)
			cell.Delta = series[mi].deltaPrev(pos)
			if series[mi].lowAt(pos) {
				cell.Low = true
				row.Low = append(row.Low, m.Label)
			}
		} else if other := otherVersionScore(run, m.Key, gbVersion); other != "" {
			cell.Value = other
		}
		row.Cells = append(row.Cells, cell)
	}
	if i > 0 {
		row.Hardware, row.Software = yabsSystemChanges(&runs[i-1].YABS, &run.YABS)
	}
	return row
}

// otherVersionScore shows a Geekbench score that takes no part in the page's
// comparisons: one from a different major version than the one followed
// ("1,234 · v5"), or one whose version was not recorded ("1,234 · v?"). It is
// real data, so it is shown — without deltas, medians or flags. "" for a
// score that is in the series, a non-Geekbench metric, or no score.
func otherVersionScore(run *model.YABSRun, key string, version int64) string {
	runVersion := run.GeekbenchVersion.Int64
	if runVersion > 0 && runVersion == version {
		return ""
	}
	var score int64
	switch key {
	case "gb_single":
		score = run.GbSingle.Int64
	case "gb_multi":
		score = run.GbMulti.Int64
	}
	if score <= 0 {
		return ""
	}
	if runVersion <= 0 {
		return commaInt(score) + " · v?"
	}
	return commaInt(score) + " · v" + strconv.FormatInt(runVersion, 10)
}

// uncomparedTile fills a Geekbench tile that has no series with the newest
// score there is, marked as not compared — rather than reading "not measured"
// over a server that plainly has a score.
func uncomparedTile(t *yabsTile, key string, runs []model.YABSRun, version int64) {
	for i := len(runs) - 1; i >= 0; i-- {
		if other := otherVersionScore(&runs[i], key, version); other != "" {
			t.Value = other
			t.Note = "Geekbench version differs or was not recorded; not compared"
			return
		}
	}
}

// buildYABSTile summarises one metric. The value is the newest measurement;
// when the latest run did not measure it, the note says which run it is from.
// Comparisons, the baseline and the trend line use only the measurements the
// newest one is comparable with (see yabsSeries.peers) — for the network
// tiles, the runs that measured the same iperf locations.
func buildYABSTile(m yabsMetric, s yabsSeries, runs []model.YABSRun) yabsTile {
	t := yabsTile{Label: m.Label, Unit: m.Unit, Value: "—"}
	n := len(s.Points)
	if n == 0 {
		t.Note = "not measured yet"
		return t
	}
	cur := s.Points[n-1]
	t.Value = fmtYABS(cur.V)
	if cur.Run != len(runs)-1 {
		t.Note = "from " + yabsDate(&runs[cur.Run].YABS, false) + "; the latest run did not measure it"
	}
	t.VsPrev = s.deltaPrev(n - 1)
	peers := s.peers(n - 1)
	if peers.hasBaseline() {
		t.VsMedian = newYabsDelta(cur.V, peers.Median, false)
		t.Low = peers.low(cur.V)
	}
	t.Stats = yabsTileStats(peers, n-len(peers.Points))
	t.Spark = newYabsSpark(peers, m.Label)
	return t
}

// yabsTileStats is the line under a tile: the baseline of the comparable
// measurements, and how many others were left out as not comparable.
func yabsTileStats(peers yabsSeries, others int) string {
	n := len(peers.Points)
	var stats string
	switch {
	case peers.hasBaseline():
		stats = fmt.Sprintf("median %s · range %s–%s · %d runs",
			fmtYABS(peers.Median), fmtYABS(peers.Min), fmtYABS(peers.Max), n)
	case n == 2:
		stats = "2 runs · a median needs 3"
	case others > 0:
		return fmt.Sprintf("not compared · %s measured other locations", plural(others, "the other run", "the other %d runs"))
	default:
		return "1 run · trends start with the next one"
	}
	if others > 0 {
		stats += " · " + plural(others, "1 more", "%d more") + " on other locations"
	}
	return stats
}

// plural picks the singular text for 1 and formats the plural one otherwise.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return fmt.Sprintf(many, n)
}

// yabsLatestNotices turns the latest run's flags into the banner lines at
// the top of the page.
func yabsLatestNotices(latest yabsHistoryRow) []yabsNotice {
	var out []yabsNotice
	if len(latest.Low) > 0 {
		out = append(out, yabsNotice{Kind: "warn", Text: fmt.Sprintf(
			"The latest run is %.0f%% or more below this host's median for: %s.",
			yabsLowPct, strings.Join(latest.Low, ", "))})
	}
	for _, c := range latest.Hardware {
		out = append(out, yabsNotice{Kind: "warn", Text: fmt.Sprintf(
			"%s changed since the previous run: %s → %s. The host may have been moved to different hardware.",
			c.Field, c.From, c.To)})
	}
	for _, c := range latest.Software {
		out = append(out, yabsNotice{Kind: "info", Text: fmt.Sprintf(
			"%s changed since the previous run: %s → %s.", c.Field, c.From, c.To)})
	}
	return out
}

// yabsBlockOrder is the order yabs.sh reports fio block sizes in.
var yabsBlockOrder = []string{"4k", "64k", "512k", "1m"}

// buildYABSDiskRows breaks the latest run's fio results down by block size,
// each side against its own history.
func buildYABSDiskRows(runs []model.YABSRun) (rows []yabsDiskRow, baseline bool) {
	latest := &runs[len(runs)-1]
	seen := map[string]bool{}
	var sizes []string
	add := func(bs string) {
		if bs != "" && !seen[bs] {
			seen[bs] = true
			sizes = append(sizes, bs)
		}
	}
	present := map[string]bool{}
	for _, d := range latest.Disks {
		present[normBlockSize(d.BlockSize)] = true
	}
	for _, bs := range yabsBlockOrder {
		if present[bs] {
			add(bs)
		}
	}
	for _, d := range latest.Disks {
		add(normBlockSize(d.BlockSize))
	}
	for _, bs := range sizes {
		read := newYabsSeries(runs, func(r *model.YABSRun) float64 { v, _ := yabsDisk(r, bs); return v }, nil)
		write := newYabsSeries(runs, func(r *model.YABSRun) float64 { _, v := yabsDisk(r, bs); return v }, nil)
		curRead, curWrite := yabsDisk(latest, bs)
		row := yabsDiskRow{
			BlockSize:   bs,
			Read:        baselineCell(curRead, read, false),
			Write:       baselineCell(curWrite, write, false),
			MedianRead:  baselineMedian(read),
			MedianWrite: baselineMedian(write),
		}
		baseline = baseline || read.hasBaseline() || write.hasBaseline()
		rows = append(rows, row)
	}
	return rows, baseline
}

// baselineCell is a latest value with its delta against the series median.
func baselineCell(cur float64, s yabsSeries, lowerBetter bool) yabsCell {
	c := yabsCell{Value: fmtYABS(cur)}
	if s.hasBaseline() {
		c.Delta = newYabsDelta(cur, s.Median, lowerBetter)
		c.Low = !lowerBetter && s.low(cur)
	}
	return c
}

// baselineMedian renders a series median, "—" until it is a baseline.
func baselineMedian(s yabsSeries) string {
	if !s.hasBaseline() {
		return "—"
	}
	return fmtYABS(s.Median)
}

// yabsNet returns a run's row for one endpoint, nil when the run has none.
func yabsNet(r *model.YABSRun, key string) *model.YABSNetworkSpeed {
	for i := range r.Network {
		if yabsNetKey(&r.Network[i]) == key {
			return &r.Network[i]
		}
	}
	return nil
}

// buildYABSNetRows breaks the latest run's iperf results down by endpoint,
// each against its own history.
func buildYABSNetRows(runs []model.YABSRun) (rows []yabsNetRow, baseline bool) {
	latest := &runs[len(runs)-1]
	seen := map[string]bool{}
	for i := range latest.Network {
		n := &latest.Network[i]
		key := yabsNetKey(n)
		if seen[key] {
			continue
		}
		seen[key] = true
		pick := func(f func(*model.YABSNetworkSpeed) float64) yabsSeries {
			return newYabsSeries(runs, func(r *model.YABSRun) float64 {
				if row := yabsNet(r, key); row != nil {
					return f(row)
				}
				return 0
			}, nil)
		}
		send := pick(func(n *model.YABSNetworkSpeed) float64 { return n.SendMbps })
		recv := pick(func(n *model.YABSNetworkSpeed) float64 { return n.RecvMbps })
		latency := pick(func(n *model.YABSNetworkSpeed) float64 { return n.LatencyMs })
		rows = append(rows, yabsNetRow{
			Location: n.Location, Provider: n.Provider, Mode: n.Mode,
			Send:          baselineCell(n.SendMbps, send, false),
			Recv:          baselineCell(n.RecvMbps, recv, false),
			MedianSend:    baselineMedian(send),
			MedianRecv:    baselineMedian(recv),
			Latency:       fmtYABS(n.LatencyMs),
			MedianLatency: baselineMedian(latency),
		})
		// Latency counts too: an endpoint can answer pings in every run and
		// be busy for iperf in most, and its latency median is still real.
		baseline = baseline || send.hasBaseline() || recv.hasBaseline() || latency.hasBaseline()
	}
	return rows, baseline
}

// ---------- Compare ----------

// yabsCompareView is the payload of the two-run comparison. A is the older
// run, B the newer, whatever order they were picked in.
type yabsCompareView struct {
	ServerID   int64
	ServerName string
	BackURL    string
	A, B       yabsCompareRun
	System     []yabsCompareField
	Scores     []yabsCompareNum
	Disk       []yabsCompareNum
	Network    []yabsCompareNet
}

type yabsCompareRun struct {
	URL  string
	Date string
}

// yabsCompareField is one system field side by side.
type yabsCompareField struct {
	Label   string
	A, B    string
	Changed bool
}

// yabsCompareNum is one number side by side with its change from A to B.
type yabsCompareNum struct {
	Label string
	Unit  string
	A, B  string
	Delta *yabsDelta
}

// yabsCompareNet is one iperf endpoint side by side.
type yabsCompareNet struct {
	Location, Provider, Mode string
	Send, Recv, Latency      yabsCompareNum
}

// handleServerYABSCompare renders GET /servers/{id}/yabs/compare?run=A&run=B.
// Anything other than two distinct runs of this server goes back to the
// dashboard with a hint, rather than rendering a half-empty comparison.
func (s *Server) handleServerYABSCompare(w http.ResponseWriter, r *http.Request) {
	srv, ok := s.yabsServer(w, r)
	if !ok {
		return
	}
	runs, err := (&model.YABSStore{DB: s.db}).History(r.Context(), srv.ID)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	sortYABSRuns(runs)
	a, b := pickCompareRuns(runs, r.URL.Query()["run"])
	if a == nil {
		s.setFlash(w, r, "err", "Select exactly two runs to compare.")
		http.Redirect(w, r, serverYABSURL(srv.ID), http.StatusSeeOther)
		return
	}
	data := s.newPageData(w, r, "Compare YABS · "+srv.Hostname, "yabs")
	data.Data = buildYABSCompareView(srv, a, b)
	s.render(w, r, "yabs_compare", data)
}

// pickCompareRuns resolves the selected run ids against a time-ordered
// history and returns them older first; nil, nil unless exactly two distinct
// runs of this history were selected.
func pickCompareRuns(runs []model.YABSRun, selected []string) (older, newer *model.YABSRun) {
	if len(selected) != 2 {
		return nil, nil
	}
	want := map[int64]bool{}
	for _, raw := range selected {
		id, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return nil, nil
		}
		want[id] = true
	}
	var picked []*model.YABSRun
	for i := range runs {
		if want[runs[i].ID] {
			picked = append(picked, &runs[i])
		}
	}
	if len(picked) != 2 {
		return nil, nil
	}
	return picked[0], picked[1]
}

// buildYABSCompareView lays two runs side by side.
func buildYABSCompareView(srv *model.Server, a, b *model.YABSRun) yabsCompareView {
	return yabsCompareView{
		ServerID:   srv.ID,
		ServerName: srv.Hostname,
		BackURL:    serverYABSURL(srv.ID),
		A:          yabsCompareRun{URL: yabsRunURL(srv.ID, a.ID), Date: yabsDate(&a.YABS, true)},
		B:          yabsCompareRun{URL: yabsRunURL(srv.ID, b.ID), Date: yabsDate(&b.YABS, true)},
		System:     compareSystem(&a.YABS, &b.YABS),
		Scores:     compareScores(&a.YABS, &b.YABS),
		Disk:       compareDisk(a, b),
		Network:    compareNetwork(a, b),
	}
}

// compareNum is one number side by side. withDelta is false when the two
// sides must not be compared (Geekbench scores of different versions).
func compareNum(label, unit string, av, bv float64, withDelta, lowerBetter bool) yabsCompareNum {
	n := yabsCompareNum{Label: label, Unit: unit, A: fmtYABS(av), B: fmtYABS(bv)}
	if withDelta {
		n.Delta = newYabsDelta(bv, av, lowerBetter)
	}
	return n
}

// compareSystem lists the system fields of two runs, marking the ones that
// differ (a field missing on either side is not a difference).
func compareSystem(a, b *model.YABS) []yabsCompareField {
	cores := func(y *model.YABS) string {
		if y.CPUCores.Int64 <= 0 {
			return ""
		}
		return strconv.FormatInt(y.CPUCores.Int64, 10)
	}
	dash := func(s string) string {
		if s == "" {
			return "—"
		}
		return s
	}
	var out []yabsCompareField
	for _, f := range []struct{ label, a, b string }{
		{"CPU", a.CPU.String, b.CPU.String},
		{"Cores", cores(a), cores(b)},
		{"RAM", a.RAM.String, b.RAM.String},
		{"Swap", a.Swap.String, b.Swap.String},
		{"Distro", a.Distro.String, b.Distro.String},
		{"Kernel", a.Kernel.String, b.Kernel.String},
	} {
		out = append(out, yabsCompareField{
			Label: f.label, A: dash(f.a), B: dash(f.b),
			Changed: f.a != "" && f.b != "" && f.a != f.b,
		})
	}
	return out
}

// compareScores lists the Geekbench scores of two runs. A change is computed
// only when both runs record the same major version: scores from different
// versions, or from a run that does not say which version it used, are shown
// side by side without a percentage.
func compareScores(a, b *model.YABS) []yabsCompareNum {
	if !yabsHasGB(a) && !yabsHasGB(b) {
		return nil
	}
	version := a.GeekbenchVersion.Int64
	sameGB := version > 0 && version == b.GeekbenchVersion.Int64
	name := "Geekbench"
	if sameGB {
		name += " " + strconv.FormatInt(version, 10)
	}
	out := []yabsCompareNum{
		compareNum(name+" single", "", float64(a.GbSingle.Int64), float64(b.GbSingle.Int64), sameGB, false),
		compareNum(name+" multi", "", float64(a.GbMulti.Int64), float64(b.GbMulti.Int64), sameGB, false),
	}
	if !sameGB {
		out = append(out, yabsCompareNum{
			Label: "Geekbench version", A: niDisplay(a.GeekbenchVersion), B: niDisplay(b.GeekbenchVersion),
		})
	}
	return out
}

// compareDisk lists fio read and write per block size, in yabs.sh's order
// followed by any other size either run has.
func compareDisk(a, b *model.YABSRun) []yabsCompareNum {
	seen := map[string]bool{}
	sizes := append([]string(nil), yabsBlockOrder...)
	for _, bs := range sizes {
		seen[bs] = true
	}
	for _, run := range []*model.YABSRun{a, b} {
		for _, d := range run.Disks {
			if bs := normBlockSize(d.BlockSize); bs != "" && !seen[bs] {
				seen[bs] = true
				sizes = append(sizes, bs)
			}
		}
	}
	var out []yabsCompareNum
	for _, bs := range sizes {
		ar, aw := yabsDisk(a, bs)
		br, bw := yabsDisk(b, bs)
		if ar+aw+br+bw == 0 {
			continue
		}
		out = append(out,
			compareNum(bs+" read", "MB/s", ar, br, true, false),
			compareNum(bs+" write", "MB/s", aw, bw, true, false))
	}
	return out
}

// compareNetwork lists every iperf endpoint either run has, the newer run's
// order first. An endpoint one run did not measure reads as unmeasured there
// and gets no change.
func compareNetwork(a, b *model.YABSRun) []yabsCompareNet {
	seen := map[string]bool{}
	var out []yabsCompareNet
	for _, run := range []*model.YABSRun{b, a} {
		for i := range run.Network {
			row := &run.Network[i]
			key := yabsNetKey(row)
			if seen[key] {
				continue
			}
			seen[key] = true
			var an, bn model.YABSNetworkSpeed
			if p := yabsNet(a, key); p != nil {
				an = *p
			}
			if p := yabsNet(b, key); p != nil {
				bn = *p
			}
			out = append(out, yabsCompareNet{
				Location: row.Location, Provider: row.Provider, Mode: row.Mode,
				Send:    compareNum("Send", "Mbit/s", an.SendMbps, bn.SendMbps, true, false),
				Recv:    compareNum("Recv", "Mbit/s", an.RecvMbps, bn.RecvMbps, true, false),
				Latency: compareNum("Latency", "ms", an.LatencyMs, bn.LatencyMs, true, true),
			})
		}
	}
	return out
}
