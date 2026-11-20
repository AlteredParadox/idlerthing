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

// yabs_stats.go — pure computations over a server's YABS history: ordering
// runs in time, pulling one comparable number per run out of each benchmark
// section, and describing how a measurement sits against the previous one
// and against the host's own baseline. No database or HTTP in here.

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
)

const (
	// yabsNoisePct is the run-to-run variation a benchmark shows on an
	// unchanged host. A change smaller than this is reported but not coloured
	// as better or worse.
	yabsNoisePct = 5.0
	// yabsLowPct flags a measurement this far below the host's own median.
	yabsLowPct = 15.0
	// yabsMinBaseline is how many measurements a median needs before it is
	// used as a baseline: with fewer, "the median" is just one of the runs.
	yabsMinBaseline = 3
)

// yabsTimeLayouts are the shapes a run timestamp is stored in: the parser's
// normalised form, the ingest-time fallback (RFC 3339), SQLite's
// CURRENT_TIMESTAMP, a raw yabs.sh stamp that passed through, and a bare date
// from an import.
var yabsTimeLayouts = []string{
	"2006-01-02 15:04:05", time.RFC3339, "2006-01-02T15:04:05", "20060102-150405", "2006-01-02",
}

// yabsRunTime is when a run happened: its own timestamp when that parses,
// otherwise when it was stored. Zero when neither does.
func yabsRunTime(y *model.YABS) time.Time {
	for _, raw := range []string{y.RunAt.String, y.CreatedAt} {
		raw = strings.TrimSpace(raw)
		for _, layout := range yabsTimeLayouts {
			if t, err := time.Parse(layout, raw); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// sortYABSRuns orders runs oldest first by run time, id breaking ties (and
// ordering runs whose time is unknown, which sort first).
func sortYABSRuns(runs []model.YABSRun) {
	sort.SliceStable(runs, func(i, j int) bool {
		ti, tj := yabsRunTime(&runs[i].YABS), yabsRunTime(&runs[j].YABS)
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return runs[i].ID < runs[j].ID
	})
}

// yabsMetric is one number per run that can be followed over time. Value
// returns 0 when the run did not measure it.
type yabsMetric struct {
	Key   string
	Label string
	Short string // column heading
	Unit  string
	Value func(r *model.YABSRun) float64
}

// normBlockSize canonicalises a fio block size ("4K " → "4k").
func normBlockSize(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// yabsDisk returns the read and write speed of one block size (0 = not
// measured, or the run has no such row).
func yabsDisk(r *model.YABSRun, blockSize string) (read, write float64) {
	for _, d := range r.Disks {
		if normBlockSize(d.BlockSize) == blockSize {
			return d.ReadMbps, d.WriteMbps
		}
	}
	return 0, 0
}

// yabsDiskTotal is read + write for one block size — the "total" yabs.sh
// prints — and 0 unless both sides were measured: a one-sided sum would look
// like a halving next to complete runs.
func yabsDiskTotal(r *model.YABSRun, blockSize string) float64 {
	read, write := yabsDisk(r, blockSize)
	if read <= 0 || write <= 0 {
		return 0
	}
	return read + write
}

// yabsNetMedian is the median throughput across the iperf locations a run
// measured, in one direction. The median, not the mean: a run always has a
// far or congested endpoint or two, and which ones answered varies run to run.
func yabsNetMedian(r *model.YABSRun, send bool) float64 {
	var vals []float64
	for _, n := range r.Network {
		v := n.RecvMbps
		if send {
			v = n.SendMbps
		}
		if v > 0 {
			vals = append(vals, v)
		}
	}
	return median(vals)
}

// yabsGBVersion is the Geekbench major version of the newest run that has a
// score, 0 when no run does. Scores are only comparable within a version, so
// every Geekbench statistic is taken over that version alone.
func yabsGBVersion(runs []model.YABSRun) int64 {
	for i := len(runs) - 1; i >= 0; i-- {
		if runs[i].GbSingle.Int64 > 0 {
			return runs[i].GeekbenchVersion.Int64
		}
	}
	return 0
}

// yabsMetrics is the fixed set of headline metrics for a history (time
// ordered). All are higher-is-better.
func yabsMetrics(runs []model.YABSRun) []yabsMetric {
	version := yabsGBVersion(runs)
	gbLabel := "Geekbench"
	if version > 0 {
		gbLabel += " " + strconv.FormatInt(version, 10)
	}
	gb := func(multi bool) func(r *model.YABSRun) float64 {
		return func(r *model.YABSRun) float64 {
			if r.GeekbenchVersion.Int64 != version {
				return 0
			}
			if multi {
				return float64(r.GbMulti.Int64)
			}
			return float64(r.GbSingle.Int64)
		}
	}
	disk := func(bs string) func(r *model.YABSRun) float64 {
		return func(r *model.YABSRun) float64 { return yabsDiskTotal(r, bs) }
	}
	gbShort := "GB"
	if version > 0 {
		gbShort += strconv.FormatInt(version, 10)
	}
	return []yabsMetric{
		{Key: "gb_single", Label: gbLabel + " single", Short: gbShort + " single", Value: gb(false)},
		{Key: "gb_multi", Label: gbLabel + " multi", Short: gbShort + " multi", Value: gb(true)},
		{Key: "disk_4k", Label: "Disk 4k read + write", Short: "Disk 4k", Unit: "MB/s", Value: disk("4k")},
		{Key: "disk_1m", Label: "Disk 1m read + write", Short: "Disk 1m", Unit: "MB/s", Value: disk("1m")},
		{Key: "net_send", Label: "Network send (median)", Short: "Net send", Unit: "Mbit/s",
			Value: func(r *model.YABSRun) float64 { return yabsNetMedian(r, true) }},
		{Key: "net_recv", Label: "Network receive (median)", Short: "Net recv", Unit: "Mbit/s",
			Value: func(r *model.YABSRun) float64 { return yabsNetMedian(r, false) }},
	}
}

// yabsPoint is one measurement: Run indexes the time-ordered run slice.
type yabsPoint struct {
	Run int
	V   float64
}

// yabsSeries is a metric's measurements over a history, oldest first, with
// the summary the views compare against.
type yabsSeries struct {
	Points           []yabsPoint
	Median, Min, Max float64
}

// newYabsSeries collects the runs that measured value.
func newYabsSeries(runs []model.YABSRun, value func(r *model.YABSRun) float64) yabsSeries {
	var s yabsSeries
	var vals []float64
	for i := range runs {
		v := value(&runs[i])
		if v <= 0 || math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		s.Points = append(s.Points, yabsPoint{Run: i, V: v})
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return s
	}
	s.Median, s.Min, s.Max = median(vals), vals[0], vals[0]
	for _, v := range vals {
		s.Min, s.Max = math.Min(s.Min, v), math.Max(s.Max, v)
	}
	return s
}

// hasBaseline reports whether the median is backed by enough measurements to
// compare against.
func (s yabsSeries) hasBaseline() bool { return len(s.Points) >= yabsMinBaseline }

// at returns the series position of the measurement taken in run, or -1.
func (s yabsSeries) at(run int) int {
	for i, p := range s.Points {
		if p.Run == run {
			return i
		}
	}
	return -1
}

// low reports whether v sits yabsLowPct or more below the baseline.
func (s yabsSeries) low(v float64) bool {
	return s.hasBaseline() && v > 0 && v <= s.Median*(1-yabsLowPct/100)
}

// median of vals (0 for none). vals is not modified.
func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := append([]float64(nil), vals...)
	sort.Float64s(sorted)
	mid := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[mid]
	}
	return (sorted[mid-1] + sorted[mid]) / 2
}

// yabsDelta describes a measurement against a reference value.
type yabsDelta struct {
	Pct   float64
	Text  string // "▲ 4.2%", "▼ 18%"
	Class string // delta-good | delta-bad | delta-flat
}

// newYabsDelta compares cur against ref; nil when either is unmeasured.
// lowerBetter flips which direction is good (latency). A change inside the
// noise band keeps its arrow and figure but is classed flat: it is reported,
// not judged.
func newYabsDelta(cur, ref float64, lowerBetter bool) *yabsDelta {
	if cur <= 0 || ref <= 0 {
		return nil
	}
	pct := (cur - ref) / ref * 100
	d := &yabsDelta{Pct: pct, Class: "delta-flat"}
	abs := math.Abs(pct)
	arrow := "▲"
	if pct < 0 {
		arrow = "▼"
	}
	switch {
	case abs < 0.05:
		d.Text = "0%"
		return d
	case abs < 10:
		d.Text = fmt.Sprintf("%s %.1f%%", arrow, abs)
	default:
		d.Text = fmt.Sprintf("%s %.0f%%", arrow, abs)
	}
	if abs >= yabsNoisePct {
		if (pct > 0) != lowerBetter {
			d.Class = "delta-good"
		} else {
			d.Class = "delta-bad"
		}
	}
	return d
}

// fmtYABS renders a benchmark number: "—" when unmeasured, whole numbers with
// thousands separators from 100 up, one decimal below, two below 1.
func fmtYABS(v float64) string {
	switch {
	case v <= 0 || math.IsNaN(v) || math.IsInf(v, 0):
		return "—"
	case v >= 99.95:
		return commaInt(int64(math.Round(v)))
	case v >= 1:
		return strconv.FormatFloat(v, 'f', 1, 64)
	default:
		return strconv.FormatFloat(v, 'f', 2, 64)
	}
}

// commaInt renders n with thousands separators (1284 → "1,284").
func commaInt(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		return "-" + s
	}
	return s
}

// yabsSpark is the geometry of a tile's trend line in a 200×40 viewBox.
type yabsSpark struct {
	Points  string // polyline points, oldest → newest
	MedianY string // y of the host-median rule; "" until there is a baseline
	EndX    string // the newest measurement, drawn as a dot
	EndY    string
	Label   string // accessible summary
}

// newYabsSpark lays a series out for drawing; nil with fewer than two
// measurements (one point is not a trend).
//
// The vertical scale always spans at least ±20% around the median. Scaling to
// the data's own min and max would stretch 2% of benchmark jitter across the
// whole tile and make a steady host look erratic; with a floor on the range,
// a flat line means steady and a drop is as deep as it is real.
func newYabsSpark(s yabsSeries, label string) *yabsSpark {
	n := len(s.Points)
	if n < 2 {
		return nil
	}
	const (
		left, right = 5.0, 195.0
		top, bottom = 6.0, 34.0
	)
	lo, hi := math.Min(s.Min, s.Median*0.8), math.Max(s.Max, s.Median*1.2)
	y := func(v float64) float64 { return bottom - (v-lo)/(hi-lo)*(bottom-top) }
	var b strings.Builder
	var x float64
	for i, p := range s.Points {
		if i > 0 {
			b.WriteByte(' ')
		}
		x = left + (right-left)*float64(i)/float64(n-1)
		fmt.Fprintf(&b, "%.1f,%.1f", x, y(p.V))
	}
	last := s.Points[n-1].V
	spark := &yabsSpark{
		Points: b.String(),
		EndX:   strconv.FormatFloat(x, 'f', 1, 64),
		EndY:   strconv.FormatFloat(y(last), 'f', 1, 64),
		Label: fmt.Sprintf("%s over %d runs: %s to %s",
			label, n, fmtYABS(s.Points[0].V), fmtYABS(last)),
	}
	if s.hasBaseline() {
		spark.MedianY = strconv.FormatFloat(y(s.Median), 'f', 1, 64)
		spark.Label += ", median " + fmtYABS(s.Median)
	}
	return spark
}

// yabsChange is one system field that differs between two runs.
type yabsChange struct {
	Field    string
	From, To string
}

// yabsSystemChanges compares the system description of two runs. Hardware
// fields (CPU model, core count, RAM) changing usually means the provider
// moved the host to another node; software fields (kernel, distro) change
// with ordinary upgrades and are reported separately so they explain a shift
// without raising an alarm. A field missing on either side is not a change.
func yabsSystemChanges(prev, cur *model.YABS) (hardware, software []yabsChange) {
	diff := func(field, a, b string) (yabsChange, bool) {
		a, b = strings.TrimSpace(a), strings.TrimSpace(b)
		if a == "" || b == "" || a == b {
			return yabsChange{}, false
		}
		return yabsChange{Field: field, From: a, To: b}, true
	}
	cores := func(y *model.YABS) string {
		if y.CPUCores.Int64 <= 0 {
			return ""
		}
		return strconv.FormatInt(y.CPUCores.Int64, 10)
	}
	for _, f := range []struct{ field, a, b string }{
		{"CPU", prev.CPU.String, cur.CPU.String},
		{"Cores", cores(prev), cores(cur)},
		{"RAM", prev.RAM.String, cur.RAM.String},
	} {
		if c, ok := diff(f.field, f.a, f.b); ok {
			hardware = append(hardware, c)
		}
	}
	for _, f := range []struct{ field, a, b string }{
		{"Kernel", prev.Kernel.String, cur.Kernel.String},
		{"Distro", prev.Distro.String, cur.Distro.String},
	} {
		if c, ok := diff(f.field, f.a, f.b); ok {
			software = append(software, c)
		}
	}
	return hardware, software
}
