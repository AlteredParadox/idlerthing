// Package yabs parses yabs.sh benchmark JSON into a normalized struct.
package yabs

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
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
	Location  string
	Provider  string
	SendMbps  float64
	RecvMbps  float64
	LatencyMs float64
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

	r.RunAt = firstString(m, "date", "time", "timestamp")
	r.CPU = firstString(m, "cpu.model", "cpu_model", "system.cpu.model")
	r.CPUCores = firstInt(m, "cpu.cores", "cpu_cores", "cores", "system.cpu.cores")
	r.RAM = firstString(m, "memory.ram", "ram", "mem.ram", "system.ram")
	r.Swap = firstString(m, "memory.swap", "swap", "mem.swap", "system.swap")
	r.Distro = firstString(m, "os.distro", "distro", "system.os.distro")
	r.Kernel = firstString(m, "os.kernel", "kernel", "system.os.kernel")
	r.Uptime = firstString(m, "os.uptime", "uptime", "system.uptime")

	gb := digMap(m, "geekbench", "gb")
	r.GeekbenchVersion = firstInt(gb, "version")
	r.GbSingle = firstInt(gb, "single", "single_core", "singlecore", "score")
	r.GbMulti = firstInt(gb, "multi", "multi_core", "multicore")
	r.GbURL = firstString(gb, "url", "link")

	for _, arr := range digArrays(m, "disk.fio", "fio", "disk", "disk_speed") {
		for _, item := range arr {
			row, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ds := DiskSpeed{
				BlockSize: firstString(row, "bs", "block_size", "blocksize", "size"),
				ReadMbps:  speedToMBps(firstAny(row, "read", "read_speed", "reads")),
				WriteMbps: speedToMBps(firstAny(row, "write", "write_speed", "writes")),
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
				Location:  firstString(row, "location", "city", "region"),
				Provider:  firstString(row, "provider", "isp", "host", "sponsor"),
				SendMbps:  speedToMBps(firstAny(row, "send", "send_speed", "upload", "up")),
				RecvMbps:  speedToMBps(firstAny(row, "recv", "receive", "recv_speed", "download", "down")),
				LatencyMs: numberToFloat(firstAny(row, "latency", "latency_ms", "ping", "rtt")),
			}
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

// firstInt returns the first path's value as an int.
func firstInt(m map[string]any, paths ...string) int {
	return int(numberToFloat(firstAny(m, paths...)))
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
func parseNumberWithUnit(s string) float64 {
	f, _ := strconv.ParseFloat(numberPart(s), 64)
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

// speedToMBps normalizes disk speeds to MB/s.
func speedToMBps(v any) float64 {
	if f, ok := v.(float64); ok {
		return f // raw numbers are assumed MB/s already
	}
	if s, ok := v.(string); ok {
		return parseNumberWithUnit(s) * unitFactor(s)
	}
	return 0
}
