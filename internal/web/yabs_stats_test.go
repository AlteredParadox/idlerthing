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
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"idlerthing/internal/model"
)

func ns(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }

func TestYABSRunTime(t *testing.T) {
	want := time.Date(2026, 11, 19, 18, 54, 20, 0, time.UTC)
	for _, raw := range []string{
		"2026-11-19 18:54:20", "2026-11-19T18:54:20Z", "2026-11-19T13:54:20-05:00",
		"2026-11-19T18:54:20", "20261119-185420", " 2026-11-19 18:54:20 ",
	} {
		if got := yabsRunTime(&model.YABS{RunAt: ns(raw)}); !got.Equal(want) {
			t.Errorf("yabsRunTime(%q) = %v, want %v", raw, got, want)
		}
	}
	if got := yabsRunTime(&model.YABS{RunAt: ns("2026-11-19")}); !got.Equal(time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("bare date = %v", got)
	}
	// An unparseable stamp falls back to when the run was stored.
	if got := yabsRunTime(&model.YABS{RunAt: ns("last tuesday"), CreatedAt: "2026-11-19 18:54:20"}); !got.Equal(want) {
		t.Errorf("fallback to created_at = %v", got)
	}
	if got := yabsRunTime(&model.YABS{RunAt: ns("nope"), CreatedAt: "nope"}); !got.IsZero() {
		t.Errorf("nothing parseable should be the zero time, got %v", got)
	}
}

func TestSortYABSRunsByTimeThenID(t *testing.T) {
	runs := []model.YABSRun{
		{YABS: model.YABS{ID: 1, RunAt: ns("2026-06-01 00:00:00")}},
		{YABS: model.YABS{ID: 2, RunAt: ns("2026-01-01 00:00:00")}}, // added later, ran earlier
		{YABS: model.YABS{ID: 4, RunAt: ns("2026-03-01 00:00:00")}},
		{YABS: model.YABS{ID: 3, RunAt: ns("2026-03-01 00:00:00")}},
		{YABS: model.YABS{ID: 5}}, // unknown time sorts first
	}
	sortYABSRuns(runs)
	var got []int64
	for _, r := range runs {
		got = append(got, r.ID)
	}
	if want := []int64{5, 2, 3, 4, 1}; !equalInt64s(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

func equalInt64s(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestMedian(t *testing.T) {
	for _, tc := range []struct {
		in   []float64
		want float64
	}{
		{nil, 0}, {[]float64{7}, 7}, {[]float64{3, 1, 2}, 2}, {[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{1, 1, 1, 100}, 1},
	} {
		if got := median(tc.in); got != tc.want {
			t.Errorf("median(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	in := []float64{3, 1, 2}
	median(in)
	if in[0] != 3 || in[1] != 1 {
		t.Errorf("median must not reorder its input: %v", in)
	}
}

func TestNewYabsDelta(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cur, ref          float64
		lowerBetter       bool
		wantText, wantCls string
	}{
		{"up beyond noise", 110, 100, false, "▲ 10%", "delta-good"},
		{"down beyond noise", 80, 100, false, "▼ 20%", "delta-bad"},
		{"up within noise", 102, 100, false, "▲ 2.0%", "delta-flat"},
		{"down within noise", 96, 100, false, "▼ 4.0%", "delta-flat"},
		{"exactly at the noise edge", 105, 100, false, "▲ 5.0%", "delta-good"},
		{"no change", 100, 100, false, "0%", "delta-flat"},
		{"latency up is bad", 120, 100, true, "▲ 20%", "delta-bad"},
		{"latency down is good", 80, 100, true, "▼ 20%", "delta-good"},
	} {
		d := newYabsDelta(tc.cur, tc.ref, tc.lowerBetter)
		if d == nil || d.Text != tc.wantText || d.Class != tc.wantCls {
			t.Errorf("%s: got %+v, want %q %q", tc.name, d, tc.wantText, tc.wantCls)
		}
	}
	// Nothing to compare when either side was not measured.
	if newYabsDelta(0, 100, false) != nil || newYabsDelta(100, 0, false) != nil {
		t.Error("an unmeasured side must give no delta")
	}
}

func TestFmtYABS(t *testing.T) {
	for in, want := range map[float64]string{
		0: "—", -1: "—", 0.566: "0.57", 1: "1.0", 39.62: "39.6", 99.94: "99.9", 99.96: "100",
		683: "683", 1284.4: "1,284", 1234567: "1,234,567",
	} {
		if got := fmtYABS(in); got != want {
			t.Errorf("fmtYABS(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", -1234567: "-1,234,567"} {
		if got := commaInt(in); got != want {
			t.Errorf("commaInt(%d) = %q, want %q", in, got, want)
		}
	}
}

// statRun builds an in-memory run for the pure helpers.
func statRun(id int64, at string, single, multi, gbVersion int64, disk4k, net float64) model.YABSRun {
	r := model.YABSRun{YABS: model.YABS{
		ID: id, RunAt: ns(at),
		GbSingle: sqlNi(single), GbMulti: sqlNi(multi), GeekbenchVersion: sqlNi(gbVersion),
	}}
	if disk4k > 0 {
		r.Disks = []model.YABSDiskSpeed{
			{BlockSize: "4k", ReadMbps: disk4k, WriteMbps: disk4k},
			{BlockSize: "1M", ReadMbps: disk4k * 10, WriteMbps: disk4k * 10},
		}
	}
	if net > 0 {
		r.Network = []model.YABSNetworkSpeed{
			{Location: "A", Mode: "IPv4", SendMbps: net, RecvMbps: net / 2, LatencyMs: 10},
			{Location: "B", Mode: "IPv4", SendMbps: net * 2, RecvMbps: net, LatencyMs: 100},
			{Location: "C", Mode: "IPv4"}, // busy
		}
	}
	return r
}

func TestYABSRunValues(t *testing.T) {
	r := statRun(1, "2026-01-01", 1000, 3000, 6, 50, 400)
	if got := yabsDiskTotal(&r, "4k"); got != 100 {
		t.Errorf("4k total = %v, want 100", got)
	}
	if got := yabsDiskTotal(&r, "1m"); got != 1000 {
		t.Errorf("block sizes match case-insensitively: 1m total = %v, want 1000", got)
	}
	// One side unmeasured: no total, rather than a sum that looks like half.
	r.Disks[0].WriteMbps = 0
	if got := yabsDiskTotal(&r, "4k"); got != 0 {
		t.Errorf("one-sided total = %v, want 0", got)
	}
	if got := yabsDiskTotal(&r, "64k"); got != 0 {
		t.Errorf("missing block size = %v, want 0", got)
	}
	// Median over the measured locations only: {400, 800}, the busy row ignored.
	if got := yabsNetMedian(&r, true); got != 600 {
		t.Errorf("net send median = %v, want 600", got)
	}
	if got := yabsNetMedian(&r, false); got != 300 {
		t.Errorf("net recv median = %v, want 300", got)
	}
	if got := yabsNetMedian(&model.YABSRun{}, true); got != 0 {
		t.Errorf("no network rows = %v, want 0", got)
	}
}

func TestYABSSeriesBaselineAndLow(t *testing.T) {
	runs := []model.YABSRun{
		statRun(1, "2026-01-01", 1000, 3000, 6, 50, 400),
		statRun(2, "2026-02-01", 0, 0, 0, 50, 400), // no geekbench in this run
		statRun(3, "2026-03-01", 1020, 3000, 6, 50, 400),
		statRun(4, "2026-04-01", 800, 3000, 6, 50, 400),
	}
	metrics := yabsMetrics(runs)
	single := newYabsSeries(runs, metrics[0].Value)
	if len(single.Points) != 3 || single.Median != 1000 || single.Min != 800 || single.Max != 1020 {
		t.Fatalf("single series = %+v", single)
	}
	if single.at(1) != -1 || single.at(3) != 2 {
		t.Errorf("at(): run 1 has no score (%d), run 3 is the 3rd point (%d)", single.at(1), single.at(3))
	}
	if !single.hasBaseline() || !single.low(800) || single.low(900) || single.low(0) {
		t.Errorf("low: 800 is ≥15%% under the median 1000, 900 is not, 0 is unmeasured")
	}
	// Two measurements are not a baseline: nothing is "low" against them.
	two := newYabsSeries(runs[:3], metrics[0].Value)
	if two.hasBaseline() || two.low(1) {
		t.Errorf("two measurements must not form a baseline: %+v", two)
	}
	if empty := newYabsSeries(nil, metrics[0].Value); len(empty.Points) != 0 || empty.Median != 0 {
		t.Errorf("empty series = %+v", empty)
	}
}

// Geekbench scores are only comparable within one major version: the series
// follows the newest run's version and ignores the others.
func TestYABSMetricsFollowLatestGeekbenchVersion(t *testing.T) {
	runs := []model.YABSRun{
		statRun(1, "2025-01-01", 900, 2100, 5, 50, 400),
		statRun(2, "2026-01-01", 1500, 4000, 6, 50, 400),
		statRun(3, "2026-02-01", 0, 0, 0, 50, 400),
	}
	if v := yabsGBVersion(runs); v != 6 {
		t.Fatalf("version = %d, want 6 (the newest run that has a score)", v)
	}
	metrics := yabsMetrics(runs)
	if metrics[0].Label != "Geekbench 6 single" || metrics[0].Short != "GB6 single" {
		t.Errorf("labels = %q / %q", metrics[0].Label, metrics[0].Short)
	}
	single := newYabsSeries(runs, metrics[0].Value)
	if len(single.Points) != 1 || single.Points[0].V != 1500 {
		t.Fatalf("the v5 score must stay out of the v6 series: %+v", single.Points)
	}
	if got := otherVersionScore(&runs[0], "gb_single", 6); got != "900 · v5" {
		t.Errorf("otherVersionScore = %q", got)
	}
	if got := otherVersionScore(&runs[0], "gb_multi", 6); got != "2,100 · v5" {
		t.Errorf("otherVersionScore multi = %q", got)
	}
	for _, key := range []string{"disk_4k", "gb_single"} {
		run := &runs[1]
		if key == "disk_4k" {
			run = &runs[0]
		}
		if got := otherVersionScore(run, key, 6); got != "" {
			t.Errorf("otherVersionScore(%s) = %q, want nothing", key, got)
		}
	}
	// A run with a multi-core score and no single-core one is still a scored
	// run (the parser treats it as one): its version is the one to follow,
	// and its multi score is measured.
	multiOnly := []model.YABSRun{
		statRun(1, "2025-01-01", 900, 2100, 5, 50, 400),
		statRun(2, "2026-01-01", 0, 4000, 6, 50, 400),
	}
	if v := yabsGBVersion(multiOnly); v != 6 {
		t.Errorf("multi-only run: version = %d, want 6", v)
	}
	mm := yabsMetrics(multiOnly)
	if s := newYabsSeries(multiOnly, mm[1].Value); len(s.Points) != 1 || s.Points[0].V != 4000 {
		t.Errorf("multi-only run: multi series = %+v", s.Points)
	}
	if s := newYabsSeries(multiOnly, mm[0].Value); len(s.Points) != 0 {
		t.Errorf("multi-only run: the v5 single score must not be the baseline: %+v", s.Points)
	}
	if none := yabsMetrics(nil); none[0].Label != "Geekbench single" {
		t.Errorf("no scores yet: label = %q", none[0].Label)
	}
}

func TestNewYabsSpark(t *testing.T) {
	series := func(vals ...float64) yabsSeries {
		runs := make([]model.YABSRun, len(vals))
		for i, v := range vals {
			runs[i] = statRun(int64(i+1), "", int64(v), 1, 6, 0, 0)
		}
		return newYabsSeries(runs, func(r *model.YABSRun) float64 { return float64(r.GbSingle.Int64) })
	}
	if newYabsSpark(series(1000), "x") != nil || newYabsSpark(series(), "x") != nil {
		t.Fatal("fewer than two measurements is not a trend")
	}

	// A steady host: 2% of jitter must stay a near-flat line in the middle,
	// not be stretched over the full height.
	steady := newYabsSpark(series(1000, 1020, 990, 1010), "GB")
	ys := sparkYs(t, steady.Points)
	if len(ys) != 4 {
		t.Fatalf("points = %q", steady.Points)
	}
	for _, y := range ys {
		if y < 17 || y > 23 {
			t.Errorf("steady series should hug the middle (20), got y=%v in %q", y, steady.Points)
		}
	}
	if steady.MedianY == "" || !strings.HasPrefix(steady.Points, "5.0,") || steady.EndX != "195.0" {
		t.Errorf("steady spark = %+v", steady)
	}
	if !strings.Contains(steady.Label, "over 4 runs") || !strings.Contains(steady.Label, "median 1,005") {
		t.Errorf("label = %q", steady.Label)
	}

	// A real drop reaches the bottom of the plot.
	drop := newYabsSpark(series(1000, 1000, 1000, 500), "GB")
	ys = sparkYs(t, drop.Points)
	if ys[3] != 34 || drop.EndY != "34.0" || ys[0] > 20 {
		t.Errorf("a halving should end at the bottom edge: %q", drop.Points)
	}

	// Two runs draw a line but no median rule (no baseline yet).
	if two := newYabsSpark(series(1000, 900), "GB"); two == nil || two.MedianY != "" || strings.Contains(two.Label, "median") {
		t.Errorf("two-run spark = %+v", two)
	}
}

func sparkYs(t *testing.T, points string) []float64 {
	t.Helper()
	var ys []float64
	for _, pair := range strings.Fields(points) {
		var x, y float64
		if _, err := fmt.Sscanf(pair, "%f,%f", &x, &y); err != nil {
			t.Fatalf("bad point %q: %v", pair, err)
		}
		ys = append(ys, y)
	}
	return ys
}

func TestYABSSystemChanges(t *testing.T) {
	prev := &model.YABS{CPU: ns("AMD EPYC 7543"), CPUCores: sqlNi(4), RAM: ns("3.8 GiB"), Kernel: ns("6.1.0-28"), Distro: ns("Debian 12")}
	same := *prev
	if hw, sw := yabsSystemChanges(prev, &same); len(hw)+len(sw) != 0 {
		t.Fatalf("identical systems: %v %v", hw, sw)
	}
	cur := &model.YABS{CPU: ns("Intel Xeon E5-2680 v2"), CPUCores: sqlNi(2), RAM: ns("3.8 GiB"), Kernel: ns("6.12.0"), Distro: ns("Debian 13")}
	hw, sw := yabsSystemChanges(prev, cur)
	if len(hw) != 2 || hw[0].Field != "CPU" || hw[0].From != "AMD EPYC 7543" || hw[0].To != "Intel Xeon E5-2680 v2" ||
		hw[1].Field != "Cores" || hw[1].From != "4" || hw[1].To != "2" {
		t.Errorf("hardware = %+v", hw)
	}
	if len(sw) != 2 || sw[0].Field != "Kernel" || sw[1].Field != "Distro" {
		t.Errorf("software = %+v", sw)
	}
	// A field one run did not report is not a change.
	if hw, sw := yabsSystemChanges(prev, &model.YABS{}); len(hw)+len(sw) != 0 {
		t.Errorf("missing fields reported as changes: %v %v", hw, sw)
	}
}

func TestPickCompareRuns(t *testing.T) {
	runs := []model.YABSRun{
		statRun(7, "2026-01-01", 1, 1, 6, 0, 0), statRun(3, "2026-02-01", 1, 1, 6, 0, 0), statRun(9, "2026-03-01", 1, 1, 6, 0, 0),
	}
	// Older first, whatever order they were ticked in.
	a, b := pickCompareRuns(runs, []string{"9", "7"})
	if a == nil || a.ID != 7 || b.ID != 9 {
		t.Fatalf("picked %v, %v", a, b)
	}
	for _, bad := range [][]string{nil, {"7"}, {"7", "3", "9"}, {"7", "7"}, {"7", "x"}, {"7", "42"}, {"", "7"}} {
		if a, b := pickCompareRuns(runs, bad); a != nil || b != nil {
			t.Errorf("pickCompareRuns(%v) should refuse", bad)
		}
	}
}
