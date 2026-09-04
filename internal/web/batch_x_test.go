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
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Batch X1 — DNS records linked to shared or reseller hosting show on that
// hosting's detail page (the card used to exist for servers/domains only).
func TestDNSCardOnHostingDetail(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	postForm(t, client, ts, "/shared", url.Values{"main_domain": {"shared.example.com"}, "active": {"on"}}).Body.Close()
	postForm(t, client, ts, "/reseller", url.Values{"main_domain": {"reseller.example.com"}, "active": {"on"}}).Body.Close()
	postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"www.shared.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.1"}, "shared_id": {"1"},
	}).Body.Close()
	postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"mail.reseller.example.com"}, "dns_type": {"MX"}, "address": {"203.0.113.2"}, "reseller_id": {"1"},
	}).Body.Close()

	for path, want := range map[string]string{
		"/shared/1":   "www.shared.example.com",
		"/reseller/1": "mail.reseller.example.com",
	} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if !strings.Contains(body, want) {
			t.Errorf("%s should list its DNS record %q", path, want)
		}
	}
	// Misc services cannot carry DNS records: no card at all.
	postForm(t, client, ts, "/misc", url.Values{"name": {"thing"}, "active": {"on"}}).Body.Close()
	resp, _ := client.Get(ts.URL + "/misc/1")
	body := readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "No DNS records linked") {
		t.Fatal("misc detail must not render a DNS card")
	}
}

// Batch X2 — a zoned IPv6 address is refused on create: the zone is scope,
// not address, and the '%' would break every later whois request URL.
func TestIPCreateRejectsZone(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "zone-host")
	resp := postForm(t, client, ts, "/ips", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "address": {"fe80::1%eth0"},
	})
	resp.Body.Close()
	var n int
	database.QueryRow("SELECT COUNT(*) FROM ips").Scan(&n)
	if n != 0 {
		t.Fatalf("zoned address stored: %d rows", n)
	}
	page, _ := client.Get(ts.URL + "/ips")
	body := readBody(t, page)
	page.Body.Close()
	if !strings.Contains(body, "Invalid IP address") {
		t.Fatal("expected the invalid-address flash")
	}
}

// Batch X3 — the Prometheus "Test" button tests the SAVED url even while
// the feature is switched off; that is when you want to validate it.
func TestPrometheusTestUsesSavedURLWhenDisabled(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	promSrv := httptest.NewServer(http.HandlerFunc(fakePromHandler))
	defer promSrv.Close()

	postForm(t, client, ts, "/settings", url.Values{
		"default_currency": {"USD"}, "dashboard_currency": {"USD"},
		"due_soon_amount": {"14"}, "recently_added_amount": {"5"}, "theme": {"dark"},
		"prometheus_url": {promSrv.URL}, // enable box left unchecked
	}).Body.Close()
	postForm(t, client, ts, "/settings/prometheus/test", url.Values{}).Body.Close()
	page, _ := client.Get(ts.URL + "/settings")
	body := readBody(t, page)
	page.Body.Close()
	if strings.Contains(body, "No Prometheus URL configured") {
		t.Fatal("test button must use the saved URL even when prometheus is disabled")
	}
	if !strings.Contains(body, "Connected") {
		t.Fatalf("expected a connection result flash, got:\n%s", body)
	}
}

// Batch X4 — the ping exec has an overall deadline: a binary that hangs
// (stalled name resolution) returns promptly instead of holding the
// handler for the resolver's whole retry budget.
func TestPingExecTimeout(t *testing.T) {
	dir := t.TempDir()
	stub := filepath.Join(dir, "ping")
	// The stub keeps a CHILD holding stdout open after the shell is killed —
	// the shape that pins CombinedOutput without WaitDelay.
	if err := os.WriteFile(stub, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	origBin, origTimeout := pingBinary, pingTimeout
	pingBinary, pingTimeout = stub, 200*time.Millisecond
	defer func() { pingBinary, pingTimeout = origBin, origTimeout }()

	start := time.Now()
	_, err := execPing("127.0.0.1")
	if err == nil {
		t.Fatal("hung ping should error")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("ping deadline not enforced: took %v", elapsed)
	}
}
