package web

import (
	"database/sql"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// flashCookieName carries a one-time status message across a redirect.
const flashCookieName = "idler_flash"

// flash is a one-time banner message shown at the top of the content area.
type flash struct {
	Kind    string // "ok" or "err"
	Message string
}

// assetVersion busts the immutable static-asset cache when assets change.
const assetVersion = "20"

// pageData is the root template context for full-page renders.
type pageData struct {
	Title     string
	Nav       string // active nav item key, e.g. "servers"
	Theme     string
	User      *user
	CSRFToken string
	Counts    Counts
	Flash     *flash
	Error     string // login page error
	AssetV    string
	AccentC   string // accent color without '#', fingerprint for accent.css
	Compact   bool
	Data      any // page-specific payload
}

// templates is the registry of parsed pages. Each page shares layout.html
// and provides a {{define "content"}} block; the login page is standalone.
type templates struct {
	pages map[string]*template.Template
}

// templateFuncs holds shared template helpers (date/unit formatting,
// pricing labels, form pre-fill helpers).
var templateFuncs = template.FuncMap{
	// dateFmt renders an ISO date/datetime string as YYYY-MM-DD.
	"dateFmt": dateOnly,
	// nstr unwraps a sql.NullString for form values.
	"nstr": func(n sql.NullString) string { return n.String },
	// nint unwraps a sql.NullInt64 for form values ("" when NULL).
	"nint": func(n sql.NullInt64) string {
		if !n.Valid {
			return ""
		}
		return strconv.FormatInt(n.Int64, 10)
	},
	// seq4 returns disk row indexes 1..4 for the fixed disk form rows.
	"seq4": func() []int { return []int{1, 2, 3, 4} },
	// seqTerms returns pricing term values 1..7.
	"seqTerms": func() []int { return []int{1, 2, 3, 4, 5, 6, 7} },
	// termLabel renders a pricing term's human label.
	"termLabel": model.TermLabel,
	// dnsTypes lists the DNS record types for selects.
	"dnsTypes": func() []string { return model.DNSTypes },
	// mbpsFmt renders a speed value compactly.
	"mbpsFmt": func(f float64) string {
		if f == 0 {
			return "—"
		}
		if f >= 100 {
			return strconv.FormatFloat(f, 'f', 0, 64)
		}
		return strconv.FormatFloat(f, 'f', 1, 64)
	},
	// fmtMB renders whole MB in the friendliest unit (1024-based).
	"fmtMB":     fmtMB,
	"fmtNullMB": fmtNullMB,
	// bwDisplay renders bandwidth ("∞ Unlimited" when NULL).
	"bwDisplay": bwDisplay,
	// unitVal/unitName pre-fill unit input-groups from stored MB.
	"unitVal":  unitVal,
	"unitName": unitName,
	"mbValid":  mbValid,
	// dict builds a map for passing multiple values to a partial.
	"dict": func(pairs ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(pairs); i += 2 {
			if k, ok := pairs[i].(string); ok {
				m[k] = pairs[i+1]
			}
		}
		return m
	},
	// priceFmt renders e.g. "$12.00/mo".
	"priceFmt": priceDisplay,
	// indexDisk returns the (1-based) i-th disk or nil.
	"indexDisk": func(disks []model.ServerDisk, i int) *model.ServerDisk {
		if i >= 1 && i <= len(disks) {
			return &disks[i-1]
		}
		return nil
	},
}

// parseTemplates builds the registry: layout.html + every page template.
func parseTemplates() (*templates, error) {
	entries, err := fs.ReadDir(assetsFS, "assets/templates")
	if err != nil {
		return nil, err
	}

	t := &templates{pages: make(map[string]*template.Template)}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".html") || name == "partials.html" {
			continue
		}
		page := strings.TrimSuffix(name, ".html")

		files := []string{"assets/templates/partials.html", "assets/templates/" + name}
		if page != "login" && page != "public" { // standalone pages, no sidebar layout
			files = append([]string{"assets/templates/layout.html"}, files...)
		}
		tm, err := template.New(name).Funcs(templateFuncs).ParseFS(assetsFS, files...)
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		t.pages[page] = tm
	}
	return t, nil
}

// render renders a page. htmx requests (HX-Request header) get only the
// content block; everything else gets the full layout.
func (s *Server) render(w http.ResponseWriter, r *http.Request, page string, data pageData) {
	tm, ok := s.tmpl.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")

	root := "layout"
	if page == "login" {
		root = "login"
	} else if r.Header.Get("HX-Request") == "true" {
		root = "content"
	}
	data.AssetV = assetVersion
	accent, compact := s.uiPrefs(r)
	data.AccentC = accent[1:]
	data.Compact = compact
	if err := tm.ExecuteTemplate(w, root, data); err != nil {
		slog.Error("render template", "page", page, "err", err)
	}
}

// renderNamed renders a single named template (partial) from a page's
// template set — used for htmx partial swaps.
func (s *Server) renderNamed(w http.ResponseWriter, page, name string, data pageData) {
	tm, ok := s.tmpl.pages[page]
	if !ok {
		http.Error(w, "unknown page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := tm.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render partial", "page", page, "name", name, "err", err)
	}
}

// renderLogin renders the standalone login page with an optional error.
func (s *Server) renderLogin(w http.ResponseWriter, r *http.Request, csrfToken, errMsg string) {
	s.render(w, r, "login", pageData{
		Title:     "Sign in",
		Theme:     s.currentTheme(r),
		CSRFToken: csrfToken,
		Error:     errMsg,
	})
}

// newPageData fills the shared template context for an authenticated page.
func (s *Server) newPageData(w http.ResponseWriter, r *http.Request, title, nav string) pageData {
	d := pageData{
		Title: title,
		Nav:   nav,
		Theme: s.currentTheme(r),
		User:  userFromCtx(r.Context()),
		Flash: s.popFlash(w, r),
	}
	// htmx partials never render the sidebar — skip the counts query.
	if r.Header.Get("HX-Request") != "true" {
		d.Counts = s.counts(r)
	}
	if sess := sessionFromCtx(r.Context()); sess != nil {
		d.CSRFToken = sess.CSRFToken
	}
	return d
}

// setFlash plants a one-time flash cookie (survives exactly one redirect).
// The value is URL-escaped since cookie values must be ASCII.
func (s *Server) setFlash(w http.ResponseWriter, r *http.Request, kind, message string) {
	http.SetCookie(w, &http.Cookie{
		Name:     flashCookieName,
		Value:    url.QueryEscape(kind + ":" + message),
		Path:     "/",
		HttpOnly: true,
		Secure:   s.cookieSecure(r),
		SameSite: http.SameSiteLaxMode,
	})
}

// popFlash reads and clears the flash cookie, returning nil when absent.
func (s *Server) popFlash(w http.ResponseWriter, r *http.Request) *flash {
	cookie, err := r.Cookie(flashCookieName)
	if err != nil || cookie.Value == "" {
		return nil
	}
	// Same attributes as the cookie being cleared (see handleLogout): a
	// non-Secure clear can be refused where the original was Secure, which
	// would make the flash sticky instead of one-shot.
	http.SetCookie(w, &http.Cookie{
		Name: flashCookieName, Value: "", Path: "/", HttpOnly: true,
		Secure: s.cookieSecure(r), MaxAge: -1,
	})
	raw, err := url.QueryUnescape(cookie.Value)
	if err != nil {
		return nil
	}
	kind, msg, _ := strings.Cut(raw, ":")
	if kind != "ok" && kind != "err" {
		return nil
	}
	return &flash{Kind: kind, Message: msg}
}
