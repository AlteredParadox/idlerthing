package prom

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// vectorJSON builds a canned /api/v1/query response.
func vectorJSON(samples ...string) string {
	return `{"status":"success","data":{"resultType":"vector","result":[` +
		strings.Join(samples, ",") + `]}}`
}

func sample(metric, value string) string {
	return fmt.Sprintf(`{"metric":{%s},"value":[1719300000,%q]}`, metric, value)
}

func fakeProm(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	return ts
}

func TestQueryParsesSamples(t *testing.T) {
	ts := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		fmt.Fprint(w, vectorJSON(
			sample(`"instance":"a:9100","nodename":"host-a"`, "0.75"),
			sample(`"instance":"b:9100","nodename":"host-b"`, "1"),
		))
	})
	c := New(ts.URL)
	samples, err := c.Query(context.Background(), "up")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
	}
	if samples[0].Metric["nodename"] != "host-a" || samples[0].Value != 0.75 {
		t.Fatalf("unexpected sample: %+v", samples[0])
	}
	if samples[1].Value != 1 {
		t.Fatalf("unexpected value: %+v", samples[1])
	}
}

func TestQueryErrorStatus(t *testing.T) {
	ts := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"status":"error","error":"parse error: bad PromQL"}`)
	})
	c := New(ts.URL)
	if _, err := c.Query(context.Background(), "nonsense{"); err == nil {
		t.Fatal("expected error on status=error")
	}
}

func TestQueryNon200(t *testing.T) {
	ts := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	c := New(ts.URL)
	if _, err := c.Query(context.Background(), "up"); err == nil {
		t.Fatal("expected error on 502")
	}
}

func TestQueryTimeout(t *testing.T) {
	ts := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	})
	c := &Client{BaseURL: ts.URL, HTTPClient: &http.Client{Timeout: 50 * time.Millisecond}}
	if _, err := c.Query(context.Background(), "up"); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestServerMetricsMapping(t *testing.T) {
	ts := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		switch {
		case q == serverQueries["up"]:
			fmt.Fprint(w, vectorJSON(
				sample(`"instance":"a:9100"`, "1"),
				sample(`"instance":"b:9100"`, "0"),
			))
		case q == serverQueries["uname"]:
			fmt.Fprint(w, vectorJSON(
				sample(`"instance":"a:9100","nodename":"host-a"`, "1"),
				sample(`"instance":"b:9100","nodename":"host-b"`, "1"),
			))
		case q == serverQueries["cpu"]:
			fmt.Fprint(w, vectorJSON(sample(`"instance":"a:9100"`, "12.5")))
		case q == serverQueries["ram"]:
			fmt.Fprint(w, vectorJSON(sample(`"instance":"a:9100"`, "31")))
		case q == serverQueries["disk"]:
			fmt.Fprint(w, vectorJSON(sample(`"instance":"a:9100"`, "88")))
		case q == serverQueries["net_rx"]:
			fmt.Fprint(w, vectorJSON(sample(`"instance":"a:9100"`, "1500000")))
		case q == serverQueries["net_tx"]:
			fmt.Fprint(w, vectorJSON(sample(`"instance":"a:9100"`, "400000")))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	})
	m := New(ts.URL).ServerMetrics(context.Background())

	a := m.ByNodename["host-a"]
	if a == nil || !a.Found || !a.Online {
		t.Fatalf("host-a: %+v", a)
	}
	if a.CPUPct != 12.5 || a.RAMPct != 31 || a.DiskPct != 88 {
		t.Fatalf("host-a pcts: %+v", a)
	}
	if a.NetRxBps != 1500000 || a.NetTxBps != 400000 {
		t.Fatalf("host-a net: %+v", a)
	}

	b := m.ByNodename["host-b"]
	if b == nil || !b.Found || b.Online {
		t.Fatalf("host-b should be found but down: %+v", b)
	}
	// b got no cpu/ram samples — stays zero, no error.
	if b.CPUPct != 0 {
		t.Fatalf("host-b should tolerate missing sections: %+v", b)
	}
}

func TestServerMetricsPartialFailure(t *testing.T) {
	// Everything except `up` fails — we still get online status.
	ts := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("query") == serverQueries["up"] {
			fmt.Fprint(w, vectorJSON(sample(`"instance":"a:9100"`, "1")))
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	})
	m := New(ts.URL).ServerMetrics(context.Background())
	h := m.ByInstance["a:9100"]
	if h == nil || !h.Found || !h.Online {
		t.Fatalf("expected partial results: %+v", h)
	}
}

func TestUptimePct(t *testing.T) {
	ts := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, vectorJSON(sample(`"instance":"a:9100"`, "99.95")))
	})
	v, err := New(ts.URL).UptimePct(context.Background(), "a:9100")
	if err != nil || v != 99.95 {
		t.Fatalf("uptime: %v %v", v, err)
	}
}

// Batch I #7 — ServerMetrics runs its 8 queries with bounded parallelism
// (≤3 in flight) and still finishes well under the sequential cost.
func TestServerMetricsConcurrencyBound(t *testing.T) {
	var inflight, maxInflight int32
	ts := fakeProm(t, func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&inflight, 1)
		for {
			m := atomic.LoadInt32(&maxInflight)
			if n <= m || atomic.CompareAndSwapInt32(&maxInflight, m, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		atomic.AddInt32(&inflight, -1)
		fmt.Fprint(w, vectorJSON(sample(`"instance":"a:9100","nodename":"host-a"`, "1")))
	})

	start := time.Now()
	m := New(ts.URL).ServerMetrics(context.Background())
	elapsed := time.Since(start)

	if got := atomic.LoadInt32(&maxInflight); got > 3 {
		t.Fatalf("parallelism bound violated: %d in flight", got)
	}
	// 8 queries at 50ms: sequential = 400ms; 3-wide ≈ 150ms (+ slack).
	if elapsed > 300*time.Millisecond {
		t.Fatalf("batch looks sequential: %v", elapsed)
	}
	if !m.Healthy {
		t.Fatal("expected healthy batch")
	}
}
