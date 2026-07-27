package importer

import (
	"context"
	"strings"
	"testing"
)

// Anything after the JSON document is refused: a truncated-then-appended
// backup must not import its first half and call that success.
func TestImportTrailingGarbage(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,"servers":[]}` + "\n" + `{"format":1}`
	_, err := Import(context.Background(), database, strings.NewReader(doc), false)
	if err == nil || !strings.Contains(err.Error(), "trailing garbage") {
		t.Fatalf("want a trailing-garbage error, got %v", err)
	}
}

// A hosting entry missing its entity object is warned about and skipped,
// not treated as a silent no-op.
func TestImportHostingItemWithoutEntity(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,"shared":[{"pricing":{"currency":"USD","price":1,"term":1}}]}`
	sum, err := Import(context.Background(), database, strings.NewReader(doc), false)
	if err != nil {
		t.Fatalf("import should succeed with a warning, got %v", err)
	}
	var n int
	database.QueryRow("SELECT COUNT(*) FROM shared_hosting").Scan(&n)
	if n != 0 {
		t.Fatalf("entity-less item was inserted anyway: %d rows", n)
	}
	if len(sum.Warnings) == 0 {
		t.Fatal("expected a warning naming the skipped item")
	}
}

// servers(): an out-of-range server_type warns and stores KVM instead of
// failing the row.
func TestImportServerTypeOutOfRange(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,"servers":[{"server":{"id":1,"hostname":"st-99","server_type":99,"active":true}}]}`
	sum, err := Import(context.Background(), database, strings.NewReader(doc), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var st int
	database.QueryRow("SELECT server_type FROM servers WHERE hostname = 'st-99'").Scan(&st)
	if st != 1 {
		t.Fatalf("server_type 99 should store KVM (1), got %d", st)
	}
	found := false
	for _, w := range sum.Warnings {
		if strings.Contains(w, "server_type 99 out of range") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the server_type warning, got %v", sum.Warnings)
	}
}

// servers(): invalid disk media warns and stores SSD; valid disks import.
func TestImportDiskMediaInvalid(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,"servers":[{"server":{"id":1,"hostname":"dm-01","server_type":1,"active":true},
		"disks":[{"size_as_mb":1024,"media":"TAPE"},{"size_as_mb":2048,"media":"NVMe"}]}]}`
	sum, err := Import(context.Background(), database, strings.NewReader(doc), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	var tape, nvme int
	database.QueryRow("SELECT COUNT(*) FROM server_disks WHERE media = 'TAPE'").Scan(&tape)
	database.QueryRow("SELECT COUNT(*) FROM server_disks WHERE media = 'NVMe'").Scan(&nvme)
	var ssd int
	database.QueryRow("SELECT COUNT(*) FROM server_disks WHERE media = 'SSD'").Scan(&ssd)
	if tape != 0 || ssd != 1 || nvme != 1 {
		t.Fatalf("media fallback wrong: tape=%d ssd=%d nvme=%d", tape, ssd, nvme)
	}
	found := false
	for _, w := range sum.Warnings {
		if strings.Contains(w, `invalid disk media "TAPE"`) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the media warning, got %v", sum.Warnings)
	}
}

// servers(): the inlined-label cap assigns 4 of 5 without error, and empty
// label names are skipped without creating an empty catalog row.
func TestImportLabelCapAndEmptyName(t *testing.T) {
	database := testDB(t)
	doc := `{"format":1,"servers":[
		{"server":{"id":1,"hostname":"lbl-cap","server_type":1,"active":true},
			"labels":[{"name":"a"},{"name":"b"},{"name":"c"},{"name":"d"},{"name":"e"}]},
		{"server":{"id":2,"hostname":"lbl-empty","server_type":1,"active":true},
			"labels":[{"name":""},{"name":"prod"}]}]}`
	sum, err := Import(context.Background(), database, strings.NewReader(doc), false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if sum.Servers != 2 {
		t.Fatalf("both servers should import: %+v", sum)
	}
	var assigned int
	database.QueryRow(`SELECT COUNT(*) FROM labels_assigned a
		JOIN servers s ON s.id = a.service_id AND a.service_type = 1
		WHERE s.hostname = 'lbl-cap'`).Scan(&assigned)
	if assigned != 4 {
		t.Fatalf("cap should assign 4 of 5 labels, got %d", assigned)
	}
	var emptyRows, prodRows int
	database.QueryRow("SELECT COUNT(*) FROM labels WHERE label = ''").Scan(&emptyRows)
	database.QueryRow("SELECT COUNT(*) FROM labels WHERE label = 'prod'").Scan(&prodRows)
	if emptyRows != 0 || prodRows != 1 {
		t.Fatalf("empty label must not create a catalog row: empty=%d prod=%d", emptyRows, prodRows)
	}
}
