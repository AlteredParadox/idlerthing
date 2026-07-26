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

// renderSettings renders the settings page, overlaying the current DB state.
func (s *Server) renderSettings(w http.ResponseWriter, r *http.Request, view settingsView) {
	var defaultCur, dashCur, theme, accent, promURL string
	var dueSoon, recent, public, compact, promEnabled int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT default_currency, dashboard_currency, due_soon_amount,
			recently_added_amount, theme, servers_public, accent_color, compact_mode,
			prometheus_enabled, prometheus_url
		FROM settings WHERE id = 1`).
		Scan(&defaultCur, &dashCur, &dueSoon, &recent, &theme, &public, &accent, &compact,
			&promEnabled, &promURL)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	view.DefaultCurrency = defaultCur
	view.DashboardCurrency = dashCur
	view.DueSoon = dueSoon
	view.RecentlyAdded = recent
	view.Theme = theme
	view.ServersPublic = public != 0
	view.AccentColor = accent
	view.Compact = compact != 0
	view.PrometheusEnabled = promEnabled != 0
	view.PrometheusURL = promURL
	view.Currencies = currencies

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
		// Field absent (older clients) — keep the current value.
		s.db.QueryRowContext(r.Context(),
			"SELECT accent_color FROM settings WHERE id = 1").Scan(&accent)
		if !accentColorRe.MatchString(accent) {
			accent = defaultAccent
		}
	} else if !accentColorRe.MatchString(accent) {
		errs["accent_color"] = "Must be a hex color like #5b9cf8."
	}

	if len(errs) > 0 {
		setFlash(w, "err", "Please fix the errors below.")
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
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	setFlash(w, "ok", "Settings saved.")
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
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte(current)) != nil {
		setFlash(w, "err", "Current password is wrong.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if len(newPass) < 8 {
		setFlash(w, "err", "New password must be at least 8 characters.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	if newPass != confirm {
		setFlash(w, "err", "New passwords do not match.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}

	newHash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(),
		`UPDATE users SET password_hash = ?, api_token_hash = NULL,
			updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		string(newHash), u.ID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Invalidate ALL sessions for the user — including the current one,
	// since a stolen copy of that cookie would otherwise stay valid. A
	// fresh session is issued on the response right after.
	if _, err := tx.ExecContext(r.Context(),
		"DELETE FROM sessions WHERE user_id = ?", u.ID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	if err := s.createSession(w, r, u.ID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	setFlash(w, "ok", "Password changed. All sessions were rotated and the API token was revoked.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// generateAPIToken creates a new API token, stores its sha256, and displays
// the plaintext exactly once.
func (s *Server) generateAPIToken(w http.ResponseWriter, r *http.Request, u *user) {
	token, err := randomToken(32)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	sum := sha256.Sum256([]byte(token))
	if _, err := s.db.ExecContext(r.Context(),
		"UPDATE users SET api_token_hash = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?",
		hex.EncodeToString(sum[:]), u.ID); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.renderSettings(w, r, settingsView{RevealedToken: token})
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
		setFlash(w, "err", "No Prometheus URL configured — save settings first.")
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	samples, err := prom.New(baseURL).Query(r.Context(), "up")
	if err != nil {
		setFlash(w, "err", "Connection failed: "+err.Error())
		http.Redirect(w, r, "/settings", http.StatusSeeOther)
		return
	}
	up := 0
	for _, sample := range samples {
		if sample.Value == 1 {
			up++
		}
	}
	setFlash(w, "ok", "Connected — "+strconv.Itoa(up)+" of "+strconv.Itoa(len(samples))+" targets up.")
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}
