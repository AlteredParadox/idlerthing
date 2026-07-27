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

// Batch N M1 — a record listing the same IP twice imports anyway (IP kept
// once, real warning) instead of being discarded by the UNIQUE constraint.
func TestMyRowFailureTolerance(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()

	records, parseWarnings, err := ParseMyJSON(strings.NewReader(`[
		{"hostname": "ok-01.example.com", "server_type": 1,
		 "pricing": {"price": 5, "currency": "USD", "term": 1}},
		{"hostname": "dup-02.example.com", "server_type": 1,
		 "ips": [{"address": "203.0.113.99", "is_ipv4": 1},
		         {"address": "203.0.113.99", "is_ipv4": 1},
		         {"address": "2001:db8::99", "is_ipv4": 0}]},
		{"hostname": "ok-03.example.com", "server_type": 1}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ImportMyIdlers(ctx, database, records, parseWarnings)
	if err != nil {
		t.Fatalf("ImportMyIdlers should not abort: %v", err)
	}
	if sum.Imported != 3 {
		t.Fatalf("expected all 3 imports, got %d: %+v", sum.Imported, sum)
	}
	if len(sum.Warnings) != 1 || !strings.Contains(sum.Warnings[0], "duplicate IP 203.0.113.99") {
		t.Fatalf("expected the duplicate-IP warning, got %v", sum.Warnings)
	}
	// The duplicated address is stored once; the distinct v6 survives too.
	var ipCount int
	database.QueryRow(`SELECT COUNT(*) FROM ips i JOIN servers s ON s.id = i.service_id
		WHERE s.hostname = 'dup-02.example.com'`).Scan(&ipCount)
	if ipCount != 2 {
		t.Fatalf("expected 2 IPs (dup kept once + distinct v6), got %d", ipCount)
	}
	var count int
	database.QueryRow("SELECT COUNT(*) FROM servers").Scan(&count)
	if count != 3 {
		t.Fatalf("expected 3 servers committed, got %d", count)
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

// Batch J #6 — numeric corruption guards: huge floats → absent, not
// implementation-defined negative ints.
func TestMyJSONImplausibleNumbers(t *testing.T) {
	recs, warnings, err := ParseMyJSON(strings.NewReader(`[
	  {"hostname": "huge-bw", "bandwidth": 1e300, "active": 1},
	  {"hostname": "huge-disk", "disk_as_gb": 1e300, "active": 1},
	  {"hostname": "huge-tb-disk", "disks": [{"disk_size": 1e300, "disk_unit": "TB", "disk_media": "SSD"}], "active": 1},
	  {"hostname": "v6-flag", "ips": [{"address": "2001:db8::5", "is_ipv4": 1}], "active": 1}
	]`))
	if err != nil {
		t.Fatalf("ParseMyJSON: %v", err)
	}
	if len(recs) != 4 {
		t.Fatalf("records: %d", len(recs))
	}
	if recs[0].BandwidthAsMB != nil {
		t.Fatalf("1e300 bandwidth must be absent, got %d", *recs[0].BandwidthAsMB)
	}
	if len(recs[1].Disks) != 0 || len(recs[2].Disks) != 0 {
		t.Fatalf("1e300 disk sizes must be skipped: %+v %+v", recs[1].Disks, recs[2].Disks)
	}
	// The file's is_ipv4=1 flag contradicts the v6 address — address wins.
	if len(recs[3].IPs) != 1 || recs[3].IPs[0].IsIPv4 {
		t.Fatalf("v6 address with is_ipv4=1 must stay v6: %+v", recs[3].IPs)
	}
	if len(warnings) < 3 {
		t.Fatalf("expected ≥3 warnings (bw + 2 disks), got %v", warnings)
	}

	// Imported rows: NULL bandwidth, no negative garbage.
	database := testDB(t)
	sum, err := ImportMyIdlers(context.Background(), database, recs, warnings)
	if err != nil {
		t.Fatalf("ImportMyIdlers: %v", err)
	}
	if sum.Imported != 4 {
		t.Fatalf("imported: %+v", sum)
	}
	var bw any
	database.QueryRow("SELECT bandwidth_as_mb FROM servers WHERE hostname = 'huge-bw'").Scan(&bw)
	if bw != nil {
		t.Fatalf("bandwidth should be NULL, got %v", bw)
	}
	var is4 int
	database.QueryRow("SELECT is_ipv4 FROM ips WHERE address = '2001:db8::5'").Scan(&is4)
	if is4 != 0 {
		t.Fatalf("v6 stored as v4: %d", is4)
	}
}

func TestMyCSVGuards(t *testing.T) {
	csvDoc := "hostname,server_type_name,bandwidth,disk_as_gb,pricing_price,pricing_currency,pricing_term,ips\n" +
		`nan-bw,KVM,NaN,,,,,` + "\n" +
		`inf-disk,KVM,,+Inf,,,,` + "\n" +
		`lower-cur,KVM,,,5,eur,1,` + "\n" +
		`bad-cur,KVM,,,5,U1,1,` + "\n" +
		`v6-flag,KVM,,,,,,"[{""address"": ""2001:db8::9"", ""is_ipv4"": 1}]"` + "\n"
	recs, warnings, err := ParseMyCSV(strings.NewReader(csvDoc))
	if err != nil {
		t.Fatalf("ParseMyCSV: %v", err)
	}
	if len(recs) != 5 {
		t.Fatalf("records: %d", len(recs))
	}
	if recs[0].BandwidthAsMB != nil {
		t.Fatalf("NaN bandwidth must be absent, got %d", *recs[0].BandwidthAsMB)
	}
	if len(recs[1].Disks) != 0 {
		t.Fatalf("+Inf disk must be skipped: %+v", recs[1].Disks)
	}
	// CSV currency goes through the same normCurrency as JSON.
	if recs[2].Pricing == nil || recs[2].Pricing.Currency != "EUR" {
		t.Fatalf("lowercase currency should normalize: %+v", recs[2].Pricing)
	}
	if recs[3].Pricing != nil {
		t.Fatalf("invalid currency must skip pricing: %+v", recs[3].Pricing)
	}
	if len(recs[4].IPs) != 1 || recs[4].IPs[0].IsIPv4 {
		t.Fatalf("v6 with is_ipv4=1 must stay v6: %+v", recs[4].IPs)
	}
	_ = warnings
}

// Batch N M2 — an out-of-range pricing term skips the pricing with a
// warning (never silently clamped to monthly).
func TestMyPricingTermSkipped(t *testing.T) {
	recs, warnings, err := ParseMyJSON(strings.NewReader(`[
		{"hostname": "tri.example.com",
		 "pricing": {"price": 100, "currency": "USD", "term": 99}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].Pricing != nil {
		t.Fatalf("term 99 must skip the pricing, got %+v", recs[0].Pricing)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "term 99 out of range") {
		t.Fatalf("expected the term warning, got %v", warnings)
	}

	database := testDB(t)
	sum, err := ImportMyIdlers(context.Background(), database, recs, warnings)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Pricings != 0 {
		t.Fatalf("no pricing may be attached: %+v", sum)
	}
}

// Batch N M3 — cpu/ram/link_speed/ssh_port bounded in both my-idlers paths.
func TestMyBounds(t *testing.T) {
	recs, warnings, err := ParseMyJSON(strings.NewReader(`[
		{"hostname": "huge-cpu", "cpu": 999999999, "ram_as_mb": 2048, "ssh": 22}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if recs[0].CPU != nil {
		t.Fatalf("cpu 999999999 must be absent, got %d", *recs[0].CPU)
	}
	if recs[0].RamAsMB == nil || *recs[0].RamAsMB != 2048 || recs[0].SSHPort == nil || *recs[0].SSHPort != 22 {
		t.Fatal("valid values must pass through")
	}
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "cpu 999999999 out of range") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the cpu warning, got %v", warnings)
	}

	csvDoc := "hostname,server_type_name,cpu,ram_as_mb\nhuge-cpu-csv,KVM,999999999,4096\n"
	csvRecs, csvWarnings, err := ParseMyCSV(strings.NewReader(csvDoc))
	if err != nil {
		t.Fatal(err)
	}
	if csvRecs[0].CPU != nil {
		t.Fatalf("CSV cpu 999999999 must be absent, got %d", *csvRecs[0].CPU)
	}
	if csvRecs[0].RamAsMB == nil || *csvRecs[0].RamAsMB != 4096 {
		t.Fatal("valid CSV values must pass through")
	}
	found = false
	for _, w := range csvWarnings {
		if strings.Contains(w, "cpu 999999999 out of range") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the CSV cpu warning, got %v", csvWarnings)
	}
}

// Batch N M3 (native) — the same caps apply to idlerthing export imports.
func TestNativeBounds(t *testing.T) {
	database := testDB(t)
	fixture := `{"format": 1, "servers": [{"server": {"id": 1, "hostname": "huge-native",
		"server_type": 1, "active": true, "cpu": 999999999, "ssh_port": 22}}]}`
	summary, err := Import(context.Background(), database, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatal(err)
	}
	var cpu any
	var port int
	database.QueryRow("SELECT cpu, ssh_port FROM servers").Scan(&cpu, &port)
	if cpu != nil {
		t.Fatalf("cpu 999999999 must be NULL, got %v", cpu)
	}
	if port != 22 {
		t.Fatalf("valid ssh_port must pass through, got %d", port)
	}
	found := false
	for _, w := range summary.Warnings {
		if strings.Contains(w, "cpu 999999999 out of range") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the cpu warning, got %v", summary.Warnings)
	}
}

// Batch N M4 — catalog get-or-create is case-insensitive.
func TestMyCatalogCaseInsensitive(t *testing.T) {
	database := testDB(t)
	recs, _, err := ParseMyJSON(strings.NewReader(`[
		{"hostname": "a.example.com", "provider": {"name": "OVH"}},
		{"hostname": "b.example.com", "provider": {"name": "ovh"}}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	sum, err := ImportMyIdlers(context.Background(), database, recs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Providers != 1 {
		t.Fatalf("expected 1 provider, got %+v", sum)
	}
	var providers, linked int
	database.QueryRow("SELECT COUNT(*) FROM providers").Scan(&providers)
	database.QueryRow("SELECT COUNT(*) FROM servers WHERE provider_id IS NOT NULL").Scan(&linked)
	if providers != 1 || linked != 2 {
		t.Fatalf("OVH/ovh must merge into one provider used by both servers: %d providers, %d linked", providers, linked)
	}
}

// Batch N M5 — CSV warnings reference the FILE line (header is line 1).
func TestMyCSVWarningRowNumbers(t *testing.T) {
	csvDoc := "hostname,server_type_name,owned_since\n" +
		"first.example.com,KVM,not-a-date\n" +
		"second.example.com,KVM,also-bad\n"
	_, warnings, err := ParseMyCSV(strings.NewReader(csvDoc))
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 2 {
		t.Fatalf("expected 2 warnings, got %v", warnings)
	}
	if !strings.Contains(warnings[0], "row 2") {
		t.Fatalf("first data row is file line 2: %q", warnings[0])
	}
	if !strings.Contains(warnings[1], "row 3") {
		t.Fatalf("second data row is file line 3: %q", warnings[1])
	}
}

// Batch O #3 — the MB plausibility cap matches the API's 1<<30.
func TestConvertBandwidthCap(t *testing.T) {
	gb := func(f float64) *float64 { return &f }
	// 2<<20 GB = 2<<30 MB → over the 1<<30 cap: absent + warn flag.
	if mb, bad := convertBandwidth(gb(2 << 20)); mb != nil || !bad {
		t.Fatalf("2<<30 MB should be rejected: %v %v", mb, bad)
	}
	if mb, bad := convertBandwidth(gb(100)); mb == nil || *mb != 100*1024 || bad {
		t.Fatalf("valid bandwidth broke: %v %v", mb, bad)
	}
	// Batch O #8 — negative warns; zero is silently unlimited.
	if mb, bad := convertBandwidth(gb(-5)); mb != nil || !bad {
		t.Fatalf("negative should warn: %v %v", mb, bad)
	}
	if mb, bad := convertBandwidth(gb(0)); mb != nil || bad {
		t.Fatalf("zero should be silent NULL: %v %v", mb, bad)
	}
}

// Batch O #5 — labels: empty names skipped, cap counts DISTINCT assigns.
func TestMyLabelsCleanAndDistinctCap(t *testing.T) {
	database := testDB(t)
	recs, _, err := ParseMyJSON(strings.NewReader(`[
		{"hostname": "lbl-01", "labels": ["", "  ", "prod"]},
		{"hostname": "lbl-02", "labels": ["a", "a", "b", "c", "d"]}
	]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(recs[0].Labels) != 1 || recs[0].Labels[0] != "prod" {
		t.Fatalf("empty labels must be dropped: %v", recs[0].Labels)
	}
	sum, err := ImportMyIdlers(context.Background(), database, recs, nil)
	if err != nil {
		t.Fatal(err)
	}
	var emptyLabels int
	database.QueryRow("SELECT COUNT(*) FROM labels WHERE label = ''").Scan(&emptyLabels)
	if emptyLabels != 0 {
		t.Fatal("empty-string label row must not be created")
	}
	// a,a,b,c,d → 4 distinct → all four assigned (cap is 4).
	var assigned int
	database.QueryRow(`SELECT COUNT(*) FROM labels_assigned a
		JOIN servers s ON s.id = a.service_id AND a.service_type = 1
		WHERE s.hostname = 'lbl-02'`).Scan(&assigned)
	if assigned != 4 {
		t.Fatalf("distinct cap should keep all 4 labels, got %d (%+v)", assigned, sum)
	}
}

// Batch O #4 — native importer bounds: hosting limits/disk/bandwidth,
// seedbox port_speed, server bandwidth + disk sizes.
func TestNativeSelectiveBounds(t *testing.T) {
	database := testDB(t)
	fixture := `{
		"format": 1,
		"servers": [{"server": {"id": 1, "hostname": "nb-01", "server_type": 1,
			"active": true, "bandwidth_as_mb": 2147483648},
			"disks": [{"size_as_mb": 2147483648, "media": "SSD"}, {"size_as_mb": 1024, "media": "SSD"}]}],
		"shared": [{"shared_hosting": {"id": 2, "main_domain": "nb.example.com",
			"active": true, "domains_limit": 2097153, "disk_as_mb": 2147483648}}],
		"seedboxes": [{"seedbox": {"id": 3, "hostname": "nb-sb", "active": true,
			"port_speed": 2097152, "bandwidth_as_mb": 2048}}]
	}`
	summary, err := Import(context.Background(), database, strings.NewReader(fixture), false)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Servers != 1 || summary.Shared != 1 || summary.Seedboxes != 1 || summary.Disks != 1 {
		t.Fatalf("summary: %+v", summary)
	}
	if len(summary.Warnings) < 5 {
		t.Fatalf("expected ≥5 bound warnings, got %v", summary.Warnings)
	}
	var bw, disk any
	database.QueryRow("SELECT bandwidth_as_mb FROM servers").Scan(&bw)
	database.QueryRow("SELECT SUM(size_as_mb) FROM server_disks").Scan(&disk)
	if bw != nil {
		t.Fatalf("2<<30 bandwidth must be NULL, got %v", bw)
	}
	if disk != int64(1024) {
		t.Fatalf("only the valid disk imports, got %v", disk)
	}
	var lim, hdisk any
	database.QueryRow("SELECT domains_limit, disk_as_mb FROM shared_hosting").Scan(&lim, &hdisk)
	if lim != nil || hdisk != nil {
		t.Fatalf("hosting over-cap values must be NULL: %v %v", lim, hdisk)
	}
	var port any
	var sbBw int64
	database.QueryRow("SELECT port_speed, bandwidth_as_mb FROM seedboxes").Scan(&port, &sbBw)
	if port != nil || sbBw != 2048 {
		t.Fatalf("seedbox bounds: %v %d", port, sbBw)
	}
}
