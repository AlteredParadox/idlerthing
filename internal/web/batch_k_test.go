package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Batch K S1 — the CSRF chokepoint rejects multipart outright and caps
// form bodies; ordinary forms keep working.
func TestCSRFBodyHardening(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	// Multipart to a protected route → 415 before any token check.
	req, _ := http.NewRequest("POST", ts.URL+"/settings", strings.NewReader("--x"))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=x")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("multipart: expected 415, got %d", resp.StatusCode)
	}

	// Oversized urlencoded body → 4xx (body too large to parse a token from).
	big := url.Values{"csrf_token": {"x"}, "pad": {strings.Repeat("a", 2<<20)}}
	resp, err = client.PostForm(ts.URL+"/prefs/theme", big)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 || resp.StatusCode >= 500 {
		t.Fatalf("oversized form: expected 4xx, got %d", resp.StatusCode)
	}

	// A normal form still passes.
	csrf := sessionCSRF(t, client, ts)
	resp, err = client.PostForm(ts.URL+"/prefs/theme", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("normal form broke: %d", resp.StatusCode)
	}
}

// Batch K S2+S3 — session, flash, and login-CSRF cookies all carry Secure
// under the same rule (here: behind a TLS proxy).
func TestCookiesSecureBehindProxy(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	srv.SetBehindTLSProxy(true)

	// Login-CSRF cookie (GET /login).
	resp, err := newProxyClient(t).Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == loginCSRFCookieName {
			found = true
			if !c.Secure {
				t.Fatal("login-CSRF cookie must be Secure behind a TLS proxy")
			}
		}
	}
	if !found {
		t.Fatal("no login-CSRF cookie set")
	}

	// Session cookie (POST /login).
	client := newProxyClient(t)
	resp = login(t, client, ts, testPassword)
	resp.Body.Close()
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && !c.Secure {
			t.Fatal("session cookie must be Secure behind a TLS proxy")
		}
	}

	// Flash cookie (any mutation that redirects with a flash).
	resp = postForm(t, client, ts, "/catalogs/providers", url.Values{"name": {"Hetzner"}})
	resp.Body.Close()
	found = false
	for _, c := range resp.Cookies() {
		if c.Name == flashCookieName {
			found = true
			if !c.Secure {
				t.Fatal("flash cookie must be Secure behind a TLS proxy")
			}
		}
	}
	if !found {
		t.Fatal("no flash cookie set")
	}
}

// Batch K S5 — the CSP carries the new directives.
func TestCSPDirectives(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := newClient(t).Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{
		"base-uri 'none'", "form-action 'self'", "object-src 'none'", "frame-ancestors 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP missing %q: %s", want, csp)
		}
	}
}

// Batch K B2 — updating a deleted (or never existed) id returns 404, not 500.
func TestUpdateDeletedIDReturns404(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, "/servers/999/update", url.Values{
		"hostname": {"ghost"}, "server_type": {"1"}, "active": {"on"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("server update on missing id: expected 404, got %d", resp.StatusCode)
	}

	resp = postForm(t, client, ts, "/dns/999/update", url.Values{
		"hostname": {"g.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("dns update on missing id: expected 404, got %d", resp.StatusCode)
	}

	resp = postForm(t, client, ts, "/misc/999/update", url.Values{
		"name": {"ghost"}, "active": {"on"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("misc update on missing id: expected 404, got %d", resp.StatusCode)
	}
}

// Batch K B3 — renaming a deleted catalog entry says so, instead of
// blaming a name conflict.
func TestCatalogRenameDeletedEntry(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	csrf := sessionCSRF(t, client, ts)
	form := url.Values{"csrf_token": {csrf}, "name": {"Ghost"}}
	req, _ := http.NewRequest("POST", ts.URL+"/catalogs/providers/999/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "no longer exists") {
		t.Fatalf("expected 'no longer exists' message, got: %s", body)
	}
	if strings.Contains(body, "already exists") {
		t.Fatal("deleted entry must not be misreported as a name conflict")
	}

	// A genuine conflict still gets the original message.
	postForm(t, client, ts, "/catalogs/providers", url.Values{"name": {"Hetzner"}}).Body.Close()
	postForm(t, client, ts, "/catalogs/providers", url.Values{"name": {"OVH"}}).Body.Close()
	form = url.Values{"csrf_token": {csrf}, "name": {"Hetzner"}}
	req, _ = http.NewRequest("POST", ts.URL+"/catalogs/providers/2/update", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err = client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "already exists") {
		t.Fatalf("name conflict should keep its message, got: %s", body)
	}
}

// Batch K B4 — note previews truncate rune-wise (no U+FFFD at the boundary).
func TestNotePreviewUTF8Boundary(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "utf8-host")

	// 119 ASCII + 'é' (2 bytes) straddles byte offset 120; more after.
	body := strings.Repeat("a", 119) + "é" + strings.Repeat("b", 30)
	if _, err := database.Exec(
		"INSERT INTO notes (service_id, service_type, body) VALUES (1, 1, ?)", body); err != nil {
		t.Fatal(err)
	}

	resp, err := client.Get(ts.URL + "/notes")
	if err != nil {
		t.Fatal(err)
	}
	page := readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(page, "�") {
		t.Fatal("preview split a multibyte character (U+FFFD present)")
	}
	if !strings.Contains(page, strings.Repeat("a", 119)+"é…") {
		t.Fatal("preview should keep all 120 runes plus the ellipsis")
	}
}

// Batch K B7 — a bogus ?status= falls back to the default (active) view.
func TestListStatusWhitelisted(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "active-host")
	resp := postForm(t, client, ts, "/servers", url.Values{
		"hostname": {"inactive-host"}, "server_type": {"1"},
	})
	resp.Body.Close()
	// Drain the "added" flash so it can't false-positive the row check.
	if resp, err := client.Get(ts.URL + "/servers"); err == nil {
		resp.Body.Close()
	}

	resp, err := client.Get(ts.URL + "/servers?status=bogus")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "active-host") {
		t.Fatal("bogus status should behave as the default (active) view")
	}
	if strings.Contains(body, "inactive-host") {
		t.Fatal("bogus status must not show inactive rows")
	}
	if strings.Contains(body, "status=bogus") {
		t.Fatal("listnav hrefs must not echo the bogus status")
	}
}
