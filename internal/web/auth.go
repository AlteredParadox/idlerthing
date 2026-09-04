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

package web

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"

	"idlerthing/internal/model"
)

const (
	sessionCookieName = "idler_session"
	sessionDuration   = 7 * 24 * time.Hour
)

// dummyHash is a valid bcrypt hash burned on unknown-email logins so failed
// attempts take a constant-ish time regardless of why they failed.
var dummyHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("idlerthing-dummy"), bcrypt.DefaultCost)
	if err != nil {
		panic(err)
	}
	return h
}()

type ctxKey int

const (
	ctxUser ctxKey = iota
	ctxSession
	ctxMemo
)

type session struct {
	Token     string
	UserID    int
	CSRFToken string
	ExpiresAt time.Time
}

type user struct {
	ID    int
	Name  string
	Email string
}

// handleLoginGet renders the standalone login page.
func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	if userFromCtx(r.Context()) != nil {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	token, err := s.issueLoginCSRF(w, r)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.renderLogin(w, r, token, "")
}

// handleLoginPost validates credentials and creates a session on success.
func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	// Pre-auth hardening: cap the body before any form parsing (FormValue
	// otherwise retains huge values as limiter keys), and refuse multipart
	// outright (its temp-file parsing burns disk for no legitimate use).
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		http.Error(w, "unsupported media type", http.StatusUnsupportedMediaType)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	token, err := s.issueLoginCSRF(w, r) // rotate on every attempt
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}

	if !s.checkLoginCSRF(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ip := s.clientIP(r)
	if !s.limit.allow(limiterKey(ip)) {
		s.logBlocked(ip)
		s.renderLogin(w, r, token, "Too many attempts. Try again later.")
		return
	}

	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")
	// Bounded inputs before they become limiter keys or bcrypt work.
	if len(email) > 254 || len(password) > 256 {
		s.renderLogin(w, r, token, "Invalid email or password.")
		return
	}

	// Per-account limiter alongside the per-IP one, so a distributed
	// credential-stuffing run against one account still trips.
	// Limiter keys are hashed so raw input can't bloat the map.
	//
	// A tripped account limiter does NOT apply to a source that has
	// authenticated successfully before: otherwise ten bad guesses a minute
	// from any single stranger keep the real owner locked out for as long
	// as the attack runs. The stranger cannot borrow the owner's address
	// (RemoteAddr, or the proxy-appended X-Forwarded-For), and the per-IP
	// limiter above still caps the bcrypt work the owner's address can
	// cause.
	if !s.emailLimit.allow(limiterKey(email)) && !s.knownIPs.has(ip) {
		s.logBlocked(ip)
		s.renderLogin(w, r, token, "Too many attempts. Try again later.")
		return
	}

	var u user
	var hash string
	err = s.db.QueryRowContext(r.Context(),
		"SELECT id, name, email, password_hash FROM users WHERE email = ?", email).
		Scan(&u.ID, &u.Name, &u.Email, &hash)
	if err == sql.ErrNoRows {
		// Burn equivalent time to blunt user enumeration.
		hash = string(dummyHash)
		err = nil
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		// The attempted address is attacker-controlled, so it goes AFTER the
		// source: a crafted value cannot shift the filter's <HOST> capture,
		// and slog's TextHandler quotes/escapes it.
		slog.Warn("login: failed authentication", "from", ip, "user", email)
		s.renderLogin(w, r, token, "Invalid email or password.")
		return
	}

	if err := s.createSession(w, r, u.ID); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.knownIPs.add(ip)
	// Audited alongside the failures so an operator can tell "the owner
	// signed in" from an attack in the same stream; the fail2ban filter
	// ignoreregex excludes it.
	slog.Info("login: authenticated", "from", ip, "user", email)
	// Lazy cleanup of expired sessions and past-window yabs capabilities.
	s.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE expires_at < ?",
		time.Now().UTC().Format(time.RFC3339))
	(&model.YABSStore{DB: s.db}).PruneCaps(r.Context())

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout deletes the session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFromCtx(r.Context()); sess != nil {
		s.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token = ?", sess.Token)
	}
	// The clearing cookie carries the SAME attributes as the one it replaces.
	// Behind a TLS proxy the session cookie is Secure, and browsers implement
	// "leave secure cookies alone" (RFC 6265bis): a non-Secure Set-Cookie may
	// be refused, leaving a stale cookie in the browser. The session row is
	// already gone server-side either way, so this is tidiness, not auth.
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: s.cookieSecure(r), MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// logBlocked records a refused-by-rate-limit attempt for a fail2ban-style
// watcher, at most ONCE per window per source. Blocked requests are refused
// before the bcrypt verify, so they are cheap to send in bulk — logging every
// one would let a flood amplify into unbounded journald writes.
func (s *Server) logBlocked(ip string) {
	if s.blockLog.allow(limiterKey(ip)) {
		slog.Warn("login: rate-limited", "from", ip)
	}
}

// cookieSecure reports whether cookies must carry the Secure attribute:
// direct TLS, or an HTTPS-terminating proxy in front (IDLER_BEHIND_TLS_PROXY).
// One rule for session, flash, and login-CSRF cookies.
func (s *Server) cookieSecure(r *http.Request) bool {
	return r.TLS != nil || s.behindTLSProxy
}

// createSession inserts a new session row and sets the session cookie.
func (s *Server) createSession(w http.ResponseWriter, r *http.Request, userID int) error {
	token, err := randomToken(32)
	if err != nil {
		return err
	}
	csrf, err := randomToken(32)
	if err != nil {
		return err
	}
	expires := time.Now().Add(sessionDuration).UTC()
	if _, err := s.db.ExecContext(r.Context(),
		"INSERT INTO sessions (token, user_id, csrf_token, expires_at) VALUES (?, ?, ?, ?)",
		token, userID, csrf, expires.Format(time.RFC3339)); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		Expires:  expires,
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// loadSession populates the request context with the session + user when a
// valid session cookie is present. It never rejects the request; that is
// requireAuth's job.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), ctxMemo, &reqMemo{}))
		cookie, err := r.Cookie(sessionCookieName)
		if err != nil || cookie.Value == "" {
			next.ServeHTTP(w, r)
			return
		}

		var sess session
		var expires string
		var u user
		err = s.db.QueryRowContext(r.Context(), `
			SELECT s.token, s.user_id, s.csrf_token, s.expires_at, u.id, u.name, u.email
			FROM sessions s JOIN users u ON u.id = s.user_id
			WHERE s.token = ?`, cookie.Value).
			Scan(&sess.Token, &sess.UserID, &sess.CSRFToken, &expires, &u.ID, &u.Name, &u.Email)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		exp, err := time.Parse(time.RFC3339, expires)
		if err != nil || time.Now().After(exp) {
			next.ServeHTTP(w, r)
			return
		}
		sess.ExpiresAt = exp

		ctx := context.WithValue(r.Context(), ctxSession, &sess)
		ctx = context.WithValue(ctx, ctxUser, &u)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// requireAuth redirects to /login when no session is in the context.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sessionFromCtx(r.Context()) == nil {
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func sessionFromCtx(ctx context.Context) *session {
	sess, _ := ctx.Value(ctxSession).(*session)
	return sess
}

func userFromCtx(ctx context.Context) *user {
	u, _ := ctx.Value(ctxUser).(*user)
	return u
}

// clientIP extracts the remote IP for rate limiting. Behind an
// HTTPS-terminating proxy it trusts the LAST X-Forwarded-For entry
// (appended by the proxy itself).
func (s *Server) clientIP(r *http.Request) string {
	if s.behindTLSProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// rateLimiter is a small per-key sliding-window limiter. The map is
// swept only when it grows past sweepThreshold, and distinct keys are
// capped — beyond the cap NEW keys fail closed (XFF-spoofed key floods
// can't grow memory or make each call O(n)).
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

const (
	sweepThreshold = 256
	maxLimiterKeys = 4096
)

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: make(map[string][]time.Time), limit: limit, window: window}
}

// allow records an attempt for key and reports whether it is within the
// limit. Stale keys are pruned so the map can't grow unboundedly.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	// Always prune the REQUESTED key first — without this, old attempts on a
	// normal small map would linger forever and lock the account out for good.
	now := time.Now()
	cutoff := now.Add(-rl.window)
	kept := rl.hits[key][:0]
	for _, t := range rl.hits[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	// Sweep UNRELATED keys only when the map is big enough to matter.
	if len(rl.hits) > sweepThreshold {
		for k, times := range rl.hits {
			if k == key {
				continue
			}
			fresh := times[:0]
			for _, t := range times {
				if t.After(cutoff) {
					fresh = append(fresh, t)
				}
			}
			if len(fresh) == 0 {
				delete(rl.hits, k)
			} else {
				rl.hits[k] = fresh
			}
		}
	}

	if kept == nil && len(rl.hits) >= maxLimiterKeys {
		return false // new key beyond cap: fail closed
	}
	if len(kept) >= rl.limit {
		rl.hits[key] = kept
		return false
	}
	rl.hits[key] = append(kept, now)
	return true
}

// knownIPs remembers (hashed) source addresses that have completed a login,
// so the per-account limiter cannot be turned into a lockout of the owner
// from a familiar address. Bounded: past max entries the oldest is evicted.
// In-memory only — after a restart the first login from a familiar address
// during an active attack is throttled like a stranger's, which is the
// pre-existing behaviour, not a regression.
type knownIPs struct {
	mu   sync.Mutex
	seen map[string]time.Time
	max  int
}

func newKnownIPs(max int) *knownIPs {
	return &knownIPs{seen: make(map[string]time.Time), max: max}
}

// add records a successful login from ip. Nil-safe for bare test Servers.
func (k *knownIPs) add(ip string) {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	key := limiterKey(ip)
	if _, ok := k.seen[key]; !ok && len(k.seen) >= k.max {
		var oldestKey string
		var oldest time.Time
		for kk, t := range k.seen {
			if oldestKey == "" || t.Before(oldest) {
				oldestKey, oldest = kk, t
			}
		}
		delete(k.seen, oldestKey)
	}
	k.seen[key] = time.Now()
}

// has reports whether ip has completed a login before.
func (k *knownIPs) has(ip string) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	_, ok := k.seen[limiterKey(ip)]
	return ok
}

// constantTimeEqual compares two strings without early exit.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// limiterKey hashes a rate-limit key to a bounded hex digest, so
// attacker-controlled input (giant emails, spoofed IPs) can't bloat the
// limiter map.
func limiterKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:16])
}
