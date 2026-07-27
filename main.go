// idlerthing — a lightweight, self-hosted inventory for hosting services.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"idlerthing/internal/config"
	"idlerthing/internal/db"
	"idlerthing/internal/importer"
	"idlerthing/internal/web"
)

// version is stamped at build time by the release workflow via
// -ldflags "-X main.version=vX.Y.Z". Local and development builds report
// "dev" — a released binary must be identifiable from the binary alone.
var version = "dev"

func main() {
	// journald stamps every entry, so Go's own date prefix is redundant —
	// and dropping it lets the fail2ban filter anchor its patterns at ^.
	log.SetFlags(0)

	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("idlerthing", version)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "import" {
		if err := runImport(os.Args[2:]); err != nil {
			slog.Error("import failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "passwd" {
		if err := runPasswd(os.Args[2:]); err != nil {
			slog.Error("passwd failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// runPasswd implements `idlerthing passwd [new-password]`: resets the
// (single) user row's password — single-admin app, so the UPDATE needs no
// WHERE — revoking all sessions and the API token, the same fallout as the
// settings password change. The password comes from argv (automation, with
// a ps/shell-history warning), piped stdin, or is generated+printed on a TTY.
func runPasswd(args []string) error {
	if len(args) > 1 {
		return fmt.Errorf("usage: idlerthing passwd [new-password]  (better: echo '<pw>' | idlerthing passwd)")
	}
	cfg := config.Load()
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	if _, err := db.Migrate(database); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	password := ""
	generated := false
	switch {
	case len(args) == 1:
		password = args[0]
		fmt.Fprintln(os.Stderr, "warning: a password on the command line is visible in ps/shell history — prefer piping it via stdin")
	default:
		// No arg: piped stdin wins; an interactive TTY gets a generated one.
		if fi, _ := os.Stdin.Stat(); fi != nil && fi.Mode()&os.ModeCharDevice == 0 {
			raw, err := io.ReadAll(io.LimitReader(os.Stdin, 256))
			if err != nil {
				return fmt.Errorf("read password from stdin: %w", err)
			}
			password = strings.TrimSpace(string(raw))
		} else {
			password, err = randomPassword(16)
			if err != nil {
				return err
			}
			generated = true
		}
	}
	// bcrypt reads at most 72 bytes; the settings UI enforces the same range.
	if len(password) < 8 || len(password) > 72 {
		return fmt.Errorf("password must be 8–72 bytes")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec("UPDATE users SET password_hash = ?, api_token_hash = NULL, updated_at = CURRENT_TIMESTAMP",
		string(hash))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no admin user yet — start the server once first")
	}
	if _, err := tx.Exec("DELETE FROM sessions"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if generated {
		fmt.Printf("generated admin password: %s\n", password)
	}
	fmt.Println("admin password updated; all sessions and the API token were revoked")
	return nil
}

// runImport implements `idlerthing import [--force] <file>`.
func runImport(args []string) error {
	force := false
	var file string
	for _, a := range args {
		if a == "--force" || a == "-force" {
			force = true
		} else {
			file = a
		}
	}
	if file == "" {
		return fmt.Errorf("usage: idlerthing import [--force] <file.json>")
	}

	cfg := config.Load()
	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()
	if _, err := db.Migrate(database); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open import file: %w", err)
	}
	defer f.Close()

	format, reader, err := importer.DetectFormat(f)
	if err != nil {
		return err
	}
	ctx := context.Background()

	switch format {
	case importer.FormatNative:
		summary, err := importer.Import(ctx, database, reader, force)
		if err != nil {
			return err
		}
		fmt.Printf("imported: %d servers, %d shared, %d reseller, %d seedboxes, %d domains, %d misc\n",
			summary.Servers, summary.Shared, summary.Reseller, summary.Seedboxes, summary.Domains, summary.Misc)
		fmt.Printf("relations: %d pricings, %d disks, %d ips, %d dns, %d notes\n",
			summary.Pricings, summary.Disks, summary.IPs, summary.DNS, summary.Notes)
		fmt.Printf("catalogs: %d providers, %d locations, %d os, %d labels\n",
			summary.Providers, summary.Locations, summary.OS, summary.Labels)
		for _, w := range summary.Warnings {
			fmt.Println("warning:", w)
		}
		return nil

	case importer.FormatMyJSON, importer.FormatMyCSV:
		var records []importer.MyServer
		var warnings []string
		if format == importer.FormatMyJSON {
			records, warnings, err = importer.ParseMyJSON(reader)
		} else {
			records, warnings, err = importer.ParseMyCSV(reader)
		}
		if err != nil {
			return err
		}
		sum, err := importer.ImportMyIdlers(ctx, database, records, warnings)
		if err != nil {
			return err
		}
		fmt.Printf("imported %d servers, skipped %d duplicates, %d warnings\n",
			sum.Imported, sum.SkippedDup, len(sum.Warnings))
		fmt.Printf("catalogs: %d providers, %d locations, %d os, %d labels\n",
			sum.Providers, sum.Locations, sum.OS, sum.Labels)
		fmt.Printf("relations: %d disks, %d ips, %d pricings\n", sum.Disks, sum.IPs, sum.Pricings)
		for _, w := range sum.Warnings {
			fmt.Println("warning:", w)
		}
		fmt.Println("note: yabs data is not imported")
		return nil
	}
	return fmt.Errorf("unrecognized import format")
}

func run() error {
	cfg := config.Load()

	database, err := db.Open(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer database.Close()

	applied, err := db.Migrate(database)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	for _, v := range applied {
		slog.Info("applied migration", "version", v)
	}

	// Validate config-level secrets before mutating any state, so a fatal
	// config error can't leave a half-initialized database behind.
	secret, err := loadSecret(cfg)
	if err != nil {
		return fmt.Errorf("load secret: %w", err)
	}

	if err := seedAdmin(database, cfg.AdminPassword); err != nil {
		return fmt.Errorf("seed admin: %w", err)
	}
	warnDenylistedPassword(database)

	webServer, err := web.New(database)
	if err != nil {
		return fmt.Errorf("init web: %w", err)
	}
	webServer.SetSecret(secret)
	webServer.SetBehindTLSProxy(cfg.BehindTLSProxy)
	webServer.SetAllowHTTPIngest(cfg.AllowHTTPIngest)
	webServer.SetBaseURL(cfg.BaseURL)
	webServer.SetLegal(License, ThirdPartyLicenses)
	webServer.SetVersion(version)

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           webServer.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second, // headroom for large exports
		IdleTimeout:       120 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("listening", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
	}

	slog.Info("shutting down")
	// 15s: a prometheus batch can run ~12s worst case.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// loadSecret returns the HMAC secret for yabs ingest URLs. IDLER_SECRET wins
// (≥16 bytes required — a weak key defeats the single-use capability
// design); otherwise a random secret is generated once and persisted next to
// the DB. Anything that undermines the file's confidentiality is fatal.
func loadSecret(cfg config.Config) ([]byte, error) {
	if cfg.Secret != "" {
		if len(cfg.Secret) < 16 {
			return nil, fmt.Errorf("IDLER_SECRET must be at least 16 bytes")
		}
		return []byte(cfg.Secret), nil
	}
	path := cfg.DBPath + ".secret"
	fi, statErr := os.Lstat(path)
	if statErr == nil {
		if !fi.Mode().IsRegular() { // symlink, device, socket, ...
			return nil, fmt.Errorf("secret file %s is not a regular file", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read secret: %w", err)
		}
		// Same minimum as IDLER_SECRET — a weak key defeats the single-use
		// capability design. The operator deletes the file to regenerate.
		if len(raw) < 16 {
			return nil, fmt.Errorf("secret file %s is too short (%d bytes, need ≥16) — delete it to regenerate", path, len(raw))
		}
		// Failure to enforce confidentiality is fatal (same rule as db.Open).
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, fmt.Errorf("chmod secret file: %w", err)
		}
		return raw, nil
	}
	if !os.IsNotExist(statErr) {
		return nil, fmt.Errorf("stat secret: %w", statErr)
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	// O_EXCL so a pre-existing (or raced) file is never reused/truncated;
	// 0600 + chmod + Stat so the umask can never leak it. A partial file
	// from a failed write is removed again.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create secret: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			os.Remove(path)
		}
	}()
	if _, err := f.Write(secret); err != nil {
		f.Close()
		return nil, fmt.Errorf("write secret: %w", err)
	}
	if err := f.Close(); err != nil {
		return nil, fmt.Errorf("write secret: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, fmt.Errorf("chmod secret: %w", err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("secret file %s must be 0600", path)
	}
	cleanup = false
	slog.Info("generated yabs ingest secret", "path", path)
	return secret, nil
}

// denylistedPasswords were publicly documented in repo history as quick-
// start examples; existing deployments keep them (seeding only runs on an
// empty users table), so check on every boot.
var denylistedPasswords = []string{"changeme", "changeme-not-this-one"}

// warnDenylistedPassword logs a LOUD warning when the stored admin hash
// matches a publicly documented example password (one compare each, once
// per boot).
func warnDenylistedPassword(db *sql.DB) {
	var hash string
	if err := db.QueryRow("SELECT password_hash FROM users LIMIT 1").Scan(&hash); err != nil {
		return
	}
	for _, weak := range denylistedPasswords {
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(weak)) == nil {
			slog.Warn("SECURITY: your admin password matches a publicly documented example — change it NOW (idlerthing passwd, or Settings → Account)")
			return
		}
	}
}

// seedAdmin creates the initial admin user when the users table is empty.
// The password comes from cfg password if set, otherwise a random one is
// generated and printed to stderr once.
func seedAdmin(db *sql.DB, password string) error {
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil {
		return err
	}
	if count > 0 {
		return nil
	}

	generated := false
	if password == "" {
		var err error
		password, err = randomPassword(16)
		if err != nil {
			return err
		}
		generated = true
	} else if len(password) < 8 || len(password) > 72 {
		// Same policy as the settings UI — never silently seed a weak admin,
		// and never let bcrypt silently truncate a >72-byte password.
		return fmt.Errorf("IDLER_ADMIN_PASSWORD must be 8–72 bytes")
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}

	if _, err := db.Exec(
		"INSERT INTO users (name, email, password_hash) VALUES (?, ?, ?)",
		"admin", "admin@localhost", string(hash),
	); err != nil {
		return err
	}

	if generated {
		fmt.Fprintf(os.Stderr, "First run: admin password is %s\n", password)
	}
	slog.Info("created admin user", "email", "admin@localhost")
	return nil
}

// randomPassword returns a random URL-safe password of n characters.
func randomPassword(n int) (string, error) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	buf := make([]byte, n)
	max := big.NewInt(int64(len(alphabet)))
	for i := range buf {
		k, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		buf[i] = alphabet[k.Int64()]
	}
	return string(buf), nil
}
