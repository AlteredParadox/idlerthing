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
