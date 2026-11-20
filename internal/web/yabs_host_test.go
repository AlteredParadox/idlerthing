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
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"idlerthing/internal/model"
)

// hostRun describes one stored run for the dashboard tests.
type hostRun struct {
	at            string
	cpu           string
	kernel        string
	single, multi int64
	gbVersion     int64
	disk, net     float64 // 4k read/write MB/s; iperf send Mbit/s (0 = endpoint busy)
}

// seedHostRuns stores runs for serverID in the given order and returns their
// ids. gbVersion defaults to 6.
func seedHostRuns(t *testing.T, database *sql.DB, serverID int64, runs ...hostRun) []int64 {
	t.Helper()
	st := &model.YABSStore{DB: database}
	var ids []int64
	for _, r := range runs {
		if r.gbVersion == 0 {
			r.gbVersion = 6
		}
		id, err := st.Create(context.Background(), &model.YABS{
			ServerID: serverID, RunAt: sqlNs(r.at), CPU: sqlNs(r.cpu), CPUCores: sqlNi(4),
			RAM: sqlNs("3.8 GiB"), Kernel: sqlNs(r.kernel), Distro: sqlNs("Debian 13"),
			GbSingle: sqlNi(r.single), GbMulti: sqlNi(r.multi), GeekbenchVersion: sqlNi(r.gbVersion),
		}, []model.YABSDiskSpeed{
			{BlockSize: "4k", ReadMbps: r.disk, WriteMbps: r.disk},
			{BlockSize: "1m", ReadMbps: r.disk * 10, WriteMbps: r.disk * 10},
		}, []model.YABSNetworkSpeed{
			{Location: "London, UK (10G)", Provider: "Clouvider", Mode: "IPv4", SendMbps: r.net, RecvMbps: r.net / 2, LatencyMs: 134},
			{Location: "NYC, NY, US (10G)", Provider: "Leaseweb", Mode: "IPv4", SendMbps: r.net, RecvMbps: r.net / 2, LatencyMs: 65},
		})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

const (
	cpuA = "AMD EPYC 7543 32-Core Processor"
	cpuB = "Intel(R) Xeon(R) CPU E5-2680 v2"
)

// A server with no runs gets an empty state that leads to the add-run page.
func TestYABSHostDashboardEmpty(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "empty-host")
	code, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	if code != http.StatusOK || !strings.Contains(page, "No YABS runs yet") ||
		!strings.Contains(page, `href="/servers/1/yabs/new"`) {
		t.Fatalf("empty dashboard (%d) should offer to add a run", code)
	}
	for _, absent := range []string{"ybs-tile", "Compare selected", "<svg"} {
		if strings.Contains(page, absent) {
			t.Errorf("empty dashboard should not render %q", absent)
		}
	}
	if code, _ := getPage(t, client, ts.URL+"/servers/99/yabs"); code != http.StatusNotFound {
		t.Errorf("unknown server = %d, want 404", code)
	}
}

// One run: values, but no comparisons, trend, or compare controls yet.
func TestYABSHostDashboardSingleRun(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "one-run-host")
	seedHostRuns(t, database, 1, hostRun{at: "2026-11-05 08:00:00", cpu: cpuA, kernel: "6.12.0", single: 1723, multi: 5210, disk: 50, net: 400})

	code, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	if code != http.StatusOK {
		t.Fatalf("dashboard: %d", code)
	}
	for _, want := range []string{
		"1 run · 2026-11-05", "Geekbench 6 single", "1,723", "5,210",
		">100<", // 4k read + write
		"trends start with the next one",
		`href="/servers/1/yabs/1"`, // latest run + history row
		"Disk by block size · latest run</h2>",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("single-run dashboard missing %q", want)
		}
	}
	for _, absent := range []string{"vs previous", "vs median", "<svg", "Compare selected", `name="run"`, "Median read", "badge-warn"} {
		if strings.Contains(page, absent) {
			t.Errorf("single-run dashboard should not render %q", absent)
		}
	}
}

// A history with a real drop: the latest run is flagged against the host's
// own median, the hardware change is called out, and rows are in run-time
// order even though the oldest run was stored last.
func TestYABSHostDashboardHistory(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "history-host")
	seedHostRuns(t, database, 1,
		hostRun{at: "2026-06-01 09:00:00", cpu: cpuA, kernel: "6.1.0-28", single: 1650, multi: 3100, disk: 80, net: 600},
		hostRun{at: "2026-07-01 09:00:00", cpu: cpuA, kernel: "6.1.0-28", single: 1660, multi: 3120, disk: 81, net: 610},
		hostRun{at: "2026-08-01 09:00:00", cpu: cpuA, kernel: "6.1.0-31", single: 1640, multi: 3090, disk: 79, net: 0},
		hostRun{at: "2026-11-01 09:00:00", cpu: cpuB, kernel: "6.1.0-31", single: 760, multi: 1420, disk: 45, net: 400},
		// Stored last, ran first: must sort to the bottom, not become "latest".
		hostRun{at: "2026-05-01 09:00:00", cpu: cpuA, kernel: "6.1.0-28", single: 1655, multi: 3110, disk: 80, net: 605},
	)

	code, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	if code != http.StatusOK {
		t.Fatalf("dashboard: %d", code)
	}
	for _, want := range []string{
		"5 runs · 2026-05-01 to 2026-11-01",
		`<a class="btn" href="/servers/1/yabs/4">Latest run</a>`,
		// Latest value, against the previous run (1,640) and the median (1,650).
		">760<", "▼ 54%</span>", "vs previous", "vs median", "median 1,650 · range 760–1,660 · 5 runs",
		"The latest run is 15% or more below this host&#39;s median for: Geekbench 6 single",
		"CPU changed since the previous run: " + cpuA + " → " + "Intel(R) Xeon(R) CPU E5-2680 v2",
		"The host may have been moved to different hardware.",
		`<span class="badge badge-warn">low</span>`,
		"CPU changed</span>", "Kernel changed</span>",
		"<svg", "ybs-spark-median", "ybs-spark-dot",
		`aria-label="Geekbench 6 single over 5 runs: 1,655 to 760, median 1,650"`,
		"Compare selected", `action="/servers/1/yabs/compare"`, `name="run" value="5"`,
		"against this host's median", "Median read", "Median send",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("history dashboard missing %q", want)
		}
	}
	// Newest first by run time: 4 (Nov), 3, 2, 1, then 5 (May).
	order := []string{`value="4"`, `value="3"`, `value="2"`, `value="1"`, `value="5"`}
	last := -1
	for _, marker := range order {
		pos := strings.Index(page, `name="run" `+marker)
		if pos < 0 || pos < last {
			t.Fatalf("history rows out of order at %s (pos %d after %d)", marker, pos, last)
		}
		last = pos
	}
	// The August run's network was busy: it has no network value and takes no
	// part in the network baseline (median of 600, 610, 605, 400 = 602.5).
	if !strings.Contains(page, "median 603 · range 400–610 · 4 runs") {
		t.Errorf("the busy run must stay out of the network series")
	}
	// The software-only change (kernel) on the August run is not a warning banner.
	if strings.Contains(page, "Kernel changed since the previous run") {
		t.Errorf("only the latest run's changes become banners")
	}
}

// A steady host raises no flags: small movements are shown, not judged.
func TestYABSHostDashboardSteady(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "steady-host")
	seedHostRuns(t, database, 1,
		hostRun{at: "2026-06-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 680},
		hostRun{at: "2026-07-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1120, multi: 3010, disk: 100, net: 690},
		hostRun{at: "2026-08-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1090, multi: 2960, disk: 98, net: 670},
	)
	_, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	for _, absent := range []string{"badge-warn", "ybs-notice", "delta-bad", "delta-good", "ybs-low"} {
		if strings.Contains(page, absent) {
			t.Errorf("a steady host should not render %q", absent)
		}
	}
	if !strings.Contains(page, "delta-flat") || !strings.Contains(page, "▼ 2.7%") {
		t.Errorf("small changes should still be shown, unjudged")
	}
}

// A run benchmarked with another Geekbench major version is shown, marked,
// and kept out of the comparisons.
func TestYABSHostDashboardMixedGeekbench(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "gb-mix-host")
	seedHostRuns(t, database, 1,
		hostRun{at: "2025-06-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 900, multi: 2100, gbVersion: 5, disk: 80, net: 600},
		hostRun{at: "2026-06-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1650, multi: 3100, disk: 80, net: 600},
		hostRun{at: "2026-07-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1660, multi: 3120, disk: 80, net: 600},
	)
	_, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	for _, want := range []string{"Geekbench 6 single", "GB6 single", "900 · v5", "2,100 · v5", "2 runs · a median needs 3"} {
		if !strings.Contains(page, want) {
			t.Errorf("mixed-version dashboard missing %q", want)
		}
	}
	// 900 → 1650 would be +83%; it must not be reported as a change.
	if strings.Contains(page, "▲ 83%") {
		t.Errorf("a cross-version Geekbench delta was computed")
	}
}

// The latest run's busy endpoint renders as unmeasured against its history.
func TestYABSHostDashboardBusyLatest(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "busy-latest-host")
	seedHostRuns(t, database, 1,
		hostRun{at: "2026-06-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 680},
		hostRun{at: "2026-07-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 690},
		hostRun{at: "2026-08-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 670},
		hostRun{at: "2026-09-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 0},
	)
	code, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	if code != http.StatusOK {
		t.Fatalf("dashboard: %d", code)
	}
	if !strings.Contains(page, "from 2026-08-01; the latest run did not measure it") {
		t.Errorf("the network tile should say its value is from the last run that measured it")
	}
	// Busy is not "low": an unmeasured value is never flagged.
	if strings.Contains(page, "badge-warn") || strings.Contains(page, "ybs-low") {
		t.Errorf("an unmeasured endpoint must not be flagged as low")
	}
}

func TestYABSCompare(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "compare-host")
	createServer(t, client, ts, "other-host")
	ids := seedHostRuns(t, database, 1,
		hostRun{at: "2026-09-15 09:00:00", cpu: cpuA, kernel: "6.1.0-28", single: 1685, multi: 3167, disk: 80, net: 600},
		hostRun{at: "2026-11-01 09:00:00", cpu: cpuB, kernel: "6.1.0-28", single: 752, multi: 1461, disk: 44, net: 400},
		hostRun{at: "2025-01-01 09:00:00", cpu: cpuA, kernel: "5.10", single: 900, multi: 2100, gbVersion: 5, disk: 80, net: 0},
	)
	other := seedHostRuns(t, database, 2, hostRun{at: "2026-01-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1, multi: 1, disk: 1, net: 1})

	// Ticked newer-first: still laid out older → newer.
	code, page := getPage(t, client, ts.URL+"/servers/1/yabs/compare?run=2&run=1")
	if code != http.StatusOK {
		t.Fatalf("compare: %d", code)
	}
	for _, want := range []string{
		"2026-09-15 09:00 → 2026-11-01 09:00",
		cpuA, "Intel(R) Xeon(R) CPU E5-2680 v2", `<span class="badge badge-warn">changed</span>`,
		"Geekbench 6 single", "1,685", "752", "▼ 55%",
		"4k read", "80.0", "44.0", "▼ 45%",
		"London, UK (10G)", "600 → 400", "▼ 33%",
		`href="/servers/1/yabs/1"`, `href="/servers/1/yabs/2"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("compare page missing %q", want)
		}
	}
	if n := strings.Count(page, `<span class="badge badge-warn">changed</span>`); n != 1 {
		t.Errorf("changed badges = %d, want 1 (CPU only)", n)
	}

	// Different Geekbench versions: shown side by side, no percentage.
	_, page = getPage(t, client, ts.URL+"/servers/1/yabs/compare?run=3&run=1")
	if !strings.Contains(page, "Geekbench version") || !strings.Contains(page, "Geekbench single") || strings.Contains(page, "▲ 87%") {
		t.Errorf("cross-version compare must not compute a Geekbench change")
	}
	// One side busy: values shown, no change computed for that endpoint.
	if !strings.Contains(page, "— → 600") {
		t.Errorf("a busy endpoint should read as unmeasured in the comparison")
	}

	// Anything but two distinct runs of this server goes back with a hint.
	for _, q := range []string{
		"", "run=1", "run=1&run=1", "run=1&run=2&run=3", "run=1&run=x",
		"run=1&run=" + itoa(other[0]), "run=" + itoa(ids[0]) + "&run=999",
	} {
		resp, err := client.Get(ts.URL + "/servers/1/yabs/compare?" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/servers/1/yabs" {
			t.Errorf("compare?%s = %d → %q, want 303 → /servers/1/yabs", q, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	if _, back := getPage(t, client, ts.URL+"/servers/1/yabs"); !strings.Contains(back, "Select exactly two runs to compare.") {
		t.Errorf("the redirect should carry a hint")
	}
	if code, _ := getPage(t, client, ts.URL+"/servers/99/yabs/compare?run=1&run=2"); code != http.StatusNotFound {
		t.Errorf("unknown server = %d, want 404", code)
	}
	if code, _ := getPage(t, newClient(t), ts.URL+"/servers/1/yabs/compare?run=1&run=2"); code == http.StatusOK {
		t.Errorf("compare served without a session")
	}
}

// A run that has only a multi-core Geekbench score is a scored run: it shows
// on the dashboard tile and in the comparison.
func TestYABSMultiOnlyGeekbench(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "multi-only-host")
	seedHostRuns(t, database, 1,
		hostRun{at: "2026-06-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 0, multi: 4321, disk: 80, net: 600},
		hostRun{at: "2026-07-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 0, multi: 4400, disk: 80, net: 600})
	_, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	if !strings.Contains(page, "Geekbench 6 multi") || !strings.Contains(page, ">4,400<") {
		t.Errorf("a multi-only run should fill the Geekbench multi tile")
	}
	_, cmp := getPage(t, client, ts.URL+"/servers/1/yabs/compare?run=1&run=2")
	if !strings.Contains(cmp, "Geekbench 6 multi") || !strings.Contains(cmp, "4,321") || !strings.Contains(cmp, "▲ 1.8%") {
		t.Errorf("multi-only runs should appear in the comparison")
	}
}

// Scores that do not record their Geekbench version are shown and never
// compared — on the dashboard and in the two-run view.
func TestYABSUnversionedGeekbench(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "unversioned-host")
	createServer(t, client, ts, "mixed-host")
	st := &model.YABSStore{DB: database}
	add := func(serverID int64, at string, single, multi, version int64) {
		t.Helper()
		if _, err := st.Create(context.Background(), &model.YABS{
			ServerID: serverID, RunAt: sqlNs(at), CPU: sqlNs(cpuA),
			GbSingle: sqlNi(single), GbMulti: sqlNi(multi), GeekbenchVersion: sqlNi(version),
		}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Server 1: two scores, neither says which version.
	add(1, "2026-01-01 00:00:00", 900, 2000, 0)
	add(1, "2026-02-01 00:00:00", 1900, 5000, 0)
	// Server 2: a known v6 score, then a newer unversioned one.
	add(2, "2026-01-01 00:00:00", 1500, 4000, 6)
	add(2, "2026-02-01 00:00:00", 1900, 5000, 0)

	_, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	for _, want := range []string{"1,900 · v?", "900 · v?", "Geekbench version differs or was not recorded; not compared"} {
		if !strings.Contains(page, want) {
			t.Errorf("unversioned dashboard missing %q", want)
		}
	}
	for _, absent := range []string{"▲ 111%", "vs previous", "badge-warn"} {
		if strings.Contains(page, absent) {
			t.Errorf("unversioned scores were compared: found %q", absent)
		}
	}
	_, cmp := getPage(t, client, ts.URL+"/servers/1/yabs/compare?run=1&run=2")
	if !strings.Contains(cmp, "Geekbench version") || !strings.Contains(cmp, "1,900") || strings.Contains(cmp, "▲ 111%") {
		t.Errorf("two unversioned scores must be shown without a percentage")
	}

	// Mixed: the known version is followed; the unversioned run is listed raw.
	_, page = getPage(t, client, ts.URL+"/servers/2/yabs")
	for _, want := range []string{"Geekbench 6 single", ">1,500<", "1,900 · v?", "1 run · trends start with the next one"} {
		if !strings.Contains(page, want) {
			t.Errorf("mixed dashboard missing %q", want)
		}
	}
	if strings.Contains(page, "▲ 27%") {
		t.Errorf("a known score was compared with an unversioned one")
	}
	_, cmp = getPage(t, client, ts.URL+"/servers/2/yabs/compare?run=3&run=4")
	if strings.Contains(cmp, "▲ 27%") || !strings.Contains(cmp, "Geekbench version") {
		t.Errorf("known vs unversioned must not produce a percentage")
	}
}

// An endpoint that answers pings in every run but is busy for iperf has a
// latency history and no throughput history; its latency median is shown.
func TestYABSLatencyBaselineWithoutThroughput(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "latency-host")
	seedHostRuns(t, database, 1,
		hostRun{at: "2026-06-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 0},
		hostRun{at: "2026-07-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 0},
		hostRun{at: "2026-08-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 0})
	_, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	if !strings.Contains(page, "Median latency") || !strings.Contains(page, `<td class="mono col-muted">134</td>`) {
		t.Errorf("the latency median should be shown when only latency has a history")
	}
}

// The history table is not inside the compare form: the row-link click
// handler skips anything inside a form, so wrapping the table would stop a
// click on a row from opening its run. The checkboxes join the form by id.
func TestYABSHistoryRowsStayClickable(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "click-host")
	seedHostRuns(t, database, 1,
		hostRun{at: "2026-06-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 600},
		hostRun{at: "2026-07-01 09:00:00", cpu: cpuA, kernel: "6.1", single: 1100, multi: 2980, disk: 99, net: 600})
	_, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	form := strings.Index(page, `<form id="yabs-compare" method="get" action="/servers/1/yabs/compare">`)
	if form < 0 {
		t.Fatal("compare form missing")
	}
	closed := form + strings.Index(page[form:], "</form>")
	table := strings.Index(page, `<table class="tbl">`)
	if closed < form || table < closed {
		t.Fatalf("the compare form must be closed before the history table (form=%d close=%d table=%d)", form, closed, table)
	}
	if strings.Count(page, `<input type="checkbox" form="yabs-compare" name="run"`) != 2 ||
		!strings.Contains(page, `<button class="btn" type="submit" form="yabs-compare">`) {
		t.Errorf("checkboxes and the button must be associated with the form by id")
	}
	if !strings.Contains(page, `<tr class="row-link" data-href="/servers/1/yabs/2">`) {
		t.Errorf("history rows should be row links")
	}
}

// A latest run that reached only one of the usual iperf locations shows what
// it measured and is not compared with the runs that measured more: no drop,
// no low flag, and the tile says why.
func TestYABSNetworkPartialLatestRun(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "partial-net-host")
	st := &model.YABSStore{DB: database}
	add := func(at string, fast, slow float64) {
		t.Helper()
		if _, err := st.Create(context.Background(), &model.YABS{ServerID: 1, RunAt: sqlNs(at), CPU: sqlNs(cpuA)}, nil,
			[]model.YABSNetworkSpeed{
				{Location: "Fast A", Mode: "IPv4", SendMbps: fast, RecvMbps: fast, LatencyMs: 5},
				{Location: "Fast B", Mode: "IPv4", SendMbps: fast, RecvMbps: fast, LatencyMs: 6},
				{Location: "Slow C", Mode: "IPv4", SendMbps: slow, RecvMbps: slow, LatencyMs: 180},
			}); err != nil {
			t.Fatal(err)
		}
	}
	add("2026-06-01 09:00:00", 900, 100)
	add("2026-07-01 09:00:00", 900, 100)
	add("2026-08-01 09:00:00", 900, 100)
	add("2026-09-01 09:00:00", 0, 100) // both fast endpoints busy; the slow one unchanged

	_, page := getPage(t, client, ts.URL+"/servers/1/yabs")
	for _, want := range []string{
		">100<", // the headline is what the latest run measured
		"not compared · the other 3 runs measured other locations",
		// The breakdown still follows the one location that did answer.
		"Slow C", "Median send",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("partial network run: page missing %q", want)
		}
	}
	for _, absent := range []string{"▼ 89%", "badge-warn", "ybs-notice", "ybs-low"} {
		if strings.Contains(page, absent) {
			t.Errorf("a run that measured fewer locations must not read as a drop: found %q", absent)
		}
	}
}

// History() loads a server's runs with their speed rows and nobody else's.
func TestYABSHistoryLoad(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "hist-a")
	createServer(t, client, ts, "hist-b")
	seedHostRuns(t, database, 1,
		hostRun{at: "2026-01-01 00:00:00", single: 1, multi: 1, disk: 10, net: 100},
		hostRun{at: "2026-02-01 00:00:00", single: 2, multi: 2, disk: 20, net: 0})
	seedHostRuns(t, database, 2, hostRun{at: "2026-03-01 00:00:00", single: 3, multi: 3, disk: 30, net: 300})

	st := &model.YABSStore{DB: database}
	runs, err := st.History(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].GbSingle.Int64 != 1 || runs[1].GbSingle.Int64 != 2 {
		t.Fatalf("runs = %+v", runs)
	}
	for i, r := range runs {
		if len(r.Disks) != 2 || len(r.Network) != 2 {
			t.Errorf("run %d: %d disk rows, %d network rows, want 2 and 2", i, len(r.Disks), len(r.Network))
		}
	}
	if runs[0].Disks[0].ReadMbps != 10 || runs[1].Disks[0].ReadMbps != 20 || runs[1].Network[0].SendMbps != 0 {
		t.Errorf("speed rows attached to the wrong run, or NULL not read as 0: %+v", runs)
	}
	if none, err := st.History(context.Background(), 99); err != nil || none != nil {
		t.Errorf("History of a server without runs = %v, %v", none, err)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
