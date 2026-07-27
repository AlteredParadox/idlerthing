package web

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Invalid dates on the JSON API are rejected per-field with 422, not
// silently dropped or 500'd.
func TestAPIServerCreateInvalidDates(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	token := setAPIToken(t, database)

	for _, tc := range []struct {
		name, body, field string
	}{
		{"owned_since", `{"hostname":"h1","owned_since":"31-12-2024"}`, "owned_since"},
		{"next_due_date", `{"hostname":"h1","pricing":{"currency":"USD","price":5,"term":1,"next_due_date":"not-a-date"}}`, "next_due_date"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest("POST", ts.URL+"/api/servers", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnprocessableEntity {
				t.Fatalf("got %d, want 422", resp.StatusCode)
			}
			var out map[string]any
			json.NewDecoder(resp.Body).Decode(&out)
			fields, _ := out["fields"].(map[string]any)
			if _, ok := fields[tc.field]; !ok {
				t.Fatalf("no %q in validation fields: %v", tc.field, out)
			}
		})
	}
}

// A DB failure during token lookup must surface as 500, not 401 — a 401
// would send an operator chasing tokens while the database is broken.
func TestAPIAuthDBErrorIs500(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	token := setAPIToken(t, database)
	database.Close() // every subsequent query errors

	req, _ := http.NewRequest("GET", ts.URL+"/api/servers", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("got %d, want 500", resp.StatusCode)
	}
}

// The web form reports a bad date inline rather than storing NULL silently.
func TestServerFormInvalidDate(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	client := newClient(t)
	login(t, client, ts, testPassword)
	csrf := sessionCSRF(t, client, ts)

	resp := postForm(t, client, ts, "/servers", url.Values{
		"csrf_token":  {csrf},
		"hostname":    {"badge-date"},
		"server_type": {"1"},
		"active":      {"on"},
		"owned_since": {"2024-13-45"},
	})
	defer resp.Body.Close()
	body := readBody(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "Invalid date.") {
		t.Fatalf("got %d, want the form re-rendered with 'Invalid date.'", resp.StatusCode)
	}
}
