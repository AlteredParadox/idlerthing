package web

import (
	"bytes"
	"context"
	"encoding/json"
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

// exportJSONDoc fetches an export endpoint and decodes the document.
func exportJSONDoc(t *testing.T, ts *httptest.Server, client *http.Client, path string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest("GET", ts.URL+path, nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("export %s: %d", path, resp.StatusCode)
	}
	var doc map[string]any
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("export %s: invalid JSON: %v", path, err)
	}
	return doc
}

// Batch L D2 — DNS links: at most one parent, and it must exist.
func TestDNSParentValidation(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "dns-parent")
	resp := postForm(t, client, ts, "/domains", url.Values{
		"domain": {"dns-parent.example.com"}, "active": {"on"},
	})
	resp.Body.Close()
	// Drain flashes.
	if resp, err := client.Get(ts.URL + "/dns"); err == nil {
		resp.Body.Close()
	}

	// Two parents → rejected with a field error.
	resp = postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"a.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.1"},
		"server_id": {"1"}, "domain_id": {"1"},
	})
	resp.Body.Close()
	resp, err := client.Get(ts.URL + "/dns")
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Link at most one service") {
		t.Fatalf("two parents should be rejected: %s", body)
	}

	// Bogus parent id → rejected.
	resp = postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"b.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.2"},
		"server_id": {"999"},
	})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/dns")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "Linked service does not exist") {
		t.Fatalf("bogus parent should be rejected: %s", body)
	}

	// One valid parent works.
	resp = postForm(t, client, ts, "/dns", url.Values{
		"hostname": {"c.example.com"}, "dns_type": {"A"}, "address": {"203.0.113.3"},
		"server_id": {"1"},
	})
	resp.Body.Close()
	resp, err = client.Get(ts.URL + "/dns")
	if err != nil {
		t.Fatal(err)
	}
	body = readBody(t, resp)
	resp.Body.Close()
	if !strings.Contains(body, "c.example.com") {
		t.Fatal("valid single-parent record should be created")
	}
}

// Batch L D2 — the importer warns when a dns parent id can't be resolved.
func TestImportDNSParentWarning(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{
		"dns": [{"dns_record": {"hostname": "orphan.example.com", "dns_type": "A",
			"address": "203.0.113.9", "server_id": 7}}]
	}`
	summary, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.DNS != 1 {
		t.Fatalf("record should import with NULL parent: %+v", summary)
	}
	found := false
	for _, w := range summary.Warnings {
		if strings.Contains(w, "server_id=7") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a parent warning, got %v", summary.Warnings)
	}
	var sid any
	dbB.QueryRow("SELECT server_id FROM dns").Scan(&sid)
	if sid != nil {
		t.Fatalf("unresolvable parent must be NULL, got %v", sid)
	}
}

// Batch L D3 / Batch M F2 — cap rows older than the 2h window are pruned
// by PruneCaps (called from the login sweep, not the ingest hot path).
func TestYABSCapPruning(t *testing.T) {
	_, database, _ := newTestServerFull(t)
	ctx := context.Background()

	old := time.Now().Add(-3 * time.Hour).Unix()
	recent := time.Now().Add(-30 * time.Minute).Unix()
	if _, err := database.Exec(
		"INSERT INTO yabs_caps (server_id, ts, consumed_at) VALUES (1, ?, '2020-01-01'), (99, ?, '2020-01-01')",
		old, recent); err != nil {
		t.Fatal(err)
	}

	(&model.YABSStore{DB: database}).PruneCaps(ctx)

	var oldN, recentN int
	database.QueryRow("SELECT COUNT(*) FROM yabs_caps WHERE ts = ?", old).Scan(&oldN)
	database.QueryRow("SELECT COUNT(*) FROM yabs_caps WHERE ts = ?", recent).Scan(&recentN)
	if oldN != 0 {
		t.Fatal("cap row past the window should be pruned")
	}
	if recentN != 1 {
		t.Fatal("recent cap rows must be kept")
	}
}

// Batch L D4 — importer enum validation: server_type, dns_type, disk media.
func TestImportEnumValidation(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{
		"servers": [{"server": {"id": 1, "hostname": "enum-01", "server_type": 99, "active": true},
			"disks": [{"size_as_mb": 1024, "media": "TAPE"}]}],
		"dns": [{"dns_record": {"hostname": "enum.example.com", "dns_type": "BOGUS", "address": "203.0.113.5"}}]
	}`
	summary, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Servers != 1 || summary.Disks != 1 || summary.DNS != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	if len(summary.Warnings) != 3 {
		t.Fatalf("expected 3 warnings, got %v", summary.Warnings)
	}
	var st int
	var mediaS, dtS string
	dbB.QueryRow("SELECT server_type FROM servers").Scan(&st)
	dbB.QueryRow("SELECT media FROM server_disks").Scan(&mediaS)
	dbB.QueryRow("SELECT dns_type FROM dns").Scan(&dtS)
	if st != model.TypeKVM {
		t.Fatalf("server_type 99 should fall back to KVM, got %d", st)
	}
	if mediaS != "SSD" {
		t.Fatalf("media TAPE should fall back to SSD, got %q", mediaS)
	}
	if dtS != "A" {
		t.Fatalf("dns_type BOGUS should fall back to A, got %q", dtS)
	}
}

// Batch L D5 / Batch M F5 — the --force guard also covers dns/notes/ips,
// and the refusal names the blocking tables.
func TestImportGuardCoversContentTables(t *testing.T) {
	dbB := freshDB(t)
	if _, err := dbB.Exec(
		"INSERT INTO dns (hostname, dns_type, address) VALUES ('g.example.com', 'A', '203.0.113.1')"); err != nil {
		t.Fatal(err)
	}
	_, err := importer.Import(context.Background(), dbB, strings.NewReader(`{}`), false)
	if err == nil {
		t.Fatal("import into a DB with only a dns row must be refused without --force")
	}
	if !strings.Contains(err.Error(), "dns: 1 rows") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("refusal should name the blocking table and the remedy: %v", err)
	}
	if _, err := importer.Import(context.Background(), dbB, strings.NewReader(`{}`), true); err != nil {
		t.Fatalf("with --force it proceeds: %v", err)
	}
}

// Batch L D6 — per-type exports are stamped partial; the importer warns.
func TestPartialExportMarking(t *testing.T) {
	ts, _ := newTestServer(t)
	client := authedClient(t, ts)
	createServer(t, client, ts, "partial-01")

	partial := exportJSONDoc(t, ts, client, "/export/json/servers")
	if partial["partial"] != true {
		t.Fatalf("per-type export should be partial: %v", partial["partial"])
	}
	raw, _ := json.Marshal(partial)
	summary, err := importer.Import(context.Background(), freshDB(t), bytes.NewReader(raw), false)
	if err != nil {
		t.Fatalf("Import partial: %v", err)
	}
	found := false
	for _, w := range summary.Warnings {
		if strings.Contains(w, "partial export") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the partial warning, got %v", summary.Warnings)
	}

	full := exportJSONDoc(t, ts, client, "/export/json")
	if full["partial"] != false {
		t.Fatalf("full export should be partial=false: %v", full["partial"])
	}
	raw, _ = json.Marshal(full)
	summary, err = importer.Import(context.Background(), freshDB(t), bytes.NewReader(raw), false)
	if err != nil {
		t.Fatalf("Import full: %v", err)
	}
	for _, w := range summary.Warnings {
		if strings.Contains(w, "partial export") {
			t.Fatalf("full export must not trigger the partial warning: %v", summary.Warnings)
		}
	}
}

// Batch L P3 — batched export inlines children for EVERY row (equivalence
// with the old per-row fetch shape).
func TestExportBatchedChildren(t *testing.T) {
	ts, dbA := newTestServer(t)
	ctx := context.Background()

	st := &model.ServerStore{DB: dbA}
	labels := &model.LabelStore{DB: dbA}
	labelID, _ := labels.FindOrCreate(ctx, "prod")
	ips := &model.IPStore{DB: dbA}
	yabsSt := &model.YABSStore{DB: dbA}
	for i, h := range []string{"b-01", "b-02"} {
		id, err := st.Create(ctx, &model.Server{Hostname: h, ServerType: model.TypeKVM, Active: true},
			[]model.ServerDisk{{SizeAsMB: int64(1024 * (i + 1)), Media: "NVMe"}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		labels.Assign(ctx, labelID, id, model.ServiceServer)
		ips.Create(ctx, &model.IP{ServiceID: id, ServiceType: model.ServiceServer,
			Address: fmt.Sprintf("203.0.113.%d", i+1), IsIPv4: true})
		if _, err := yabsSt.Create(ctx, &model.YABS{ServerID: id},
			[]model.YABSDiskSpeed{{BlockSize: "4k", ReadMbps: 88, WriteMbps: 90}},
			[]model.YABSNetworkSpeed{{Location: "FRA", SendMbps: 900}}); err != nil {
			t.Fatal(err)
		}
	}

	doc := exportJSONDoc(t, ts, authedClient(t, ts), "/export/json")
	servers := doc["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("servers: %d", len(servers))
	}
	for _, s := range servers {
		m := s.(map[string]any)
		if len(m["disks"].([]any)) != 1 || len(m["labels"].([]any)) != 1 || len(m["ips"].([]any)) != 1 {
			t.Fatalf("children not inlined: %v", m)
		}
	}
	runs := doc["yabs"].([]any)
	if len(runs) != 2 {
		t.Fatalf("yabs runs: %d", len(runs))
	}
	for _, y := range runs {
		m := y.(map[string]any)
		if len(m["disk_speed"].([]any)) != 1 || len(m["network_speed"].([]any)) != 1 {
			t.Fatalf("speed rows not inlined: %v", m)
		}
	}
}

// Batch L P4 — sidebar counts are cached by the dashboard generation and
// refreshed after a write.
func TestCountsCachedByGeneration(t *testing.T) {
	_, _, srv := newTestServerFull(t)
	req := httptest.NewRequest("GET", "/", nil)

	c1 := srv.counts(req)
	srv.dash.mu.Lock()
	cached := srv.dash.countsOK
	srv.dash.mu.Unlock()
	if !cached {
		t.Fatal("counts should be cached after the first call")
	}
	c2 := srv.counts(req)
	if c2 != c1 {
		t.Fatal("cached counts must be stable within a generation")
	}

	// A write bumps the generation → next call re-queries.
	if _, err := (&model.ServerStore{DB: srv.db}).Create(t.Context(),
		&model.Server{Hostname: "cnt-01", ServerType: model.TypeKVM, Active: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	srv.touchDashboard()
	c3 := srv.counts(req)
	if c3.Servers != c1.Servers+1 {
		t.Fatalf("counts should refresh after a write: %d → %d", c1.Servers, c3.Servers)
	}
}
