// Package prom is a minimal Prometheus HTTP API client for live metrics.
package prom

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Client queries a Prometheus server's instant-query API.
type Client struct {
	// BaseURL is injectable for tests.
	BaseURL string
	// HTTPClient defaults to a 5s-timeout client when nil.
	HTTPClient *http.Client
}

// New creates a client for baseURL (e.g. "http://prometheus:9090").
func New(baseURL string) *Client {
	return &Client{
		BaseURL:    baseURL,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

// Sample is one instant-vector result: metric labels + value.
type Sample struct {
	Metric map[string]string
	Value  float64
}

// queryResponse mirrors the Prometheus /api/v1/query envelope.
type queryResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  [2]any            `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Query runs an instant query and returns the result vector.
func (c *Client) Query(ctx context.Context, promql string) ([]Sample, error) {
	hc := c.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 5 * time.Second}
	}
	u := c.BaseURL + "/api/v1/query?query=" + url.QueryEscape(promql)
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
	var body queryResponse
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
	var out []Sample
	for _, r := range body.Data.Result {
		var v float64
		if len(r.Value) == 2 {
			if s, ok := r.Value[1].(string); ok {
				v, _ = strconv.ParseFloat(s, 64)
			}
		}
		out = append(out, Sample{Metric: r.Metric, Value: v})
	}
	return out, nil
}

// HostMetrics holds the live numbers for one machine.
type HostMetrics struct {
	Instance       string
	Found          bool
	Online         bool
	CPUPct         float64
	RAMPct         float64
	DiskPct        float64
	NetRxBps       float64
	NetTxBps       float64
	Uptime30d      float64
	Uptime30dValid bool
}

// Metrics bundles everything ServerMetrics fetched: lookup by nodename
// (matching key) and by instance. Healthy reports whether at least one
// query succeeded — distinguishes "prometheus down" from "zero targets".
type Metrics struct {
	ByNodename map[string]*HostMetrics
	ByInstance map[string]*HostMetrics
	Healthy    bool
}

// Instant queries used by ServerMetrics.
var serverQueries = map[string]string{
	"up":     `up`,
	"uname":  `node_uname_info`,
	"cpu":    `100 - (avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])) * 100)`,
	"ram":    `100 * (1 - (node_memory_MemAvailable_bytes / node_memory_MemTotal_bytes))`,
	"disk":   `100 * (1 - (node_filesystem_avail_bytes{mountpoint="/",fstype!~"tmpfs|overlay"} / node_filesystem_size_bytes{mountpoint="/",fstype!~"tmpfs|overlay"}))`,
	"net_rx": `sum by (instance) (rate(node_network_receive_bytes_total{device!~"lo|veth.*|docker.*"}[5m]))`,
	"net_tx": `sum by (instance) (rate(node_network_transmit_bytes_total{device!~"lo|veth.*|docker.*"}[5m]))`,
	"uptime": `avg_over_time(up[30d:15m]) * 100`,
}

// queryBatch runs queries with bounded parallelism (3 in flight).
func (c *Client) queryBatch(ctx context.Context, queries map[string]string) map[string][]Sample {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 3)
	var mu sync.Mutex
	out := map[string][]Sample{}
	for key, ql := range queries {
		wg.Add(1)
		sem <- struct{}{}
		go func(key, ql string) {
			defer wg.Done()
			defer func() { <-sem }()
			samples, err := c.Query(ctx, ql)
			if err != nil {
				return // tolerate
			}
			mu.Lock()
			out[key] = samples
			mu.Unlock()
		}(key, ql)
	}
	wg.Wait()
	return out
}

// ServerMetrics batch-fetches all host metrics in parallel (shared 8s
// deadline across the batch, ~3 queries in flight at a time). Individual
// query failures are tolerated — whatever was fetched is returned.
func (c *Client) ServerMetrics(ctx context.Context) *Metrics {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	m := &Metrics{
		ByNodename: map[string]*HostMetrics{},
		ByInstance: map[string]*HostMetrics{},
	}

	var mu sync.Mutex
	hostFor := func(instance string) *HostMetrics {
		mu.Lock()
		defer mu.Unlock()
		h, ok := m.ByInstance[instance]
		if !ok {
			h = &HostMetrics{Instance: instance}
			m.ByInstance[instance] = h
		}
		return h
	}

	results := c.queryBatch(ctx, serverQueries)

	apply := func(key string, fn func(Sample)) {
		samples, ok := results[key]
		if !ok {
			return // tolerate: leave those fields at zero values
		}
		m.Healthy = true
		for _, s := range samples {
			fn(s)
		}
	}

	apply("uname", func(s Sample) {
		instance := s.Metric["instance"]
		nodename := s.Metric["nodename"]
		if instance == "" || nodename == "" {
			return
		}
		m.ByNodename[nodename] = hostFor(instance)
	})
	apply("up", func(s Sample) {
		h := hostFor(s.Metric["instance"])
		h.Found = true
		h.Online = s.Value == 1
	})
	apply("cpu", func(s Sample) { hostFor(s.Metric["instance"]).CPUPct = s.Value })
	apply("ram", func(s Sample) { hostFor(s.Metric["instance"]).RAMPct = s.Value })
	apply("disk", func(s Sample) { hostFor(s.Metric["instance"]).DiskPct = s.Value })
	apply("net_rx", func(s Sample) { hostFor(s.Metric["instance"]).NetRxBps = s.Value })
	apply("net_tx", func(s Sample) { hostFor(s.Metric["instance"]).NetTxBps = s.Value })
	apply("uptime", func(s Sample) {
		h := hostFor(s.Metric["instance"])
		h.Uptime30d = s.Value
		h.Uptime30dValid = true
	})

	// Mark hosts known via uname as found even when up is missing.
	for _, h := range m.ByNodename {
		if h.Found {
			continue
		}
		// Only mark found if we have any signal at all.
		if h.Online || h.CPUPct != 0 || h.RAMPct != 0 {
			h.Found = true
		}
	}
	return m
}

// UptimePct returns the 30-day uptime percentage for one instance. The 15m
// subquery step keeps it cheap (full-resolution 30d range vectors can take
// several seconds against busy Prometheus servers and blow the client
// timeout); 15m sampling is plenty for an uptime percentage.
func (c *Client) UptimePct(ctx context.Context, instance string) (float64, error) {
	samples, err := c.Query(ctx,
		`avg_over_time(up{instance=`+strconv.Quote(instance)+`}[30d:15m]) * 100`)
	if err != nil {
		return 0, err
	}
	if len(samples) == 0 {
		return 0, fmt.Errorf("no data")
	}
	return samples[0].Value, nil
}
