package web

import (
	"database/sql"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"

	"idlerthing/internal/db"
)

const testPassword = "testpass"

var csrfRe = regexp.MustCompile(`name="csrf_token" value="([^"]+)"`)

// newTestServer spins up the full server against a temp DB with a seeded
// admin (admin@localhost / testpass).
func newTestServer(t *testing.T) (*httptest.Server, *sql.DB) {
	t.Helper()
	ts, database, _ := newTestServerFull(t)
	return ts, database
}

// newTestServerFull additionally returns the web Server so tests can inject
// fake whois/rates endpoints.
func newTestServerFull(t *testing.T) (*httptest.Server, *sql.DB, *Server) {
	t.Helper()

	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	if _, err := db.Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	// MinCost keeps the suite fast; the cost rides in the hash anyway.
	hash, err := bcrypt.GenerateFromPassword([]byte(testPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	if _, err := database.Exec(
		"INSERT INTO users (name, email, password_hash) VALUES (?, ?, ?)",
		"admin", "admin@localhost", string(hash)); err != nil {
		t.Fatalf("seed admin: %v", err)
	}

	srv, err := New(database)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts, database, srv
}

// newClient returns an HTTP client with a cookie jar that does not follow
// redirects (tests assert on redirect targets themselves).
func newClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar: %v", err)
	}
	return &http.Client{
		Jar: jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// getLoginCSRF fetches the login page and returns the double-submit token.
func getLoginCSRF(t *testing.T, client *http.Client, ts *httptest.Server) string {
	t.Helper()
	resp, err := client.Get(ts.URL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	m := csrfRe.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no csrf_token field on login page")
	}
	return m[1]
}

// login performs the full login flow and returns the response.
func login(t *testing.T, client *http.Client, ts *httptest.Server, password string) *http.Response {
	t.Helper()
	csrf := getLoginCSRF(t, client, ts)
	resp, err := client.PostForm(ts.URL+"/login", url.Values{
		"csrf_token": {csrf},
		"email":      {"admin@localhost"},
		"password":   {password},
	})
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(buf)
}

func hasSessionCookie(resp *http.Response) bool {
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookieName && c.Value != "" {
			return true
		}
	}
	return false
}

func TestUnauthRedirectsToLogin(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := newClient(t).Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("expected redirect to /login, got %q", loc)
	}
}

func TestLoginWrongPassword(t *testing.T) {
	ts, _ := newTestServer(t)
	client := newClient(t)

	resp := login(t, client, ts, "wrongpass")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", resp.StatusCode)
	}
	if !strings.Contains(readBody(t, resp), "Invalid email or password") {
		t.Fatal("expected generic error message")
	}
	if hasSessionCookie(resp) {
		t.Fatal("session cookie must not be set on failed login")
	}
}

func TestLoginRequiresCSRF(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := newClient(t).PostForm(ts.URL+"/login", url.Values{
		"email":    {"admin@localhost"},
		"password": {testPassword},
	})
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF token, got %d", resp.StatusCode)
	}
}

func TestLoginSuccess(t *testing.T) {
	ts, _ := newTestServer(t)
	client := newClient(t)

	resp := login(t, client, ts, testPassword)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Fatalf("expected redirect to /, got %q", loc)
	}
	if !hasSessionCookie(resp) {
		t.Fatal("expected session cookie")
	}
}

func TestAuthedDashboard(t *testing.T) {
	ts, _ := newTestServer(t)
	client := newClient(t)
	resp := login(t, client, ts, testPassword)
	resp.Body.Close()

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, "Dashboard") {
		t.Fatal("expected dashboard content")
	}
	if !strings.Contains(body, "Servers") {
		t.Fatal("expected sidebar nav")
	}
}

// sessionCSRF extracts the authenticated CSRF token from the dashboard page.
func sessionCSRF(t *testing.T, client *http.Client, ts *httptest.Server) string {
	t.Helper()
	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	m := csrfRe.FindStringSubmatch(readBody(t, resp))
	if m == nil {
		t.Fatal("no csrf_token in authenticated page")
	}
	return m[1]
}

func TestLogout(t *testing.T) {
	ts, _ := newTestServer(t)
	client := newClient(t)
	resp := login(t, client, ts, testPassword)
	resp.Body.Close()

	csrf := sessionCSRF(t, client, ts)
	resp, err := client.PostForm(ts.URL+"/logout", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatalf("POST /logout: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}

	// Session must be gone: / redirects to /login again.
	resp, err = client.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303 after logout, got %d", resp.StatusCode)
	}
}

func TestThemePrefCSRF(t *testing.T) {
	ts, database := newTestServer(t)
	client := newClient(t)
	resp := login(t, client, ts, testPassword)
	resp.Body.Close()

	// Without token: 403.
	resp, err := client.PostForm(ts.URL+"/prefs/theme", url.Values{})
	if err != nil {
		t.Fatalf("POST /prefs/theme: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF token, got %d", resp.StatusCode)
	}

	// With token: theme flips dark → light in the DB.
	csrf := sessionCSRF(t, client, ts)
	resp, err = client.PostForm(ts.URL+"/prefs/theme", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatalf("POST /prefs/theme: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}

	var theme string
	if err := database.QueryRow("SELECT theme FROM settings WHERE id = 1").Scan(&theme); err != nil {
		t.Fatalf("read theme: %v", err)
	}
	if theme != "light" {
		t.Fatalf("expected theme to flip to light, got %q", theme)
	}
}

func TestShortHostnamesPref(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "web01.example.com")

	// Without token: 403.
	resp, err := client.PostForm(ts.URL+"/prefs/short-hostnames", url.Values{})
	if err != nil {
		t.Fatalf("POST /prefs/short-hostnames: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 without CSRF token, got %d", resp.StatusCode)
	}

	// Default: full hostname shown.
	listBody := func() string {
		resp, err := client.Get(ts.URL + "/servers?status=all")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return readBody(t, resp)
	}
	if body := listBody(); !strings.Contains(body, ">web01.example.com</a>") {
		t.Fatal("expected full hostname by default")
	}

	// Toggle on: pref persists, list shows the short form + title attr.
	csrf := sessionCSRF(t, client, ts)
	resp, err = client.PostForm(ts.URL+"/prefs/short-hostnames", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatalf("POST /prefs/short-hostnames: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	var v string
	if err := database.QueryRow(
		"SELECT value FROM user_prefs WHERE key = 'short_hostnames'").Scan(&v); err != nil || v != "1" {
		t.Fatalf("expected pref '1', got %q (%v)", v, err)
	}
	body := listBody()
	if !strings.Contains(body, `title="web01.example.com">web01</a>`) {
		t.Fatal("expected shortened hostname with full title attr")
	}

	// Toggle off: full hostname again.
	resp, err = client.PostForm(ts.URL+"/prefs/short-hostnames", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatalf("POST /prefs/short-hostnames: %v", err)
	}
	resp.Body.Close()
	if body := listBody(); !strings.Contains(body, ">web01.example.com</a>") {
		t.Fatal("expected full hostname after toggling off")
	}
}

func TestStaticImmutableCache(t *testing.T) {
	ts, _ := newTestServer(t)
	resp, err := newClient(t).Get(ts.URL + "/static/app.css?v=1")
	if err != nil {
		t.Fatalf("GET /static/app.css: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if cc := resp.Header.Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Fatalf("expected immutable Cache-Control, got %q", cc)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("expected CSS content type, got %q", ct)
	}
}
