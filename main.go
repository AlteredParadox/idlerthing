package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/crypto/bcrypt"

	"idlerthing/internal/config"
	"idlerthing/internal/db"
	"idlerthing/internal/importer"
	"idlerthing/internal/web"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "import" {
		if err := runImport(os.Args[2:]); err != nil {
			slog.Error("import failed", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
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

	webServer, err := web.New(database)
	if err != nil {
		return fmt.Errorf("init web: %w", err)
	}
	webServer.SetSecret(secret)
	webServer.SetBehindTLSProxy(cfg.BehindTLSProxy)
	webServer.SetBaseURL(cfg.BaseURL)

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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
		if len(raw) == 0 {
			return nil, fmt.Errorf("secret file %s is empty", path)
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
	// 0600 + chmod + Stat so the umask can never leak it.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create secret: %w", err)
	}
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
	slog.Info("generated yabs ingest secret", "path", path)
	return secret, nil
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
	} else if len(password) < 8 {
		// Same policy as the settings UI — never silently seed a weak admin.
		return fmt.Errorf("IDLER_ADMIN_PASSWORD must be at least 8 characters")
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
