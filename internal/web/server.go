// Package web implements the HTTP layer: routes, middleware, auth, and templates.
package web

import (
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/base64"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"idlerthing/internal/model"
	"idlerthing/internal/pricing"
)

//go:embed assets/static assets/templates
var assetsFS embed.FS

// staticFS holds the embedded static assets subtree.
var staticFS = mustSub(assetsFS, "assets/static")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err)
	}
	return sub
}

// Server is the web application. Construct with New, serve with Handler.
type Server struct {
	db         *sql.DB
	tmpl       *templates
	limit      *rateLimiter
	emailLimit *rateLimiter
	pingLimit  *rateLimiter
	servers    *model.ServerStore
	catalogs   *model.CatalogStore
	pricings   *model.PricingStore
	rates      *pricing.Rates
	whoisURL   string // injectable for tests; default ipwho.is
	dash       *dashboardCache
	secret     []byte // yabs ingest HMAC key
	prom       *promCache
	uptime     uptimeCache
	livemon    liveMonCache

	publicCache     publicCacheEntry
	behindTLSProxy  bool
	allowHTTPIngest bool           // IDLER_BEHIND_TLS_PROXY
	baseURL         string         // IDLER_BASE_URL ("" = derive from request)
	whoisRate       whoisRateLimit // per-server whois throttle (fresh in tests)
}

// New creates a Server backed by db.
func New(db *sql.DB) (*Server, error) {
	t, err := parseTemplates()
	if err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	return &Server{
		db:         db,
		tmpl:       t,
		limit:      newRateLimiter(10, time.Minute),
		emailLimit: newRateLimiter(10, time.Minute),
		pingLimit:  newRateLimiter(10, time.Minute),
		servers:    &model.ServerStore{DB: db},
		catalogs:   &model.CatalogStore{DB: db},
		pricings:   &model.PricingStore{DB: db},
		rates:      pricing.NewRates(),
		dash:       &dashboardCache{},
		secret:     secret,
		prom:       &promCache{},
	}, nil
}

// SetSecret overrides the yabs ingest HMAC key.
func (s *Server) SetSecret(secret []byte) {
	s.secret = secret
}

// SetBehindTLSProxy marks the app as behind an HTTPS-terminating proxy.
func (s *Server) SetBehindTLSProxy(behind bool) {
	s.behindTLSProxy = behind
}

// SetAllowHTTPIngest permits plain-http ingest URLs on LAN hosts.
func (s *Server) SetAllowHTTPIngest(allow bool) {
	s.allowHTTPIngest = allow
}

// SetBaseURL sets the external base URL for the yabs ingest command.
// Only http(s) URLs without single-quote characters are accepted.
func (s *Server) SetBaseURL(u string) {
	u = strings.TrimRight(u, "/")
	if strings.Contains(u, "'") ||
		!(strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://")) {
		u = ""
	}
	s.baseURL = u
}

// Handler returns the root handler with the full middleware chain:
// recover → security headers → session loading → routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Public routes.
	mux.HandleFunc("GET /static/accent.css", s.handleAccentCSS)
	mux.Handle("GET /static/", s.withCacheHeaders(http.StripPrefix("/static/", http.FileServerFS(staticFS))))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		// Bounded: Ping would otherwise queue behind the single connection
		// for as long as a slow write takes.
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := s.db.PingContext(ctx); err != nil {
			http.Error(w, "unhealthy\n", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /login", s.handleLoginGet)
	mux.HandleFunc("POST /login", s.handleLoginPost)

	// Protected routes (session required, CSRF enforced on unsafe methods).
	mux.Handle("GET /{$}", s.protect(http.HandlerFunc(s.handleDashboard)))
	mux.Handle("GET /", s.protect(http.HandlerFunc(s.handleNotFound)))
	mux.Handle("POST /logout", s.protect(http.HandlerFunc(s.handleLogout)))
	mux.Handle("POST /prefs/theme", s.protect(http.HandlerFunc(s.handleThemePref)))
	mux.Handle("POST /prefs/servers-cols", s.protect(http.HandlerFunc(s.handleServerColsPref)))
	mux.Handle("POST /prefs/short-hostnames", s.protect(http.HandlerFunc(s.handleShortHostnamesPref)))

	// Catalogs.
	mux.Handle("GET /catalogs/{kind}", s.protect(http.HandlerFunc(s.handleCatalogList)))
	mux.Handle("POST /catalogs/{kind}", s.protect(http.HandlerFunc(s.handleCatalogCreate)))
	mux.Handle("POST /catalogs/{kind}/{id}/update", s.protect(http.HandlerFunc(s.handleCatalogUpdate)))
	mux.Handle("POST /catalogs/{kind}/{id}/delete", s.protect(http.HandlerFunc(s.handleCatalogDelete)))

	// Servers.
	mux.Handle("GET /servers", s.protect(http.HandlerFunc(s.handleServerList)))
	mux.Handle("GET /servers/new", s.protect(http.HandlerFunc(s.handleServerNew)))
	mux.Handle("POST /servers", s.protect(http.HandlerFunc(s.handleServerCreate)))
	mux.Handle("GET /servers/{id}", s.protect(http.HandlerFunc(s.handleServerDetail)))
	mux.Handle("GET /servers/{id}/edit", s.protect(http.HandlerFunc(s.handleServerEdit)))
	mux.Handle("POST /servers/{id}/update", s.protect(http.HandlerFunc(s.handleServerUpdate)))
	mux.Handle("POST /servers/{id}/delete", s.protect(http.HandlerFunc(s.handleServerDelete)))

	// Shared + reseller hosting.
	for _, cfg := range []*hostingConfig{s.sharedConfig(), s.resellerConfig()} {
		sec := s.hostingSection(cfg)
		mux.Handle("GET "+cfg.base, s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleSectionList(w, r, sec)
		})))
		mux.Handle("GET "+cfg.base+"/new", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleHostingNew(w, r, cfg)
		})))
		mux.Handle("POST "+cfg.base, s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleHostingCreate(w, r, cfg)
		})))
		mux.Handle("GET "+cfg.base+"/{id}", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleSectionDetail(w, r, sec)
		})))
		mux.Handle("GET "+cfg.base+"/{id}/edit", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleHostingEdit(w, r, cfg)
		})))
		mux.Handle("POST "+cfg.base+"/{id}/update", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleHostingUpdate(w, r, cfg)
		})))
		mux.Handle("POST "+cfg.base+"/{id}/delete", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.handleSectionDelete(w, r, sec)
		})))
	}

	// Seedboxes.
	seedboxSec := s.seedboxSection()
	mux.Handle("GET /seedboxes", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionList(w, r, seedboxSec) })))
	mux.Handle("GET /seedboxes/new", s.protect(http.HandlerFunc(s.handleSeedboxNew)))
	mux.Handle("POST /seedboxes", s.protect(http.HandlerFunc(s.handleSeedboxCreate)))
	mux.Handle("GET /seedboxes/{id}", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDetail(w, r, seedboxSec) })))
	mux.Handle("GET /seedboxes/{id}/edit", s.protect(http.HandlerFunc(s.handleSeedboxEdit)))
	mux.Handle("POST /seedboxes/{id}/update", s.protect(http.HandlerFunc(s.handleSeedboxUpdate)))
	mux.Handle("POST /seedboxes/{id}/delete", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDelete(w, r, seedboxSec) })))

	// Domains.
	domainSec := s.domainSection()
	mux.Handle("GET /domains", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionList(w, r, domainSec) })))
	mux.Handle("GET /domains/new", s.protect(http.HandlerFunc(s.handleDomainNew)))
	mux.Handle("POST /domains", s.protect(http.HandlerFunc(s.handleDomainCreate)))
	mux.Handle("GET /domains/{id}", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDetail(w, r, domainSec) })))
	mux.Handle("GET /domains/{id}/edit", s.protect(http.HandlerFunc(s.handleDomainEdit)))
	mux.Handle("POST /domains/{id}/update", s.protect(http.HandlerFunc(s.handleDomainUpdate)))
	mux.Handle("POST /domains/{id}/delete", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDelete(w, r, domainSec) })))

	// Misc services.
	miscSec := s.miscSection()
	mux.Handle("GET /misc", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionList(w, r, miscSec) })))
	mux.Handle("GET /misc/new", s.protect(http.HandlerFunc(s.handleMiscNew)))
	mux.Handle("POST /misc", s.protect(http.HandlerFunc(s.handleMiscCreate)))
	mux.Handle("GET /misc/{id}", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDetail(w, r, miscSec) })))
	mux.Handle("GET /misc/{id}/edit", s.protect(http.HandlerFunc(s.handleMiscEdit)))
	mux.Handle("POST /misc/{id}/update", s.protect(http.HandlerFunc(s.handleMiscUpdate)))
	mux.Handle("POST /misc/{id}/delete", s.protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.handleSectionDelete(w, r, miscSec) })))

	// Labels, notes, IPs, DNS.
	mux.Handle("POST /labels/assign", s.protect(http.HandlerFunc(s.handleLabelAssign)))
	mux.Handle("POST /labels/unassign", s.protect(http.HandlerFunc(s.handleLabelUnassign)))
	mux.Handle("GET /notes", s.protect(http.HandlerFunc(s.handleNotesIndex)))
	mux.Handle("POST /notes", s.protect(http.HandlerFunc(s.handleNoteCreate)))
	mux.Handle("POST /notes/{id}/delete", s.protect(http.HandlerFunc(s.handleNoteDelete)))
	mux.Handle("GET /ips", s.protect(http.HandlerFunc(s.handleIPsIndex)))
	mux.Handle("POST /ips", s.protect(http.HandlerFunc(s.handleIPCreate)))
	mux.Handle("POST /ips/{id}/delete", s.protect(http.HandlerFunc(s.handleIPDelete)))
	mux.Handle("POST /ips/{id}/whois", s.protect(http.HandlerFunc(s.handleIPWhois)))
	mux.Handle("GET /dns", s.protect(http.HandlerFunc(s.handleDNSIndex)))
	mux.Handle("POST /dns", s.protect(http.HandlerFunc(s.handleDNSCreate)))
	mux.Handle("GET /dns/{id}/edit", s.protect(http.HandlerFunc(s.handleDNSEdit)))
	mux.Handle("POST /dns/{id}/update", s.protect(http.HandlerFunc(s.handleDNSUpdate)))
	mux.Handle("POST /dns/{id}/delete", s.protect(http.HandlerFunc(s.handleDNSDelete)))

	// Settings.
	mux.Handle("GET /settings", s.protect(http.HandlerFunc(s.handleSettingsGet)))
	mux.Handle("POST /settings", s.protect(http.HandlerFunc(s.handleSettingsUpdate)))
	mux.Handle("POST /settings/account", s.protect(http.HandlerFunc(s.handleSettingsAccount)))
	mux.Handle("POST /settings/prometheus/test", s.protect(http.HandlerFunc(s.handlePrometheusTest)))

	// Export.
	mux.Handle("GET /export/json", s.protect(http.HandlerFunc(s.handleExportJSON)))
	mux.Handle("GET /export/json/{type}", s.protect(http.HandlerFunc(s.handleExportJSON)))
	mux.Handle("GET /export/csv", s.protect(http.HandlerFunc(s.handleExportCSV)))

	// JSON API (token auth, no session/CSRF). Method-qualified patterns so
	// they don't conflict with the "GET /" catch-all.
	apiHandler := s.apiMux()
	mux.Handle("GET /api/", apiHandler)
	mux.Handle("POST /api/", apiHandler)
	mux.Handle("PUT /api/", apiHandler)
	mux.Handle("DELETE /api/", apiHandler)

	// Public yabs ingest (signature-authed, no session/CSRF/token) and the
	// public servers page.
	mux.HandleFunc("POST /api/yabs/{id}", s.handleYABSIngest)
	mux.HandleFunc("GET /public", s.handlePublic)

	// YABS views + ping tool.
	mux.Handle("GET /yabs", s.protect(http.HandlerFunc(s.handleYABSIndex)))
	mux.Handle("GET /servers/{id}/yabs", s.protect(http.HandlerFunc(s.handleServerYABS)))
	mux.Handle("GET /servers/{id}/yabs/{yid}", s.protect(http.HandlerFunc(s.handleServerYABSDetail)))
	mux.Handle("POST /servers/{id}/yabs/{yid}/delete", s.protect(http.HandlerFunc(s.handleServerYABSDelete)))
	mux.Handle("POST /tools/ping", s.protect(http.HandlerFunc(s.handlePing)))

	return s.recoverMiddleware(s.securityHeaders(s.loadSession(mux)))
}

// protect wraps a handler with session auth + CSRF protection.
func (s *Server) protect(next http.Handler) http.Handler {
	return s.requireAuth(s.csrfProtect(next))
}

// cacheStatusWriter injects the immutable Cache-Control only when the
// response is 2xx — a 404 for a missing asset must not be pinned in
// browsers for a year.
type cacheStatusWriter struct {
	http.ResponseWriter
	wrote bool
}

func (w *cacheStatusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.wrote = true
		if code >= 200 && code < 300 {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *cacheStatusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(b)
}

// withCacheHeaders marks static assets as immutable; templates reference them
// with a version query param (e.g. app.css?v=1) for cache busting.
func (s *Server) withCacheHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(&cacheStatusWriter{ResponseWriter: w}, r)
	})
}

// securityHeaders sets baseline hardening headers on every response.
// Authenticated HTML, the API, and one-time secrets are no-store; the
// static file handler overrides with its own immutable policy.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; script-src 'self'; "+
				"base-uri 'none'; form-action 'self'; object-src 'none'; frame-ancestors 'none'")
		next.ServeHTTP(w, r)
	})
}

// recoverMiddleware converts panics into 500s instead of dropping the connection.
func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("panic serving request", "err", err, "path", r.URL.Path)
				http.Error(w, errMsgServerErr, http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// randomToken returns n random bytes encoded URL-safe.
func randomToken(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}
