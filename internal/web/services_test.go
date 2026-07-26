package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// servicePathCase describes one service section's happy path for the
// create → list → edit → delete cycle.
type servicePathCase struct {
	name       string
	base       string     // "/shared"
	createForm url.Values // must include the display-name field
	wantName   string     // string expected in list + detail HTML
	wantInList []string   // extra strings expected in list HTML
}

func runServicePath(t *testing.T, c servicePathCase) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	// Create.
	resp := postForm(t, client, ts, c.base, c.createForm)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("%s: create expected 303, got %d", c.name, resp.StatusCode)
	}

	// Appears in list.
	resp, err := client.Get(ts.URL + c.base + "?status=all")
	if err != nil {
		t.Fatalf("%s: GET list: %v", c.name, err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	for _, want := range append(c.wantInList, c.wantName) {
		if !strings.Contains(body, want) {
			t.Fatalf("%s: list should contain %q", c.name, want)
		}
	}

	// Detail renders.
	resp, err = client.Get(ts.URL + c.base + "/1")
	if err != nil {
		t.Fatalf("%s: GET detail: %v", c.name, err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, c.wantName) {
		t.Fatalf("%s: detail missing %q", c.name, c.wantName)
	}

	// Edit page pre-fills.
	resp, err = client.Get(ts.URL + c.base + "/1/edit")
	if err != nil {
		t.Fatalf("%s: GET edit: %v", c.name, err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, c.wantName) {
		t.Fatalf("%s: edit form not pre-filled", c.name)
	}

	// Search filters it out.
	resp, err = client.Get(ts.URL + c.base + "?status=all&q=zzz-no-match")
	if err != nil {
		t.Fatalf("%s: GET search: %v", c.name, err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, c.wantName) {
		t.Fatalf("%s: search did not filter", c.name)
	}

	// Delete removes.
	resp = postForm(t, client, ts, c.base+"/1/delete", url.Values{})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("%s: delete expected 303, got %d", c.name, resp.StatusCode)
	}
	resp, err = client.Get(ts.URL + c.base + "?status=all")
	if err != nil {
		t.Fatalf("%s: GET list: %v", c.name, err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if strings.Contains(body, c.wantName) {
		t.Fatalf("%s: not deleted", c.name)
	}
}

func TestSharedHostingPath(t *testing.T) {
	runServicePath(t, servicePathCase{
		name: "shared",
		base: "/shared",
		createForm: url.Values{
			"main_domain": {"blog.example.com"}, "svc_type": {"cPanel"},
			"active": {"on"}, "domains_limit": {"10"}, "disk_as_gb": {"50"},
			"price": {"8"}, "currency": {"EUR"}, "term": {"1"},
		},
		wantName:   "blog.example.com",
		wantInList: []string{"cPanel", "10 dom", "€8.00/mo"},
	})
}

func TestResellerHostingPath(t *testing.T) {
	runServicePath(t, servicePathCase{
		name: "reseller",
		base: "/reseller",
		createForm: url.Values{
			"main_domain": {"host.example.com"}, "svc_type": {"WHM"},
			"active": {"on"}, "db_limit": {"25"},
			"price": {"60"}, "currency": {"USD"}, "term": {"4"},
		},
		wantName:   "host.example.com",
		wantInList: []string{"WHM", "25 db", "$60.00/yr"},
	})
}

func TestSeedboxPath(t *testing.T) {
	runServicePath(t, servicePathCase{
		name: "seedbox",
		base: "/seedboxes",
		createForm: url.Values{
			"hostname": {"seed-01.example.com"}, "title": {"Racing box"},
			"seed_box_type": {"ruTorrent"}, "active": {"on"},
			"port_speed": {"10000"}, "disk_as_gb": {"2000"},
			"price": {"15"}, "currency": {"USD"}, "term": {"1"},
		},
		wantName:   "seed-01.example.com",
		wantInList: []string{"ruTorrent", "10000 Mbps", "$15.00/mo"},
	})
}

func TestDomainPath(t *testing.T) {
	runServicePath(t, servicePathCase{
		name: "domain",
		base: "/domains",
		createForm: url.Values{
			"domain": {"example.com"}, "extension": {".com"},
			"ns1": {"ns1.example.com"}, "active": {"on"},
			"price": {"12"}, "currency": {"USD"}, "term": {"4"},
			"next_due_date": {"2027-03-01"},
		},
		wantName:   "example.com",
		wantInList: []string{"ns1.example.com", "$12.00/yr"},
	})
}

func TestMiscPath(t *testing.T) {
	runServicePath(t, servicePathCase{
		name: "misc",
		base: "/misc",
		createForm: url.Values{
			"name": {"VPN subscription"}, "active": {"on"},
			"price": {"5"}, "currency": {"USD"}, "term": {"1"},
		},
		wantName:   "VPN subscription",
		wantInList: []string{"$5.00/mo"},
	})
}

func TestServiceValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	// Missing required field re-renders with an inline error.
	resp := postForm(t, client, ts, "/misc", url.Values{"name": {""}})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 re-render, got %d", resp.StatusCode)
	}
	if !strings.Contains(readBody(t, resp), "Name is required") {
		t.Fatal("expected validation error")
	}
}
