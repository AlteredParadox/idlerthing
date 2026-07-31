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
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func settingsPost(t *testing.T, client *http.Client, ts *httptest.Server, extra url.Values) *http.Response {
	t.Helper()
	vals := url.Values{
		"default_currency": {"USD"}, "dashboard_currency": {"USD"},
		"due_soon_amount": {"14"}, "recently_added_amount": {"5"},
		"theme": {"dark"},
	}
	for k, v := range extra {
		vals[k] = v
	}
	return postForm(t, client, ts, "/settings", vals)
}

func TestAccentColorSettings(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	// Valid color persists.
	resp := settingsPost(t, client, ts, url.Values{"accent_color": {"#A78BFA"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	var accent string
	database.QueryRow("SELECT accent_color FROM settings WHERE id = 1").Scan(&accent)
	if accent != "#a78bfa" {
		t.Fatalf("accent not persisted: %q", accent)
	}

	// Garbage rejected with inline error.
	resp = settingsPost(t, client, ts, url.Values{"accent_color": {"violet"}})
	if !strings.Contains(readBody(t, resp), "hex color") {
		t.Fatal("expected validation error for garbage color")
	}
	resp.Body.Close()
	database.QueryRow("SELECT accent_color FROM settings WHERE id = 1").Scan(&accent)
	if accent != "#a78bfa" {
		t.Fatal("garbage color should not overwrite")
	}

	// Compact checkbox persists.
	resp = settingsPost(t, client, ts, url.Values{"compact_mode": {"on"}})
	resp.Body.Close()
	var compact int
	database.QueryRow("SELECT compact_mode FROM settings WHERE id = 1").Scan(&compact)
	if compact != 1 {
		t.Fatal("compact_mode not persisted")
	}
}

func TestAccentCSSEndpoint(t *testing.T) {
	ts, database := newTestServer(t)
	get := func() string {
		resp, err := http.Get(ts.URL + "/static/accent.css?c=test")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		return string(body)
	}

	// Default accent.
	if css := get(); !strings.Contains(css, "--acc: #5b9cf8") {
		t.Fatalf("default accent missing: %s", css)
	}

	// Light accent → dark foreground text.
	database.Exec("UPDATE settings SET accent_color = '#f0f0f0' WHERE id = 1")
	css := get()
	if !strings.Contains(css, "--acc: #f0f0f0") || !strings.Contains(css, "--accfg: #08101c") {
		t.Fatalf("light accent derivation wrong: %s", css)
	}
	if !strings.Contains(css, "--accSoft: rgba(240, 240, 240, 0.14)") {
		t.Fatalf("accSoft wrong: %s", css)
	}

	// Dark accent → white text.
	database.Exec("UPDATE settings SET accent_color = '#123456' WHERE id = 1")
	if css := get(); !strings.Contains(css, "--accfg: #ffffff") {
		t.Fatalf("dark accent should get white text: %s", css)
	}

	// Invalid stored color falls back to default.
	database.Exec("UPDATE settings SET accent_color = 'junk' WHERE id = 1")
	if css := get(); !strings.Contains(css, "--acc: #5b9cf8") {
		t.Fatalf("invalid color should fall back: %s", css)
	}
}

func TestLayoutAccentAndCompact(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "/static/accent.css?c=5b9cf8") {
		t.Fatal("layout should link fingerprinted accent.css")
	}
	if strings.Contains(body, `class="compact"`) {
		t.Fatal("compact class should be absent by default")
	}

	database.Exec("UPDATE settings SET compact_mode = 1, accent_color = '#a78bfa' WHERE id = 1")
	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `class="compact"`) {
		t.Fatal("compact class should be on body when enabled")
	}
	if !strings.Contains(body, "/static/accent.css?c=a78bfa") {
		t.Fatal("fingerprint should follow the configured color")
	}
}

func TestFaviconSVG(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	// Unauthenticated: the icon is public, like accent.css.
	resp, err := http.Get(ts.URL + "/static/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "image/svg+xml") {
		t.Fatalf("wrong content-type: %q", ct)
	}
	if !strings.Contains(resp.Header.Get("Cache-Control"), "immutable") {
		t.Fatalf("icon should be immutably cacheable, got %q", resp.Header.Get("Cache-Control"))
	}
	if !strings.Contains(body, defaultAccent) {
		t.Fatalf("default accent missing from icon: %s", body)
	}

	// The icon follows the accent setting, so the ?c= fingerprint is honest.
	settingsPost(t, client, ts, url.Values{"accent_color": {"#A78BFA"}}).Body.Close()
	resp, err = client.Get(ts.URL + "/static/favicon.svg")
	if err != nil {
		t.Fatal(err)
	}
	if body := readBody(t, resp); !strings.Contains(body, "#a78bfa") {
		t.Fatalf("icon did not follow accent change: %s", body)
	}
}

// The whole point of the favicon work: a bare /favicon.ico must not 404 on
// every page load. Browsers request it whether or not <link rel="icon"> is
// present, and each miss was a log line.
func TestFaviconICONotFound(t *testing.T) {
	ts, _ := newTestServer(t)

	resp, err := http.Get(ts.URL + "/favicon.ico")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", resp.StatusCode)
	}
}

// Every page that renders its own <head> must link the icon — layout covers
// the app, but login and public are standalone documents.
func TestFaviconLinkedOnAllHeads(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	if _, err := database.Exec("UPDATE settings SET servers_public = 1 WHERE id = 1"); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name, path string
		authed     bool
	}{
		{"layout", "/", true},
		{"login", "/login", false},
		{"public", "/public", false},
	} {
		c := ts.Client()
		if tc.authed {
			c = client
		}
		resp, err := c.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, `rel="icon"`) {
			t.Errorf("%s head has no favicon link", tc.name)
		}
		// public.html builds its data as a map, so a missing AccentC would
		// render as "<no value>" instead of failing — assert the real value.
		if !strings.Contains(body, "favicon.svg?v="+assetVersion+"-"+defaultAccent[1:]) {
			t.Errorf("%s head has no version+accent fingerprint on the icon", tc.name)
		}
	}
}
