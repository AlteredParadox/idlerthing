package web

import (
	"net/http"
	"strconv"
	"strings"
)

// colDef describes one choosable servers-list column.
type colDef struct {
	Key   string
	Label string
}

// serverColumns lists all choosable columns in display order (Actions is
// always last and not choosable). Order per spec:
// Hostname, Type, OS, CPU, CPU Model, RAM, Disk, BW, Link Speed, Net,
// Location, Provider, Price, Price/YR, Due, Since, Uptime.
var serverColumns = []colDef{
	{"hostname", "Hostname"},
	{"type", "Type"},
	{"os", "OS"},
	{"cpu", "CPU"},
	{"cpu_model", "CPU Model"},
	{"ram", "RAM"},
	{"disk", "Disk"},
	{"bw", "BW"},
	{"link_speed", "Link Speed"},
	{"net", "Net"},
	{"location", "Location"},
	{"provider", "Provider"},
	{"price", "Price"},
	{"price_yr", "Price/YR (USD)"},
	{"due", "Due"},
	{"since", "Since"},
	{"uptime", "Uptime"},
}

// defaultHiddenCols applies when the user has no servers_cols pref yet.
const defaultHiddenCols = "cpu_model,link_speed,price_yr,since,uptime"

// hiddenCols reads the user's hidden-column set from user_prefs.
// An absent pref means the default hidden set; an existing (even empty)
// pref is honored as-is.
func (s *Server) hiddenCols(r *http.Request) map[string]bool {
	hidden := map[string]bool{}
	apply := func(v string) {
		for _, k := range strings.Split(v, ",") {
			if k != "" {
				hidden[k] = true
			}
		}
	}
	v, ok := s.memoPref(r, "servers_cols")
	if !ok {
		apply(defaultHiddenCols) // no pref row yet
		return hidden
	}
	apply(v)
	return hidden
}

// Col reports whether a column is visible.
func (v serversListView) Col(key string) bool {
	return !v.HiddenCols[key]
}

// chooserItem is one checkbox row of the column chooser.
type chooserItem struct {
	Key     string
	Label   string
	Visible bool
}

// ChooserItems lists all columns for the chooser panel.
func (v serversListView) ChooserItems() []chooserItem {
	var out []chooserItem
	for _, c := range serverColumns {
		out = append(out, chooserItem{Key: c.Key, Label: c.Label, Visible: v.Col(c.Key)})
	}
	return out
}

// VisibleCount renders the "Columns N/M" button label counts.
func (v serversListView) VisibleCount() int {
	n := 0
	for _, c := range serverColumns {
		if v.Col(c.Key) {
			n++
		}
	}
	return n
}

// TotalCount is the chooser column total.
func (v serversListView) TotalCount() int {
	return len(serverColumns)
}

// handleServerColsPref handles POST /prefs/servers-cols — persists the
// hidden-column set (always written, so unhide-everything sticks too).
func (s *Server) handleServerColsPref(w http.ResponseWriter, r *http.Request) {
	u := userFromCtx(r.Context())
	if u == nil {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}
	var hidden []string
	for _, c := range serverColumns {
		if r.FormValue("col_"+c.Key) == "" {
			hidden = append(hidden, c.Key)
		}
	}
	if _, err := s.db.ExecContext(r.Context(),
		`INSERT INTO user_prefs (user_id, key, value) VALUES (?, 'servers_cols', ?)
		 ON CONFLICT(user_id, key) DO UPDATE SET value = excluded.value`,
		u.ID, strings.Join(hidden, ",")); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, safeRedirectTarget(r.Referer(), "/servers"), http.StatusSeeOther)
}

// linkSpeedDisplay formats mbps (`500 Mbps`, `1 Gbps`, `10 Gbps`).
func linkSpeedDisplay(mbps int64) string {
	if mbps <= 0 {
		return "—"
	}
	if mbps >= 1000 {
		if mbps%1000 == 0 {
			return strings.TrimSpace(strconv.FormatInt(mbps/1000, 10)) + " Gbps"
		}
		return trim2(float64(mbps)/1000) + " Gbps"
	}
	return strconv.FormatInt(mbps, 10) + " Mbps"
}
