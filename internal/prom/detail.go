package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Physical filesystem types (fork's FSTYPES, minus the job filter).
const FSTypes = "ext4|xfs|btrfs|zfs"

// NetDeviceExclude matches the fork's NET_DEVICE_EXCLUDE.
const NetDeviceExclude = "lo|docker.*|veth.*|br.*|cni.*|flannel.*"

// Point is one (time, value) sample of a range series.
type Point struct {
	T time.Time
	V float64
}

// rangeResponse mirrors the Prometheus /api/v1/query_range envelope.
type rangeResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Values [][2]any `json:"values"`
		} `json:"result"`
	} `json:"data"`
}

// QueryRange runs a range query and returns the first series' points.
func (c *Client) QueryRange(ctx context.Context, promql string, start, end time.Time, stepSeconds int) ([]Point, error) {
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	u := fmt.Sprintf("%s/api/v1/query_range?query=%s&start=%d&end=%d&step=%d",
		c.BaseURL, url.QueryEscape(promql), start.Unix(), end.Unix(), stepSeconds)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus returned %d", resp.StatusCode)
	}
	var body rangeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&body); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if body.Status != "success" {
		msg := body.Error
		if msg == "" {
			msg = body.Status
		}
		return nil, fmt.Errorf("query failed: %s", msg)
	}
	if len(body.Data.Result) == 0 {
		return nil, nil
	}
	var out []Point
	for _, v := range body.Data.Result[0].Values {
		ts, ok1 := v[0].(float64)
		vs, ok2 := v[1].(string)
		if !ok1 || !ok2 {
			continue
		}
		f, err := strconv.ParseFloat(vs, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			continue
		}
		out = append(out, Point{T: time.Unix(int64(ts), 0), V: f})
	}
	return out, nil
}

// Series is one metric's window: current/avg/max plus a sparkline series.
type Series struct {
	Found   bool
	Current float64
	Avg     float64
	Max     float64
	Points  []Point // downsampled to ~60 points
}

// summarize computes stats over points and downsamples the series.
func summarize(points []Point) Series {
	if len(points) == 0 {
		return Series{}
	}
	s := Series{Found: true, Max: points[0].V, Current: points[len(points)-1].V}
	sum := 0.0
	for _, p := range points {
		sum += p.V
		if p.V > s.Max {
			s.Max = p.V
		}
	}
	s.Avg = sum / float64(len(points))

	// Downsample to at most 60 points (even stride).
	const cap = 60
	if len(points) <= cap {
		s.Points = points
		return s
	}
	stride := float64(len(points)-1) / float64(cap-1)
	for i := 0; i < cap; i++ {
		s.Points = append(s.Points, points[int(float64(i)*stride+0.5)])
	}
	return s
}

// Detail bundles the per-instance metric windows.
type Detail struct {
	CPU       Series
	IOWait    Series
	Steal     Series
	RAM       Series
	Swap      Series
	Disk      Series
	NetRx     Series
	NetTx     Series
	DiskRead  Series
	DiskWrite Series
}

// detailQuery builds the instance-filtered PromQL for one metric.
func detailQuery(instance, metric string) string {
	inst := strconv.Quote(instance)
	fs := FSTypes
	ne := NetDeviceExclude
	switch metric {
	case "cpu":
		return `100 * (1 - avg(rate(node_cpu_seconds_total{instance=` + inst + `,mode="idle"}[2m])))`
	case "iowait":
		return `100 * avg(rate(node_cpu_seconds_total{instance=` + inst + `,mode="iowait"}[2m]))`
	case "steal":
		return `100 * avg(rate(node_cpu_seconds_total{instance=` + inst + `,mode="steal"}[2m]))`
	case "ram":
		return `100 * (1 - node_memory_MemAvailable_bytes{instance=` + inst + `} / node_memory_MemTotal_bytes{instance=` + inst + `})`
	case "swap":
		return `clamp_min(100 * (1 - node_memory_SwapFree_bytes{instance=` + inst + `} / node_memory_SwapTotal_bytes{instance=` + inst + `}), 0)`
	case "disk":
		return `100 * (1 - sum(node_filesystem_avail_bytes{instance=` + inst + `,fstype=~"` + fs + `"}) / sum(node_filesystem_size_bytes{instance=` + inst + `,fstype=~"` + fs + `"}))`
	case "net_rx":
		return `sum(rate(node_network_receive_bytes_total{instance=` + inst + `,device!~"` + ne + `"}[2m]))`
	case "net_tx":
		return `sum(rate(node_network_transmit_bytes_total{instance=` + inst + `,device!~"` + ne + `"}[2m]))`
	case "disk_read":
		return `sum(rate(node_disk_read_bytes_total{instance=` + inst + `}[1m]))`
	case "disk_write":
		return `sum(rate(node_disk_written_bytes_total{instance=` + inst + `}[1m]))`
	}
	return ""
}

// detailMetrics lists the metric keys in display order.
var detailMetrics = []string{
	"cpu", "iowait", "steal", "ram", "swap", "disk",
	"net_rx", "net_tx", "disk_read", "disk_write",
}

// HostDetail fetches all metric windows for one instance over the last 6
// hours (step 60s). Individual query failures are tolerated — the affected
// series comes back with Found=false; it never returns a non-nil error.
func (c *Client) HostDetail(ctx context.Context, instance string) *Detail {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	end := time.Now()
	start := end.Add(-6 * time.Hour)
	d := &Detail{}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var mu sync.Mutex
	series := map[string]Series{}
	for _, m := range detailMetrics {
		wg.Add(1)
		sem <- struct{}{}
		go func(m string) {
			defer wg.Done()
			defer func() { <-sem }()
			points, err := c.QueryRange(ctx, detailQuery(instance, m), start, end, 60)
			if err != nil {
				return
			}
			mu.Lock()
			series[m] = summarize(points)
			mu.Unlock()
		}(m)
	}
	wg.Wait()
	for _, m := range detailMetrics {
		s := series[m]
		switch m {
		case "cpu":
			d.CPU = s
		case "iowait":
			d.IOWait = s
		case "steal":
			d.Steal = s
		case "ram":
			d.RAM = s
		case "swap":
			d.Swap = s
		case "disk":
			d.Disk = s
		case "net_rx":
			d.NetRx = s
		case "net_tx":
			d.NetTx = s
		case "disk_read":
			d.DiskRead = s
		case "disk_write":
			d.DiskWrite = s
		}
	}
	return d
}

// Filesystem is one mounted filesystem's usage.
type Filesystem struct {
	Device     string
	Mount      string
	SizeBytes  float64
	AvailBytes float64
	UsedPct    float64
}

// Filesystems returns physical filesystems for one instance, size+avail
// joined by mountpoint, bind-mount duplicates removed, zero-size skipped.
func (c *Client) Filesystems(ctx context.Context, instance string) ([]Filesystem, error) {
	inst := strconv.Quote(instance)
	filter := `{instance=` + inst + `,fstype=~"` + FSTypes + `"}`
	sizes, err := c.Query(ctx, "node_filesystem_size_bytes"+filter)
	if err != nil {
		return nil, err
	}
	avails, err := c.Query(ctx, "node_filesystem_avail_bytes"+filter)
	if err != nil {
		return nil, err
	}
	availByMount := map[string]float64{}
	for _, a := range avails {
		availByMount[a.Metric["mountpoint"]] = a.Value
	}

	// Bind-mount dedupe: keep the SHORTEST mountpoint per device
	// (deterministic — Prometheus row order isn't). Rows missing from
	// avail_bytes are skipped entirely (a 100%-used reading would be wrong).
	byDevice := map[string]Filesystem{}
	for _, sz := range sizes {
		mount := sz.Metric["mountpoint"]
		device := sz.Metric["device"]
		if mount == "" || sz.Value <= 0 {
			continue
		}
		avail, ok := availByMount[mount]
		if !ok {
			continue
		}
		if prev, exists := byDevice[device]; !exists || len(mount) < len(prev.Mount) {
			byDevice[device] = Filesystem{
				Device:     device,
				Mount:      mount,
				SizeBytes:  sz.Value,
				AvailBytes: avail,
				UsedPct:    100 * (1 - avail/sz.Value),
			}
		}
	}
	out := make([]Filesystem, 0, len(byDevice))
	for _, f := range byDevice {
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Mount < out[j].Mount })
	return out, nil
}
