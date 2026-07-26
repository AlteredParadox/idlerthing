package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

const yabsFixture = `{
  "version": "v2025-04-20",
  "os": {"distro": "Debian GNU/Linux 12", "kernel": "6.1.0", "uptime": "3 days"},
  "cpu": {"model": "AMD EPYC 7443P", "cores": 4},
  "memory": {"ram": "7.7 GiB", "swap": "975 MiB"},
  "disk": {"fio": [{"bs": "4k", "read": "88.0 MB/s", "write": "88.3 MB/s"}]},
  "network": {"iperf": [{"location": "Frankfurt", "provider": "Clouvider", "send": "1.76 Gbits/sec", "recv": "1.80 Gbits/sec", "latency": "18.2 ms"}]},
  "geekbench": {"version": 6, "single": 1686, "multi": 4820, "url": "https://browser.geekbench.com/v6/cpu/999"}
}`

// signYABSTest signs like the server does.
func signYABSTest(secret []byte, serverID, ts int64) string {
	mac := hmac.New(sha256.New, secret)
	fmt.Fprintf(mac, "%d.%d", serverID, ts)
	return hex.EncodeToString(mac.Sum(nil))
}

func postYABS(t *testing.T, ts *httptest.Server, srv *Server, serverID int64, tsOverride *int64, sigOverride string) *http.Response {
	t.Helper()
	now := time.Now().Unix()
	if tsOverride != nil {
		now = *tsOverride
	}
	sig := sigOverride
	if sig == "" {
		sig = signYABSTest(srv.secret, serverID, now)
	}
	resp, err := http.Post(
		fmt.Sprintf("%s/api/yabs/%d?sig=%s&ts=%d", ts.URL, serverID, sig, now),
		"application/json", strings.NewReader(yabsFixture))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestYABSIngestFlow(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "yabs-host")

	// Bad signature → 403.
	resp := postYABS(t, ts, srv, 1, nil, "deadbeef")
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("bad sig: expected 403, got %d", resp.StatusCode)
	}

	// Expired ts → 403.
	old := time.Now().Add(-13 * time.Hour).Unix()
	resp = postYABS(t, ts, srv, 1, &old, signYABSTest(srv.secret, 1, old))
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired: expected 403, got %d", resp.StatusCode)
	}

	// Good signature → 200 ok + row + speeds + has_yabs flip.
	resp = postYABS(t, ts, srv, 1, nil, "")
	var out map[string]any
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("good sig: expected 200, got %d (%s)", resp.StatusCode, body)
	}
	jsonUnmarshal(body, &out)
	if out["status"] != "ok" {
		t.Fatalf("expected ok: %v", out)
	}
	var hasYabs, diskRows, netRows int
	database.QueryRow("SELECT has_yabs FROM servers WHERE id = 1").Scan(&hasYabs)
	database.QueryRow("SELECT COUNT(*) FROM yabs_disk_speed").Scan(&diskRows)
	database.QueryRow("SELECT COUNT(*) FROM yabs_network_speed").Scan(&netRows)
	if hasYabs != 1 || diskRows != 1 || netRows != 1 {
		t.Fatalf("ingest incomplete: has_yabs=%d disks=%d net=%d", hasYabs, diskRows, netRows)
	}

	// Duplicate → duplicate status, no second row.
	resp = postYABS(t, ts, srv, 1, nil, "")
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	jsonUnmarshal(body, &out)
	if out["status"] != "duplicate" {
		t.Fatalf("expected duplicate: %v", out)
	}
	var runs int
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	if runs != 1 {
		t.Fatalf("duplicate inserted: %d runs", runs)
	}

	// Views render with run data.
	resp, err := client.Get(ts.URL + "/servers/1/yabs")
	if err != nil {
		t.Fatal(err)
	}
	bodyB := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(bodyB, "1686") {
		t.Fatal("runs list should show gb single")
	}

	resp, err = client.Get(ts.URL + "/servers/1/yabs/1")
	if err != nil {
		t.Fatal(err)
	}
	bodyB = readBody(t, resp)
	resp.Body.Close()
	for _, want := range []string{"AMD EPYC 7443P", "Debian GNU/Linux 12", "4820", "88.0", "Frankfurt"} {
		if !strings.Contains(bodyB, want) {
			t.Fatalf("run detail should contain %q", want)
		}
	}

	// Index page.
	resp, err = client.Get(ts.URL + "/yabs")
	if err != nil {
		t.Fatal(err)
	}
	bodyB = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(bodyB, "yabs-host") {
		t.Fatal("index should show server hostname")
	}

	// YABS card with command on the server detail.
	resp, err = client.Get(ts.URL + "/servers/1")
	if err != nil {
		t.Fatal(err)
	}
	bodyB = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(bodyB, "/api/yabs/1?sig=") || !strings.Contains(bodyB, "copy-btn") {
		t.Fatal("detail should show ingest command + copy button")
	}
}

func jsonUnmarshal(b []byte, v any) {
	_ = json.Unmarshal(b, v)
}

func TestPingValidationAndRunner(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)

	// Stub the runner.
	orig := pingRunner
	defer func() { pingRunner = orig }()
	pingRunner = func(host string) (float64, error) {
		if host == "good.test" {
			return 12.3, nil
		}
		return 0, fmt.Errorf("unreachable")
	}

	csrf := sessionCSRF(t, client, ts)
	post := func(host string) string {
		resp, err := client.PostForm(ts.URL+"/tools/ping", url.Values{
			"csrf_token": {csrf}, "host": {host},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}

	if out := post("good.test"); !strings.Contains(out, "12.3 ms") || !strings.Contains(out, "dot-ok") {
		t.Fatalf("good ping: %s", out)
	}
	if out := post("dead.test"); !strings.Contains(out, "unreachable") {
		t.Fatalf("bad ping: %s", out)
	}
	// Shell metacharacters rejected before exec.
	if out := post("; rm -rf /"); !strings.Contains(out, "invalid host") {
		t.Fatalf("metachar injection: %s", out)
	}
	if out := post("a\"b"); !strings.Contains(out, "invalid host") {
		t.Fatalf("quote injection: %s", out)
	}
	// Plain IP accepted by validation (fails at runner here).
	if out := post("192.0.2.1"); !strings.Contains(out, "unreachable") {
		t.Fatalf("ip target: %s", out)
	}
}

func TestPublicPage(t *testing.T) {
	ts, database := newTestServer(t)
	client := authedClient(t, ts)

	// Disabled by default → 404 (no auth needed).
	resp, err := newClient(t).Get(ts.URL + "/public")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 when disabled, got %d", resp.StatusCode)
	}

	// Enable + seed one public and one private server.
	database.Exec("UPDATE settings SET servers_public = 1 WHERE id = 1")
	resp = postForm(t, client, ts, "/servers", url.Values{
		"hostname": {"public-srv"}, "server_type": {"1"}, "active": {"on"},
		"show_public": {"on"}, "ram_as_mb": {"2048"},
	})
	resp.Body.Close()
	createServer(t, client, ts, "private-srv")

	resp, err = newClient(t).Get(ts.URL + "/public")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 when enabled, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "public-srv") || strings.Contains(body, "private-srv") {
		t.Fatal("public page should show only show_public rows")
	}
	if strings.Contains(body, "sidebar") {
		t.Fatal("public page must not render the sidebar")
	}
}

func TestUnknownRouteIs404(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	resp, err := client.Get(ts.URL + "/no-such-page")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", resp.StatusCode)
	}
	if !strings.Contains(body, "404") || !strings.Contains(body, "sidebar") {
		t.Fatal("expected styled 404 page with layout")
	}
}

func TestYABSDeleteScopedToServer(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "yabs-host")
	createServer(t, client, ts, "other-host")

	// Ingest one run for server 1.
	resp := postYABS(t, ts, srv, 1, nil, "")
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ingest: %d", resp.StatusCode)
	}

	csrf := sessionCSRF(t, client, ts)
	// Mismatched URL (run 1 belongs to server 1, not server 2) → no delete.
	resp, err := client.PostForm(ts.URL+"/servers/2/yabs/1/delete", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for mismatched delete, got %d", resp.StatusCode)
	}
	var n int
	database.QueryRow("SELECT COUNT(*) FROM yabs WHERE id = 1").Scan(&n)
	if n != 1 {
		t.Fatal("run must survive mismatched delete")
	}

	// Correct URL deletes.
	resp, err = client.PostForm(ts.URL+"/servers/1/yabs/1/delete", url.Values{"csrf_token": {csrf}})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("expected 303, got %d", resp.StatusCode)
	}
	database.QueryRow("SELECT COUNT(*) FROM yabs WHERE id = 1").Scan(&n)
	if n != 0 {
		t.Fatal("run should be deleted via matching URL")
	}
}
