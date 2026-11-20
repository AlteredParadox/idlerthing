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
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"idlerthing/internal/importer"
	"idlerthing/internal/model"
)

// busyYABS is a compact yabs.sh-shaped body with one measured iperf row and
// one "busy" row (the endpoint refused the run), plus a fio row whose write
// side is missing.
const busyYABS = `{"time":"20261119-185420","cpu":{"model":"Test CPU","cores":4},` +
	`"fio":[{"bs":"4k","speed_r":99590,"speed_units":"KBps"}],` +
	`"iperf":[{"mode":"IPv4","provider":"Clouvider","loc":"London, UK (10G)","send":"683 Mbits/sec","recv":"343 Mbits/sec","latency":"134 ms"},` +
	`{"mode":"IPv4","provider":"Edgoo","loc":"Sao Paulo, BR (1G)","send":"busy ","recv":"busy ","latency":"190 ms"}],` +
	`"geekbench":[{"version":6,"single":1102,"multi":2981,"url":"Local (--no-upload)"}]}`

// ingestSigned posts body to the signed ingest URL of serverID. Each ingest
// URL works once, so calls within one test pass distinct slot numbers: slots
// are a minute apart, which keeps two calls on different URLs even when the
// clock ticks over between them (a one-second spacing would not — the second
// call could land on the first one's timestamp).
func ingestSigned(t *testing.T, ts *httptest.Server, srv *Server, serverID, slot int64, body string) (int, string) {
	t.Helper()
	stamp := time.Now().Unix() - 60*slot
	resp, err := http.Post(
		fmt.Sprintf("%s/api/yabs/%d?sig=%s&ts=%d", ts.URL, serverID, signYABSTest(srv.secret, serverID, stamp), stamp),
		"application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(t, resp)
}

func getPage(t *testing.T, client *http.Client, url string) (int, string) {
	t.Helper()
	resp, err := client.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode, readBody(t, resp)
}

// Ingest keeps the raw body byte for byte, the run page offers it, and the
// download is scoped to the run's own server.
func TestYABSPayloadKeptAndServed(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "payload-host")
	createServer(t, client, ts, "other-host")

	if code, out := ingestSigned(t, ts, srv, 1, 0, busyYABS); code != http.StatusOK || !strings.Contains(out, `"ok"`) {
		t.Fatalf("ingest: %d %s", code, out)
	}
	var stored string
	if err := database.QueryRow("SELECT body FROM yabs_payload WHERE yabs_id = 1").Scan(&stored); err != nil {
		t.Fatalf("payload row: %v", err)
	}
	if stored != busyYABS {
		t.Fatalf("stored payload differs from the ingested body:\n%s", stored)
	}

	code, page := getPage(t, client, ts.URL+"/servers/1/yabs/1")
	if code != http.StatusOK || !strings.Contains(page, `href="/servers/1/yabs/1/raw"`) {
		t.Fatalf("run page (%d) should link the raw JSON", code)
	}

	resp, err := client.Get(ts.URL + "/servers/1/yabs/1/raw")
	if err != nil {
		t.Fatal(err)
	}
	raw := readBody(t, resp)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || raw != busyYABS {
		t.Fatalf("raw: %d, body differs: %q", resp.StatusCode, raw)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("raw Content-Type = %q", ct)
	}
	if cd := resp.Header.Get("Content-Disposition"); cd != `attachment; filename="yabs-run-1.json"` {
		t.Errorf("raw Content-Disposition = %q", cd)
	}

	// Another server's URL must not reach this run's payload.
	for _, path := range []string{"/servers/2/yabs/1/raw", "/servers/1/yabs/99/raw", "/servers/x/yabs/1/raw"} {
		if code, _ := getPage(t, client, ts.URL+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
	// Signed out: no payload.
	if code, _ := getPage(t, newClient(t), ts.URL+"/servers/1/yabs/1/raw"); code == http.StatusOK {
		t.Errorf("raw payload served without a session")
	}

	// Deleting the run removes its payload with it.
	resp = postForm(t, client, ts, "/servers/1/yabs/1/delete", url.Values{})
	resp.Body.Close()
	var left int
	database.QueryRow("SELECT COUNT(*) FROM yabs_payload").Scan(&left)
	if left != 0 {
		t.Errorf("payload rows after run delete = %d, want 0", left)
	}
}

// A run stored before payloads were kept has none: no link, and the raw URL
// is a 404 rather than an empty download.
func TestYABSRunWithoutPayload(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "legacy-host")
	if _, err := (&model.YABSStore{DB: database}).Create(context.Background(),
		&model.YABS{ServerID: 1, GbSingle: sqlNi(900)}, nil, nil); err != nil {
		t.Fatal(err)
	}
	code, page := getPage(t, client, ts.URL+"/servers/1/yabs/1")
	if code != http.StatusOK || strings.Contains(page, "/raw") {
		t.Fatalf("run page (%d) must not link a payload that is not kept", code)
	}
	if code, _ := getPage(t, client, ts.URL+"/servers/1/yabs/1/raw"); code != http.StatusNotFound {
		t.Fatalf("raw for a run without payload = %d, want 404", code)
	}
}

// An unmeasured speed ("busy", or a missing fio side) is NULL in the
// database, so aggregates skip it, and still reads back and renders.
func TestYABSUnmeasuredStoredAsNull(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "busy-host")
	if code, out := ingestSigned(t, ts, srv, 1, 0, busyYABS); code != http.StatusOK {
		t.Fatalf("ingest: %d %s", code, out)
	}

	var send, recv, latency sql.NullFloat64
	database.QueryRow("SELECT send_mbps, recv_mbps, latency_ms FROM yabs_network_speed WHERE provider = 'Edgoo'").
		Scan(&send, &recv, &latency)
	if send.Valid || recv.Valid || !latency.Valid || latency.Float64 != 190 {
		t.Fatalf("busy row: send=%v recv=%v latency=%v, want NULL/NULL/190", send, recv, latency)
	}
	var read, write sql.NullFloat64
	database.QueryRow("SELECT read_mbps, write_mbps FROM yabs_disk_speed").Scan(&read, &write)
	if !read.Valid || write.Valid {
		t.Fatalf("fio row: read=%v write=%v, want a value and NULL", read, write)
	}
	// The point of NULL: the average is over measured rows only.
	var avg float64
	database.QueryRow("SELECT AVG(send_mbps) FROM yabs_network_speed").Scan(&avg)
	if avg != 683 {
		t.Fatalf("AVG(send_mbps) = %v, want 683 (the busy row must not count as 0)", avg)
	}

	code, page := getPage(t, client, ts.URL+"/servers/1/yabs/1")
	if code != http.StatusOK || !strings.Contains(page, "Sao Paulo, BR (1G)") || !strings.Contains(page, "683") {
		t.Fatalf("run page (%d) must render a run with unmeasured rows", code)
	}
}

// Backup round trip: payloads and unmeasured values survive export + import.
func TestYABSBackupRoundTripPayloadAndNulls(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "rt-payload-host")
	if code, out := ingestSigned(t, ts, srv, 1, 0, busyYABS); code != http.StatusOK {
		t.Fatalf("ingest: %d %s", code, out)
	}

	resp, err := client.Get(ts.URL + "/export/json")
	if err != nil {
		t.Fatal(err)
	}
	exportJSON, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export: %d", resp.StatusCode)
	}

	dbB := freshDB(t)
	sum, err := importer.Import(context.Background(), dbB, bytes.NewReader(exportJSON), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, w := range sum.Warnings {
		if strings.Contains(w, "payload") {
			t.Errorf("unexpected payload warning: %s", w)
		}
	}
	var body string
	if err := dbB.QueryRow("SELECT body FROM yabs_payload").Scan(&body); err != nil || body != busyYABS {
		t.Fatalf("restored payload: err=%v, equal=%v", err, body == busyYABS)
	}
	var nullSend, measured, nullWrite int
	dbB.QueryRow("SELECT COUNT(*) FROM yabs_network_speed WHERE send_mbps IS NULL AND recv_mbps IS NULL").Scan(&nullSend)
	dbB.QueryRow("SELECT COUNT(*) FROM yabs_network_speed WHERE send_mbps = 683").Scan(&measured)
	dbB.QueryRow("SELECT COUNT(*) FROM yabs_disk_speed WHERE write_mbps IS NULL AND read_mbps > 0").Scan(&nullWrite)
	if nullSend != 1 || measured != 1 || nullWrite != 1 {
		t.Fatalf("restored speeds: busy-null=%d measured=%d fio-null=%d, want 1/1/1", nullSend, measured, nullWrite)
	}
}

// The add-run page for a server offers the signed command and a paste form;
// a pasted result is stored exactly like an ingested one.
func TestYABSAddRunByPaste(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	srv.SetBaseURL("https://idler.example.com")
	client := authedClient(t, ts)
	createServer(t, client, ts, "paste-host")

	code, page := getPage(t, client, ts.URL+"/servers/1/yabs/new")
	if code != http.StatusOK {
		t.Fatalf("add-run page: %d", code)
	}
	for _, want := range []string{
		"https://idler.example.com/api/yabs/1?sig=", `action="/servers/1/yabs"`,
		`name="payload"`, `name="csrf_token"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("add-run page missing %q", want)
		}
	}
	// The run lists link to it.
	for _, path := range []string{"/servers/1/yabs", "/servers/1"} {
		if _, p := getPage(t, client, ts.URL+path); !strings.Contains(p, `href="/servers/1/yabs/new"`) {
			t.Errorf("%s should link the add-run page", path)
		}
	}

	// Pasted the way a browser submits a textarea: CRLF and a trailing newline.
	pasted := strings.ReplaceAll(busyYABS, `,"fio"`, ",\r\n\"fio\"") + "\r\n"
	resp := postForm(t, client, ts, "/servers/1/yabs", url.Values{"payload": {pasted}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/servers/1/yabs/1" {
		t.Fatalf("paste: %d → %q, want 303 → /servers/1/yabs/1", resp.StatusCode, resp.Header.Get("Location"))
	}
	var single, netRows, payloads, hasYabs int
	database.QueryRow(`SELECT y.gb_single, (SELECT COUNT(*) FROM yabs_network_speed WHERE yabs_id = y.id),
		(SELECT COUNT(*) FROM yabs_payload WHERE yabs_id = y.id), s.has_yabs
		FROM yabs y JOIN servers s ON s.id = y.server_id`).Scan(&single, &netRows, &payloads, &hasYabs)
	if single != 1102 || netRows != 2 || payloads != 1 || hasYabs != 1 {
		t.Fatalf("pasted run: single=%d net=%d payload=%d has_yabs=%d", single, netRows, payloads, hasYabs)
	}

	// Pasting it again is recognised, not stored twice.
	resp = postForm(t, client, ts, "/servers/1/yabs", url.Values{"payload": {pasted}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/servers/1/yabs" {
		t.Fatalf("duplicate paste: %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	var runs int
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	if runs != 1 {
		t.Fatalf("runs after duplicate paste = %d, want 1", runs)
	}
}

// Line-break normalisation makes a pasted single-line result hash like the
// same result sent by yabs.sh, so adding it both ways stores it once.
func TestYABSPasteThenIngestIsDuplicate(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "both-ways-host")

	resp := postForm(t, client, ts, "/servers/1/yabs", url.Values{"payload": {"  " + busyYABS + "\r\n"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("paste: %d", resp.StatusCode)
	}
	if code, out := ingestSigned(t, ts, srv, 1, 0, busyYABS); code != http.StatusOK || !strings.Contains(out, "duplicate") {
		t.Fatalf("ingest after paste: %d %s, want duplicate", code, out)
	}
	var runs int
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	if runs != 1 {
		t.Fatalf("runs = %d, want 1", runs)
	}
}

// Rejected pastes re-render the form with the reason; nothing is stored.
func TestYABSAddRunRejections(t *testing.T) {
	ts, database, _ := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "reject-host")

	for name, tc := range map[string]struct {
		payload, wantMsg string
		echoed           bool
	}{
		"empty":     {"  \r\n ", "Paste the JSON", false},
		"not json":  {"<b>hello</b> not json", "not a yabs.sh JSON result", true},
		"no data":   {`{"unrelated": true}`, "not a yabs.sh JSON result", true},
		"too large": {`{"cpu":{"model":"` + strings.Repeat("x", model.YABSPayloadMax) + `"}}`, "larger than", false},
	} {
		resp := postForm(t, client, ts, "/servers/1/yabs", url.Values{"payload": {tc.payload}})
		page := readBody(t, resp)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(page, tc.wantMsg) {
			t.Errorf("%s: status %d, want 422 with %q", name, resp.StatusCode, tc.wantMsg)
		}
		if tc.echoed && !strings.Contains(page, "not json") && !strings.Contains(page, "unrelated") {
			t.Errorf("%s: the pasted text should be echoed back", name)
		}
		// Echoed text is escaped, never markup.
		if strings.Contains(page, "<b>hello</b>") {
			t.Errorf("%s: pasted text rendered unescaped", name)
		}
	}
	var runs int
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	if runs != 0 {
		t.Fatalf("runs after rejected pastes = %d, want 0", runs)
	}

	// Unknown server, and a cross-site post without the session's token.
	resp := postForm(t, client, ts, "/servers/99/yabs", url.Values{"payload": {busyYABS}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("unknown server: %d, want 404", resp.StatusCode)
	}
	for _, path := range []string{"/servers/99/yabs/new", "/servers/x/yabs/new"} {
		if code, _ := getPage(t, client, ts.URL+path); code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, code)
		}
	}
	resp = postForm(t, client, ts, "/servers/1/yabs", url.Values{"payload": {busyYABS}, "csrf_token": {"wrong"}})
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad CSRF token: %d, want 403", resp.StatusCode)
	}
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	if runs != 0 {
		t.Fatalf("runs after unauthorised posts = %d, want 0", runs)
	}
}

// /yabs/new asks which server, then hands over to that server's add-run page.
func TestYABSAddRunServerPicker(t *testing.T) {
	ts, _, _ := newTestServerFull(t)
	client := authedClient(t, ts)

	code, page := getPage(t, client, ts.URL+"/yabs/new")
	if code != http.StatusOK || !strings.Contains(page, "No servers yet") {
		t.Fatalf("picker with no servers: %d", code)
	}
	if _, list := getPage(t, client, ts.URL+"/yabs"); !strings.Contains(list, `href="/yabs/new"`) {
		t.Errorf("/yabs should link the add-run page")
	}

	createServer(t, client, ts, "picker-b")
	createServer(t, client, ts, "picker-a")
	_, page = getPage(t, client, ts.URL+"/yabs/new")
	a, b := strings.Index(page, "picker-a"), strings.Index(page, "picker-b")
	if a < 0 || b < 0 || a > b || !strings.Contains(page, `name="server"`) {
		t.Fatalf("picker should list both servers by name (a=%d b=%d)", a, b)
	}

	resp, err := client.Get(ts.URL + "/yabs/new?server=2")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/servers/2/yabs/new" {
		t.Fatalf("picker submit: %d → %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	// Anything that is not a positive id just shows the picker again.
	for _, q := range []string{"abc", "-1", "0", "2/../../x"} {
		if code, _ := getPage(t, client, ts.URL+"/yabs/new?server="+url.QueryEscape(q)); code != http.StatusOK {
			t.Errorf("?server=%s = %d, want the picker (200)", q, code)
		}
	}
}

// With the ingest command withheld (plain http on a routable host), the
// add-run page says how to enable it and still offers the paste form.
func TestYABSAddRunCommandWithheld(t *testing.T) {
	ts, _, srv := newTestServerFull(t)
	srv.SetBaseURL("http://idler.example.com")
	client := authedClient(t, ts)
	createServer(t, client, ts, "withheld-host")
	_, page := getPage(t, client, ts.URL+"/servers/1/yabs/new")
	if strings.Contains(page, "/api/yabs/1?sig=") || !strings.Contains(page, "IDLER_BASE_URL") || !strings.Contains(page, `name="payload"`) {
		t.Fatalf("withheld command: page should show the hint and the paste form only")
	}
}

// A body json.Unmarshal accepts but that is not valid UTF-8 cannot be kept
// byte for byte: the JSON backup would rewrite the invalid bytes, the
// restored copy would no longer match payload_hash, and the raw payload would
// silently vanish after a backup round trip. Such a run is stored without a
// raw copy from the start, so what is kept always survives export + import.
func TestYABSNonUTF8PayloadNotRetained(t *testing.T) {
	ts, database, srv := newTestServerFull(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "latin1-host")

	// 0xE9 is "é" in Latin-1 and an invalid sequence in UTF-8.
	body := "{\"cpu\":{\"model\":\"Caf\xe9 CPU\",\"cores\":2},\"geekbench\":[{\"version\":6,\"single\":900,\"multi\":1700}]}"
	if code, out := ingestSigned(t, ts, srv, 1, 0, body); code != http.StatusOK || !strings.Contains(out, `"ok"`) {
		t.Fatalf("ingest: %d %s", code, out)
	}
	var runs, payloads int
	database.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	database.QueryRow("SELECT COUNT(*) FROM yabs_payload").Scan(&payloads)
	if runs != 1 || payloads != 0 {
		t.Fatalf("runs=%d payloads=%d, want the run stored and no raw copy", runs, payloads)
	}
	if code, page := getPage(t, client, ts.URL+"/servers/1/yabs/1"); code != http.StatusOK || strings.Contains(page, "/raw") {
		t.Fatalf("run page (%d) must not offer a raw copy that was not kept", code)
	}
	// A retained payload (valid UTF-8, here with non-ASCII text) and the
	// unretained one both come through a backup round trip with nothing lost
	// and nothing dropped on the way.
	utf8Body := `{"cpu":{"model":"Café CPU ✓","cores":2},"geekbench":[{"version":6,"single":901,"multi":1701}]}`
	if code, out := ingestSigned(t, ts, srv, 1, 1, utf8Body); code != http.StatusOK || !strings.Contains(out, `"ok"`) {
		t.Fatalf("ingest utf8: %d %s", code, out)
	}
	resp, err := client.Get(ts.URL + "/export/json")
	if err != nil {
		t.Fatal(err)
	}
	exportJSON, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	dbB := freshDB(t)
	sum, err := importer.Import(context.Background(), dbB, bytes.NewReader(exportJSON), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	for _, w := range sum.Warnings {
		if strings.Contains(w, "payload") {
			t.Errorf("a retained payload was dropped on restore: %s", w)
		}
	}
	var restored string
	dbB.QueryRow("SELECT COUNT(*) FROM yabs").Scan(&runs)
	dbB.QueryRow("SELECT COUNT(*) FROM yabs_payload").Scan(&payloads)
	dbB.QueryRow("SELECT body FROM yabs_payload").Scan(&restored)
	if runs != 2 || payloads != 1 || restored != utf8Body {
		t.Fatalf("after restore: runs=%d payloads=%d exact=%v, want 2/1/true", runs, payloads, restored == utf8Body)
	}
}
