package web

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// resolvePingFrom prefers fixed absolute paths so a PATH entry in a writable
// directory cannot substitute a shim for the binary we exec.
func TestResolvePingFrom(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "ping")
	if err := os.WriteFile(real, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory named "ping" must not be mistaken for the binary.
	dirCandidate := filepath.Join(dir, "notabinary")
	if err := os.Mkdir(dirCandidate, 0o755); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "does-not-exist")

	// Empty PATH so the LookPath fallback is deterministic.
	t.Setenv("PATH", filepath.Join(dir, "empty"))

	// LookPath fallback: no fixed candidate matches, but PATH has one.
	t.Run("falls back to PATH", func(t *testing.T) {
		t.Setenv("PATH", dir)
		if got := resolvePingFrom([]string{missing}); got != real {
			t.Fatalf("resolvePingFrom fallback = %q, want %q", got, real)
		}
	})

	for _, tc := range []struct {
		name       string
		candidates []string
		want       string
	}{
		{"first hit wins", []string{real, missing}, real},
		{"skips missing", []string{missing, real}, real},
		{"skips a directory", []string{dirCandidate, real}, real},
		{"nothing found, empty PATH", []string{missing}, ""},
		{"no candidates at all", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolvePingFrom(tc.candidates); got != tc.want {
				t.Fatalf("resolvePingFrom(%v) = %q, want %q", tc.candidates, got, tc.want)
			}
		})
	}
}

// With no ping on the box, the tool degrades to "ping n/a" rather than
// reporting every host unreachable.
func TestExecPingWithoutBinary(t *testing.T) {
	saved := pingBinary
	t.Cleanup(func() { pingBinary = saved })

	pingBinary = ""
	_, err := execPing("192.0.2.1")
	if err == nil || err.Error() != "ping not available" {
		t.Fatalf("want 'ping not available', got %v", err)
	}
}

// The real resolver returns either nothing or an existing regular file —
// never a bare relative name that would be re-resolved through PATH per exec.
func TestResolvePingReturnsAbsoluteOrEmpty(t *testing.T) {
	got := resolvePing()
	if got == "" {
		t.Skip("no ping on this machine")
	}
	if !strings.HasPrefix(got, "/") {
		t.Fatalf("resolvePing() = %q, want an absolute path", got)
	}
	fi, err := os.Stat(got)
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("resolvePing() = %q, not a regular file (%v)", got, err)
	}
}
