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
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"idlerthing/internal/model"
)

// apiJSON sends a JSON body with the Bearer token and decodes the reply.
func apiJSON(t *testing.T, ts *httptest.Server, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// Batch U1 — an explicit 0 survives a GET → PUT round-trip. ptrToNull used
// to map 0 to NULL, and for bandwidth NULL means UNLIMITED: a metered
// 0-bandwidth plan re-saved through the API became "∞".
func TestAPIZeroIsNotNull(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)

	status, created := apiJSON(t, ts, "POST", "/api/servers", token,
		`{"hostname": "zero-host", "bandwidth_as_mb": 0, "ssh_port": 0, "cpu": 0, "cpu_model": "   "}`)
	if status != http.StatusCreated {
		t.Fatalf("create: %d %v", status, created)
	}
	data := created["data"].(map[string]any)
	for _, k := range []string{"bandwidth_as_mb", "ssh_port", "cpu"} {
		if v, ok := data[k].(float64); !ok || v != 0 {
			t.Errorf("create: %s = %v, want 0", k, data[k])
		}
	}
	if data["cpu_model"] != nil {
		t.Errorf("blank cpu_model must be NULL, got %v", data["cpu_model"])
	}

	// PUT the GET payload back unchanged.
	_, got := apiGet(t, ts, "/api/servers/1", token)
	round, _ := json.Marshal(got["data"])
	status, updated := apiJSON(t, ts, "PUT", "/api/servers/1", token, string(round))
	if status != http.StatusOK {
		t.Fatalf("round-trip PUT: %d %v", status, updated)
	}
	var bw, port, cpu sql.NullInt64
	if err := database.QueryRow("SELECT bandwidth_as_mb, ssh_port, cpu FROM servers WHERE id = 1").Scan(&bw, &port, &cpu); err != nil {
		t.Fatal(err)
	}
	if !bw.Valid || bw.Int64 != 0 || !port.Valid || port.Int64 != 0 || !cpu.Valid || cpu.Int64 != 0 {
		t.Fatalf("round-trip turned 0 into NULL: bw=%v port=%v cpu=%v", bw, port, cpu)
	}

	// The list view renders 0 MB, not the unlimited glyph.
	client := authedClient(t, ts)
	resp, err := client.Get(ts.URL + "/servers")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "0 MB") {
		t.Fatal("servers list should show the metered 0 MB bandwidth")
	}
}

// Batch U2 — dangling catalog ids are a 422 with the offending field, not
// an FK-violation 500; and the disk count is capped like the form.
func TestAPIServerValidationRefsAndDisks(t *testing.T) {
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)

	status, body := apiJSON(t, ts, "POST", "/api/servers", token,
		`{"hostname": "refs", "os_id": 999, "provider_id": 998, "location_id": 997}`)
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("dangling refs: %d %v", status, body)
	}
	fields, _ := body["fields"].(map[string]any)
	for _, f := range []string{"os_id", "provider_id", "location_id"} {
		if fields[f] == nil {
			t.Errorf("expected a field error for %s, got %v", f, fields)
		}
	}

	// A real catalog entry passes.
	client := authedClient(t, ts)
	postForm(t, client, ts, "/catalogs/os", url.Values{"name": {"Debian 13"}}).Body.Close()
	status, body = apiJSON(t, ts, "POST", "/api/servers", token, `{"hostname": "refs-ok", "os_id": 1}`)
	if status != http.StatusCreated {
		t.Fatalf("valid ref: %d %v", status, body)
	}

	var disks bytes.Buffer
	disks.WriteString(`{"hostname": "many-disks", "disks": [`)
	for i := 0; i < 5; i++ {
		if i > 0 {
			disks.WriteString(",")
		}
		disks.WriteString(`{"size_as_mb": 1024, "media": "SSD"}`)
	}
	disks.WriteString(`]}`)
	status, body = apiJSON(t, ts, "POST", "/api/servers", token, disks.String())
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("5 disks: %d %v", status, body)
	}
	if fields, _ := body["fields"].(map[string]any); fields["disks"] == nil {
		t.Fatalf("expected a disks field error, got %v", body)
	}
}

// Batch U3 — reflection-derived JSON keys match the DB columns and the
// import format: is_ipv4 (not is_i_pv4) and ip_id (not ipid).
func TestAPIJSONFieldNames(t *testing.T) {
	ip := flatten(model.IP{Address: "203.0.113.1", IsIPv4: true}).(map[string]any)
	if _, ok := ip["is_ipv4"]; !ok {
		t.Fatalf("IP.IsIPv4 should flatten to is_ipv4, got keys %v", keysOf(ip))
	}
	if _, ok := ip["is_i_pv4"]; ok {
		t.Fatal("is_i_pv4 must be gone")
	}
	note := flatten(model.Note{IPID: sql.NullInt64{Int64: 3, Valid: true}}).(map[string]any)
	if v, ok := note["ip_id"]; !ok || v != int64(3) {
		t.Fatalf("Note.IPID should flatten to ip_id=3, got %v", note)
	}
	if _, ok := note["ipid"]; ok {
		t.Fatal("ipid must be gone")
	}

	// End to end: the IPs endpoint and the export agree.
	ts, database := newTestServer(t)
	token := setAPIToken(t, database)
	client := authedClient(t, ts)
	createServer(t, client, ts, "names-host")
	postForm(t, client, ts, "/ips", url.Values{"service_id": {"1"}, "service_type": {"1"}, "address": {"203.0.113.7"}}).Body.Close()
	_, body := apiGet(t, ts, "/api/ips", token)
	row := body["data"].([]any)[0].(map[string]any)["ip"].(map[string]any)
	if row["is_ipv4"] != true {
		t.Fatalf("/api/ips row should carry is_ipv4=true, got %v", row)
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
