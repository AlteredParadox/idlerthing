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

package prom

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// matrixJSON builds a canned query_range response with a linear series.
func matrixJSON(baseTS, n int, values []float64) string {
	var b strings.Builder
	fmt.Fprintf(&b, `{"status":"success","data":{"resultType":"matrix","result":[{"values":[`)
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `[%d,"%g"]`, int64(baseTS)+int64(i*60), v)
	}
	fmt.Fprintf(&b, `]}]}}`)
	return b.String()
}

func TestQueryRangeParses(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("step") != "60" {
			t.Errorf("expected step=60, got %s", r.URL.Query().Get("step"))
		}
		fmt.Fprint(w, matrixJSON(1719300000, 3, []float64{1, 2, 3}))
	}))
	defer ts.Close()

	points, err := New(ts.URL).QueryRange(context.Background(), "up",
		time.Unix(1719300000, 0), time.Unix(1719303600, 0), 60)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(points) != 3 || points[0].V != 1 || points[2].V != 3 {
		t.Fatalf("unexpected points: %+v", points)
	}
	if points[0].T.Unix() != 1719300000 {
		t.Fatalf("unexpected time: %v", points[0].T)
	}
}

func TestSummarizeStats(t *testing.T) {
	points := []Point{
		{V: 10}, {V: 20}, {V: 30}, {V: 40},
	}
	s := summarize(points)
	if !s.Found || s.Current != 40 || s.Avg != 25 || s.Max != 40 {
		t.Fatalf("stats wrong: %+v", s)
	}
	if len(s.Points) != 4 {
		t.Fatalf("short series should pass through: %d", len(s.Points))
	}

	// Long series downsamples to 60.
	var long []Point
	for i := 0; i < 200; i++ {
		long = append(long, Point{V: float64(i)})
	}
	s = summarize(long)
	if len(s.Points) != 60 {
		t.Fatalf("expected 60 downsampled points, got %d", len(s.Points))
	}
	if s.Points[0].V != 0 || s.Points[59].V != 199 {
		t.Fatalf("downsample endpoints wrong: %+v", s.Points)
	}

	if s := summarize(nil); s.Found {
		t.Fatal("empty series should not be Found")
	}
}

// detailHandler serves matrix responses for all HostDetail queries.
func detailHandler(t *testing.T, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("query")
	switch {
	case strings.Contains(q, "mode=\"idle\""):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{10, 20, 30, 20, 10}))
	case strings.Contains(q, "iowait"):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{1, 2, 1, 2, 1}))
	case strings.Contains(q, "steal"):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{0, 0, 0, 0, 0}))
	case strings.Contains(q, "MemAvailable"):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{50, 60, 70, 60, 50}))
	case strings.Contains(q, "SwapFree"):
		// SwapTotal=0 → Prometheus yields no data.
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"matrix","result":[]}}`)
	case strings.Contains(q, "sum(node_filesystem_avail_bytes"):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{80, 82, 84, 86, 88}))
	case strings.Contains(q, "network_receive"):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{1e6, 2e6, 3e6, 2e6, 1e6}))
	case strings.Contains(q, "network_transmit"):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{5e5, 5e5, 5e5, 5e5, 5e5}))
	case strings.Contains(q, "disk_read"):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{1e7, 2e7, 1e7, 2e7, 1e7}))
	case strings.Contains(q, "disk_written"):
		fmt.Fprint(w, matrixJSON(1719300000, 5, []float64{5e6, 5e6, 5e6, 5e6, 5e6}))
	case strings.Contains(q, "node_filesystem_size_bytes"):
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"device":"/dev/sda1","mountpoint":"/","fstype":"ext4"},"value":[1719300000,"100000000000"]},
			{"metric":{"device":"/dev/sdb1","mountpoint":"/data","fstype":"xfs"},"value":[1719300000,"200000000000"]},
			{"metric":{"device":"/dev/sda1","mountpoint":"/mnt/bind","fstype":"ext4"},"value":[1719300000,"100000000000"]},
			{"metric":{"device":"tmpfs","mountpoint":"/empty","fstype":"ext4"},"value":[1719300000,"0"]}
		]}}`)
	case strings.Contains(q, "node_filesystem_avail_bytes"):
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
			{"metric":{"device":"/dev/sda1","mountpoint":"/","fstype":"ext4"},"value":[1719300000,"25000000000"]},
			{"metric":{"device":"/dev/sdb1","mountpoint":"/data","fstype":"xfs"},"value":[1719300000,"50000000000"]},
			{"metric":{"device":"/dev/sda1","mountpoint":"/mnt/bind","fstype":"ext4"},"value":[1719300000,"25000000000"]}
		]}}`)
	default:
		w.WriteHeader(http.StatusBadRequest)
	}
}

func TestHostDetailAndFilesystems(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		detailHandler(t, w, r)
	}))
	defer ts.Close()

	c := New(ts.URL)
	d := c.HostDetail(context.Background(), "a:9100")
	if !d.CPU.Found || d.CPU.Current != 10 || d.CPU.Max != 30 || d.CPU.Avg != 18 {
		t.Fatalf("cpu: %+v", d.CPU)
	}
	if !d.IOWait.Found || d.Steal.Max != 0 {
		t.Fatalf("iowait/steal: %+v %+v", d.IOWait, d.Steal)
	}
	if d.Swap.Found {
		t.Fatal("swap should be no-data (SwapTotal=0)")
	}
	if !d.Disk.Found || d.Disk.Current != 88 {
		t.Fatalf("disk: %+v", d.Disk)
	}
	if d.NetRx.Max != 3e6 || d.DiskRead.Max != 2e7 {
		t.Fatalf("rates: %+v %+v", d.NetRx, d.DiskRead)
	}

	filesystems, err := c.Filesystems(context.Background(), "a:9100")
	if err != nil {
		t.Fatalf("Filesystems: %v", err)
	}
	if len(filesystems) != 2 {
		t.Fatalf("expected 2 filesystems (bind-dup + zero-size skipped), got %d: %+v", len(filesystems), filesystems)
	}
	if filesystems[0].Mount != "/" || filesystems[0].UsedPct != 75 {
		t.Fatalf("root fs: %+v", filesystems[0])
	}
	if filesystems[1].Mount != "/data" || filesystems[1].UsedPct != 75 {
		t.Fatalf("data fs: %+v", filesystems[1])
	}
}

// Filesystems: bind-mount dedupe keeps the SHORTEST mountpoint per device;
// rows missing from avail_bytes are skipped.
func TestFilesystemsShortestMountAndMissingAvail(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if strings.Contains(q, "node_filesystem_size_bytes") {
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"device":"/dev/sda1","mountpoint":"/mnt/bind","fstype":"ext4"},"value":[1719300000,"100000000000"]},
				{"metric":{"device":"/dev/sda1","mountpoint":"/","fstype":"ext4"},"value":[1719300000,"100000000000"]},
				{"metric":{"device":"/dev/sdb1","mountpoint":"/data","fstype":"xfs"},"value":[1719300000,"200000000000"]}
			]}}`)
			return
		}
		if strings.Contains(q, "node_filesystem_avail_bytes") {
			// /data has NO avail row — must be skipped, not read as 100% used.
			fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[
				{"metric":{"device":"/dev/sda1","mountpoint":"/mnt/bind","fstype":"ext4"},"value":[1719300000,"50000000000"]},
				{"metric":{"device":"/dev/sda1","mountpoint":"/","fstype":"ext4"},"value":[1719300000,"25000000000"]}
			]}}`)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	filesystems, err := New(ts.URL).Filesystems(context.Background(), "a:9100")
	if err != nil {
		t.Fatal(err)
	}
	if len(filesystems) != 1 {
		t.Fatalf("expected 1 filesystem (shortest mount wins, missing avail skipped), got %+v", filesystems)
	}
	if filesystems[0].Mount != "/" || filesystems[0].UsedPct != 75 {
		t.Fatalf("shortest mount + pct: %+v", filesystems[0])
	}
}
