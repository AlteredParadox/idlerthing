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
)

// hxGet performs an htmx GET (HX-Request: true) and returns the body.
func hxGet(t *testing.T, client *http.Client, ts *httptest.Server, path string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	req.Header.Set("HX-Request", "true")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	return resp.StatusCode, body
}

// hxPostForm performs an htmx POST with the session CSRF token, returning
// the raw response (redirects are not followed).
func hxPostForm(t *testing.T, client *http.Client, ts *httptest.Server, path string, vals url.Values) *http.Response {
	t.Helper()
	if vals.Get("csrf_token") == "" {
		vals.Set("csrf_token", sessionCSRF(t, client, ts))
	}
	req, _ := http.NewRequest("POST", ts.URL+path, strings.NewReader(vals.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

// Batch S1 — an htmx swap of a generic section list keeps its column
// headers, row count and empty state. handleSectionList used to fill only
// listNav+Rows on the HX-Request path, so sorting or searching on /shared,
// /seedboxes, /domains or /misc rendered a header-less table.
func TestSectionListHTMXKeepsHeaderAndEmptyState(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	postForm(t, client, ts, "/seedboxes", url.Values{"hostname": {"box-one"}, "active": {"on"}}).Body.Close()

	status, body := hxGet(t, client, ts, "/seedboxes?sort=hostname&dir=asc")
	if status != http.StatusOK {
		t.Fatalf("htmx list: status %d", status)
	}
	for _, want := range []string{"Hostname <span class=\"arrow\">", "box-one", "1 rows"} {
		if !strings.Contains(body, want) {
			t.Errorf("htmx list swap lost %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "<html") {
		t.Fatal("htmx list swap must be the partial, not a full page")
	}

	// No-match search: the empty state must carry its title and button label.
	_, body = hxGet(t, client, ts, "/seedboxes?q=nothing-matches-this")
	for _, want := range []string{"No seedboxes yet", "Add your first one", "Add seedbox"} {
		if !strings.Contains(body, want) {
			t.Errorf("htmx empty state lost %q:\n%s", want, body)
		}
	}
}

// Batch S2 — the note/IP/DNS delete forms are hx-post with no target, so
// their handlers must answer htmx with HX-Redirect + 204 (like the other
// delete handlers) rather than a 303 that htmx would swap into the form.
func TestExtrasDeleteHTMXRedirects(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "hx-host")

	postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "body": {"a note"}, "back": {"/servers/1"},
	}).Body.Close()
	postForm(t, client, ts, "/ips", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "address": {"203.0.113.9"},
	}).Body.Close()
	postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"a.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.9"}, "server_id": {"1"},
	}).Body.Close()

	cases := []struct{ path, back, table string }{
		{"/notes/1/delete", "/servers/1", "notes"},
		{"/ips/1/delete", "/ips", "ips"},
		{"/dns/1/delete", "/dns", "dns"},
	}
	for _, c := range cases {
		resp := hxPostForm(t, client, ts, c.path, url.Values{"back": {c.back}})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent {
			t.Errorf("%s: htmx delete status %d, want 204", c.path, resp.StatusCode)
		}
		if got := resp.Header.Get("HX-Redirect"); got != c.back {
			t.Errorf("%s: HX-Redirect %q, want %q", c.path, got, c.back)
		}
		var n int
		if err := database.QueryRow("SELECT COUNT(*) FROM " + c.table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %s rows after delete = %d (err %v), want 0", c.path, c.table, n, err)
		}
	}

	// A non-htmx post still gets the plain redirect.
	postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"1"}, "body": {"another"},
	}).Body.Close()
	resp := postForm(t, client, ts, "/notes/2/delete", url.Values{"back": {"/notes"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/notes" {
		t.Fatalf("plain delete: %d %q, want 303 /notes", resp.StatusCode, resp.Header.Get("Location"))
	}
}

// Batch S3 — deleting a service that is already gone is a 404, not a 500
// (the model returns sql.ErrNoRows; the API already mapped it to 404).
func TestDeleteMissingServiceIs404(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	for _, path := range []string{
		"/servers/999/delete", "/domains/999/delete", "/seedboxes/999/delete",
		"/shared/999/delete", "/reseller/999/delete", "/misc/999/delete",
	} {
		resp := postForm(t, client, ts, path, url.Values{})
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", path, resp.StatusCode)
		}
	}
}

// Batch S4 — the delete flash names one item properly ("Seedbox deleted.",
// not "SeedBoxe deleted." from trimming a trailing "s" off the title).
func TestSectionDeleteFlashSingular(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	postForm(t, client, ts, "/seedboxes", url.Values{"hostname": {"box-two"}, "active": {"on"}}).Body.Close()

	resp := postForm(t, client, ts, "/seedboxes/1/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("delete: status %d", resp.StatusCode)
	}
	resp, err := client.Get(ts.URL + "/seedboxes")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Seedbox deleted.") || strings.Contains(body, "SeedBoxe deleted") {
		t.Fatalf("flash should read 'Seedbox deleted.':\n%s", body)
	}
}
