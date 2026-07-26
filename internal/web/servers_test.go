package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// postForm posts a form with the session CSRF token and returns the response.
func postForm(t *testing.T, client *http.Client, ts *httptest.Server, path string, vals url.Values) *http.Response {
	t.Helper()
	if vals.Get("csrf_token") == "" {
		vals.Set("csrf_token", sessionCSRF(t, client, ts))
	}
	resp, err := client.PostForm(ts.URL+path, vals)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	return resp
}

func authedClient(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	client := newClient(t)
	resp := login(t, client, ts, testPassword)
	resp.Body.Close()
	return client
}

func createServer(t *testing.T, client *http.Client, ts *httptest.Server, hostname string) {
	t.Helper()
	resp := postForm(t, client, ts, "/servers", url.Values{
		"hostname":        {hostname},
		"server_type":     {"1"},
		"active":          {"on"},
		"ram_as_mb":       {"2048"},
		"price":           {"10"},
		"currency":        {"USD"},
		"term":            {"1"},
		"disk1_size":      {"50"},
		"disk1_size_unit": {"GB"},
		"disk1_media":     {"NVMe"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		body := readBody(t, resp)
		t.Fatalf("create server %s: expected 303, got %d: %s", hostname, resp.StatusCode, body[:min(300, len(body))])
	}
}

// TestListSortPersists verifies that choosing a sort on a list page saves
// it per user, so bare revisits and tab switches keep the same sort.
func TestListSortPersists(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "sort-01")

	// Explicit sort: saved to user_prefs.
	resp, err := client.Get(ts.URL + "/servers?sort=price&dir=desc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var v string
	if err := database.QueryRow(
		"SELECT value FROM user_prefs WHERE key = 'sort_servers'").Scan(&v); err != nil || v != "price,desc" {
		t.Fatalf("expected saved sort 'price,desc', got %q (%v)", v, err)
	}

	// Bare revisit: sort indicator shows price desc (sorted class + ↓ arrow).
	resp, err = client.Get(ts.URL + "/servers")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `sorted`) || !strings.Contains(body, "↓") {
		t.Fatal("bare /servers should keep the saved price-desc sort")
	}
	// Tab switch (no sort params) keeps it too.
	resp, err = client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `sorted`) || !strings.Contains(body, "↓") {
		t.Fatal("tab switch should keep the saved sort")
	}

	// Generic list (shared): same machinery.
	resp, err = client.Get(ts.URL + "/shared?sort=provider&dir=desc")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if err := database.QueryRow(
		"SELECT value FROM user_prefs WHERE key = 'sort_shared'").Scan(&v); err != nil || v != "provider,desc" {
		t.Fatalf("expected saved sort 'provider,desc', got %q (%v)", v, err)
	}

	// Create one shared service so the table (and its headers) render.
	resp = postForm(t, client, ts, "/shared", url.Values{
		"main_domain": {"sort-test.example.com"}, "active": {"on"},
	})
	resp.Body.Close()

	resp, err = client.Get(ts.URL + "/shared")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `sorted`) || !strings.Contains(body, "↓") {
		t.Fatal("bare /shared should keep the saved sort")
	}
}

// TestCostTotalsExcludeInactive verifies monthly/yearly totals (list page
// and dashboard) only count active services with active pricings.
func TestCostTotalsExcludeInactive(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	createServer(t, client, ts, "active-01") // $10/mo active

	// Inactive server with an expensive pricing attached ($100/mo).
	resp := postForm(t, client, ts, "/servers", url.Values{
		"hostname":    {"inactive-01"},
		"server_type": {"1"},
		"ram_as_mb":   {"2048"},
		"price":       {"100"},
		"currency":    {"USD"},
		"term":        {"1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create inactive server: expected 303, got %d", resp.StatusCode)
	}

	// Servers list: only the active server's cost counts.
	resp, err := client.Get(ts.URL + "/servers?status=all")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "$10.00/mo") || !strings.Contains(body, "$120.00/yr") {
		t.Fatal("list totals should only count the active server ($10.00/mo, $120.00/yr)")
	}
	if strings.Contains(body, "$110.00/mo") {
		t.Fatal("list totals must exclude inactive services")
	}

	// Dashboard: same.
	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "$10.00/mo") || !strings.Contains(body, "$120.00/yr") {
		t.Fatal("dashboard totals should only count the active server")
	}
	if strings.Contains(body, "$110.00/mo") {
		t.Fatal("dashboard totals must exclude inactive services")
	}
}

func TestServerCreateAppearsInList(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	createServer(t, client, ts, "web-01.example.com")

	resp, err := client.Get(ts.URL + "/servers")
	if err != nil {
		t.Fatalf("GET /servers: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	for _, want := range []string{"web-01.example.com", "KVM", "$10.00/mo", "$120.00/yr", "2 GB", "50 GB", "NVMe"} {
		if !strings.Contains(body, want) {
			t.Fatalf("list should contain %q", want)
		}
	}
}

func TestServerCreateValidationError(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	resp := postForm(t, client, ts, "/servers", url.Values{
		"hostname": {""},
		"price":    {"-5"},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Hostname is required") {
		t.Fatal("expected hostname validation error")
	}
}

func TestServerSearchAndSort(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	createServer(t, client, ts, "alpha-srv")
	createServer(t, client, ts, "beta-srv")

	// Drain the create flash so the banner can't pollute assertions.
	drain, err := client.Get(ts.URL + "/servers")
	if err != nil {
		t.Fatalf("GET /servers: %v", err)
	}
	drain.Body.Close()

	// Search filters.
	resp, err := client.Get(ts.URL + "/servers?status=all&q=alpha")
	if err != nil {
		t.Fatalf("GET /servers?q=: %v", err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "alpha-srv") || strings.Contains(body, "beta-srv") {
		t.Fatal("search did not filter")
	}

	// Sort by hostname desc reverses order.
	resp, err = client.Get(ts.URL + "/servers?status=all&sort=hostname&dir=desc")
	if err != nil {
		t.Fatalf("GET /servers?sort: %v", err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Index(body, "beta-srv") > strings.Index(body, "alpha-srv") {
		t.Fatal("hostname desc sort wrong order")
	}

	// Bogus sort param doesn't break.
	resp, err = client.Get(ts.URL + "/servers?sort=nonsense&dir=desc")
	if err != nil {
		t.Fatalf("GET bogus sort: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 on bogus sort, got %d", resp.StatusCode)
	}
}

func TestServerEditAndDelete(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "edit-me")

	// Edit page pre-fills.
	resp, err := client.Get(ts.URL + "/servers/1/edit")
	if err != nil {
		t.Fatalf("GET edit: %v", err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, `value="edit-me"`) {
		t.Fatal("edit form not pre-filled")
	}

	// Update renames.
	resp = postForm(t, client, ts, "/servers/1/update", url.Values{
		"hostname":    {"renamed"},
		"server_type": {"3"},
		"active":      {"on"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	resp, err = client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatalf("GET detail: %v", err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "renamed") || !strings.Contains(body, "DEDI") {
		t.Fatal("update not reflected on detail page")
	}

	// Delete removes it.
	resp = postForm(t, client, ts, "/servers/1/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 on delete, got %d", resp.StatusCode)
	}
	resp, err = client.Get(ts.URL + "/servers")
	if err != nil {
		t.Fatalf("GET /servers: %v", err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "renamed") {
		t.Fatal("server not deleted")
	}
}

func TestCatalogCRUDAndInUseFlash(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	// Create.
	resp := postForm(t, client, ts, "/catalogs/providers", url.Values{"name": {"Hetzner"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}

	// Appears in list.
	resp, err := client.Get(ts.URL + "/catalogs/providers")
	if err != nil {
		t.Fatalf("GET /catalogs/providers: %v", err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Hetzner") {
		t.Fatal("provider not listed")
	}

	// Attach a server → delete refused.
	createServer(t, client, ts, "uses-provider")
	resp = postForm(t, client, ts, "/servers/1/update", url.Values{
		"hostname": {"uses-provider"}, "server_type": {"1"}, "active": {"on"},
		"provider_id": {"1"},
	})
	resp.Body.Close()

	resp = postForm(t, client, ts, "/catalogs/providers/1/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	// Provider still there.
	resp, err = client.Get(ts.URL + "/catalogs/providers")
	if err != nil {
		t.Fatalf("GET /catalogs/providers: %v", err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Hetzner") {
		t.Fatal("in-use provider should not be deleted")
	}
}

func TestServersRequiresAuth(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := newClient(t).Get(ts.URL + "/servers")
	if err != nil {
		t.Fatalf("GET /servers: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected redirect, got %d", resp.StatusCode)
	}
}
