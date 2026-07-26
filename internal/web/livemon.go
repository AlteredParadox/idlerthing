package web

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"idlerthing/internal/prom"
)

// liveMonView is the payload for the Live monitoring detail section.
type liveMonView struct {
	Cards       []liveCard
	Filesystems []fsRow
	Unavailable bool
}

// liveCard is one metric card (value, stats, sparkline).
type liveCard struct {
	Name       string
	Current    string
	AvgMax     string
	Points     string // svg polyline points
	ColorClass string // c-ok / c-warn / c-err / c-acc
	NoData     bool
}

// fsRow is one filesystem row.
type fsRow struct {
	Mount string
	Size  string
	Avail string
	Meter *meterView
}

// liveMonCache caches the section per instance (60s).
type liveMonCache struct {
	mu sync.Mutex
	at map[string]time.Time
	v  map[string]*liveMonView
}

// liveMonEntry fetches + caches the section for one instance.
func (s *Server) liveMonEntry(r *http.Request, instance string) *liveMonView {
	enabled, baseURL := s.promSettings(r)
	if !enabled || instance == "" {
		return nil
	}

	s.livemon.mu.Lock()
	if s.livemon.at == nil {
		s.livemon.at = map[string]time.Time{}
		s.livemon.v = map[string]*liveMonView{}
	}
	at, ok := s.livemon.at[instance]
	cached := s.livemon.v[instance]
	s.livemon.mu.Unlock()
	ttl := 60 * time.Second
	if cached != nil && cached.Unavailable {
		ttl = 30 * time.Second // negative-cache failures briefly
	}
	if ok && cached != nil && time.Since(at) < ttl {
		return cached
	}

	client := prom.New(baseURL)
	detail, err := client.HostDetail(r.Context(), instance)
	var view *liveMonView
	if err != nil || detail == nil {
		view = &liveMonView{Unavailable: true}
	} else {
		filesystems, _ := client.Filesystems(r.Context(), instance) // tolerate failure
		view = buildLiveMonView(detail, filesystems)
	}

	s.livemon.mu.Lock()
	s.livemon.at[instance] = time.Now()
	s.livemon.v[instance] = view
	s.livemon.mu.Unlock()
	return view
}

// buildLiveMonView converts prom.Detail + filesystems into cards.
// When every metric failed, the section degrades to an unavailable note.
func buildLiveMonView(d *prom.Detail, filesystems []prom.Filesystem) *liveMonView {
	v := &liveMonView{}
	pctCard := func(name string, s prom.Series) liveCard {
		return metricCard(name, s, func(x float64) string { return fmtPct(x) }, true)
	}
	rateCard := func(name string, s prom.Series, fmtVal func(float64) string) liveCard {
		return metricCard(name, s, fmtVal, false)
	}

	v.Cards = []liveCard{
		pctCard("CPU", d.CPU),
		pctCard("IOWait", d.IOWait),
		pctCard("Steal", d.Steal),
		pctCard("RAM", d.RAM),
		pctCard("Swap", d.Swap),
		pctCard("Disk", d.Disk),
		rateCard("Net RX", d.NetRx, fmtBps),
		rateCard("Net TX", d.NetTx, fmtBps),
		rateCard("Disk read", d.DiskRead, fmtBytesPerSec),
		rateCard("Disk write", d.DiskWrite, fmtBytesPerSec),
	}

	anyData := false
	for _, c := range v.Cards {
		if !c.NoData {
			anyData = true
			break
		}
	}
	if !anyData {
		return &liveMonView{Unavailable: true}
	}

	for _, f := range filesystems {
		v.Filesystems = append(v.Filesystems, fsRow{
			Mount: f.Mount,
			Size:  fmtBytes(f.SizeBytes),
			Avail: fmtBytes(f.AvailBytes),
			Meter: meter(f.UsedPct),
		})
	}
	return v
}

// metricCard builds one card from a series.
func metricCard(name string, s prom.Series, fmtVal func(float64) string, severityColor bool) liveCard {
	c := liveCard{Name: name, ColorClass: "c-acc"}
	if !s.Found {
		c.NoData = true
		return c
	}
	c.Current = fmtVal(s.Current)
	c.AvgMax = "avg " + fmtVal(s.Avg) + " · max " + fmtVal(s.Max)
	c.Points = sparklinePoints(s.Points)
	if severityColor {
		switch {
		case s.Current > 85:
			c.ColorClass = "c-err"
		case s.Current >= 60:
			c.ColorClass = "c-warn"
		default:
			c.ColorClass = "c-ok"
		}
	}
	return c
}

// sparklinePoints renders "x1,y1 x2,y2 ..." for a 200x36 viewBox.
func sparklinePoints(points []prom.Point) string {
	if len(points) == 0 {
		return ""
	}
	if len(points) == 1 {
		return "0,18 200,18"
	}
	minV, maxV := points[0].V, points[0].V
	for _, p := range points {
		if p.V < minV {
			minV = p.V
		}
		if p.V > maxV {
			maxV = p.V
		}
	}
	span := maxV - minV
	var b strings.Builder
	for i, p := range points {
		if i > 0 {
			b.WriteByte(' ')
		}
		x := 200 * float64(i) / float64(len(points)-1)
		y := 18.0
		if span > 0 {
			y = 33 - (p.V-minV)/span*29 // 4px top pad, 3px bottom pad
		}
		fmt.Fprintf(&b, "%.1f,%.1f", x, y)
	}
	return b.String()
}

// fmtPct renders a percentage with sensible precision.
func fmtPct(v float64) string {
	if v >= 10 {
		return fmt.Sprintf("%.0f%%", v)
	}
	return fmt.Sprintf("%.1f%%", v)
}

// fmtBytesPerSec renders bytes/sec as MB/s (or KB/s below 1).
func fmtBytesPerSec(bps float64) string {
	mbs := bps / 1e6
	if mbs >= 1000 {
		return fmt.Sprintf("%.1f GB/s", mbs/1000)
	}
	if mbs >= 10 {
		return fmt.Sprintf("%.0f MB/s", mbs)
	}
	if mbs >= 1 {
		return fmt.Sprintf("%.1f MB/s", mbs)
	}
	return fmt.Sprintf("%.0f KB/s", bps/1e3)
}

// fmtBytes renders a byte count as GB (or MB below 1).
func fmtBytes(b float64) string {
	gb := b / 1e9
	if gb >= 100 {
		return fmt.Sprintf("%.0f GB", gb)
	}
	if gb >= 1 {
		return fmt.Sprintf("%.1f GB", gb)
	}
	return fmt.Sprintf("%.0f MB", b/1e6)
}
