package web

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"

	"golang.org/x/crypto/bcrypt"

	"idlerthing/internal/prom"
)

// settingsView is the template payload for GET /settings.
type settingsView struct {
	DefaultCurrency   string
	DashboardCurrency string
	DueSoon           int
	RecentlyAdded     int
	Theme             string
	ServersPublic     bool
	AccentColor       string
	Compact           bool
	PrometheusEnabled bool
	PrometheusURL     string
	Currencies        []string
	TokenSet          bool
	RevealedToken     string // shown once after generation
	Errors            map[string]string
}

// handleSettingsGet renders GET /settings.
func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	s.renderSettings(w, r, settingsView{})
}

// renderSettings renders the settings page, overlaying the current state
// via the per-request memo (also correct on the POST-error path: the memo
// is per-request and the success path redirects).
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, view settingsView) {
	row := s.memoSettings(r)
	view.DefaultCurrency = row.DefaultCurrency
	view.DashboardCurrency = row.DashboardCurrency
	view.DueSoon = row.DueSoon
	view.RecentlyAdded = row.RecentlyAdded
	view.Theme = row.Theme
	view.ServersPublic = row.ServersPublic
	view.AccentColor = row.AccentColor
	view.Compact = row.Compact
	view.PrometheusEnabled = row.PrometheusEnabled
	view.PrometheusURL = row.PrometheusURL
	view.Currencies = currencies
	view.RevealedToken = s.popRevealedToken(w, r)

	u := userFromCtx(r.Context())
	if u != nil {
		var tokenHash *string
		s.db.QueryRowContext(r.Context(),
			"SELECT api_token_hash FROM users WHERE id = ?", u.ID).Scan(&tokenHash)
		view.TokenSet = tokenHash != nil && *tokenHash != ""
	}

	data := s.newPageData(w, r, "Settings", "settings")
	data.Data = view
	s.render(w, r, "settings", data)
}

// handleSettingsUpdate handles POST /settings (general + display sections).
func (s *Server) handleSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	errs := map[string]string{}

	defaultCur := validCurrency(r.FormValue("default_currency"))
	dashCur := validCurrency(r.FormValue("dashboard_currency"))
	theme := r.FormValue("theme")
	if theme != "dark" && theme != "light" {
		theme = "dark"
	}
	dueSoon, err := strconv.Atoi(r.FormValue("due_soon_amount"))
	if err != nil || dueSoon < 1 || dueSoon > 365 {
		errs["due_soon_amount"] = "Must be between 1 and 365."
	}
	recent, err := strconv.Atoi(r.FormValue("recently_added_amount"))
	if err != nil || recent < 1 || recent > 100 {
		errs["recently_added_amount"] = "Must be between 1 and 100."
	}
	serversPublic := r.FormValue("servers_public") != ""
	compact := r.FormValue("compact_mode") != ""
	promEnabled := r.FormValue("prometheus_enabled") != ""
	promURL := strings.TrimSpace(r.FormValue("prometheus_url"))
	if promEnabled && !validPromURL(promURL) {
		errs["prometheus_url"] = "Must be an http(s) URL, e.g. http://localhost:9090."
	}

	accent := strings.ToLower(strings.TrimSpace(r.FormValue("accent_color")))
	if accent == "" {
		// Field absent (older clients) — keep the current (memoized) value.
		accent = s.memoSettings(r).AccentColor
		if !accentColorRe.MatchString(accent) {
			accent = defaultAccent
		}
	} else if !accentColorRe.MatchString(accent) {
		errs["accent_color"] = "Must be a hex color like #5b9cf8."
	}

	if len(errs) > 0 {
		s.setFlash(w, r, "err", "Please fix the errors below.")
		s.renderSettings(w, r, settingsView{Errors: errs})
		return
	}

	if _, err := s.db.ExecContext(r.Context(), `
		UPDATE settings SET default_currency = ?, dashboard_currency = ?,
			due_soon_amount = ?, recently_added_amount = ?, theme = ?,
			servers_public = ?, accent_color = ?, compact_mode = ?,
			prometheus_enabled = ?, prometheus_url = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = 1`,
		defaultCur, dashCur, dueSoon, recent, theme, boolToIntWeb(serversPublic),
		accent, boolToIntWeb(compact), boolToIntWeb(promEnabled), promURL); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", "Settings saved.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// handleSettingsAccount handles POST /settings/account (action=password|token).
func (s *Server) handleSettingsAccount(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	switch r.FormValue("action") {
	case "password":
		s.changePassword(w, r, u)
	case "token":
		s.generateAPIToken(w, r, u)
	default:
		http.Error(w, "bad request", http.StatusBadRequest)
	}
}

// changePassword validates + updates the password, killing other sessions.
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, u *user) {
	current := r.FormValue("current_password")
	newPass := r.FormValue("new_password")
	confirm := r.FormValue("confirm_password")

	var hash string
	if err := s.db.QueryRowContext(r.Context(),
		"SELECT password_hash FROM users WHERE id = ?", u.ID).Scan(&hash); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		s.setFlash(w, r, "err", "Current password is wrong.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if len(newPass) < 8 || len(newPass) > 72 {
		// bcrypt reads at most 72 bytes — reject longer instead of 500ing.
		s.setFlash(w, r, "err", "New password must be 8–72 characters.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if newPass != confirm {
		s.setFlash(w, r, "err", "New passwords do not match.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE users SET password_hash = ?, api_token_hash = NULL,
			updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		string(newHash), u.ID); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	// Invalidate ALL sessions for the user — including the current one,
	// since a stolen copy of that cookie would otherwise stay valid. A
	// fresh session is issued on the response right after.
	if _, err := tx.ExecContext(r.Context(),
		"DELETE FROM sessions WHERE user_id = ?", u.ID); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	if err := s.createSession(w, r, u.ID); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.setFlash(w, r, "ok", "Password changed. All sessions were rotated and the API token was revoked.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// apiTokenCookieName carries the freshly generated API token across the
// PRG redirect — the plaintext is shown exactly once.
const apiTokenCookieName = "idler_api_token"

// generateAPIToken creates a new API token, stores its sha256, and redirects
// (PRG) with the plaintext riding a one-time cookie — rendering it directly
// with a 200 would regenerate+revoke the token on every F5.
func (s *Server) generateAPIToken(w http.ResponseWriter, r *http.Request, u *user) {
	token, err := randomToken(32)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256([]byte(token))
	if _, err := s.db.ExecContext(r.Context(),
		"UPDATE users SET api_token_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		hex.EncodeToString(sum[:]), u.ID); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name:     apiTokenCookieName,
		Value:    token,
		Path:     "/settings",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// popRevealedToken reads and clears the one-time API-token cookie.
func (s *Server) popRevealedToken(w http.ResponseWriter, r *http.Request) string {
	cookie, err := r.Cookie(apiTokenCookieName)
	if err != nil || cookie.Value == "" {
		return ""
	}
	http.SetCookie(w, &http.Cookie{
		Name: apiTokenCookieName, Value: "", Path: "/settings", HttpOnly: true, MaxAge: -1,
	})
	return cookie.Value
}

// validCurrency returns the code when supported, else USD.
func validCurrency(code string) string {
	for _, c := range currencies {
		if code == c {
			return code
		}
	}
	return "USD"
}

func boolToIntWeb(b bool) int {
	if b {
		return 1
	}
	return 0
}

// handlePrometheusTest handles POST /settings/prometheus/test — runs an `up`
// instant query against the configured URL and flashes the outcome.
func (s *Server) handlePrometheusTest(w http.ResponseWriter, r *http.Request) {
	_, baseURL := s.promSettings(r)
	if baseURL == "" {
		s.setFlash(w, r, "err", "No Prometheus URL configured — save settings first.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	samples, err := prom.New(baseURL).Query(r.Context(), "up")
	if err != nil {
		s.setFlash(w, r, "err", "Connection failed: "+err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	up := 0
	for _, sample := range samples {
		if sample.Value == 1 {
			up++
		}
	}
	s.setFlash(w, r, "ok", "Connected — "+strconv.Itoa(up)+" of "+strconv.Itoa(len(samples))+" targets up.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
