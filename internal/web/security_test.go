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
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// #1 — Secure cookies + X-Forwarded-For behind a TLS proxy.
func TestBehindTLSProxySecureCookieAndXFF(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	srv.SetBehindTLSProxy(true)
	client := newProxyClient(t)

	// Login over plain HTTP (httptest has no TLS) — cookie must still be Secure.
	resp := login(t, client, ts, testPassword)
	defer resp.Body.Close()
	var session *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			session = c
		}
	}
	if session == nil || !session.Secure {
		t.Fatal("session cookie must be Secure behind TLS proxy")
	}

	// clientIP trusts the LAST X-Forwarded-For entry when the flag is on.
	req, _ := http.NewRequest("GET", "/", nil)
	req.RemoteAddr = "10.0.0.1:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	if got := srv.clientIP(req); got != "5.6.7.8" {
		t.Fatalf("XFF: got %q", got)
	}

	// Flag off → RemoteAddr, cookie not Secure.
	ts2, _, _ := newTestServerFull(t)
	client2 := newClient(t)
	resp2 := login(t, client2, ts2, testPassword)
	defer resp2.Body.Close()
	for _, c := range resp2.Cookies() {
		if c.Name == sessionCookieName && c.Secure {
			t.Fatal("cookie must not be Secure without the flag")
		}
	}
	_, _, srvPlain := newTestServerFull(t)
	if got := srvPlain.clientIP(&http.Request{RemoteAddr: "10.0.0.1:1234"}); got != "10.0.0.1" {
		t.Fatalf("no flag: got %q", got)
	}
}

// #2 — YABS window shortened to 2h.
func TestYABSSigWindowTwoHours(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "yabs-host")

	// 3h-old signature: valid under the old 12h window, must now be rejected.
	old := time.Now().Add(-3 * time.Hour).Unix()
	resp := postYABS(t, ts, srv, 1, &old, signYABSTest(srv.secret, 1, old))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("3h-old sig: expected 403, got %d", resp.StatusCode)
	}

	// 1h-old is still fine.
	recent := time.Now().Add(-1 * time.Hour).Unix()
	resp = postYABS(t, ts, srv, 1, &recent, signYABSTest(srv.secret, 1, recent))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("1h-old sig: expected 200, got %d", resp.StatusCode)
	}
}

// #3 — CSV formula injection guard.
func TestExportCSVFormulaInjection(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "=HYPERLINK(\"http://evil\",\"x\")")

	req, _ := http.NewRequest("GET", ts.URL+"/export/csv", nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	zr, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
	if err != nil {
		t.Fatalf("invalid zip: %v", err)
	}
	for _, f := range zr.File {
		if f.Name != "servers.csv" {
			continue
		}
		rc, _ := f.Open()
		content, _ := io.ReadAll(rc)
		rc.Close()
		if !strings.Contains(string(content), `'=HYPERLINK`) {
			t.Fatal("formula-leading hostname must be quote-prefixed")
		}
		return
	}
	t.Fatal("servers.csv missing")
}

// #4 — password change revokes the API token.
func TestPasswordChangeRevokesAPIToken(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	client := authedClient(t, ts)

	resp, _ := apiGet(t, ts, "/api/servers", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatal("token should work before password change")
	}

	resp = postForm(t, client, ts, "/settings/account", url.Values{
		"action": {"password"}, "current_password": {testPassword},
		"new_password": {"newpassword1"}, "confirm_password": {"newpassword1"},
	})
	resp.Body.Close()

	resp, _ = apiGet(t, ts, "/api/servers", token)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("old token should 401 after password change, got %d", resp.StatusCode)
	}
	var hash *string
	database.QueryRow("SELECT api_token_hash FROM users").Scan(&hash)
	if hash != nil {
		t.Fatal("api_token_hash should be cleared")
	}
}

// #5 — per-email limiter works across IPs; stale keys are pruned.
func TestLoginEmailLimiter(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	// Disable the per-IP limiter to isolate the per-email one.
	srv.limit = newRateLimiter(1000, time.Minute)
	client := newClient(t)

	for i := 0; i < 10; i++ {
		resp := login(t, client, ts, "wrongpass")
		resp.Body.Close()
	}
	resp := login(t, client, ts, testPassword) // 11th: even correct pw is limited
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Too many attempts") {
		t.Fatal("per-email limiter should trip on the 11th attempt")
	}
}

func TestRateLimiterPrunesStaleKeys(t *testing.T) {
	rl := newRateLimiter(5, 20*time.Millisecond)
	// Sweep only kicks in past the threshold.
	for i := 0; i < sweepThreshold+50; i++ {
		rl.allow(fmt.Sprintf("key%d", i))
	}
	time.Sleep(30 * time.Millisecond)
	rl.allow("fresh")
	if len(rl.hits) != 1 {
		t.Fatalf("stale keys should be pruned past threshold, got %d keys", len(rl.hits))
	}
}

func TestRateLimiterKeyCap(t *testing.T) {
	rl := newRateLimiter(5, time.Minute)
	for i := 0; i < maxLimiterKeys; i++ {
		rl.allow(fmt.Sprintf("k%d", i))
	}
	// New key beyond the cap fails closed; existing keys still pass.
	if rl.allow("new-key") {
		t.Fatal("new key beyond cap must be denied")
	}
	if !rl.allow("k0") {
		t.Fatal("existing key must keep working past the cap")
	}
}

// #6 — redirectBack rejects backslash/protocol-relative targets.
func TestRedirectBackHardening(t *testing.T) {
	ts, _ := newTestServer(t)
	client := seedOneServer(t, ts)

	resp := postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"1"},
		"body": {"x"}, "back": {"/\\evil.com"},
	})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if loc != "/notes" {
		t.Fatalf("backslash target should fall back, got %q", loc)
	}

	resp = postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"1"},
		"body": {"x"}, "back": {"/servers/1"},
	})
	loc = resp.Header.Get("Location")
	resp.Body.Close()
	if loc != "/servers/1" {
		t.Fatalf("legit same-origin path should pass, got %q", loc)
	}
}

// #8 — API write body cap.
func TestAPIServerBodyCap(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)

	huge := `{"hostname":"` + strings.Repeat("x", 2<<20) + `"}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/servers", strings.NewReader(huge))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", resp.StatusCode)
	}
}

// #9 — unknown export type is 404.
func TestExportUnknownType404(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/export/json/evil", nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
}

// #10 — IDLER_BASE_URL drives the displayed ingest command.
func TestYABSCommandBaseURL(t *testing.T) {
	_, _, srv := newTestServerFull(t)
	srv.SetBaseURL("https://idlers.example.com/")
	req, _ := http.NewRequest("GET", "http://internal:8080/servers/1", nil)
	cmd := srv.yabsCommand(req, 1)
	if !strings.Contains(cmd, "-s 'https://idlers.example.com/api/yabs/1?sig=") {
		t.Fatalf("base url not applied: %s", cmd)
	}

	// Without it, scheme+Host is used (loopback http is always allowed).
	_, _, srv2 := newTestServerFull(t)
	reqLAN, _ := http.NewRequest("GET", "http://127.0.0.1:8080/servers/1", nil)
	cmd = srv2.yabsCommand(reqLAN, 1)
	if !strings.Contains(cmd, "-s 'http://127.0.0.1:8080/api/yabs/1?sig=") {
		t.Fatalf("fallback host not applied: %s", cmd)
	}
}

// Batch P #5 — http ingest is loopback-only unless IDLER_ALLOW_HTTP_INGEST
// opts in; https always wins.
func TestYABSCommandHTTPSRule(t *testing.T) {
	_, _, srv := newTestServerFull(t)
	cmd := func(rawurl string) string {
		req, _ := http.NewRequest("GET", rawurl, nil)
		return srv.yabsCommand(req, 1)
	}
	if c := cmd("http://127.0.0.1:8080/servers/1"); c == "" {
		t.Fatal("http + loopback should emit the command")
	}
	if c := cmd("http://localhost:8080/servers/1"); c == "" {
		t.Fatal("http + localhost should emit the command")
	}
	if c := cmd("http://[::1]:8080/servers/1"); c == "" {
		t.Fatal("http + ::1 should emit the command")
	}
	// LAN/public http: withheld without the opt-in flag.
	for _, u := range []string{
		"http://192.168.1.5:8080/servers/1", "http://10.0.0.2/servers/1",
		"http://[fd00::1]/servers/1", "http://idlers.example.com/servers/1",
		"http://203.0.113.10/servers/1",
	} {
		if c := cmd(u); c != "" {
			t.Fatalf("%s must withhold the command without the flag, got %q", u, c)
		}
	}

	// Opt-in: LAN http emits; public still withheld.
	srv.SetAllowHTTPIngest(true)
	if c := cmd("http://192.168.1.5:8080/servers/1"); c == "" {
		t.Fatal("http + RFC1918 with the flag should emit the command")
	}
	if c := cmd("http://10.0.0.2/servers/1"); c == "" {
		t.Fatal("http + 10/8 with the flag should emit the command")
	}
	if c := cmd("http://[fd00::1]/servers/1"); c == "" {
		t.Fatal("http + ULA with the flag should emit the command")
	}
	if c := cmd("http://203.0.113.10/servers/1"); c != "" {
		t.Fatalf("http + public IP still withheld with the flag, got %q", c)
	}

	// IDLER_BASE_URL: https wins, http public withholds.
	srv.SetBaseURL("https://idlers.example.com")
	if c := cmd("http://internal:8080/servers/1"); c == "" {
		t.Fatal("https base URL should emit the command")
	}
	srv.SetBaseURL("http://idlers.example.com")
	if c := cmd("http://127.0.0.1/servers/1"); c != "" {
		t.Fatalf("http public IDLER_BASE_URL must withhold the command, got %q", c)
	}

	// Page level: the card shows the hint instead of a command. The Host
	// override keeps the jar from sending the session cookie, so attach it.
	ts, _, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "hint-host")
	req, _ := http.NewRequest("GET", ts.URL+"/servers/1", nil)
	req.Host = "idlers.example.com"
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "IDLER_BASE_URL=https://") {
		t.Fatal("YABS card should show the https hint")
	}
	if strings.Contains(body, "copy-btn") {
		t.Fatal("no copyable command may render over http on a public host")
	}
}

// #1 — limiter windows actually expire: past the window, attempts reset.
func TestRateLimiterWindowExpires(t *testing.T) {
	rl := newRateLimiter(2, 50*time.Millisecond)
	if !rl.allow("k") {
		t.Fatal("first attempt should pass")
	}
	if !rl.allow("k") {
		t.Fatal("second attempt should pass")
	}
	if rl.allow("k") {
		t.Fatal("third within window should be denied")
	}
	time.Sleep(60 * time.Millisecond)
	if !rl.allow("k") {
		t.Fatal("attempt past the window must be allowed again")
	}
}

// #2 — login pre-auth hardening.
func TestLoginHardening(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	csrf := ""
	getCSRF := func(c *http.Client) string {
		if csrf == "" {
			csrf = getLoginCSRF(t, c, ts)
		}
		return csrf
	}
	_ = getCSRF
	client := newClient(t)
	token := getLoginCSRF(t, client, ts)

	// Oversized body → 4xx (MaxBytesReader parse error), no limiter hit.
	big := "csrf_token=" + token + "&email=" + strings.Repeat("a", 40<<10) + "&password=x"
	req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusSeeOther {
		t.Fatalf("oversized body should be rejected, got %d", resp.StatusCode)
	}

	// Multipart → rejected outright.
	req, _ = http.NewRequest("POST", ts.URL+"/login", strings.NewReader("--x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("multipart: expected 415, got %d", resp.StatusCode)
	}

	// Limiter keys are hashed digests, never raw input.
	if got := limiterKey(strings.Repeat("a", 1<<20)); len(got) != 32 {
		t.Fatalf("limiter key should be a 32-char hex digest, got %d chars", len(got))
	}
	if limiterKey("x") == "x" {
		t.Fatal("limiter key must not be raw input")
	}
	srv.limit.mu.Lock()
	for k := range srv.limit.hits {
		if strings.Contains(k, "aaa") {
			t.Fatal("raw input must not appear as a limiter key")
		}
	}
	srv.limit.mu.Unlock()
}

// #3 — displayed yabs command is quoted and https-hardened.
func TestYABSCommandQuoting(t *testing.T) {
	_, _, srv := newTestServerFull(t)
	srv.SetBaseURL("https://idlers.example.com")
	req, _ := http.NewRequest("GET", "http://internal:8080/servers/1", nil)
	cmd := srv.yabsCommand(req, 1)
	if !strings.Contains(cmd, "curl -fsSL --proto '=https' https://yabs.sh") {
		t.Fatalf("curl prefix: %s", cmd)
	}
	if !strings.Contains(cmd, "-s 'https://idlers.example.com/api/yabs/1?sig=") {
		t.Fatalf("URL should be single-quoted: %s", cmd)
	}
	if !strings.HasSuffix(cmd, "'") {
		t.Fatalf("quote should wrap the whole URL incl &ts=: %s", cmd)
	}

	// behindTLSProxy without base URL → https + host.
	_, _, srv2 := newTestServerFull(t)
	srv2.SetBehindTLSProxy(true)
	cmd = srv2.yabsCommand(req, 1)
	if !strings.Contains(cmd, "-s 'https://internal:8080/api/yabs/1?sig=") {
		t.Fatalf("proxy scheme: %s", cmd)
	}

	// base URL with a quote char is rejected → falls back to host.
	srv.SetBaseURL("https://evil.com/'x")
	if srv.baseURL != "" {
		t.Fatal("quote-containing base URL must be rejected")
	}
}
