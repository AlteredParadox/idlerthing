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
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// liveMonFakeProm serves instant + range data for one host (live-low/a:9100).
func liveMonFakeProm(w http.ResponseWriter, r *http.Request) {
	vec := func(samples ...string) {
		fmt.Fprint(w, `{"status":"success","data":{"resultType":"vector","result":[`+strings.Join(samples, ",")+`]}}`)
	}
	s := func(metric, value string) string {
		return fmt.Sprintf(`{"metric":{%s},"value":[1719300000,%q]}`, metric, value)
	}
	q := r.URL.Query().Get("query")
	matrix := func(vals ...float64) {
		var b strings.Builder
		fmt.Fprintf(&b, `{"status":"success","data":{"resultType":"matrix","result":[{"values":[`)
		for i, v := range vals {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `[171930000%d,"%g"]`, i*60, v)
		}
		fmt.Fprintf(&b, `]}]}}`)
		fmt.Fprint(w, b.String())
	}

	switch {
	case r.URL.Path == "/api/v1/query_range":
		switch {
		case strings.Contains(q, "mode=\"idle\""):
			matrix(10, 20, 30, 40, 50, 90, 20)
		case strings.Contains(q, "MemAvailable"):
			matrix(50, 55, 60, 65, 70, 65, 62)
		case strings.Contains(q, "sum(node_filesystem_avail"):
			matrix(40, 45, 50, 55, 60, 55, 50)
		default:
			matrix(1, 2, 3, 4, 5, 6, 7)
		}
	case q == `up`:
		vec(s(`"instance":"a:9100"`, "1"))
	case strings.HasPrefix(q, "avg_over_time"):
		vec(s(`"instance":"a:9100"`, "99.5"))
	case q == `node_uname_info`:
		vec(s(`"instance":"a:9100","nodename":"live-low"`, "1"))
	case strings.Contains(q, "node_cpu_seconds_total"):
		vec(s(`"instance":"a:9100"`, "12"))
	case strings.Contains(q, "node_memory_MemAvailable"):
		vec(s(`"instance":"a:9100"`, "31"))
	case strings.Contains(q, "node_filesystem_size_bytes"):
		vec(
			s(`"device":"/dev/sda1","mountpoint":"/","fstype":"ext4"`, "100000000000"),
			s(`"device":"/dev/sdb1","mountpoint":"/data","fstype":"xfs"`, "200000000000"),
		)
	case strings.Contains(q, "node_filesystem_avail_bytes"):
		vec(
			s(`"device":"/dev/sda1","mountpoint":"/","fstype":"ext4"`, "25000000000"),
			s(`"device":"/dev/sdb1","mountpoint":"/data","fstype":"xfs"`, "50000000000"),
		)
	case strings.Contains(q, "node_filesystem_avail"):
		vec(s(`"instance":"a:9100"`, "44"))
	case strings.Contains(q, "node_network_receive"):
		vec(s(`"instance":"a:9100"`, "1500000"))
	case strings.Contains(q, "node_network_transmit"):
		vec(s(`"instance":"a:9100"`, "400000"))
	default:
		vec()
	}
}

func TestLiveMonSection(t *testing.T) {
	ts, database := newTestServer(t)
	promSrv := httptest.NewServer(http.HandlerFunc(liveMonFakeProm))
	defer promSrv.Close()
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)

	client := authedClient(t, ts)
	createServer(t, client, ts, "live-low")

	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()

	if !strings.Contains(body, "Live monitoring") {
		t.Fatal("expected Live monitoring section")
	}
	// All 10 metric cards.
	for _, name := range []string{"CPU", "IOWait", "Steal", "RAM", "Swap", "Disk", "Net RX", "Net TX", "Disk read", "Disk write"} {
		if !strings.Contains(body, `>`+name+`<`) {
			t.Fatalf("missing card %q", name)
		}
	}
	// Current/avg/max from the canned cpu series (10,20,30,40,50,90,20):
	// current 20%, avg ~37%, max 90%.
	if !strings.Contains(body, "20%") || !strings.Contains(body, "90%") {
		t.Fatal("expected current/max values from series")
	}
	if !strings.Contains(body, "avg ") || !strings.Contains(body, "max ") {
		t.Fatal("expected avg/max stats line")
	}
	// Sparklines.
	if !strings.Contains(body, "<polyline class=\"spark-line\"") {
		t.Fatal("expected sparkline polylines")
	}
	// Filesystem table.
	if !strings.Contains(body, "/data") || !strings.Contains(body, "75%") {
		t.Fatal("expected filesystem rows with used pct")
	}

	// Unmatched server: no section.
	createServer(t, client, ts, "not-monitored")
	resp, err = client.Get(ts.URL + "/servers/2")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "Live monitoring") {
		t.Fatal("unmatched server should not get the section")
	}
}

func TestLiveMonUnavailable(t *testing.T) {
	ts, database := newTestServer(t)
	// Instant queries work (match succeeds) but query_range AND filesystem
	// queries are broken → total failure → the unavailable note.
	promSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		if r.URL.Path == "/api/v1/query_range" || strings.Contains(q, "node_filesystem") {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		liveMonFakeProm(w, r)
	}))
	defer promSrv.Close()
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = ? WHERE id = 1", promSrv.URL)
	client := authedClient(t, ts)
	createServer(t, client, ts, "live-low")

	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "Live data unavailable") {
		t.Fatal("expected unavailable note when range queries fail")
	}
	if strings.Contains(body, "metric-card") {
		t.Fatal("no metric cards expected on total failure")
	}
}

func TestLiveMonPromDown(t *testing.T) {
	ts, database := newTestServer(t)
	// Enabled but pointing at a dead server: no metrics at all → no section
	// (matching fails, page renders as today).
	database.Exec("UPDATE settings SET prometheus_enabled = 1, prometheus_url = 'http://127.0.0.1:1/down' WHERE id = 1")
	client := authedClient(t, ts)
	createServer(t, client, ts, "live-low")

	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if strings.Contains(body, "Live monitoring") {
		t.Fatal("dead prometheus should not render the section")
	}
}
