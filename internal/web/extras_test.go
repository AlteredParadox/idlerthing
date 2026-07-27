package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// seedOneServer creates one server via the form and returns the client.
func seedOneServer(t *testing.T, ts *httptest.Server) *http.Client {
	t.Helper()
	client := authedClient(t, ts)
	createServer(t, client, ts, "detail-01")
	// Drain flash.
	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return client
}

func TestLabelsAssignUnassignOnDetail(t *testing.T) {
	ts, _ := newTestServer(t)
	client := seedOneServer(t, ts)

	// Create + assign a new label via the detail page form.
	resp := postForm(t, client, ts, "/labels/assign", url.Values{
		"service_id": {"1"}, "service_type": {"1"},
		"new_label": {"production"}, "back": {"/servers/1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("assign: expected 303, got %d", resp.StatusCode)
	}

	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	// "production" appears in the chip AND in the add-form select.
	if strings.Count(body, "production") != 2 {
		t.Fatalf("label chip should appear on detail page (count=%d)", strings.Count(body, "production"))
	}

	// Unassign.
	resp = postForm(t, client, ts, "/labels/unassign", url.Values{
		"label_id": {"1"}, "service_id": {"1"}, "service_type": {"1"}, "back": {"/servers/1"},
	})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	// Only the select option remains.
	if strings.Count(body, "production") != 1 {
		t.Fatalf("label chip should be gone after unassign (count=%d)", strings.Count(body, "production"))
	}
}

func TestLabelsMaxFourFlash(t *testing.T) {
	ts, _ := newTestServer(t)
	client := seedOneServer(t, ts)

	for i, name := range []string{"a", "b", "c", "d", "e"} {
		resp := postForm(t, client, ts, "/labels/assign", url.Values{
			"service_id": {"1"}, "service_type": {"1"},
			"new_label": {name}, "back": {"/servers/1"},
		})
		resp.Body.Close()
		_ = i
	}
	// Fifth assign redirects back with an error flash; the detail page
	// should show only 4 chips and no add form.
	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Maximum of 4 labels") {
		t.Fatal("expected max-labels flash")
	}
	if strings.Contains(body, `>e</span>`) || strings.Contains(body, "chip\">\n        e") {
		t.Fatal("fifth label should not be assigned")
	}
}

func TestNotesAddAndDelete(t *testing.T) {
	ts, _ := newTestServer(t)
	client := seedOneServer(t, ts)

	resp := postForm(t, client, ts, "/notes", url.Values{
		"service_id": {"1"}, "service_type": {"1"},
		"body": {"root password rotated"}, "back": {"/servers/1"},
	})
	resp.Body.Close()

	resp, err := client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "root password rotated") {
		t.Fatal("note should appear on detail page")
	}

	// Index page shows it with the target link.
	resp, err = client.Get(ts.URL + "/notes")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "root password rotated") || !strings.Contains(body, "/servers/1") {
		t.Fatal("notes index should list note with target")
	}

	resp = postForm(t, client, ts, "/notes/1/delete", url.Values{"back": {"/notes"}})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/notes")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "root password rotated") {
		t.Fatal("note not deleted")
	}
}

func TestIPAddInvalidAndWhois(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	client := seedOneServer(t, ts)

	// Invalid address refused with flash, nothing attached.
	resp := postForm(t, client, ts, "/ips", url.Values{
		"service_id": {"1"}, "service_type": {"1"},
		"address": {"999.1.2.3"}, "back": {"/servers/1"},
	})
	resp.Body.Close()
	resp, err := client.Get(ts.URL + "/ips")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "999.1.2.3") {
		t.Fatal("invalid IP should not be stored")
	}

	// Valid address attaches and shows on the detail card.
	resp = postForm(t, client, ts, "/ips", url.Values{
		"service_id": {"1"}, "service_type": {"1"},
		"address": {"203.0.113.10"}, "back": {"/servers/1"},
	})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "203.0.113.10") {
		t.Fatal("IP should appear on detail card")
	}

	// Whois success via injected endpoint.
	whoisSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"success":true,"country":"Germany","region":"Bavaria","city":"Nuremberg","connection":{"asn":24940,"org":"Hetzner","isp":"Hetzner Online"}}`))
	}))
	defer whoisSrv.Close()
	srv.whoisURL = whoisSrv.URL

	resp = postForm(t, client, ts, "/ips/1/whois", url.Values{"back": {"/ips"}})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/ips")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	for _, want := range []string{"Germany", "Hetzner", "AS24940"} {
		if !strings.Contains(body, want) {
			t.Fatalf("ips index should contain %q after whois", want)
		}
	}

	// Whois failure keeps old data and flashes an error.
	// (Reset the throttle so this exercises the failure path, not the bounce.)
	srv.whoisRate.last = time.Time{}
	srv.whoisURL = "http://127.0.0.1:1/unreachable"
	resp = postForm(t, client, ts, "/ips/1/whois", url.Values{"back": {"/ips"}})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/ips")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Whois refresh failed") || !strings.Contains(body, "Germany") {
		t.Fatal("failure should flash error and keep old data")
	}
}

func TestDNSPageCRUD(t *testing.T) {
	ts, _ := newTestServer(t)
	client := seedOneServer(t, ts)

	resp := postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"www.example.com"}, "dns_type": {"A"},
		"address": {"203.0.113.5"}, "server_id": {"1"},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("create: expected 303, got %d", resp.StatusCode)
	}

	resp, err := client.Get(ts.URL + "/dns")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	for _, want := range []string{"www.example.com", "203.0.113.5", "detail-01"} {
		if !strings.Contains(body, want) {
			t.Fatalf("dns index should contain %q", want)
		}
	}

	// DNS card on the server detail shows the record.
	resp, err = client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "www.example.com") {
		t.Fatal("server detail should show linked DNS record")
	}

	// Edit.
	resp = postForm(t, client, ts, "/dns/1/update", url.Values{
		"hostname": {"mail.example.com"}, "dns_type": {"MX"},
		"address": {"10 mail.example.com"}, "server_id": {"1"},
	})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/dns")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "mail.example.com") {
		t.Fatal("update not applied")
	}

	// Delete.
	resp = postForm(t, client, ts, "/dns/1/delete", url.Values{"back": {"/dns"}})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/dns")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, "mail.example.com") {
		t.Fatal("record not deleted")
	}
}

func TestDashboardDueSoonExcludesInactive(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	soon := time.Now().AddDate(0, 0, 5).Format(time.DateOnly)

	// Active shared due soon → appears.
	resp := postForm(t, client, ts, "/shared", url.Values{
		"main_domain": {"active-due.example.com"}, "active": {"on"},
		"price": {"10"}, "currency": {"USD"}, "term": {"1"},
		"next_due_date": {soon},
	})
	resp.Body.Close()

	// Inactive shared due soon → excluded from the due-soon list.
	resp = postForm(t, client, ts, "/shared", url.Values{
		"main_domain": {"inactive-due.example.com"},
		"price":       {"10"}, "currency": {"USD"}, "term": {"1"},
		"next_due_date": {soon},
	})
	resp.Body.Close()

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()

	// Scope assertions to the due-soon section (the inactive service still
	// legitimately appears under "Recently added").
	start := strings.Index(body, "Due soon")
	end := strings.Index(body, "Recently added")
	if start < 0 || end < 0 || end <= start {
		t.Fatal("could not locate due-soon section")
	}
	dueSection := body[start:end]
	if !strings.Contains(dueSection, "active-due.example.com") {
		t.Fatal("active service due soon should appear")
	}
	if strings.Contains(dueSection, "inactive-due.example.com") {
		t.Fatal("inactive service must not appear in due soon")
	}
}

func TestDashboardRendersWithData(t *testing.T) {
	ts, _, srv := newTestServerFull(t)

	// Inject rates so the dashboard converts currencies.
	ratesSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"base":"USD","rates":{"EUR":0.5,"GBP":0.8}}`))
	}))
	defer ratesSrv.Close()
	srv.rates.BaseURL = ratesSrv.URL

	client := authedClient(t, ts)

	// One USD-monthly server + one EUR-annual shared, one past-due domain.
	createServer(t, client, ts, "dash-srv") // $10/mo
	resp := postForm(t, client, ts, "/shared", url.Values{
		"main_domain": {"dash.example.com"}, "active": {"on"},
		"price": {"60"}, "currency": {"EUR"}, "term": {"4"},
		"next_due_date": {"2020-01-10"}, // past → due soon after rollover? no: advances
	})
	resp.Body.Close()

	resp, err := client.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()

	if !strings.Contains(body, "Dashboard") {
		t.Fatal("dashboard should render")
	}
	// Monthly cost: $10 (server) + €60/yr → 60/0.5/12 = $10 → $20.00/mo.
	if !strings.Contains(body, "$20.00/mo") {
		t.Fatal("monthly cost should convert EUR via rates: expected $20.00/mo")
	}
	// Yearly cost: 20 × 12 → $240.00/yr.
	if !strings.Contains(body, "$240.00/yr") {
		t.Fatal("yearly cost should be 12× monthly: expected $240.00/yr")
	}
	// Due soon card should include the shared service (advanced due date).
	if !strings.Contains(body, "dash.example.com") {
		t.Fatal("due soon / recent list should mention the shared service")
	}
	// Spec summary card exists.
	if !strings.Contains(body, "CPU cores") {
		t.Fatal("spec summary missing")
	}
}
