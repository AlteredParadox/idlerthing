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
	settings := s.memoSettings(r)
	if !accentColorRe.MatchString(settings.AccentColor) {
		return defaultAccent, false
	}
	return strings.ToLower(settings.AccentColor), settings.Compact
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

// faviconSVG is the sidebar logo mark (.logo-mark in app.css) as an icon:
// the accent-colored rounded square with a mono "i". The glyph is drawn as
// geometry rather than <text> because favicon rasterizers have no font stack
// to fall back on, and a 16px "i" set in a missing font renders as nothing.
// Verb 1 is the accent, verb 2 the contrasting foreground.
const faviconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">` +
	`<rect width="32" height="32" rx="8" fill="%[1]s"/>` +
	`<circle cx="16" cy="8.6" r="2.6" fill="%[2]s"/>` +
	`<rect x="13.4" y="13" width="5.2" height="11.4" rx="2.6" fill="%[2]s"/>` +
	`</svg>` + "\n"

// handleFaviconSVG serves GET /static/favicon.svg. The response is immutable,
// so the URL carries ?v=<assetVersion>-<rrggbb>: the content varies on BOTH
// the accent setting and the glyph above, and keying on the accent alone would
// pin browsers to a stale icon after any edit to faviconSVG. uiPrefs only ever
// returns a value matching accentColorRe, so nothing user-controlled reaches
// the markup.
func (s *Server) handleFaviconSVG(w http.ResponseWriter, r *http.Request) {
	accent, _ := s.uiPrefs(r)
	red, green, blue := hexRGB(accent)

	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	fmt.Fprintf(w, faviconSVG, accent, accentForeground(red, green, blue))
}

// handleFaviconICO answers the bare /favicon.ico that browsers, bots, and
// feed readers request regardless of <link rel="icon">. 204 rather than a
// real .ico: the point is to stop every page load logging a 404, and clients
// that honour the link tag already have the SVG.
func (s *Server) handleFaviconICO(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
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
