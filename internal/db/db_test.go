package db

import (
	"os"
	"path/filepath"
	"testing"
)

func permOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Mode().Perm()
}

// Batch I #3 — the DB file and any directory WE create are private, but a
// pre-existing directory is never chmodded (IDLER_DB=/tmp/x.db must not
// tighten /tmp).
func TestOpenPermissionsSharedParent(t *testing.T) {
	// Pre-existing 0755 parent: left untouched, db file still 0600.
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(parent, "shared.db")
	database, err := Open(shared)
	if err != nil {
		t.Fatalf("Open shared: %v", err)
	}
	database.Close()
	if got := permOf(t, parent); got != 0o755 {
		t.Fatalf("pre-existing dir chmodded: %o, want 755", got)
	}
	if got := permOf(t, shared); got != 0o600 {
		t.Fatalf("db file in shared dir: %o, want 600", got)
	}

	// A pre-existing loose db file is tightened on open.
	loose := filepath.Join(parent, "loose.db")
	if err := os.WriteFile(loose, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	database, err = Open(loose)
	if err != nil {
		t.Fatalf("Open loose: %v", err)
	}
	database.Close()
	if got := permOf(t, loose); got != 0o600 {
		t.Fatalf("loose db not tightened: %o, want 600", got)
	}
}

// Batch P #1 — URI-reserved characters in IDLER_DB stay literal: the
// database lands at the REAL path, not a truncated URI interpretation.
func TestOpenURIReservedChars(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "we ird#db?v100%.db")
	database, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer database.Close()
	if _, err := database.Exec("CREATE TABLE t (id INTEGER)"); err != nil {
		t.Fatalf("write through the literal path: %v", err)
	}
	// The real file exists with content; no truncated decoy was created.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("literal db file missing: %v", err)
	}
	if fi.Size() == 0 {
		t.Fatal("literal db file is empty — writes went elsewhere")
	}
	decoy := filepath.Join(dir, "we ird")
	if _, err := os.Stat(decoy); !os.IsNotExist(err) {
		t.Fatalf("decoy file %q exists — path was parsed as a URI", decoy)
	}
	// The content is really there.
	var n int
	if err := database.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 't'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("content check: %d %v", n, err)
	}
}
