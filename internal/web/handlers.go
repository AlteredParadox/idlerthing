package web

import (
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

// Counts holds per-table row counts shown in the sidebar nav.
type Counts struct {
	Servers   int
	Shared    int
	Reseller  int
	Seedboxes int
	Domains   int
	Misc      int
	DNS       int
	IPs       int
	Locations int
	OS        int
	Providers int
	Labels    int
	YABS      int
	Notes     int
}

// counts runs a cheap COUNT(*) per table. Fine at this app's scale.
// countsTables maps Counts fields to tables in scan order.
const countsTables = `SELECT
	(SELECT COUNT(*) FROM servers),
	(SELECT COUNT(*) FROM shared_hosting),
	(SELECT COUNT(*) FROM reseller_hosting),
	(SELECT COUNT(*) FROM seedboxes),
	(SELECT COUNT(*) FROM domains),
	(SELECT COUNT(*) FROM misc_services),
	(SELECT COUNT(*) FROM dns),
	(SELECT COUNT(*) FROM ips),
	(SELECT COUNT(*) FROM locations),
	(SELECT COUNT(*) FROM os),
	(SELECT COUNT(*) FROM providers),
	(SELECT COUNT(*) FROM labels),
	(SELECT COUNT(*) FROM yabs),
	(SELECT COUNT(*) FROM notes)`

// counts runs a cheap COUNT(*) per table. Cached across requests keyed to
// the dashboard generation: only the first render after a write re-queries.
// (dash is nil in tests that build a bare Server — then no caching.)
func (s *Server) counts(r *http.Request) Counts {
	var gen uint64
	if s.dash != nil {
		s.dash.mu.Lock()
		gen = s.dash.gen
		cached, ok := s.dash.counts, s.dash.countsOK
		stale := !ok || s.dash.countsGen != gen
		s.dash.mu.Unlock()
		if !stale {
			return cached
		}
	}

	var c Counts
	// One round trip for all 14 counts.
	s.db.QueryRowContext(r.Context(), countsTables).Scan(
		&c.Servers, &c.Shared, &c.Reseller, &c.Seedboxes, &c.Domains,
		&c.Misc, &c.DNS, &c.IPs, &c.Locations, &c.OS, &c.Providers,
		&c.Labels, &c.YABS, &c.Notes)

	// Tag with the gen captured BEFORE the query: a write landing mid-query
	// bumps gen, so the result is stale on arrival and re-queried next time.
	if s.dash != nil {
		s.dash.mu.Lock()
		s.dash.countsGen, s.dash.counts, s.dash.countsOK = gen, c, true
		s.dash.mu.Unlock()
	}
	return c
}

// currentTheme reads the theme setting, defaulting to dark.
func (s *Server) currentTheme(r *http.Request) string {
	theme := s.memoSettings(r).Theme
	if theme != "dark" && theme != "light" {
		return "dark"
	}
	return theme
}

// handleNotFound renders a styled 404 for unmatched routes.
func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	data := s.newPageData(w, r, "Not found", "")
	w.WriteHeader(http.StatusNotFound)
	s.render(w, r, "notfound", data)
}

// handleThemePref flips settings.theme and redirects back to the referer.
func (s *Server) handleThemePref(w http.ResponseWriter, r *http.Request) {
	next := "light"
	if s.currentTheme(r) == "light" {
		next = "dark"
	}
	if _, err := s.db.ExecContext(r.Context(),
		"UPDATE settings SET theme = ?, updated_at = CURRENT_TIMESTAMP WHERE id = 1", next); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	target := safeRedirectTarget(r.Referer(), "/")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// shortHostnames reports whether the current user prefers hostnames
// stripped to their first DNS label on the servers list (user_prefs).
func (s *Server) shortHostnames(r *http.Request) bool {
	u := userFromCtx(r.Context())
	if u == nil {
		return false
	}
	var v string
	err := s.db.QueryRowContext(r.Context(),
		"SELECT value FROM user_prefs WHERE user_id = ? AND key = 'short_hostnames'", u.ID).Scan(&v)
	return err == nil && v == "1"
}

// listSort resolves the sort column and direction for a list page.
// Explicit URL params win and are persisted per user (user_prefs key
// "sort_<name>" = "col,dir"); absent params fall back to the saved pref,
// then to the list's default. This keeps the chosen sort across tab
// switches and page reloads.
func (s *Server) listSort(r *http.Request, name, defaultSort string) (sort, dir string) {
	q := r.URL.Query()
	sort, dir = q.Get("sort"), q.Get("dir")
	u := userFromCtx(r.Context())

	if sort != "" {
		if dir != "desc" {
			dir = "asc"
		}
		if u != nil {
			// Best-effort persistence of the sort choice (a side effect of
			// the GET) — log failures rather than erroring the page.
			if _, err := s.db.ExecContext(r.Context(),
				`INSERT INTO user_prefs (user_id, key, value) VALUES (?, 'sort_' || ?, ?)
				 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`,
				u.ID, name, sort+","+dir); err != nil {
				slog.Error("persist sort pref", "err", err)
			}
		}
		return sort, dir
	}

	if u != nil {
		var v string
		err := s.db.QueryRowContext(r.Context(),
			"SELECT value FROM user_prefs WHERE user_id = ? AND key = 'sort_' || ?", u.ID, name).Scan(&v)
		if err == nil {
			if col, d, ok := strings.Cut(v, ","); ok && col != "" {
				if d != "desc" {
					d = "asc"
				}
				return col, d
			}
		}
	}
	return defaultSort, "asc"
}

// handleShortHostnamesPref flips the short_hostnames user pref and
// redirects back to the referer.
func (s *Server) handleShortHostnamesPref(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u != nil {
		next := "1"
		if s.shortHostnames(r) {
			next = "0"
		}
		if _, err := s.db.ExecContext(r.Context(),
			`INSERT INTO user_prefs (user_id, key, value) VALUES (?, 'short_hostnames', ?)
			 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`, u.ID, next); err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
	}

	target := safeRedirectTarget(r.Referer(), "/servers")
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// safeRedirectTarget validates a Referer header into a redirect target:
// same-origin absolute paths only — no protocol-relative //evil.com, no
// backslashes, no scheme/host.
func safeRedirectTarget(referer, fallback string) string {
	ref, err := url.Parse(referer)
	if err != nil {
		return fallback
	}
	p := ref.Path
	if p == "" || strings.HasPrefix(p, "//") || strings.Contains(p, "\\") {
		return fallback
	}
	if ref.RawQuery != "" {
		p += "?" + ref.RawQuery
	}
	return p
}
