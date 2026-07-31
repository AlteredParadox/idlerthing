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

package yabs

import (
	"math"
	"os"
	"testing"
)

// fixture mirrors the documented yabs.sh -s JSON shape.
const fixture = `{
  "version": "v2025-04-20",
  "date": "2026-07-25 10:00:00",
  "os": {"distro": "Debian GNU/Linux 12 (bookworm)", "kernel": "6.1.0-31-amd64", "uptime": "10 days 3:22"},
  "cpu": {"model": "AMD EPYC 7443P 24-Core Processor", "cores": 4},
  "memory": {"ram": "7.7 GiB", "swap": "975.0 MiB"},
  "disk": {"fio": [
    {"bs": "4k", "read": "88.0 MB/s", "write": "88.3 MB/s"},
    {"bs": "64k", "read": "1.02 GB/s", "write": "1.02 GB/s"},
    {"bs": "512k", "read": "2.15 GB/s", "write": "2.26 GB/s"},
    {"bs": "1m", "read": "2.47 GB/s", "write": "2.64 GB/s"}
  ]},
  "network": {"iperf": [
    {"location": "Clermont-Ferrand", "provider": "SCALEWAY", "send": "1.91 Gbits/sec", "recv": "1.89 Gbits/sec", "latency": "31.4 ms"},
    {"location": "Frankfurt", "provider": "Clouvider", "send": "1.76 Gbits/sec", "recv": "1.80 Gbits/sec", "latency": "18.2 ms"}
  ]},
  "geekbench": {"version": 6, "single": 1686, "multi": 4820, "url": "https://browser.geekbench.com/v6/cpu/12345678"}
}`

func TestParseFixture(t *testing.T) {
	r, err := Parse([]byte(fixture))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.Distro != "Debian GNU/Linux 12 (bookworm)" || r.Kernel != "6.1.0-31-amd64" {
		t.Fatalf("os: %+v", r)
	}
	if r.CPU != "AMD EPYC 7443P 24-Core Processor" || r.CPUCores != 4 {
		t.Fatalf("cpu: %+v", r)
	}
	if r.RAM != "7.7 GiB" || r.Swap != "975.0 MiB" {
		t.Fatalf("memory: %+v", r)
	}
	if r.GbSingle != 1686 || r.GbMulti != 4820 || r.GeekbenchVersion != 6 {
		t.Fatalf("geekbench: %+v", r)
	}
	if r.GbURL != "https://browser.geekbench.com/v6/cpu/12345678" {
		t.Fatalf("gb url: %s", r.GbURL)
	}
	if len(r.Disks) != 4 {
		t.Fatalf("expected 4 disk rows, got %d", len(r.Disks))
	}
	if r.Disks[0].BlockSize != "4k" || r.Disks[0].ReadMbps != 88 {
		t.Fatalf("4k row: %+v", r.Disks[0])
	}
	if r.Disks[1].ReadMbps != 1020 || r.Disks[1].WriteMbps != 1020 {
		t.Fatalf("64k GB/s normalization: %+v", r.Disks[1])
	}
	if len(r.Network) != 2 {
		t.Fatalf("expected 2 network rows, got %d", len(r.Network))
	}
	if r.Network[0].SendMbps != 1910 || r.Network[0].RecvMbps != 1890 || r.Network[0].LatencyMs != 31.4 {
		t.Fatalf("network normalization: %+v", r.Network[0])
	}
	if r.PayloadHash == "" {
		t.Fatal("payload hash missing")
	}
	if r.RunAt != "2026-07-25 10:00:00" {
		t.Fatalf("run_at: %s", r.RunAt)
	}
}

func TestParseMissingSections(t *testing.T) {
	// Only a cpu section; everything else absent.
	r, err := Parse([]byte(`{"cpu": {"model": "QEMU Virtual CPU", "cores": 1}}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CPU != "QEMU Virtual CPU" || r.CPUCores != 1 {
		t.Fatalf("cpu: %+v", r)
	}
	if len(r.Disks) != 0 || len(r.Network) != 0 || r.GbSingle != 0 || r.Distro != "" {
		t.Fatalf("missing sections should be empty: %+v", r)
	}
}

func TestParseInvalidJSON(t *testing.T) {
	if _, err := Parse([]byte(`{not json`)); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
}

func TestUnitFactorIEC(t *testing.T) {
	if got := speedToMBps("1.5 GiB/s"); got < 1610.6 || got > 1610.7 {
		t.Fatalf("GiB conversion: %v", got)
	}
	if got := speedToMBps("500 MiB/s"); got < 524.2 || got > 524.3 {
		t.Fatalf("MiB conversion: %v", got)
	}
	if got := speedToMBps("1.02 GB/s"); got != 1020 {
		t.Fatalf("SI GB still works: %v", got)
	}
}

// Batch I #4 — non-finite/implausible numbers collapse to 0 instead of
// persisting (Go's json encoder would otherwise 500 the export).
func TestParseAbsurdNumbers(t *testing.T) {
	r, err := Parse([]byte(`{
	  "cpu": {"model": "X", "cores": 1000000000},
	  "disk": {"fio": [{"bs": "4k", "read": "99999999999999 MB/s", "write": "1 MB/s"}]},
	  "network": {"iperf": [{"location": "L", "send": "99999999999999 Gbits/sec", "recv": "2 Gbits/sec", "latency": "99999999999999 ms"}]},
	  "geekbench": {"version": 6, "single": 99999999999, "multi": 5}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CPUCores != 0 {
		t.Fatalf("absurd cores should collapse to 0, got %d", r.CPUCores)
	}
	if len(r.Disks) != 1 || r.Disks[0].ReadMbps != 0 || r.Disks[0].WriteMbps != 1 {
		t.Fatalf("disk speeds: %+v", r.Disks)
	}
	if len(r.Network) != 1 || r.Network[0].SendMbps != 0 || r.Network[0].RecvMbps != 2000 {
		t.Fatalf("network speeds: %+v", r.Network)
	}
	if r.Network[0].LatencyMs != 0 {
		t.Fatalf("absurd latency should collapse to 0, got %v", r.Network[0].LatencyMs)
	}
	if r.GbSingle != 0 || r.GbMulti != 5 {
		t.Fatalf("geekbench: single=%d multi=%d", r.GbSingle, r.GbMulti)
	}
}

// Batch J #11 — huge-but-FINITE values beyond plausibility bounds are
// treated as absent (not persisted).
func TestParseHugeButFinite(t *testing.T) {
	r, err := Parse([]byte(`{
	  "disk": {"fio": [{"bs": "4k", "read": "2000000 MB/s", "write": "500 MB/s"}]},
	  "network": {"iperf": [{"location": "L", "send": "2000 Gbits/sec", "recv": "1 Gbits/sec", "latency": "200000 ms"}]},
	  "geekbench": {"version": 6, "single": 2000000, "multi": 5000}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// 2e6 MB/s > 1e6 cap → 0; 500 MB/s fine.
	if r.Disks[0].ReadMbps != 0 || r.Disks[0].WriteMbps != 500 {
		t.Fatalf("disk caps: %+v", r.Disks[0])
	}
	// 2000 Gbit/s = 2e6 Mbit/s > cap → 0; latency 2e5 ms > 1e5 → 0.
	if r.Network[0].SendMbps != 0 || r.Network[0].RecvMbps != 1000 || r.Network[0].LatencyMs != 0 {
		t.Fatalf("network caps: %+v", r.Network[0])
	}
	// gb single 2e6 > 1e6 → 0; multi 5000 fine.
	if r.GbSingle != 0 || r.GbMulti != 5000 {
		t.Fatalf("gb caps: single=%d multi=%d", r.GbSingle, r.GbMulti)
	}
}

// Batch P #7g — negative speeds, scores, and latency are treated as absent.
func TestParseNegativeValues(t *testing.T) {
	r, err := Parse([]byte(`{
	  "cpu": {"model": "X", "cores": -4},
	  "disk": {"fio": [{"bs": "4k", "read": "-88 MB/s", "write": "1 MB/s"}]},
	  "network": {"iperf": [{"location": "L", "send": "-1 Gbits/sec", "latency": "-5 ms"}]},
	  "geekbench": {"version": 6, "single": -100, "multi": 5}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if r.CPUCores != 0 {
		t.Fatalf("negative cores: %d", r.CPUCores)
	}
	if r.Disks[0].ReadMbps != 0 || r.Disks[0].WriteMbps != 1 {
		t.Fatalf("disk: %+v", r.Disks[0])
	}
	if r.Network[0].SendMbps != 0 || r.Network[0].LatencyMs != 0 {
		t.Fatalf("network: %+v", r.Network[0])
	}
	if r.GbSingle != 0 || r.GbMulti != 5 {
		t.Fatalf("gb: single=%d multi=%d", r.GbSingle, r.GbMulti)
	}
}

// TestParseRealPayload runs the parser against genuine yabs.sh output
// (v2026-07-24, captured from a live box) rather than a hand-written shape.
// The invented fixture above is what let four separate key-name mismatches
// ship green: geekbench-as-array, fio speed_r/speed_w, iperf loc, and mode.
// Any future schema drift should be caught HERE first.
func TestParseRealPayload(t *testing.T) {
	body, err := os.ReadFile("testdata/real_v2026-07-24.json")
	if err != nil {
		t.Fatal(err)
	}
	r, err := Parse(body)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	// System info: mem is a bare KiB number + a units key, uptime raw
	// seconds, and the stamp is yabs.sh's own "Ymd-His".
	if r.RunAt != "2026-07-30 16:31:30" {
		t.Errorf("RunAt = %q", r.RunAt)
	}
	if r.CPU != "AMD Ryzen 9 9950X 16-Core Processor" || r.CPUCores != 2 {
		t.Errorf("CPU = %q cores=%d", r.CPU, r.CPUCores)
	}
	if r.RAM != "2.1 GiB" || r.Swap != "1.0 GiB" {
		t.Errorf("RAM = %q Swap = %q (want formatted, not raw KiB)", r.RAM, r.Swap)
	}
	if r.Uptime != "8 days, 22 hours, 33 minutes" {
		t.Errorf("Uptime = %q (want humanized, not raw seconds)", r.Uptime)
	}

	// fio: speed_r/speed_w in KBps per the row's own speed_units. 40034 KBps
	// is 40.034 MB/s — reading it as MB/s would overstate the disk by 1000x.
	if len(r.Disks) != 4 {
		t.Fatalf("disks = %d, want 4", len(r.Disks))
	}
	if r.Disks[0].BlockSize != "4k" ||
		math.Abs(r.Disks[0].ReadMbps-40.034) > 1e-6 ||
		math.Abs(r.Disks[0].WriteMbps-40.130) > 1e-6 {
		t.Errorf("disk[0] = %+v", r.Disks[0])
	}
	if math.Abs(r.Disks[1].ReadMbps-196.122) > 1e-6 {
		t.Errorf("disk[1] read = %v, want 196.122", r.Disks[1].ReadMbps)
	}

	// geekbench is an ARRAY carrying both v5 and v6; the highest wins.
	if r.GeekbenchVersion != 6 {
		t.Errorf("GeekbenchVersion = %d, want 6", r.GeekbenchVersion)
	}
	if r.GbURL != "https://browser.geekbench.com/v6/cpu/18872474" {
		t.Errorf("GbURL = %q", r.GbURL)
	}
	// This capture has "single": null / "multi": null at source — the run
	// uploaded but its scores never came back. Absent stays 0; it must not
	// pick up the v5 entry's values or invent one.
	if r.GbSingle != 0 || r.GbMulti != 0 {
		t.Errorf("null scores should stay 0, got single=%d multi=%d", r.GbSingle, r.GbMulti)
	}

	// iperf: 7 locations tested over BOTH families = 14 rows, each tagged.
	if len(r.Network) != 14 {
		t.Fatalf("network rows = %d, want 14", len(r.Network))
	}
	var v4, v6 int
	for _, n := range r.Network {
		switch n.Mode {
		case "IPv4":
			v4++
		case "IPv6":
			v6++
		default:
			t.Errorf("row %q has no mode", n.Location)
		}
		if n.Location == "" {
			t.Errorf("blank location on %+v", n)
		}
	}
	if v4 != 7 || v6 != 7 {
		t.Errorf("mode split = %d v4 / %d v6, want 7/7", v4, v6)
	}

	first := r.Network[0]
	if first.Location != "London, UK (10G)" || first.Provider != "Clouvider" {
		t.Errorf("network[0] = %+v", first)
	}
	// "1.40 Gbits/sec" is a BIT rate: 1400 Mbit/s, not 175.
	if first.SendMbps != 1400 || first.RecvMbps != 719 || first.LatencyMs != 113 {
		t.Errorf("network[0] speeds = %+v", first)
	}

	// A "busy " endpoint parses to 0 but keeps its row: the test was
	// attempted, and dropping it would look identical to never running.
	busy := r.Network[3]
	if busy.Location != "Singapore, SG (10G)" || busy.SendMbps != 0 || busy.RecvMbps != 854 {
		t.Errorf("busy row = %+v", busy)
	}
}
