package web

import "net/http"

// Per-request memo: the settings singleton and user_prefs are read by
// several helpers during one render (theme, accent, prometheus, due-soon,
// sort prefs, column prefs). loadSession stashes a reqMemo in the request
// context; each loader fills it lazily ONCE per request. No cross-request
// caching — writes during the request are re-read on the next one.

type reqMemo struct {
	settings *settingsRow
	prefs    map[string]string
}

// settingsRow is the singleton settings row in struct form.
type settingsRow struct {
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
}

// memo returns the request's memo (installed by loadSession; created
// defensively for tests that bypass the middleware).
func memo(r *http.Request) *reqMemo {
	m, _ := r.Context().Value(ctxMemo).(*reqMemo)
	if m == nil {
		m = &reqMemo{}
	}
	return m
}

// memoSettings loads the settings row at most once per request.
func (s *Server) memoSettings(r *http.Request) *settingsRow {
	m := memo(r)
	if m.settings != nil {
		return m.settings
	}
	row := &settingsRow{Theme: "dark", AccentColor: defaultAccent}
	var theme, accent, promURL string
	var serversPublic, compact, promEnabled int
	err := s.db.QueryRowContext(r.Context(), `
		SELECT default_currency, dashboard_currency, due_soon_amount,
			recently_added_amount, theme, servers_public, accent_color,
			compact_mode, prometheus_enabled, prometheus_url
		FROM settings WHERE id = 1`).
		Scan(&row.DefaultCurrency, &row.DashboardCurrency, &row.DueSoon,
			&row.RecentlyAdded, &theme, &serversPublic, &accent, &compact,
			&promEnabled, &promURL)
	if err == nil {
		row.Theme = theme
		row.ServersPublic = serversPublic != 0
		row.AccentColor = accent
		row.Compact = compact != 0
		row.PrometheusEnabled = promEnabled != 0
		row.PrometheusURL = promURL
	}
	m.settings = row
	return row
}

// memoPref loads ALL of the user's prefs at most once per request.
func (s *Server) memoPref(r *http.Request, key string) (string, bool) {
	m := memo(r)
	if m.prefs == nil {
		m.prefs = map[string]string{}
		if u := userFromCtx(r.Context()); u != nil {
			rows, err := s.db.QueryContext(r.Context(),
				"SELECT key, value FROM user_prefs WHERE user_id = ?", u.ID)
			if err == nil {
				defer rows.Close()
				for rows.Next() {
					var k, v string
					if err := rows.Scan(&k, &v); err == nil {
						m.prefs[k] = v
					}
				}
			}
		}
	}
	v, ok := m.prefs[key]
	return v, ok
}
