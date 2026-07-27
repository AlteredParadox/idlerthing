package web

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// ---------- Settings ----------

func TestSettingsPageAndUpdate(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	resp, err := client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	for _, want := range []string{"Default currency", "Due soon", "Change password", "API token"} {
		if !strings.Contains(body, want) {
			t.Fatalf("settings page should contain %q", want)
		}
	}

	// Valid update persists.
	resp = postForm(t, client, ts, "/settings", url.Values{
		"default_currency": {"EUR"}, "dashboard_currency": {"GBP"},
		"due_soon_amount": {"30"}, "recently_added_amount": {"10"},
		"theme": {"light"}, "servers_public": {"on"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	var cur, dash, theme string
	var due, recent, public int
	database.QueryRow(`SELECT default_currency, dashboard_currency, due_soon_amount,
		recently_added_amount, theme, servers_public FROM settings WHERE id = 1`).
		Scan(&cur, &dash, &due, &recent, &theme, &public)
	if cur != "EUR" || dash != "GBP" || due != 30 || recent != 10 || theme != "light" || public != 1 {
		t.Fatalf("settings not persisted: %s %s %d %d %s %d", cur, dash, due, recent, theme, public)
	}

	// Invalid value rejected.
	resp = postForm(t, client, ts, "/settings", url.Values{
		"default_currency": {"USD"}, "dashboard_currency": {"USD"},
		"due_soon_amount": {"0"}, "recently_added_amount": {"5"}, "theme": {"dark"},
	})
	if !strings.Contains(readBody(t, resp), "between 1 and 365") {
		t.Fatal("expected validation error")
	}
	resp.Body.Close()
}

func TestPasswordChangeFlow(t *testing.T) {
	ts, _ := newTestServer(t)
	client1 := authedClient(t, ts)
	client2 := authedClient(t, ts) // second session

	// Wrong current password rejected.
	resp := postForm(t, client1, ts, "/settings/account", url.Values{
		"action": {"password"}, "current_password": {"nope"},
		"new_password": {"newpassword1"}, "confirm_password": {"newpassword1"},
	})
	resp.Body.Close()

	// Correct change succeeds.
	resp = postForm(t, client1, ts, "/settings/account", url.Values{
		"action": {"password"}, "current_password": {testPassword},
		"new_password": {"newpassword1"}, "confirm_password": {"newpassword1"},
	})
	resp.Body.Close()

	// client1 stays logged in.
	resp, err := client1.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatal("current session should survive password change")
	}

	// client2's session is invalidated.
	resp, err = client2.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatal("other session should be invalidated")
	}

	// Old password no longer works, new one does.
	client3 := newClient(t)
	resp = login(t, client3, ts, testPassword)
	resp.Body.Close()
	if hasSessionCookie(resp) {
		t.Fatal("old password should not work")
	}
	resp = login(t, client3, ts, "newpassword1")
	resp.Body.Close()
	if !hasSessionCookie(resp) {
		t.Fatal("new password should work")
	}
}

func TestAPITokenGeneration(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	// PRG: POST redirects; the revealed token rides a one-time cookie.
	resp := postForm(t, client, ts, "/settings/account", url.Values{"action": {"token"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 PRG redirect, got %d", resp.StatusCode)
	}

	// First GET shows the token exactly once.
	resp, err := client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "will not be shown again") {
		t.Fatal("expected copy warning with revealed token")
	}

	// Second GET (incl. F5) does NOT regenerate or reveal anything.
	resp, err = client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	body2 := readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body2, "will not be shown again") {
		t.Fatal("token must be revealed exactly once")
	}

	var hash *string
	database.QueryRow("SELECT api_token_hash FROM users WHERE email = 'admin@localhost'").Scan(&hash)
	if hash == nil || len(*hash) != 64 {
		t.Fatal("expected sha256 hex in users.api_token_hash")
	}
	// The plaintext must not be the stored value.
	if strings.Contains(body, *hash) {
		t.Fatal("stored hash must not be revealed")
	}
}

// ---------- API ----------

// setAPIToken stores a known token hash and returns the plaintext token.
func setAPIToken(t *testing.T, database *sql.DB) string {
	t.Helper()
	token := "test-api-token-0123456789abcdef"
	sum := sha256.Sum256([]byte(token))
	if _, err := database.Exec("UPDATE users SET api_token_hash = ? WHERE email = 'admin@localhost'",
		hex.EncodeToString(sum[:])); err != nil {
		t.Fatal(err)
	}
	return token
}

func apiGet(t *testing.T, ts *httptest.Server, path, token string) (*http.Response, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	body, _ := io.ReadAll(resp.Body)
	json.Unmarshal(body, &out)
	return resp, out
}

func TestAPIAuth(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)

	// No token → 401 JSON.
	resp, body := apiGet(t, ts, "/api/servers", "")
	if resp.StatusCode != http.StatusUnauthorized || body["error"] != "unauthorized" {
		t.Fatalf("expected 401 unauthorized, got %d %v", resp.StatusCode, body)
	}
	// Bad token → 401.
	resp, _ = apiGet(t, ts, "/api/servers", "wrong")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", resp.StatusCode)
	}
	// Good token works.
	resp, body = apiGet(t, ts, "/api/servers", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if _, ok := body["data"]; !ok {
		t.Fatal("expected data envelope")
	}
}

func TestAPIServerCRUD(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)

	// Create.
	payload := `{
		"hostname": "api-srv-01", "server_type": 1, "active": true,
		"ram_as_mb": 4096, "cpu": 2,
		"disks": [{"size_as_mb": 102400, "media": "NVMe"}],
		"pricing": {"currency": "USD", "price": 10, "term": 1, "next_due_date": "2027-01-01"}
	}`
	req, _ := http.NewRequest("POST", ts.URL+"/api/servers", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var created map[string]any
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %v", resp.StatusCode, created)
	}
	data := created["data"].(map[string]any)
	if data["hostname"] != "api-srv-01" {
		t.Fatalf("unexpected create response: %v", data)
	}

	// GET with relations.
	resp, body := apiGet(t, ts, "/api/servers/1", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body["disks"] == nil || body["pricing"] == nil {
		t.Fatalf("expected disks+pricing in response: %v", body)
	}
	pricing := body["pricing"].(map[string]any)
	if pricing["price"] != 10.0 {
		t.Fatalf("unexpected pricing: %v", pricing)
	}

	// Update.
	req, _ = http.NewRequest("PUT", ts.URL+"/api/servers/1", strings.NewReader(
		`{"hostname": "api-srv-renamed", "server_type": 3, "active": false}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	_, body = apiGet(t, ts, "/api/servers/1", token)
	if body["data"].(map[string]any)["hostname"] != "api-srv-renamed" {
		t.Fatal("update not applied")
	}

	// Validation error as JSON.
	req, _ = http.NewRequest("POST", ts.URL+"/api/servers", strings.NewReader(`{"hostname": ""}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var valErr map[string]any
	json.NewDecoder(resp.Body).Decode(&valErr)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity || valErr["error"] != "validation failed" {
		t.Fatalf("expected 422 validation, got %d %v", resp.StatusCode, valErr)
	}

	// Delete.
	req, _ = http.NewRequest("DELETE", ts.URL+"/api/servers/1", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp, _ = apiGet(t, ts, "/api/servers/1", token)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 after delete, got %d", resp.StatusCode)
	}
}

func TestAPIListPagination(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	client := authedClient(t, ts)

	// Seed a few domains.
	for _, d := range []string{"a.com", "b.com", "c.com"} {
		resp := postForm(t, client, ts, "/domains", url.Values{
			"domain": {d}, "active": {"on"},
		})
		resp.Body.Close()
	}

	resp, body := apiGet(t, ts, "/api/domains?per=2&page=1", token)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if body["total"] != 3.0 || body["per"] != 2.0 {
		t.Fatalf("unexpected envelope: %v", body)
	}
	if len(body["data"].([]any)) != 2 {
		t.Fatalf("expected 2 items on page 1: %v", body["data"])
	}

	_, body = apiGet(t, ts, "/api/domains?per=2&page=2", token)
	if len(body["data"].([]any)) != 1 {
		t.Fatalf("expected 1 item on page 2: %v", body["data"])
	}

	// Catalog + notes endpoints respond.
	for _, path := range []string{"/api/providers", "/api/locations", "/api/os", "/api/labels", "/api/ips", "/api/dns", "/api/shared", "/api/reseller", "/api/seedboxes", "/api/misc"} {
		resp, body := apiGet(t, ts, path, token)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: expected 200, got %d", path, resp.StatusCode)
		}
		if _, ok := body["data"]; !ok {
			t.Fatalf("%s: expected data envelope, got %v", path, body)
		}
	}

	// /api/pricings with a real row (regression: nested query deadlock).
	resp = postForm(t, client, ts, "/domains/1/update", url.Values{
		"domain": {"a.com"}, "active": {"on"},
		"price": {"12"}, "currency": {"USD"}, "term": {"4"}, "next_due_date": {"2027-01-01"},
	})
	resp.Body.Close()
	resp, body = apiGet(t, ts, "/api/pricings", token)
	if resp.StatusCode != http.StatusOK || body["total"] != 1.0 {
		t.Fatalf("pricings: %d %v", resp.StatusCode, body)
	}
	if body["data"].([]any)[0].(map[string]any)["service_name"] != "a.com" {
		t.Fatalf("pricings should resolve service_name: %v", body["data"])
	}

	// Notes endpoint with a real row too.
	resp = postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"4"}, "body": {"registrar note"}, "back": {"/notes"},
	})
	resp.Body.Close()
	resp, body = apiGet(t, ts, "/api/notes", token)
	if resp.StatusCode != http.StatusOK || body["total"] != 1.0 {
		t.Fatalf("notes: %d %v", resp.StatusCode, body)
	}
}

// ---------- Export ----------

func TestExportJSON(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "export-srv")

	req, _ := http.NewRequest("GET", ts.URL+"/export/json", nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "attachment") {
		t.Fatalf("expected attachment, got %q", cd)
	}
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	for _, key := range []string{"servers", "shared", "reseller", "seedboxes", "domains", "misc",
		"pricings", "ips", "dns", "labels", "notes", "providers", "locations", "os"} {
		if _, ok := out[key]; !ok {
			t.Fatalf("export missing key %q", key)
		}
	}
	servers := out["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(servers))
	}
	srv := servers[0].(map[string]any)
	inner := srv["server"].(map[string]any)
	if inner["hostname"] != "export-srv" || srv["disks"] == nil || srv["pricing"] == nil {
		t.Fatalf("server export incomplete: %v", srv)
	}
}

func TestExportCSVZip(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "csv-srv")

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
	want := map[string]bool{
		"servers.csv": false, "shared.csv": false, "reseller.csv": false,
		"seedboxes.csv": false, "domains.csv": false, "misc.csv": false,
	}
	for _, f := range zr.File {
		if _, ok := want[f.Name]; ok {
			want[f.Name] = true
			if f.Name == "servers.csv" {
				rc, _ := f.Open()
				content, _ := io.ReadAll(rc)
				rc.Close()
				if !strings.Contains(string(content), "hostname") || !strings.Contains(string(content), "csv-srv") {
					t.Fatal("servers.csv should have header + data row")
				}
			}
		}
	}
	for name, found := range want {
		if !found {
			t.Fatalf("zip missing %s", name)
		}
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
