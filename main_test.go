package main

import (
	"path/filepath"
	"testing"

	"idlerthing/internal/db"
)

// Batch I #9 — a short IDLER_ADMIN_PASSWORD is refused (no weak admin seeded).
func TestSeedAdminShortPassword(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()
	if _, err := db.Migrate(database); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	if err := seedAdmin(database, "short"); err == nil {
		t.Fatal("expected refusal of a <8 char password")
	}
	var count int
	if err := database.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("no user must be seeded on refusal, got %d", count)
	}

	// A compliant password seeds; a second call is a no-op.
	if err := seedAdmin(database, "long-enough-password"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := seedAdmin(database, "another-password"); err != nil {
		t.Fatalf("second seed should be a no-op: %v", err)
	}
	if err := database.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 user, got %d", count)
	}
}
