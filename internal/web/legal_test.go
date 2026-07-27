package web

import (
	"net/http"
	"strings"
	"testing"
)

// AGPL §13 obliges the source offer to reach every network user, so these
// three routes must answer WITHOUT a session. A regression that puts them
// behind requireAuth is a licence-compliance bug, not a UX one.
func TestLegalRoutesAreUnauthenticated(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	srv.SetLegal([]byte("AGPL TEXT"), []byte("# Third-party licenses"))

	client := newClient(t) // never logs in

	for _, tc := range []struct {
		path, wantBody string
	}{
		{"/license", "AGPL TEXT"},
		{"/third-party-licenses", "# Third-party licenses"},
	} {
		resp, err := client.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: got %d, want 200 without a session", tc.path, resp.StatusCode)
		}
		if !strings.Contains(body, tc.wantBody) {
			t.Fatalf("%s: body %q missing %q", tc.path, body, tc.wantBody)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
			t.Fatalf("%s: Content-Type %q, want text/plain", tc.path, ct)
		}
	}
}

// /source redirects to the Corresponding Source, pinned to the running
// version when the build is stamped.
func TestSourceOfferPinsToVersion(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	client := newClient(t)

	// Unstamped build: the repository root is all we can honestly offer.
	resp, err := client.Get(ts.URL + "/source")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("got %d, want 302", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != sourceBaseURL {
		t.Fatalf("dev build: Location %q, want %q", loc, sourceBaseURL)
	}

	// Stamped release: pin the offer to the exact revision that is running.
	srv.SetVersion("v1.2.3")
	resp, err = client.Get(ts.URL + "/source")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if want := sourceBaseURL + "/tree/v1.2.3"; resp.Header.Get("Location") != want {
		t.Fatalf("stamped build: Location %q, want %q", resp.Header.Get("Location"), want)
	}
}

// A binary built without the embedded texts must say so rather than serve an
// empty 200 that looks like a licence with no terms.
func TestLegalTextMissingIs500(t *testing.T) {
	ts, _, _ := newTestServerFull(t) // SetLegal never called
	resp, err := newClient(t).Get(ts.URL + "/license")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500 when the text is absent", resp.StatusCode)
	}
}

// The settings page surfaces the links, so the offer is discoverable in the
// UI and not only by knowing the URL.
func TestSettingsShowsLegalLinks(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	client := newClient(t)
	login(t, client, ts, testPassword).Body.Close()

	resp, err := client.Get(ts.URL + "/settings")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	for _, href := range []string{`href="/license"`, `href="/third-party-licenses"`, `href="/source"`} {
		if !strings.Contains(body, href) {
			t.Fatalf("settings page missing %s", href)
		}
	}
}

// ...and so does the login page, which is the only page a signed-out user
// ever reaches.
func TestLoginPageShowsLegalLinks(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	resp, err := newClient(t).Get(ts.URL + "/login")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body := readBody(t, resp)
	for _, href := range []string{`href="/license"`, `href="/source"`} {
		if !strings.Contains(body, href) {
			t.Fatalf("login page missing %s", href)
		}
	}
}

// The templates carry the AGPL notice as a GO TEMPLATE comment ({{/* … */}}),
// not an HTML one, so it is stripped at render. Shipping it in the markup
// would add it to every response for no benefit.
func TestLicenseHeaderNotServedInHTML(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	client := newClient(t)
	login(t, client, ts, testPassword).Body.Close()

	for _, path := range []string{"/login", "/", "/servers", "/settings"} {
		resp, err := client.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body := readBody(t, resp)
		resp.Body.Close()
		if strings.Contains(body, "GNU Affero General Public License as published") {
			t.Fatalf("%s ships the license header in its markup", path)
		}
	}
}
