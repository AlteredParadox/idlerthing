package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"idlerthing/internal/prom"
)

// ---------- settings ----------

// promSettings reads the prometheus config from settings.
func (s *Server) promSettings(r *http.Request) (enabled bool, baseURL string) {
	settings := s.memoSettings(r)
	if !settings.PrometheusEnabled {
		return false, ""
	}
	return true, strings.TrimRight(strings.TrimSpace(settings.PrometheusURL), "/")
}

// validPromURL reports whether u is an http(s) URL with a host.
func validPromURL(u string) bool {
	parsed, err := url.Parse(u)
	return err == nil && parsed.Host != "" &&
		(parsed.Scheme == "http" || parsed.Scheme == "https")
}

// ---------- metrics cache (45s, stale-on-error) ----------

// promCache caches the last successful ServerMetrics batch and the last
// fetch ATTEMPT time (failures back off refetches instead of re-running
// ~40s of queries on every request). Keyed by baseURL: a settings change
// to a different Prometheus must not serve the old server's data. inFlight
// singleflights a cold burst onto one upstream batch.
type promCache struct {
	mu       sync.Mutex
	baseURL  string
	at       time.Time
	metrics  *prom.Metrics
	lastTry  time.Time
	inFlight chan struct{}
}

// currentPromURL re-reads the prometheus URL straight from the DB (NOT the
// per-request memo, and detached from any request ctx — a disconnecting
// leader must not fail this check) — used to decide whether a slow fetch
// still belongs in the cache after a possible mid-fetch settings change.
func (s *Server) currentPromURL() string {
	var u string
	s.db.QueryRowContext(context.Background(),
		"SELECT prometheus_url FROM settings WHERE id = 1").Scan(&u)
	return strings.TrimRight(strings.TrimSpace(u), "/")
}

// liveMetrics returns metrics when prometheus is enabled, else nil.
// On fetch failure it serves stale data; with no data yet it degrades
// silently (nil — the list renders exactly as without prometheus).
func (s *Server) liveMetrics(r *http.Request) *prom.Metrics {
	enabled, baseURL := s.promSettings(r)
	if !enabled || baseURL == "" {
		return nil
	}

	// Check the cache under lock; the leader fetches WITHOUT the lock (a
	// hung prometheus must not serialize every page), followers wait on the
	// in-flight batch (singleflight) and re-check.
	var cached *prom.Metrics
	for {
		s.prom.mu.Lock()
		if s.prom.baseURL != baseURL {
			s.prom.baseURL = baseURL
			s.prom.metrics = nil
			s.prom.at = time.Time{}
			s.prom.lastTry = time.Time{}
		}
		cached = s.prom.metrics
		fresh := cached != nil && time.Since(s.prom.at) < 45*time.Second
		if fresh {
			s.prom.mu.Unlock()
			return cached
		}
		// Join the in-flight batch BEFORE the backoff check — the leader
		// just stamped lastTry, which must not read as "recent failure".
		if s.prom.inFlight != nil {
			ch := s.prom.inFlight
			s.prom.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-r.Context().Done():
				// Disconnecting waiter: serve stale immediately instead of
				// parking until the leader finishes.
				return cached
			}
		}
		backoff := time.Since(s.prom.lastTry) < 30*time.Second
		if backoff {
			s.prom.mu.Unlock()
			return cached
		}
		s.prom.inFlight = make(chan struct{})
		s.prom.lastTry = time.Now()
		s.prom.mu.Unlock()
		break
	}

	// Fetch on a DETACHED context: a disconnecting leader must not cancel
	// the batch its followers are waiting on (ServerMetrics applies its own
	// 8s deadline internally, so this stays bounded).
	m := prom.New(baseURL).ServerMetrics(context.Background())

	// Store only when settings STILL point at the same endpoint AND the
	// cache slot still belongs to it — a slow fetch to A must not land in
	// B's slot after a switch (or a triple flip A→B→A).
	stillCurrent := s.currentPromURL() == baseURL
	s.prom.mu.Lock()
	close(s.prom.inFlight)
	s.prom.inFlight = nil
	if !m.Healthy {
		s.prom.mu.Unlock()
		return cached // fetch failed: stale (or nil) until the backoff expires
	}
	// Zero results from a HEALTHY prometheus is a valid state — store it.
	if stillCurrent && s.prom.baseURL == baseURL {
		s.prom.metrics = m
		s.prom.at = time.Now()
	}
	s.prom.mu.Unlock()
	return m
}

// matchLive finds live metrics for a hostname. It tries, in order:
// nodename exact, instance exact (port stripped), then first-DNS-label
// against nodename — so both FQDN ("de-sn-csv01.example.com") and short
// ("de-sn-csv01") inventory hostnames match a typical node-exporter setup.
func matchLive(m *prom.Metrics, hostname string) *prom.HostMetrics {
	if m == nil {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(hostname))
	short := firstLabel(key)

	// Pass 1: exact matches (nodename, or instance with scrape port stripped).
	for nodename, h := range m.ByNodename {
		if !h.Found {
			continue
		}
		if strings.ToLower(strings.TrimSpace(nodename)) == key {
			return h
		}
	}
	for instance, h := range m.ByInstance {
		if !h.Found {
			continue
		}
		if stripPort(instance) == key {
			return h
		}
	}

	// Pass 2: first-label fallback — only when EXACTLY ONE candidate matches
	// (ambiguous first labels return nil rather than cross-matching).
	var found *prom.HostMetrics
	for nodename, h := range m.ByNodename {
		if !h.Found {
			continue
		}
		if firstLabel(strings.ToLower(strings.TrimSpace(nodename))) == short {
			if found != nil && found != h {
				return nil
			}
			found = h
		}
	}
	for instance, h := range m.ByInstance {
		if !h.Found {
			continue
		}
		if firstLabel(stripPort(instance)) == short {
			if found != nil && found != h {
				return nil
			}
			found = h
		}
	}
	return found
}

// ---------- meter rendering ----------

// meterView carries one thin meter bar + label.
type meterView struct {
	WidthClass string // w0..w100 in 5% buckets (CSP-safe, no inline styles)
	ColorClass string // c-ok / c-warn / c-err
	Label      string
}

// meter builds the view for a percentage.
func meter(pct float64) *meterView {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	bucket := int(pct/5+0.5) * 5
	if bucket > 100 {
		bucket = 100
	}
	color := "c-ok"
	if pct > 85 {
		color = "c-err"
	} else if pct >= 60 {
		color = "c-warn"
	}
	return &meterView{
		WidthClass: fmt.Sprintf("w%d", bucket),
		ColorClass: color,
		Label:      fmt.Sprintf("%.0f%%", pct),
	}
}

// pctInline renders the compact-mode inline pct ("31%").
func pctInline(pct float64) string {
	return fmt.Sprintf("%.0f%%", pct)
}

// fmtBps renders bytes/sec as Mbit/s throughput.
func fmtBps(bps float64) string {
	mbps := bps * 8 / 1e6
	switch {
	case mbps >= 1000:
		return fmt.Sprintf("%.1f Gbps", mbps/1000)
	case mbps >= 10:
		return fmt.Sprintf("%.0f Mbps", mbps)
	case mbps >= 1:
		return fmt.Sprintf("%.1f Mbps", mbps)
	default:
		return fmt.Sprintf("%.0f Kbps", bps*8/1e3)
	}
}

// throughput renders "↓ 12 Mbps ↑ 3 Mbps".
func throughput(h *prom.HostMetrics) string {
	return "↓ " + fmtBps(h.NetRxBps) + " ↑ " + fmtBps(h.NetTxBps)
}

// ---------- detail Live card ----------

// liveView is the Live card payload on the server detail page. OnlineKnown
// false means the `up` query failed — render "unknown", never "down".
type liveView struct {
	Online      bool
	OnlineKnown bool
	CPU         *meterView
	RAM         *meterView
	Disk        *meterView
	Throughput  string
	Uptime      string
}

// uptimeCache caches 30-day uptime per instance (60s success, 30s failure),
// keyed by the prometheus baseURL, singleflighted on a cold burst.
type uptimeCache struct {
	mu       sync.Mutex
	baseURL  string
	at       map[string]time.Time
	v        map[string]float64
	failed   map[string]bool
	inFlight chan struct{}
}

// buildLive builds the Live card for one server from pre-fetched metrics
// (nil when not matched).
func (s *Server) buildLive(r *http.Request, m *prom.Metrics, hostname string) *liveView {
	h := matchLive(m, hostname)
	if h == nil {
		return nil
	}
	v := &liveView{
		Online:      h.Online,
		OnlineKnown: h.OnlineKnown,
		CPU:         meter(h.CPUPct),
		RAM:         meter(h.RAMPct),
		Disk:        meter(h.DiskPct),
		Throughput:  throughput(h),
	}
	v.Uptime = s.uptime30d(r, h.Instance)
	return v
}

// uptime30d lazily queries 30-day uptime with a 60s cache.
func (s *Server) uptime30d(r *http.Request, instance string) string {
	if instance == "" {
		return "—"
	}
	enabled, baseURL := s.promSettings(r)
	if !enabled {
		return "—"
	}

	for {
		s.uptime.mu.Lock()
		if s.uptime.at == nil || s.uptime.baseURL != baseURL {
			s.uptime.at = map[string]time.Time{}
			s.uptime.v = map[string]float64{}
			s.uptime.failed = map[string]bool{}
			s.uptime.baseURL = baseURL
		}
		at, ok := s.uptime.at[instance]
		cached := s.uptime.v[instance]
		failed := s.uptime.failed[instance]
		// Failures are cached briefly too — a down prometheus shouldn't be
		// re-queried on every page view. Check them before the success path.
		if failed && time.Since(at) < 30*time.Second {
			s.uptime.mu.Unlock()
			return "—"
		}
		if ok && !failed && time.Since(at) < 60*time.Second {
			s.uptime.mu.Unlock()
			return fmt.Sprintf("%.2f%%", cached)
		}
		if s.uptime.inFlight != nil {
			ch := s.uptime.inFlight
			s.uptime.mu.Unlock()
			select {
			case <-ch:
				continue
			case <-r.Context().Done():
				// Disconnecting waiter: degrade now, don't park.
				return "—"
			}
		}
		s.uptime.inFlight = make(chan struct{})
		s.uptime.mu.Unlock()
		break
	}

	// Detached fetch (bounded by the client's 5s timeout): a disconnecting
	// leader must not cancel the query its followers wait on or stamp a
	// spurious failure entry.
	v, err := prom.New(baseURL).UptimePct(context.Background(), instance)

	// Same store-guard as liveMetrics: only cache against the endpoint the
	// fetch actually went to AND while the slot still belongs to it.
	stillCurrent := s.currentPromURL() == baseURL
	s.uptime.mu.Lock()
	close(s.uptime.inFlight)
	s.uptime.inFlight = nil
	if stillCurrent && s.uptime.baseURL == baseURL {
		s.uptime.at[instance] = time.Now()
		if err != nil {
			s.uptime.failed[instance] = true
		} else {
			s.uptime.failed[instance] = false
			s.uptime.v[instance] = v
		}
	}
	s.uptime.mu.Unlock()
	if err != nil {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", v)
}

// firstLabel returns the part before the first dot.
func firstLabel(host string) string {
	if i := strings.IndexByte(host, '.'); i > 0 {
		return host[:i]
	}
	return host
}

// stripPort removes a scrape port suffix (host:9100 → host).
func stripPort(instance string) string {
	inst := strings.ToLower(strings.TrimSpace(instance))
	if i := strings.LastIndexByte(inst, ':'); i > 0 {
		return inst[:i]
	}
	return inst
}
