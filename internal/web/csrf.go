package web

import (
	"net/http"
	"time"
)

// loginCSRFCookieName carries the double-submit token for the pre-session
// login form. Once authenticated, the per-session token from
// sessions.csrf_token is used instead.
const loginCSRFCookieName = "idler_login_csrf"

// csrfProtect rejects unsafe methods whose CSRF token does not match the
// session token. It must run inside requireAuth (session in context).
// /api/ routes use Bearer-token auth and stay outside this chain.
func (s *Server) csrfProtect(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}

		sess := sessionFromCtx(r.Context())
		if sess == nil {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		// Form field first, then the htmx header.
		token := r.FormValue("csrf_token")
		if token == "" {
			token = r.Header.Get("X-CSRF-Token")
		}
		if token == "" || !constantTimeEqual(token, sess.CSRFToken) {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// issueLoginCSRF sets (or rotates) the double-submit cookie for the login
// form and returns the token to embed in the form. The cookie is HttpOnly:
// the server plants both copies, so script access is unnecessary.
func (s *Server) issueLoginCSRF(w http.ResponseWriter) (string, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     loginCSRFCookieName,
		Value:    token,
		Path:     "/login",
		HttpOnly: true,
		Secure:   s.behindTLSProxy,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(30 * time.Minute),
	})
	return token, nil
}

// checkLoginCSRF verifies the double-submit pair on POST /login.
func (s *Server) checkLoginCSRF(r *http.Request) bool {
	cookie, err := r.Cookie(loginCSRFCookieName)
	if err != nil || cookie.Value == "" {
		return false
	}
	return constantTimeEqual(cookie.Value, r.FormValue("csrf_token"))
}
