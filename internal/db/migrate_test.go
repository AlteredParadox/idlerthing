package db

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var expectedTables = []string{
	"servers", "server_disks", "shared_hosting", "reseller_hosting",
	"seedboxes", "domains", "misc_services", "pricings", "ips", "dns",
	"labels", "labels_assigned", "notes", "providers", "locations", "os",
	"yabs", "yabs_disk_speed", "yabs_network_speed", "settings",
	"users", "sessions", "user_prefs",
}

func openTemp(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

func TestMigrateFreshDB(t *testing.T) {
	db, err := Open(openTemp(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != 10 || applied[0] != 1 || applied[1] != 2 || applied[2] != 3 || applied[3] != 4 || applied[4] != 5 || applied[5] != 6 || applied[6] != 7 || applied[7] != 8 || applied[8] != 9 || applied[9] != 10 {
		t.Fatalf("expected applied=[1 2 3 4 5 6 7 8 9 10], got %v", applied)
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 10 {
		t.Fatalf("expected user_version 10, got %d", version)
	}

	for _, table := range expectedTables {
		var name string
		err := db.QueryRow(
			"SELECT name FROM sqlite_master WHERE type='table' AND name=?", table,
		).Scan(&name)
		if err != nil {
			t.Errorf("table %q missing: %v", table, err)
		}
	}
}

func TestMigrateIdempotent(t *testing.T) {
	db, err := Open(openTemp(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := Migrate(db); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second Migrate should be a no-op, applied %v", applied)
	}

	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		t.Fatalf("read user_version: %v", err)
	}
	if version != 10 {
		t.Fatalf("expected user_version 10, got %d", version)
	}
}

func TestSettingsSeeded(t *testing.T) {
	db, err := Open(openTemp(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	if _, err := Migrate(db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	var id int
	var currency, theme string
	err = db.QueryRow("SELECT id, default_currency, theme FROM settings WHERE id = 1").
		Scan(&id, &currency, &theme)
	if err != nil {
		t.Fatalf("settings row id=1 missing: %v", err)
	}
	if currency != "USD" || theme != "dark" {
		t.Fatalf("unexpected settings defaults: currency=%q theme=%q", currency, theme)
	}

	var accent string
	var compact int
	err = db.QueryRow("SELECT accent_color, compact_mode FROM settings WHERE id = 1").
		Scan(&accent, &compact)
	if err != nil {
		t.Fatalf("accent/compact columns missing: %v", err)
	}
	if accent != "#5b9cf8" || compact != 0 {
		t.Fatalf("unexpected ui defaults: accent=%q compact=%d", accent, compact)
	}
}

// TestMigration0003UnitConversion builds a user_version=2 database with
// known GB/TB values and verifies 0003 converts them to MB (0 bw → NULL).
func TestMigration0003UnitConversion(t *testing.T) {
	db, err := Open(openTemp(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	// Replay migrations 1+2 manually, land on user_version 2.
	for _, name := range []string{"0001_init.sql", "0002_yabs_hash.sql"} {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("replay %s: %v", name, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 2"); err != nil {
		t.Fatal(err)
	}

	// Old-shape rows: 50 GB disk, 20 TB bw, 0 TB bw (→ NULL), 2 TB shared bw.
	if _, err := db.Exec(`
		INSERT INTO servers (hostname, bandwidth) VALUES ('conv-srv', 20);
		INSERT INTO servers (hostname, bandwidth) VALUES ('zero-bw-srv', 0);
		INSERT INTO servers (hostname, bandwidth, network_type) VALUES ('nat-srv', 1, 'NAT+IPv4');
		INSERT INTO servers (hostname, bandwidth, network_type) VALUES ('shared-srv', 1, 'IPv4 (shared)');
		INSERT INTO server_disks (server_id, size_as_gb) VALUES (1, 50);
		INSERT INTO shared_hosting (main_domain, disk_as_gb, bandwidth) VALUES ('conv.example.com', 100, 2);
		INSERT INTO reseller_hosting (main_domain, disk_as_gb, bandwidth) VALUES ('res.example.com', 40, 0);
		INSERT INTO seedboxes (hostname, disk_as_gb, bandwidth) VALUES ('conv-seed', 500, 10);
	`); err != nil {
		t.Fatal(err)
	}

	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != 8 || applied[0] != 3 || applied[1] != 4 || applied[2] != 5 || applied[3] != 6 || applied[4] != 7 || applied[5] != 8 || applied[6] != 9 || applied[7] != 10 {
		t.Fatalf("expected migrations [3 4 5 6 7 8 9 10] applied, got %v", applied)
	}

	// 0006: legacy network_type values merge into 'IPv4 NAT'.
	for _, host := range []string{"nat-srv", "shared-srv"} {
		var nt string
		db.QueryRow("SELECT network_type FROM servers WHERE hostname = ?", host).Scan(&nt)
		if nt != "IPv4 NAT" {
			t.Fatalf("%s network_type: got %q, want 'IPv4 NAT'", host, nt)
		}
	}

	var diskMB int64
	db.QueryRow("SELECT size_as_mb FROM server_disks WHERE server_id = 1").Scan(&diskMB)
	if diskMB != 50*1024 {
		t.Fatalf("disk conversion: %d", diskMB)
	}

	var bw int64
	db.QueryRow("SELECT bandwidth_as_mb FROM servers WHERE hostname = 'conv-srv'").Scan(&bw)
	if bw != 20*1024*1024 {
		t.Fatalf("server bw conversion: %d", bw)
	}

	var nullBW any
	db.QueryRow("SELECT bandwidth_as_mb FROM servers WHERE hostname = 'zero-bw-srv'").Scan(&nullBW)
	if nullBW != nil {
		t.Fatalf("0 bandwidth should become NULL, got %v", nullBW)
	}

	var sharedDisk, sharedBW int64
	db.QueryRow("SELECT disk_as_mb, bandwidth_as_mb FROM shared_hosting").Scan(&sharedDisk, &sharedBW)
	if sharedDisk != 100*1024 || sharedBW != 2*1024*1024 {
		t.Fatalf("shared conversion: %d %d", sharedDisk, sharedBW)
	}

	var resBW any
	db.QueryRow("SELECT bandwidth_as_mb FROM reseller_hosting").Scan(&resBW)
	if resBW != nil {
		t.Fatalf("reseller 0 bw should become NULL, got %v", resBW)
	}

	var seedDisk, seedBW int64
	db.QueryRow("SELECT disk_as_mb, bandwidth_as_mb FROM seedboxes").Scan(&seedDisk, &seedBW)
	if seedDisk != 500*1024 || seedBW != 10*1024*1024 {
		t.Fatalf("seedbox conversion: %d %d", seedDisk, seedBW)
	}

	var version int
	db.QueryRow("PRAGMA user_version").Scan(&version)
	if version != 10 {
		t.Fatalf("expected user_version 10, got %d", version)
	}
}

// TestMigration0007OrphanCleanup seeds orphans into a user_version=6 DB and
// asserts they are removed while legit rows survive.
func TestMigration0007OrphanCleanup(t *testing.T) {
	db, err := Open(openTemp(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()

	for _, name := range []string{"0001_init.sql", "0002_yabs_hash.sql", "0003_units_mb.sql", "0004_accent_compact.sql", "0005_prometheus.sql", "0006_network_types.sql"} {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("replay %s: %v", name, err)
		}
	}
	if _, err := db.Exec("PRAGMA user_version = 6"); err != nil {
		t.Fatal(err)
	}

	// One legit server + children; orphan children with no parent.
	if _, err := db.Exec(`
		INSERT INTO servers (id, hostname) VALUES (1, 'legit-01');
		INSERT INTO pricings (service_id, service_type, currency, price, term) VALUES (1, 1, 'USD', 10, 1);
		INSERT INTO pricings (service_id, service_type, currency, price, term) VALUES (999, 1, 'USD', 10, 1);
		INSERT INTO pricings (service_id, service_type, currency, price, term) VALUES (1, 9, 'USD', 10, 1);
		INSERT INTO ips (service_id, service_type, address) VALUES (1, 1, '203.0.113.1');
		INSERT INTO ips (service_id, service_type, address) VALUES (999, 1, '203.0.113.2');
		INSERT INTO ips (service_id, service_type, address) VALUES (1, 4, '203.0.113.3');
		INSERT INTO notes (service_id, service_type, body) VALUES (1, 1, 'legit note');
		INSERT INTO notes (service_id, service_type, body) VALUES (999, 1, 'orphan note');
		INSERT INTO notes (service_id, service_type, ip_id, body) VALUES (NULL, NULL, 1, 'ip note legit');
		INSERT INTO notes (service_id, service_type, ip_id, body) VALUES (NULL, NULL, 2, 'ip note on orphan ip');
		INSERT INTO labels (id, label) VALUES (1, 'x');
		INSERT INTO labels_assigned (label_id, service_id, service_type) VALUES (1, 1, 1);
		INSERT INTO labels_assigned (label_id, service_id, service_type) VALUES (1, 999, 1);
	`); err != nil {
		t.Fatal(err)
	}

	applied, err := Migrate(db)
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if len(applied) != 4 || applied[0] != 7 || applied[1] != 8 || applied[2] != 9 || applied[3] != 10 {
		t.Fatalf("expected migrations [7 8 9 10], got %v", applied)
	}

	count := func(q string) int {
		var n int
		db.QueryRow(q).Scan(&n)
		return n
	}
	if n := count("SELECT COUNT(*) FROM pricings"); n != 1 {
		t.Fatalf("expected 1 legit pricing, got %d", n)
	}
	if n := count("SELECT COUNT(*) FROM ips"); n != 1 {
		t.Fatalf("expected 1 legit ip, got %d", n)
	}
	if n := count("SELECT COUNT(*) FROM notes"); n != 2 {
		t.Fatalf("expected 2 legit notes (service + ip-keyed), got %d", n)
	}
	if n := count("SELECT COUNT(*) FROM labels_assigned"); n != 1 {
		t.Fatalf("expected 1 legit assignment, got %d", n)
	}
}

// TestNewerDBRefused: a user_version beyond the newest migration errors out.
func TestNewerDBRefused(t *testing.T) {
	db, err := Open(openTemp(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(db); err == nil {
		t.Fatal("expected refusal of newer database")
	} else if !strings.Contains(err.Error(), "newer version") {
		t.Fatalf("wrong error: %v", err)
	}
}

// TestOpenPermissions: db dir 0700, file 0600.
func TestOpenPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "test.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatal(err)
	}
	// Re-open so the post-ping chmod covers the wal/shm siblings too.
	db.Close()
	db, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer db.Close()

	info, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode: %o", got)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("db file mode: %o", got)
	}
}
