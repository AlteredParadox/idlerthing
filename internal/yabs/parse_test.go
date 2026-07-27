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
