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
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// loginFrom performs the login flow with X-Forwarded-For set to ip (the
// server must be in behind-TLS-proxy mode for it to be trusted).
func loginFrom(t *testing.T, client *http.Client, ts *httptest.Server, ip, password string) *http.Response {
	t.Helper()
	csrf := getLoginCSRF(t, client, ts)
	form := url.Values{"csrf_token": {csrf}, "email": {"admin@localhost"}, "password": {password}}
	req, _ := http.NewRequest("POST", ts.URL+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Forwarded-For", ip)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /login from %s: %v", ip, err)
	}
	return resp
}

// Batch T1 — the password-change verify shares /login's per-source budget:
// a stolen session must not buy unthrottled guessing of the real password.
func TestPasswordChangeRateLimited(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	attempt := func(current string) *http.Response {
		return postForm(t, client, ts, "/settings/account", url.Values{
			"action": {"password"}, "current_password": {current},
			"new_password": {"brand-new-pass"}, "confirm_password": {"brand-new-pass"},
		})
	}
	// The login itself spent one of the 10/min; burn the rest with bad guesses.
	for i := 0; i < 9; i++ {
		attempt("wrong-guess").Body.Close()
	}
	// Even the CORRECT password is refused while the source is limited.
	resp := attempt(testPassword)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("limited attempt: status %d", resp.StatusCode)
	}
	page, err := client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, page)
	page.Body.Close()
	if !strings.Contains(body, "Too many attempts") {
		t.Fatalf("expected rate-limit flash, got:\n%s", body)
	}
	// And the password did not change (a login check would itself be
	// throttled here — same source, same budget — so read the hash).
	var hash string
	if err := database.QueryRow("SELECT password_hash FROM users WHERE email = 'admin@localhost'").Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(testPassword)) != nil {
		t.Fatal("the limited change must not have applied — old password no longer matches")
	}
}

// Batch T2 — a tripped per-account limiter no longer locks the owner out
// from an address that has authenticated before, while a stranger with the
// right password is still throttled.
func TestLoginAccountLimiterSparesKnownIP(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	srv.SetBehindTLSProxy(true)

	// The owner signs in from 10.0.0.2 (now a known source), then signs out.
	owner := newProxyClient(t)
	resp := loginFrom(t, owner, ts, "10.0.0.2", testPassword)
	resp.Body.Close()
	if !hasSessionCookie(resp) {
		t.Fatal("owner login from 10.0.0.2 should succeed")
	}
	postForm(t, owner, ts, "/logout", url.Values{}).Body.Close()

	// An attacker from 10.0.0.1 exhausts the account limiter (10/min).
	attacker := newProxyClient(t)
	var last string
	for i := 0; i < 10; i++ {
		r := loginFrom(t, attacker, ts, "10.0.0.1", "not-the-password")
		last = readBody(t, r)
		r.Body.Close()
	}
	if !strings.Contains(last, "Too many attempts") {
		t.Fatalf("account limiter should have tripped for the attacker:\n%s", last)
	}

	// A stranger with the right password is still refused by the account limiter.
	stranger := newProxyClient(t)
	r := loginFrom(t, stranger, ts, "10.0.0.3", testPassword)
	body := readBody(t, r)
	r.Body.Close()
	if hasSessionCookie(r) || !strings.Contains(body, "Too many attempts") {
		t.Fatal("a never-seen source must still be throttled by the account limiter")
	}

	// The owner from the known address gets in.
	owner2 := newProxyClient(t)
	r = loginFrom(t, owner2, ts, "10.0.0.2", testPassword)
	r.Body.Close()
	if !hasSessionCookie(r) {
		t.Fatal("known source must bypass the account lockout")
	}
	// ...but a wrong password from the known address is still a failure.
	r = loginFrom(t, newProxyClient(t), ts, "10.0.0.2", "still-wrong")
	r.Body.Close()
	if hasSessionCookie(r) {
		t.Fatal("known source bypasses the lockout, not the password check")
	}
}

// knownIPs stays bounded and evicts the oldest entry.
func TestKnownIPsBounded(t *testing.T) {
	k := newKnownIPs(2)
	k.add("1.1.1.1")
	k.add("2.2.2.2")
	k.add("3.3.3.3")
	if k.has("1.1.1.1") {
		t.Fatal("oldest entry should have been evicted")
	}
	if !k.has("2.2.2.2") || !k.has("3.3.3.3") {
		t.Fatal("newer entries must survive")
	}
	if len(k.seen) != 2 {
		t.Fatalf("size %d, want 2", len(k.seen))
	}
}

// Batch T3 — HSTS is emitted exactly when the app knows it is behind TLS.
func TestHSTSOnlyWhenSecure(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	client := newClient(t)

	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := resp.Header.Get("Strict-Transport-Security"); h != "" {
		t.Fatalf("plain http must not send HSTS, got %q", h)
	}

	srv.SetBehindTLSProxy(true)
	resp, err = client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if h := resp.Header.Get("Strict-Transport-Security"); !strings.HasPrefix(h, "max-age=") {
		t.Fatalf("behind a TLS proxy HSTS must be set, got %q", h)
	}
}
