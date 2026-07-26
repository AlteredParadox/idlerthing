package web

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/bcrypt"
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
	token, err := s.issueLoginCSRF(w)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.renderLogin(w, r, token, "")
}

// handleLoginPost validates credentials and creates a session on success.
func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	token, err := s.issueLoginCSRF(w) // rotate on every attempt
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	if !s.checkLoginCSRF(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if ip := s.clientIP(r); !s.limit.allow(ip) {
		s.renderLogin(w, r, token, "Too many attempts. Try again later.")
		return
	}

	email := strings.TrimSpace(r.FormValue("email"))
	password := r.FormValue("password")

	// Per-account limiter alongside the per-IP one, so a distributed
	// credential-stuffing run against one account still trips.
	if !s.emailLimit.allow(strings.ToLower(email)) {
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
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) != nil {
		s.renderLogin(w, r, token, "Invalid email or password.")
		return
	}

	if err := s.createSession(w, r, u.ID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Lazy cleanup of expired sessions.
	s.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE expires_at < ?",
		time.Now().UTC().Format(time.RFC3339))

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout deletes the session and clears the cookie.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if sess := sessionFromCtx(r.Context()); sess != nil {
		s.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token = ?", sess.Token)
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookieName, Value: "", Path: "/",
		HttpOnly: true, MaxAge: -1,
	})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
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
		Secure:   r.TLS != nil || s.behindTLSProxy,
		SameSite: http.SameSiteLaxMode,
	})
	return nil
}

// loadSession populates the request context with the session + user when a
// valid session cookie is present. It never rejects the request; that is
// requireAuth's job.
func (s *Server) loadSession(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// rateLimiter is a small per-key sliding-window limiter.
type rateLimiter struct {
	mu     sync.Mutex
	hits   map[string][]time.Time
	limit  int
	window time.Duration
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{hits: make(map[string][]time.Time), limit: limit, window: window}
}

// allow records an attempt for key and reports whether it is within the
// limit. Stale keys are pruned so the map can't grow unboundedly.
func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	cutoff := time.Now().Add(-rl.window)
	// Global sweep: drop keys with no recent attempts.
	for k, times := range rl.hits {
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

	kept := rl.hits[key]
	if len(kept) >= rl.limit {
		return false
	}
	rl.hits[key] = append(kept, time.Now())
	return true
}

// constantTimeEqual compares two strings without early exit.
func constantTimeEqual(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
