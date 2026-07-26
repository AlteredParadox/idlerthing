package importer

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"idlerthing/internal/db"
)

func testDB(t *testing.T) *sql.DB {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if _, err := db.Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return database
}

func TestDetectFormat(t *testing.T) {
	cases := []struct {
		in   string
		want Format
	}{
		{`  [{"hostname":"a"}]`, FormatMyJSON},
		{`{"servers": []}`, FormatNative},
		{"id,hostname,server_type_name\n1,a,KVM\n", FormatMyCSV},
		{"something else entirely\n", FormatUnknown},
	}
	for _, c := range cases {
		got, _, err := DetectFormat(strings.NewReader(c.in))
		if c.want == FormatUnknown {
			if err == nil {
				t.Errorf("%q: expected error", c.in)
			}
			continue
		}
		if err != nil || got != c.want {
			t.Errorf("%q: got %v err %v, want %v", c.in, got, err, c.want)
		}
	}
}

const myJSONFixture = `[
  {
    "id": "R4DxYqPT",
    "hostname": "full-01.example.com",
    "ns1": "ns1.example.com",
    "ns2": null,
    "server_type": 3,
    "cpu": 4,
    "cpu_model": "AMD EPYC 7543P",
    "ram_as_mb": 8192,
    "disk_as_gb": 0,
    "disks": [
      {"disk_size": 62, "disk_unit": "GB", "disk_media": "SSD"},
      {"disk_size": 1, "disk_unit": "TB", "disk_media": "HDD"}
    ],
    "bandwidth": 2000,
    "link_speed": 1000,
    "network_type": "IPv4+IPv6",
    "ssh": 2222,
    "was_promo": 1,
    "transferrable": 1,
    "active": 1,
    "show_public": 1,
    "owned_since": "2025-08-26",
    "os": {"id": "47", "name": "Debian 13"},
    "location": {"id": "92", "name": "Falkenstein, DE"},
    "provider": {"id": "108", "name": "Hetzner"},
    "ips": [
      {"address": "203.0.113.10", "is_ipv4": 1},
      {"address": "2001:db8::1", "is_ipv4": 0}
    ],
    "labels": ["production", "eu", "backup", "web", "fifth"],
    "pricing": {"price": 7, "currency": "USD", "term": 4, "next_due_date": "2026-08-26"},
    "yabs": [{"some": "yabs-data"}]
  },
  {
    "hostname": "null-fields-02.example.com",
    "server_type": 1,
    "network_type": null,
    "transferrable": null,
    "bandwidth": 0,
    "ssh": 22,
    "active": 0,
    "os": null,
    "location": null,
    "provider": null,
    "ips": [],
    "labels": [],
    "pricing": null,
    "disks": [],
    "disk_as_gb": 0
  },
  {
    "hostname": "legacy-disk-03.example.com",
    "server_type": 1,
    "disks": [],
    "disk_as_gb": 250,
    "network_type": "NAT+IPv4",
    "pricing": {"price": 5, "currency": "EUR", "term": 1, "next_due_date": null}
  },
  {
    "hostname": "full-01.example.com",
    "server_type": 1
  }
]`

func TestMyJSONImport(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	records, warnings, err := ParseMyJSON(strings.NewReader(myJSONFixture))
	if err != nil {
		t.Fatalf("ParseMyJSON: %v", err)
	}
	if len(records) != 4 {
		t.Fatalf("expected 4 records, got %d", len(records))
	}

	sum, err := ImportMyIdlers(ctx, database, records, warnings)
	if err != nil {
		t.Fatalf("ImportMyIdlers: %v", err)
	}
	if sum.Imported != 3 || sum.SkippedDup != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	if sum.Providers != 1 || sum.Locations != 1 || sum.OS != 1 {
		t.Fatalf("catalogs: %+v", sum)
	}
	if sum.Labels != 4 || sum.IPs != 2 || sum.Disks != 3 || sum.Pricings != 2 {
		t.Fatalf("relations: %+v", sum)
	}

	// Full record spot-checks.
	var ssh int64
	var ram, bw any
	var netType, osName, provName, locName string
	var active, promo, transferrable, public int
	err = database.QueryRow(`
		SELECT s.ssh_port, s.ram_as_mb, s.bandwidth_as_mb, s.network_type,
			os.name, p.name, l.name, s.active, s.was_promo, s.transferrable, s.show_public
		FROM servers s
		JOIN os ON os.id = s.os_id
		JOIN providers p ON p.id = s.provider_id
		JOIN locations l ON l.id = s.location_id
		WHERE s.hostname = 'full-01.example.com'`).
		Scan(&ssh, &ram, &bw, &netType, &osName, &provName, &locName, &active, &promo, &transferrable, &public)
	if err != nil {
		t.Fatal(err)
	}
	if ssh != 2222 || ram != int64(8192) || bw != int64(2000*1024) {
		t.Fatalf("scalars wrong: %d %v %v", ssh, ram, bw)
	}
	if netType != "IPv4+IPv6" || osName != "Debian 13" || provName != "Hetzner" || locName != "Falkenstein, DE" {
		t.Fatalf("catalogs wrong: %s %s %s %s", netType, osName, provName, locName)
	}
	if active != 1 || promo != 1 || transferrable != 1 || public != 1 {
		t.Fatalf("flags wrong: %d %d %d %d", active, promo, transferrable, public)
	}

	// Disks: GB + TB converted, max-4 labels.
	var diskCount, tbDisk int
	database.QueryRow("SELECT COUNT(*), COALESCE(SUM(size_as_mb = 1048576), 0) FROM server_disks").Scan(&diskCount, &tbDisk)
	if diskCount != 3 || tbDisk != 1 {
		t.Fatalf("disks: %d %d", diskCount, tbDisk)
	}
	var labelCount int
	database.QueryRow("SELECT COUNT(*) FROM labels_assigned").Scan(&labelCount)
	if labelCount != 4 {
		t.Fatalf("labels should cap at 4, got %d", labelCount)
	}

	// IPv6 ip stored as non-v4.
	var v4 int
	database.QueryRow("SELECT is_ipv4 FROM ips WHERE address = '2001:db8::1'").Scan(&v4)
	if v4 != 0 {
		t.Fatal("ipv6 flag wrong")
	}

	// Null-fields record: NULL bandwidth/network_type, no pricing, no catalogs.
	var nullBW, nullNet any
	var pricingCount int
	database.QueryRow("SELECT bandwidth_as_mb, network_type FROM servers WHERE hostname = 'null-fields-02.example.com'").Scan(&nullBW, &nullNet)
	if nullBW != nil || nullNet != nil {
		t.Fatalf("expected NULLs: %v %v", nullBW, nullNet)
	}
	database.QueryRow(`SELECT COUNT(*) FROM pricings p JOIN servers s ON s.id = p.service_id AND p.service_type = 1
		WHERE s.hostname = 'null-fields-02.example.com'`).Scan(&pricingCount)
	if pricingCount != 0 {
		t.Fatal("null pricing should not create a row")
	}

	// Legacy disk fallback + NAT mapping.
	var legacyMB int64
	var natType string
	database.QueryRow(`SELECT sd.size_as_mb, s.network_type FROM servers s
		JOIN server_disks sd ON sd.server_id = s.id
		WHERE s.hostname = 'legacy-disk-03.example.com'`).Scan(&legacyMB, &natType)
	if legacyMB != 250*1024 {
		t.Fatalf("legacy disk fallback: %d", legacyMB)
	}
	if natType != "IPv4 NAT" {
		t.Fatalf("NAT mapping: %s", natType)
	}
}

const myCSVFixture = `id,hostname,server_type,server_type_name,cpu,ram_as_mb,disks,disk_as_gb,bandwidth,ssh,active,network_type,os_name,location_name,provider_name,ips,labels,pricing_price,pricing_currency,pricing_term,pricing_next_due_date
A1,csv-01.example.com,1,KVM,2,4096,"[{""disk_size"":1,""disk_unit"":""TB"",""disk_media"":""NVMe""}]",0,2000,22,1,IPv4 NAT + IPv6,Debian 13,Falkenstein,Hetzner,"[{""address"":""203.0.113.20"",""is_ipv4"":1},{""address"":""2001:db8::5"",""is_ipv4"":0}]","[""prod""]",9.5,EUR,1,2026-09-01
A2,csv-02.example.com,3,DEDI,8,65536,,500,0,2222,0,,,,,,,,
`

func TestMyCSVImport(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	records, _, err := ParseMyCSV(strings.NewReader(myCSVFixture))
	if err != nil {
		t.Fatalf("ParseMyCSV: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	sum, err := ImportMyIdlers(ctx, database, records, nil)
	if err != nil {
		t.Fatalf("ImportMyIdlers: %v", err)
	}
	if sum.Imported != 2 {
		t.Fatalf("summary: %+v", sum)
	}

	var diskMB int64
	var netType string
	var v4 int
	database.QueryRow(`SELECT sd.size_as_mb, s.network_type FROM servers s
		JOIN server_disks sd ON sd.server_id = s.id
		WHERE s.hostname = 'csv-01.example.com'`).Scan(&diskMB, &netType)
	if diskMB != 1024*1024 || netType != "IPv4 NAT + IPv6" {
		t.Fatalf("csv mapping: %d %s", diskMB, netType)
	}
	database.QueryRow("SELECT is_ipv4 FROM ips WHERE address = '2001:db8::5'").Scan(&v4)
	if v4 != 0 {
		t.Fatal("csv ipv6 flag wrong")
	}

	// Legacy disk fallback from CSV + 0 bandwidth NULL.
	var legacyMB int64
	var nullBW any
	database.QueryRow(`SELECT sd.size_as_mb, s.bandwidth_as_mb FROM servers s
		JOIN server_disks sd ON sd.server_id = s.id
		WHERE s.hostname = 'csv-02.example.com'`).Scan(&legacyMB, &nullBW)
	if legacyMB != 500*1024 || nullBW != nil {
		t.Fatalf("csv fallback/unlimited: %d %v", legacyMB, nullBW)
	}
}

func TestMyDedup(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	// Pre-existing hostname.
	database.Exec("INSERT INTO servers (hostname) VALUES ('dup-01.example.com')")

	records, _, err := ParseMyJSON(strings.NewReader(`[
		{"hostname": "dup-01.example.com", "server_type": 1},
		{"hostname": "new-01.example.com", "server_type": 1}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ImportMyIdlers(ctx, database, records, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Imported != 1 || sum.SkippedDup != 1 {
		t.Fatalf("first run: %+v", sum)
	}

	// Second run: everything skipped.
	records, _, _ = ParseMyJSON(strings.NewReader(`[
		{"hostname": "new-01.example.com", "server_type": 1}
	]`))
	sum, err = ImportMyIdlers(ctx, database, records, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Imported != 0 || sum.SkippedDup != 1 {
		t.Fatalf("second run: %+v", sum)
	}
	var count int
	database.QueryRow("SELECT COUNT(*) FROM servers WHERE hostname = 'new-01.example.com'").Scan(&count)
	if count != 1 {
		t.Fatalf("expected 1 row, got %d", count)
	}
}

func TestMyRowFailureTolerance(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	// One record has the SAME ip twice: the second ip insert violates
	// UNIQUE(service_id, service_type, address) → the row is skipped with
	// a warning, the transaction stays usable, later rows still import.
	records, _, err := ParseMyJSON(strings.NewReader(`[
		{"hostname": "ok-01.example.com", "server_type": 1,
		 "pricing": {"price": 5, "currency": "USD", "term": 1}},
		{"hostname": "bad-02.example.com", "server_type": 1,
		 "ips": [{"address": "203.0.113.99", "is_ipv4": 1},
		         {"address": "203.0.113.99", "is_ipv4": 1}]},
		{"hostname": "ok-03.example.com", "server_type": 1}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ImportMyIdlers(ctx, database, records, nil)
	if err != nil {
		t.Fatalf("ImportMyIdlers should not abort: %v", err)
	}
	if sum.Imported != 2 {
		t.Fatalf("expected 2 imports (1 warned row), got %d: %+v", sum.Imported, sum)
	}
	if len(sum.Warnings) != 1 {
		t.Fatalf("expected 1 warning, got %v", sum.Warnings)
	}
	var count int
	database.QueryRow("SELECT COUNT(*) FROM servers").Scan(&count)
	if count != 2 {
		t.Fatalf("expected 2 servers committed, got %d", count)
	}
}

// #6 — date normalization forms.
func TestNormDate(t *testing.T) {
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"2026-07-25", "2026-07-25", false},
		{"2026-07-25T10:30:00Z", "2026-07-25", false},
		{"", "", false},
		{"2026-1-2", "", true},
		{"garbage", "", true},
		{"25/07/2026", "", true},
	}
	for _, c := range cases {
		got, ok := normDate(c.in)
		if c.wantErr {
			if ok {
				t.Errorf("%q: expected invalid, got %q", c.in, got)
			}
			continue
		}
		if !ok || got != c.want {
			t.Errorf("%q: got %q ok=%v, want %q", c.in, got, ok, c.want)
		}
	}
}
