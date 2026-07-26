package web

import (
	"bytes"
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"idlerthing/internal/db"
	"idlerthing/internal/importer"
	"idlerthing/internal/model"
)

func freshDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(database); err != nil {
		t.Fatal(err)
	}
	return database
}

func TestImportRoundTrip(t *testing.T) {
	// DB A: seed via the model stores.
	ts, dbA := newTestServer(t)
	ctx := context.Background()
	cat := &model.CatalogStore{DB: dbA}
	provID, _ := cat.Create(ctx, model.Catalogs["providers"], "Hetzner")
	locID, _ := cat.Create(ctx, model.Catalogs["locations"], "Falkenstein")
	osID, _ := cat.Create(ctx, model.Catalogs["os"], "Debian 12")
	ref := func(i int64) sql.NullInt64 { return sql.NullInt64{Int64: i, Valid: true} }
	str := func(s string) sql.NullString { return sql.NullString{String: s, Valid: true} }

	st := &model.ServerStore{DB: dbA}
	srvID, err := st.Create(ctx, &model.Server{
		Hostname: "rt-srv-01", ServerType: model.TypeKVM, OsID: ref(osID),
		ProviderID: ref(provID), LocationID: ref(locID),
		RamAsMB: ref(4096), Active: true,
	}, []model.ServerDisk{{SizeAsMB: 80 * 1024, Media: "NVMe"}},
		&model.Pricing{Currency: "EUR", Price: 10, Term: model.TermMonthly, NextDueDate: str("2027-01-01")})
	if err != nil {
		t.Fatal(err)
	}
	labels := &model.LabelStore{DB: dbA}
	labelID, _ := labels.FindOrCreate(ctx, "production")
	labels.Assign(ctx, labelID, srvID, model.ServiceServer)
	ips := &model.IPStore{DB: dbA}
	ips.Create(ctx, &model.IP{ServiceID: srvID, ServiceType: model.ServiceServer, Address: "203.0.113.7", IsIPv4: true})
	notes := &model.NoteStore{DB: dbA}
	notes.Create(ctx, &model.Note{ServiceID: ref(srvID), ServiceType: ref(model.ServiceServer), Body: "round-trip note"})
	dnsSt := &model.DNSStore{DB: dbA}
	dnsSt.Create(ctx, &model.DNSRecord{Hostname: "rt.example.com", DNSType: "A", Address: "203.0.113.7", ServerID: ref(srvID)})
	sharedSt := &model.SharedStore{DB: dbA}
	sharedSt.Create(ctx, &model.SharedHosting{MainDomain: "rt-host.example.com", ProviderID: ref(provID), Active: true},
		&model.Pricing{Currency: "USD", Price: 5, Term: model.TermMonthly})

	// Export A via the real endpoint.
	client := authedClient(t, ts)
	req, _ := http.NewRequest("GET", ts.URL+"/export/json", nil)
	for _, c := range client.Jar.Cookies(mustURL(t, ts.URL)) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	exportJSON, _ := io.ReadAll(resp.Body)

	// Import into fresh DB B.
	dbB := freshDB(t)
	summary, err := importer.Import(ctx, dbB, bytes.NewReader(exportJSON), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Servers != 1 || summary.Shared != 1 || summary.Disks != 1 ||
		summary.IPs != 1 || summary.DNS != 1 || summary.Notes != 1 || summary.Pricings != 2 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	// Spot-check fields survived the round-trip.
	var hostname, osName, provName, locName string
	var ramMB, diskMB int64
	err = dbB.QueryRow(`
		SELECT s.hostname, os.name, p.name, l.name, s.ram_as_mb,
			(SELECT SUM(size_as_mb) FROM server_disks d WHERE d.server_id = s.id)
		FROM servers s
		JOIN os ON os.id = s.os_id
		JOIN providers p ON p.id = s.provider_id
		JOIN locations l ON l.id = s.location_id`).
		Scan(&hostname, &osName, &provName, &locName, &ramMB, &diskMB)
	if err != nil {
		t.Fatal(err)
	}
	if hostname != "rt-srv-01" || osName != "Debian 12" || provName != "Hetzner" ||
		locName != "Falkenstein" || ramMB != 4096 || diskMB != 80*1024 {
		t.Fatalf("server round-trip mismatch: %s %s %s %s %d %d", hostname, osName, provName, locName, ramMB, diskMB)
	}

	var currency string
	var price float64
	dbB.QueryRow("SELECT currency, price FROM pricings WHERE service_type = 1").Scan(&currency, &price)
	if currency != "EUR" || price != 10 {
		t.Fatalf("pricing mismatch: %s %v", currency, price)
	}

	var label string
	dbB.QueryRow(`
		SELECT l.label FROM labels l JOIN labels_assigned a ON a.label_id = l.id
		WHERE a.service_type = 1`).Scan(&label)
	if label != "production" {
		t.Fatalf("label assignment lost: %q", label)
	}

	var ipAddr, dnsHost, noteBody string
	dbB.QueryRow("SELECT address FROM ips").Scan(&ipAddr)
	dbB.QueryRow("SELECT hostname FROM dns WHERE server_id IS NOT NULL").Scan(&dnsHost)
	dbB.QueryRow("SELECT body FROM notes").Scan(&noteBody)
	if ipAddr != "203.0.113.7" || dnsHost != "rt.example.com" || noteBody != "round-trip note" {
		t.Fatalf("relations mismatch: %s %s %s", ipAddr, dnsHost, noteBody)
	}

	// Re-import without force must refuse (tables now non-empty).
	if _, err := importer.Import(ctx, dbB, bytes.NewReader(exportJSON), false); err == nil {
		t.Fatal("expected refusal on non-empty DB without force")
	}
	// With force it proceeds (duplicating).
	if _, err := importer.Import(ctx, dbB, bytes.NewReader(exportJSON), true); err != nil {
		t.Fatalf("force import: %v", err)
	}
	var serverCount int
	dbB.QueryRow("SELECT COUNT(*) FROM servers").Scan(&serverCount)
	if serverCount != 2 {
		t.Fatalf("expected duplicated services, got %d", serverCount)
	}
}

func TestImportGarbage(t *testing.T) {
	dbB := freshDB(t)
	if _, err := importer.Import(context.Background(), dbB, bytes.NewReader([]byte("{nope")), false); err == nil {
		t.Fatal("expected decode error")
	}
	// Empty document imports cleanly as a no-op.
	summary, err := importer.Import(context.Background(), dbB, bytes.NewReader([]byte("{}")), false)
	if err != nil {
		t.Fatalf("empty doc: %v", err)
	}
	if summary.Servers != 0 || summary.Shared != 0 || summary.Domains != 0 ||
		summary.Pricings != 0 || len(summary.Warnings) != 0 {
		t.Fatalf("empty doc should be a no-op: %+v", summary)
	}
}

// TestImportOutOfRangeServiceType feeds ips/notes with service_type 9 —
// must not panic, rows are skipped with warnings, good rows import.
func TestImportOutOfRangeServiceType(t *testing.T) {
	dbB := freshDB(t)
	fixture := `{
		"servers": [{"server": {"id": 1, "hostname": "ok-01", "server_type": 1, "active": true}}],
		"ips": [
			{"ip": {"service_id": 1, "service_type": 1, "address": "203.0.113.10", "is_ipv4": true}},
			{"ip": {"service_id": 1, "service_type": 9, "address": "203.0.113.99", "is_ipv4": true}}
		],
		"notes": [
			{"note": {"service_id": 1, "service_type": 1, "body": "good note"}},
			{"note": {"service_id": 1, "service_type": 9, "body": "bad note"}}
		]
	}`
	summary, err := importer.Import(context.Background(), dbB, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	if summary.Servers != 1 || summary.IPs != 1 || summary.Notes != 1 {
		t.Fatalf("good rows should import: %+v", summary)
	}
	if len(summary.Warnings) != 2 {
		t.Fatalf("expected 2 warnings (ip + note), got %v", summary.Warnings)
	}
	var badIP, badNote int
	dbB.QueryRow("SELECT COUNT(*) FROM ips WHERE address = '203.0.113.99'").Scan(&badIP)
	dbB.QueryRow("SELECT COUNT(*) FROM notes WHERE body = 'bad note'").Scan(&badNote)
	if badIP != 0 || badNote != 0 {
		t.Fatalf("out-of-range rows must be skipped: ip=%d note=%d", badIP, badNote)
	}
}
