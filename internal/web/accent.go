package web

import (
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

// accentColorRe validates #rrggbb.
var accentColorRe = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

const defaultAccent = "#5b9cf8"

// uiPrefs reads the accent color + compact flag from settings.
func (s *Server) uiPrefs(r *http.Request) (accent string, compact bool) {
	var c string
	var cm int
	err := s.db.QueryRowContext(r.Context(),
		"SELECT accent_color, compact_mode FROM settings WHERE id = 1").Scan(&c, &cm)
	if err != nil || !accentColorRe.MatchString(c) {
		return defaultAccent, false
	}
	return strings.ToLower(c), cm != 0
}

// handleAccentCSS serves GET /static/accent.css — the accent variables
// derived from settings. The URL carries a color fingerprint (?c=rrggbb)
// so it can be cached like any other static asset.
func (s *Server) handleAccentCSS(w http.ResponseWriter, r *http.Request) {
	accent, _ := s.uiPrefs(r)
	red, green, blue := hexRGB(accent)

	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	fmt.Fprintf(w, ":root {\n  --acc: %s;\n  --accfg: %s;\n  --accSoft: rgba(%d, %d, %d, 0.14);\n}\n",
		accent, accentForeground(red, green, blue), red, green, blue)
}

// hexRGB parses "#rrggbb" into components (defaults to the standard accent).
func hexRGB(color string) (int, int, int) {
	if !accentColorRe.MatchString(color) {
		color = defaultAccent
	}
	r, _ := strconv.ParseInt(color[1:3], 16, 0)
	g, _ := strconv.ParseInt(color[3:5], 16, 0)
	b, _ := strconv.ParseInt(color[5:7], 16, 0)
	return int(r), int(g), int(b)
}

// accentForeground picks text color on accent by relative luminance:
// light accents get near-black text, dark accents white.
func accentForeground(r, g, b int) string {
	luminance := 0.2126*float64(r) + 0.7152*float64(g) + 0.0722*float64(b)
	if luminance/255 > 0.55 {
		return "#08101c"
	}
	return "#ffffff"
}
