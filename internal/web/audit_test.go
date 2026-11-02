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
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Regression tests for the November 2026 audit fixes. Each one reproduces
// the audited behaviour against the real handlers.

// sourceKey groups IPv6 by /64 and normalises mapped and zoned forms, so a
// host holding a whole /64 cannot mint thousands of limiter keys.
func TestSourceKeyGroupsIPv6By64(t *testing.T) {
	if sourceKey("2001:db8:1::1") != sourceKey("2001:db8:1::ffff:1234") {
		t.Fatal("two addresses in one /64 must share a limiter key")
	}
	if sourceKey("2001:db8:1::1") == sourceKey("2001:db8:2::1") {
		t.Fatal("different /64s must not share a key")
	}
	if sourceKey("::ffff:203.0.113.9") != sourceKey("203.0.113.9") {
		t.Fatal("an IPv4-mapped address must key like its IPv4 form")
	}
	if sourceKey("fe80::1%eth0") != sourceKey("fe80::1") {
		t.Fatal("a zone must not create a distinct key")
	}
	if sourceKey("203.0.113.9") == sourceKey("203.0.113.10") {
		t.Fatal("distinct IPv4 addresses must keep distinct keys")
	}
	if sourceKey("not-an-ip") != limiterKey("not-an-ip") {
		t.Fatal("non-addresses are hashed as-is")
	}
}

// clientIP behind a proxy reads the last element of the LAST header line,
// so a client-supplied first X-Forwarded-For line (HAProxy-style separate
// lines) cannot choose its own limiter and fail2ban identity. Values that
// are not addresses fall through to RemoteAddr.
func TestClientIPUsesLastForwardedLine(t *testing.T) {
	_, _, srv := newTestServerFull(t)
	srv.SetBehindTLSProxy(true)

	req := &http.Request{RemoteAddr: "10.0.0.1:1234", Header: http.Header{}}
	req.Header.Add("X-Forwarded-For", "203.0.113.66")
	req.Header.Add("X-Forwarded-For", "198.51.100.7")
	if got := srv.clientIP(req); got != "198.51.100.7" {
		t.Fatalf("separate header lines: got %q, want the proxy's line", got)
	}

	req.Header = http.Header{}
	req.Header.Add("X-Forwarded-For", "203.0.113.66, evil.example")
	if got := srv.clientIP(req); got != "10.0.0.1" {
		t.Fatalf("non-address XFF element must fall back to RemoteAddr, got %q", got)
	}

	req.Header = http.Header{}
	req.Header.Add("X-Forwarded-For", "::ffff:198.51.100.7")
	if got := srv.clientIP(req); got != "198.51.100.7" {
		t.Fatalf("mapped address must be unmapped, got %q", got)
	}
}

// saturateLimiter fills rl to its key cap with synthetic keys.
func saturateLimiter(rl *rateLimiter) {
	for i := 0; i < maxLimiterKeys; i++ {
		rl.allow(fmt.Sprintf("flood-%d", i))
	}
}

// A flood of distinct sources saturates the per-source limiter's key map.
// That must refuse unfamiliar sources without naming them in the log, and
// must NOT refuse a source that has signed in before.
func TestLoginLimiterSaturationSparesKnownSource(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	srv.SetBehindTLSProxy(true)
	const owner = "2001:db8:a::1"

	resp := loginFrom(t, newProxyClient(t), ts, owner, testPassword)
	resp.Body.Close()
	if !hasSessionCookie(resp) {
		t.Fatal("owner login should succeed on a fresh server")
	}

	// The flood arrives after the owner's own key has aged out of the window.
	srv.limit.mu.Lock()
	delete(srv.limit.hits, sourceKey(owner))
	srv.limit.mu.Unlock()
	saturateLimiter(srv.limit)
	buf := captureLog(t)

	// Unfamiliar source, correct password: refused for capacity.
	resp = loginFrom(t, newProxyClient(t), ts, "2001:db8:b::77", testPassword)
	body := readBody(t, resp)
	resp.Body.Close()
	if hasSessionCookie(resp) || !strings.Contains(body, "Too many attempts") {
		t.Fatal("a never-seen source must be refused while the map is saturated")
	}

	// Known source, correct password: admitted through the known-source budget.
	resp = loginFrom(t, newProxyClient(t), ts, owner, testPassword)
	resp.Body.Close()
	if !hasSessionCookie(resp) {
		t.Fatal("a known source must still be able to sign in while the map is saturated")
	}
	// ...and a wrong password from it is still a failure, under a budget.
	for i := 0; i < 10; i++ {
		r := loginFrom(t, newProxyClient(t), ts, owner, "wrong-password")
		r.Body.Close()
	}
	resp = loginFrom(t, newProxyClient(t), ts, owner, testPassword)
	body = readBody(t, resp)
	resp.Body.Close()
	if hasSessionCookie(resp) || !strings.Contains(body, "Too many attempts") {
		t.Fatal("the known-source budget must still throttle guessing")
	}

	// Capacity refusals are logged once, without a from= the jail could ban.
	logs := buf.String()
	if !strings.Contains(logs, "login: limiter saturated") {
		t.Fatalf("expected a saturation line, got:\n%s", logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		if reRateLimit.MatchString(line) && strings.Contains(line, "2001:db8:b::77") {
			t.Fatalf("a capacity refusal must not name the refused source:\n%s", line)
		}
	}
}

// All addresses in one /64 occupy one limiter key, so a single IPv6 host
// cannot saturate the map by rotating its interface identifier.
func TestLoginLimiterOneKeyPerIPv6Prefix(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	srv.SetBehindTLSProxy(true)
	for i := 0; i < 20; i++ {
		r := loginFrom(t, newProxyClient(t), ts, fmt.Sprintf("2001:db8:c::%x", i+1), "wrong-password")
		r.Body.Close()
	}
	srv.limit.mu.Lock()
	n := len(srv.limit.hits)
	srv.limit.mu.Unlock()
	if n != 1 {
		t.Fatalf("20 addresses in one /64 should hold 1 limiter key, got %d", n)
	}
}

// An over-length password can never match, so it is refused before it
// costs a limiter hit or mints a key.
func TestLoginOverlengthDoesNotMintLimiterKey(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	srv.SetBehindTLSProxy(true)
	const src = "198.51.100.42"
	r := loginFrom(t, newProxyClient(t), ts, src, strings.Repeat("x", 300))
	r.Body.Close()
	srv.limit.mu.Lock()
	_, present := srv.limit.hits[sourceKey(src)]
	srv.limit.mu.Unlock()
	if present {
		t.Fatal("an over-length attempt must not create a limiter key")
	}
}

// Logout must not report success when the session row cannot be revoked:
// a copied cookie would otherwise stay valid behind a reassuring redirect.
func TestLogoutFailsClosedWhenRevocationFails(t *testing.T) {
	ts, database := newTestServer(t)
	client := newClient(t)
	login(t, client, ts, testPassword).Body.Close()
	csrf := sessionCSRF(t, client, ts)

	if _, err := database.Exec(`CREATE TRIGGER block_logout BEFORE DELETE ON sessions
		BEGIN SELECT RAISE(ABORT, 'forced failure'); END`); err != nil {
		t.Fatal(err)
	}
	resp, err := client.PostForm(ts.URL+"/logout", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("logout with a failed DELETE must not claim success, got %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName {
			t.Fatal("the cookie must not be cleared while the row survives")
		}
	}
	// The row (and so the session) is still there; the server says so.
	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("session should still be live after the failed logout, got %d", resp.StatusCode)
	}

	if _, err := database.Exec("DROP TRIGGER block_logout"); err != nil {
		t.Fatal(err)
	}
	resp, err = client.PostForm(ts.URL+"/logout", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout should succeed once the DELETE works, got %d", resp.StatusCode)
	}
}

// A percent-encoded control character in the Referer path decodes to a
// path browsers rewrite into a cross-origin target. Every prefs endpoint
// must fall back instead.
func TestSafeRedirectTargetRejectsControlChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://app/%09/evil.example", "/fb"},
		{"https://app/%0a/evil.example", "/fb"},
		{"https://app/%0d/evil.example", "/fb"},
		{"https://app/%7f/evil.example", "/fb"},
		{"https://app/x/%5Cevil.example", "/fb"},
		{"https://app/servers?status=all", "/servers?status=all"},
		{"https://app/servers/caf%C3%A9", "/servers/caf%C3%A9"}, // re-escaped, not decoded
		{"https://app/servers%2F1", "/servers%2F1"},             // encoded slash stays encoded
	}
	for _, c := range cases {
		if got := safeRedirectTarget(c.in, "/fb"); got != c.want {
			t.Errorf("safeRedirectTarget(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "redir-01")
	csrf := sessionCSRF(t, client, ts)
	for _, path := range []string{"/prefs/theme", "/prefs/short-hostnames", "/prefs/servers-cols"} {
		form := url.Values{"csrf_token": {csrf}}
		req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Referer", ts.URL+"/%09/evil.example")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		loc := resp.Header.Get("Location")
		if resp.StatusCode != http.StatusSeeOther || strings.Contains(loc, "evil") || strings.ContainsAny(loc, "\t\n\r") {
			t.Fatalf("%s: status %d Location %q", path, resp.StatusCode, loc)
		}
	}
}

// "///host" has no Host for url.Parse but is scheme-relative to a browser.
func TestRedirectBackRejectsSchemeRelativeTripleSlash(t *testing.T) {
	ts, _ := newTestServer(t)
	client := seedOneServer(t, ts)
	resp := postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"1"},
		"body": {"x"}, "back": {"///evil.example"},
	})
	loc := resp.Header.Get("Location")
	resp.Body.Close()
	if loc != "/notes" {
		t.Fatalf("scheme-relative target should fall back, got %q", loc)
	}
}

// ?sort= is reachable by a cross-site GET and persisted, so a value that is
// not key-shaped is dropped before it is stored or rendered.
func TestListSortRejectsMalformedKey(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "sort-02")

	for _, bad := range []string{"price&q=pinned", "price\"><b>", strings.Repeat("a", 40), "Price"} {
		resp, err := client.Get(ts.URL + "/servers?sort=" + url.QueryEscape(bad) + "&dir=desc")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("sort=%q: status %d", bad, resp.StatusCode)
		}
	}
	var n int
	if err := database.QueryRow("SELECT COUNT(*) FROM user_prefs WHERE key = 'sort_servers'").Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a malformed sort key must not be persisted")
	}

	// A well-formed one still is.
	resp, err := client.Get(ts.URL + "/servers?sort=price&dir=desc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var v string
	if err := database.QueryRow("SELECT value FROM user_prefs WHERE key = 'sort_servers'").Scan(&v); err != nil || v != "price,desc" {
		t.Fatalf("expected 'price,desc' persisted, got %q (%v)", v, err)
	}
}

// Minting an API token is a credential change: a session alone is not
// enough, the current password must be presented.
func TestAPITokenRequiresCurrentPassword(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	tokenHash := func() *string {
		var h *string
		if err := database.QueryRow("SELECT api_token_hash FROM users WHERE email = 'admin@localhost'").Scan(&h); err != nil {
			t.Fatal(err)
		}
		return h
	}
	for _, pw := range []string{"", "not-the-password"} {
		resp := postForm(t, client, ts, "/settings/account", url.Values{
			"action": {"token"}, "current_password": {pw},
		})
		resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("password %q: status %d", pw, resp.StatusCode)
		}
		if tokenHash() != nil {
			t.Fatalf("password %q: a token was minted without the current password", pw)
		}
	}
	page, err := client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, page)
	page.Body.Close()
	if !strings.Contains(body, "Current password is wrong.") {
		t.Fatalf("expected the wrong-password flash, got:\n%s", body)
	}

	resp := postForm(t, client, ts, "/settings/account", url.Values{
		"action": {"token"}, "current_password": {testPassword},
	})
	resp.Body.Close()
	if h := tokenHash(); h == nil || len(*h) != 64 {
		t.Fatal("the correct password must mint a token")
	}
}
