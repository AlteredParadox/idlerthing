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

// Package yabs parses yabs.sh benchmark JSON into a normalized struct.
package yabs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// Result is the normalized benchmark payload.
type Result struct {
	RunAt            string
	CPU              string
	CPUCores         int
	RAM              string
	Swap             string
	Distro           string
	Kernel           string
	Uptime           string
	GeekbenchVersion int
	GbSingle         int
	GbMulti          int
	GbURL            string
	Disks            []DiskSpeed
	Network          []NetworkSpeed
	PayloadHash      string
}

// DiskSpeed is one fio block-size row, normalized to MB/s.
type DiskSpeed struct {
	BlockSize string
	ReadMbps  float64
	WriteMbps float64
}

// NetworkSpeed is one iperf location row, normalized to Mbit/s + ms.
type NetworkSpeed struct {
	Location string
	Provider string
	// Mode is the address family the run used: "IPv4", "IPv6", or "" when
	// the payload does not say. yabs.sh tests each location over both, so
	// without this the two results collapse into indistinguishable rows.
	Mode      string
	SendMbps  float64
	RecvMbps  float64
	LatencyMs float64
}

// Plausibility bounds for normalized values — beyond these the payload is
// corrupt, not fast (100 GB/s is beyond any real fio/iperf run).
const (
	maxSpeedMbps = 1e6
	maxLatencyMs = 1e5
	maxGBScore   = 1e6
)

// capped returns v, or 0 when v is negative or exceeds the plausibility
// bound (speeds and latencies are never negative).
func capped(v, max float64) float64 {
	if v < 0 || math.Abs(v) > max {
		return 0
	}
	return v
}

// Parse decodes a yabs.sh JSON body. Sections are optional; unknown shapes
// degrade to empty values rather than errors. Only invalid JSON is an error.
func Parse(body []byte) (*Result, error) {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, err
	}
	m, _ := root.(map[string]any)
	if m == nil {
		m = map[string]any{}
	}

	r := &Result{}
	sum := sha256.Sum256(body)
	r.PayloadHash = hex.EncodeToString(sum[:])

	r.RunAt = runAtString(firstString(m, "date", "time", "timestamp"))
	r.CPU = firstString(m, "cpu.model", "cpu_model", "system.cpu.model")
	r.CPUCores = firstInt(m, "cpu.cores", "cpu_cores", "cores", "system.cpu.cores")
	r.RAM = memString(m,
		[]string{"mem.ram", "memory.ram", "ram", "system.ram"},
		[]string{"mem.ram_units", "memory.ram_units", "ram_units"})
	r.Swap = memString(m,
		[]string{"mem.swap", "memory.swap", "swap", "system.swap"},
		[]string{"mem.swap_units", "memory.swap_units", "swap_units"})
	r.Distro = firstString(m, "os.distro", "distro", "system.os.distro")
	r.Kernel = firstString(m, "os.kernel", "kernel", "system.os.kernel")
	r.Uptime = uptimeString(m, "os.uptime", "uptime", "system.uptime")

	gb := bestGeekbench(m)
	r.GeekbenchVersion = firstInt(gb, "version")
	r.GbSingle = firstInt(gb, "single", "single_core", "singlecore", "score")
	r.GbMulti = firstInt(gb, "multi", "multi_core", "multicore")
	if r.GbSingle > maxGBScore {
		r.GbSingle = 0
	}
	if r.GbMulti > maxGBScore {
		r.GbMulti = 0
	}
	r.GbURL = firstString(gb, "url", "link")

	for _, arr := range digArrays(m, "disk.fio", "fio", "disk", "disk_speed") {
		for _, item := range arr {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			unitHint := firstString(row, "speed_units", "units")
			readVal, readUnit := fioValue(row, unitHint, "speed_r", "read", "read_speed", "reads")
			writeVal, writeUnit := fioValue(row, unitHint, "speed_w", "write", "write_speed", "writes")
			ds := DiskSpeed{
				BlockSize: firstString(row, "bs", "block_size", "blocksize", "size"),
				ReadMbps:  fioSpeed(readVal, readUnit),
				WriteMbps: fioSpeed(writeVal, writeUnit),
			}
			if ds.BlockSize == "" && ds.ReadMbps == 0 && ds.WriteMbps == 0 {
				continue
			}
			r.Disks = append(r.Disks, ds)
		}
		if len(r.Disks) > 0 {
			break
		}
	}

	for _, arr := range digArrays(m, "network.iperf", "iperf", "network", "network_speed", "iperf3") {
		for _, item := range arr {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ns := NetworkSpeed{
				// "loc" is what yabs.sh actually emits; the rest are aliases.
				Location:  firstString(row, "loc", "location", "city", "region"),
				Provider:  firstString(row, "provider", "isp", "host", "sponsor"),
				Mode:      normalizeMode(firstString(row, "mode", "ip_mode", "family")),
				SendMbps:  speedToMBps(firstAny(row, "send", "send_speed", "upload", "up")),
				RecvMbps:  speedToMBps(firstAny(row, "recv", "receive", "recv_speed", "download", "down")),
				LatencyMs: capped(numberToFloat(firstAny(row, "latency", "latency_ms", "ping", "rtt")), maxLatencyMs),
			}
			// Keep a row that names a location even with no throughput:
			// yabs.sh reports "busy " for both directions when an iperf
			// endpoint refuses the run, and a row rendering as "—" says the
			// test was attempted, which dropping it silently would not.
			if ns.Location == "" && ns.Provider == "" && ns.SendMbps == 0 && ns.RecvMbps == 0 {
				continue
			}
			r.Network = append(r.Network, ns)
		}
		if len(r.Network) > 0 {
			break
		}
	}

	return r, nil
}

// runAtString normalizes the run timestamp. yabs.sh stamps "20260730-163130";
// anything it does not recognize passes through untouched rather than being
// dropped, since a raw stamp still beats a blank field.
func runAtString(s string) string {
	if t, err := time.Parse("20060102-150405", strings.TrimSpace(s)); err == nil {
		return t.Format("2006-01-02 15:04:05")
	}
	return s
}

// byteFactor converts a declared memory unit to bytes. yabs.sh reports RAM
// and swap in KiB, so that is the default when no unit key accompanies them.
func byteFactor(unit string) float64 {
	switch strings.ToUpper(strings.TrimSpace(unit)) {
	case "B", "BYTES":
		return 1
	case "KB":
		return 1000
	case "MB":
		return 1e6
	case "GB":
		return 1e9
	case "MIB":
		return 1 << 20
	case "GIB":
		return 1 << 30
	default: // "KIB" and absent both mean KiB here
		return 1 << 10
	}
}

// memString renders a memory amount for display. yabs.sh reports a bare
// number alongside a sibling units key ("ram": 2153728, "ram_units": "KiB"),
// which read raw as an unlabelled seven-digit number. A payload that already
// carries a formatted string ("7.7 GiB") passes straight through.
func memString(m map[string]any, valuePaths, unitPaths []string) string {
	v := firstAny(m, valuePaths...)
	if s, ok := v.(string); ok {
		return s
	}
	f, ok := v.(float64)
	if !ok || f <= 0 {
		return ""
	}
	bytes := f * byteFactor(firstString(m, unitPaths...))
	if bytes >= 1<<30 {
		return strconv.FormatFloat(bytes/(1<<30), 'f', 1, 64) + " GiB"
	}
	return strconv.FormatFloat(bytes/(1<<20), 'f', 0, 64) + " MiB"
}

// uptimeString renders os.uptime. Modern yabs.sh emits raw seconds
// (772401.24); older shapes carried a preformatted string.
func uptimeString(m map[string]any, paths ...string) string {
	v := firstAny(m, paths...)
	if s, ok := v.(string); ok {
		return s
	}
	f, ok := v.(float64)
	if !ok || f < 0 {
		return ""
	}
	secs := int(f)
	days, hours, mins := secs/86400, (secs%86400)/3600, (secs%3600)/60
	if days > 0 {
		return fmt.Sprintf("%d days, %d hours, %d minutes", days, hours, mins)
	}
	return fmt.Sprintf("%d hours, %d minutes", hours, mins)
}

// fioValue picks one fio speed and the unit to read it with.
//
// yabs.sh emits bare numbers under speed_r/speed_w together with a row-level
// "speed_units" (KBps in every version seen). Those default to KBps when the
// hint is absent — a bare speed_r is never MB/s, and assuming so overstates
// the disk by 1000x. The generic read/write spellings come from hand-written
// payloads that carry the unit inside the string, so they keep the original
// already-MB/s default.
func fioValue(row map[string]any, unitHint, native string, aliases ...string) (any, string) {
	if v := firstAny(row, native); v != nil {
		if unitHint == "" {
			unitHint = "KBps"
		}
		return v, unitHint
	}
	return firstAny(row, aliases...), unitHint
}

// fioSpeed normalizes a disk speed to MB/s. A string carrying its own unit
// ("88.0 MB/s") wins over the row-level hint; a bare value uses the hint.
func fioSpeed(v any, unitHint string) float64 {
	switch t := v.(type) {
	case float64:
		return capped(t*unitFactor(unitHint), maxSpeedMbps)
	case string:
		unit := t
		if strings.TrimSpace(numberPart(t)) == strings.TrimSpace(t) {
			unit = unitHint // digits only — no unit of its own
		}
		return capped(parseNumberWithUnit(t)*unitFactor(unit), maxSpeedMbps)
	}
	return 0
}

// bestGeekbench returns the geekbench entry to record. Real yabs.sh emits an
// ARRAY — one run can carry both a v5 and a v6 result — while the older
// hand-written shape was a single object, so accept either. When several
// entries are present the highest version wins: the yabs table holds one
// score set, and v6 is what current runs are compared against.
func bestGeekbench(m map[string]any) map[string]any {
	if obj := digMap(m, "geekbench", "gb"); len(obj) > 0 {
		return obj
	}
	var best map[string]any
	bestVer := -1
	for _, arr := range digArrays(m, "geekbench", "gb") {
		for _, item := range arr {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if v := firstInt(row, "version"); v > bestVer {
				best, bestVer = row, v
			}
		}
	}
	if best == nil {
		return map[string]any{}
	}
	return best
}

// normalizeMode canonicalizes an iperf row's address family. yabs.sh writes
// "IPv4"/"IPv6"; accept any casing plus the bare forms so a version that
// spells it differently still splits correctly instead of silently merging.
func normalizeMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "ipv4", "v4", "4", "inet":
		return "IPv4"
	case "ipv6", "v6", "6", "inet6":
		return "IPv6"
	}
	return ""
}

// digMap walks the first key path that resolves to a map.
func digMap(m map[string]any, paths ...string) map[string]any {
	for _, p := range paths {
		cur := any(m)
		ok := true
		for _, key := range strings.Split(p, ".") {
			mm, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			cur = mm[key]
		}
		if ok {
			if mm, isMap := cur.(map[string]any); isMap {
				return mm
			}
		}
	}
	return map[string]any{}
}

// digArrays walks each path and returns every array it resolves to.
func digArrays(m map[string]any, paths ...string) [][]any {
	var out [][]any
	for _, p := range paths {
		cur := any(m)
		ok := true
		for _, key := range strings.Split(p, ".") {
			mm, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			cur = mm[key]
		}
		if ok {
			if arr, isArr := cur.([]any); isArr {
				out = append(out, arr)
			}
		}
	}
	return out
}

// firstString returns the first path's value as a string.
func firstString(m map[string]any, paths ...string) string {
	v := firstAny(m, paths...)
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	}
	return ""
}

// firstAny returns the raw value at the first existing key path.
func firstAny(m map[string]any, paths ...string) any {
	for _, p := range paths {
		cur := any(m)
		ok := true
		for _, key := range strings.Split(p, ".") {
			mm, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			if v, exists := mm[key]; exists {
				cur = v
			} else {
				ok = false
				break
			}
		}
		if ok && cur != nil {
			return cur
		}
	}
	return nil
}

// firstInt returns the first path's value as an int (implausible
// magnitudes or negatives collapse to 0 instead of overflowing).
func firstInt(m map[string]any, paths ...string) int {
	f := numberToFloat(firstAny(m, paths...))
	if f > 1e7 || f < 0 {
		return 0
	}
	return int(f)
}

// numberToFloat converts numbers or numeric strings (units stripped).
func numberToFloat(v any) float64 {
	switch t := v.(type) {
	case nil:
		return 0
	case float64:
		return t
	case string:
		return parseNumberWithUnit(t)
	}
	return 0
}

// parseNumberWithUnit extracts the leading number from e.g. "1.23 Gbits/sec".
// NaN, infinities, and implausible magnitudes (>1e12) are treated as
// absent (0) rather than persisted — Go's json encoder rejects non-finite
// floats, and absurd values would poison exports and displays.
func parseNumberWithUnit(s string) float64 {
	f, err := strconv.ParseFloat(numberPart(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e12 {
		return 0
	}
	return f
}

// numberPart returns the leading numeric token of a string.
func numberPart(s string) string {
	s = strings.TrimSpace(s)
	end := 0
	for i, c := range s {
		if (c >= '0' && c <= '9') || c == '.' || c == '-' || c == '+' {
			end = i + 1
		} else if end > 0 {
			break
		}
	}
	if end == 0 {
		return "0"
	}
	return s[:end]
}

// unitFactor extracts a size/rate multiplier from a unit string. IEC
// units (GiB/MiB/KiB) are checked before the SI substrings so they win.
func unitFactor(s string) float64 {
	s = strings.ToUpper(s)
	switch {
	case strings.Contains(s, "GIB"):
		return 1073.741824 // 1024^3 / 1e6 MB
	case strings.Contains(s, "MIB"):
		return 1.048576 // 1024^2 / 1e6 MB
	case strings.Contains(s, "KIB"):
		return 0.001048576
	case strings.Contains(s, "GB"), strings.Contains(s, "GBIT"):
		return 1000
	case strings.Contains(s, "KB"), strings.Contains(s, "KBIT"):
		return 0.001
	case strings.Contains(s, "MB"), strings.Contains(s, "MBIT"):
		return 1
	case strings.Contains(s, "B/S") || strings.Contains(s, "BPS"):
		return 0.000001
	}
	return 1
}

// speedToMBps normalizes disk speeds to MB/s, capped at the plausibility
// bound (raw numbers are assumed MB/s already).
func speedToMBps(v any) float64 {
	if f, ok := v.(float64); ok {
		return capped(f, maxSpeedMbps)
	}
	if s, ok := v.(string); ok {
		return capped(parseNumberWithUnit(s)*unitFactor(s), maxSpeedMbps)
	}
	return 0
}
