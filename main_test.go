package main

import (
	"os"
	"path/filepath"
	"testing"

	"idlerthing/internal/config"
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

// Batch J #5 — secret file hardening.
func TestLoadSecret(t *testing.T) {
	dir := t.TempDir()
	cfg := func() (config.Config, string) {
		dbPath := filepath.Join(dir, "test.db")
		return config.Config{DBPath: dbPath}, dbPath + ".secret"
	}

	// Short IDLER_SECRET → fatal.
	c, _ := cfg()
	c.Secret = "tooshort"
	if _, err := loadSecret(c); err == nil {
		t.Fatal("short IDLER_SECRET must be refused")
	}
	c.Secret = "0123456789abcdef"
	if s, err := loadSecret(c); err != nil || len(s) != 16 {
		t.Fatalf("valid IDLER_SECRET: %v %v", s, err)
	}

	// A loose 0644 pre-existing secret is tightened to 0600.
	c, path := cfg()
	if err := os.WriteFile(path, []byte("existing-secret-value"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSecret(c); err != nil {
		t.Fatalf("load existing: %v", err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("loose secret not tightened: %o", fi.Mode().Perm())
	}

	// A symlinked secret path is fatal (never followed).
	link := filepath.Join(dir, "link.db.secret")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	c2 := config.Config{DBPath: filepath.Join(dir, "link.db")}
	if _, err := loadSecret(c2); err == nil {
		t.Fatal("symlink secret must be fatal")
	}

	// Fresh generation: 0600 from the start, second call reads it back.
	dir2 := t.TempDir()
	c3 := config.Config{DBPath: filepath.Join(dir2, "gen.db")}
	s1, err := loadSecret(c3)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if fi, _ := os.Stat(c3.DBPath + ".secret"); fi.Mode().Perm() != 0o600 {
		t.Fatalf("generated secret mode: %o", fi.Mode().Perm())
	}
	s2, err := loadSecret(c3)
	if err != nil || string(s1) != string(s2) {
		t.Fatalf("generated secret not persisted: %v", err)
	}
}
