package web

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
	"idlerthing/internal/pricing"
)

// ---------- Generic list page machinery ----------

// statCard is one summary card above a list table.
type statCard struct {
	Label string
	Value string
}

// listColumn is one table header.
type listColumn struct {
	Key      string // sort key; "" = not sortable
	Label    string
	Sortable bool
}

// listCell is one table cell. Dot renders a status dot before Main.
type listCell struct {
	Main  string
	Sub   string
	Class string // extra td class, e.g. "mono due-warn"
	Dot   string // "ok", "off", or ""
	Badge bool   // render Main as a badge
	Link  string // wrap Main in a link
}

// listRow is one table row.
type listRow struct {
	Cells         []listCell
	Link          string // row click target
	EditURL       string
	DeleteURL     string
	DeleteConfirm string
}

// listView is the template payload for generic list pages.
type listView struct {
	listNav
	Title         string
	Sub           string
	AddLabel      string
	SearchHint    string
	Cards         []statCard
	Columns       []listColumn
	Rows          []listRow
	ActiveCount   int
	InactiveCount int
	RowCount      int
	EmptyTitle    string
	EmptySub      string
}

// section describes one generic service section (list + delete + detail).
type section struct {
	Base        string // "/shared"
	Kind        string // nav key
	Title       string
	ServiceType int // pricings/extras service_type
	AddLabel    string
	SearchHint  string
	EmptyTitle  string
	EmptySub    string
	DefaultSort string
	Columns     []listColumn
	// List returns rows for the current filter/sort.
	List func(r *http.Request, opts model.ListOptions) ([]listRow, error)
	// Counts returns active and inactive totals.
	Counts func(ctx context.Context) (int, int, error)
	// Cards returns the summary stat cards.
	Cards func(r *http.Request, active, inactive int) []statCard
	// Delete removes one entity by id.
	Delete func(ctx context.Context, id int64) error
	// Detail builds the detail view; sql.ErrNoRows → 404.
	Detail func(r *http.Request, id int64) (*detailView, error)
}

// parseListOptions reads status/q from the request; sort/dir are
// resolved by listSort (a per-user pref) at the call site.
func parseListOptions(r *http.Request) model.ListOptions {
	q := r.URL.Query()
	opts := model.ListOptions{
		Status: q.Get("status"),
		Q:      strings.TrimSpace(q.Get("q")),
	}
	if opts.Status == "" {
		opts.Status = "active"
	}
	return opts
}

// handleSectionList renders a generic service list page (or htmx partial).
func (s *Server) handleSectionList(w http.ResponseWriter, r *http.Request, sec *section) {
	opts := parseListOptions(r)
	opts.Sort, opts.Dir = s.listSort(r, strings.TrimPrefix(sec.Base, "/"), sec.DefaultSort)

	rows, err := sec.List(r, opts)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	// htmx swaps re-render only the table partial — rows, sort state, and the
	// CSRF token. Skip the counts/cards queries the layout would need.
	if r.Header.Get("HX-Request") == "true" {
		data := s.newPageData(w, r, sec.Title, sec.Kind)
		data.Data = listView{listNav: listNav{
			Base: sec.Base, Status: opts.Status, Q: opts.Q, Sort: opts.Sort, Dir: opts.Dir,
		}, Rows: rows}
		s.renderNamed(w, "service_list", "list_table", data)
		return
	}

	active, inactive, err := sec.Counts(r.Context())
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	view := listView{
		listNav:       listNav{Base: sec.Base, Status: opts.Status, Q: opts.Q, Sort: opts.Sort, Dir: opts.Dir},
		Title:         sec.Title,
		Sub:           fmt.Sprintf("%d active · %d inactive", active, inactive),
		AddLabel:      sec.AddLabel,
		SearchHint:    sec.SearchHint,
		Cards:         sec.Cards(r, active, inactive),
		Columns:       sec.Columns,
		Rows:          rows,
		ActiveCount:   active,
		InactiveCount: inactive,
		RowCount:      len(rows),
		EmptyTitle:    sec.EmptyTitle,
		EmptySub:      sec.EmptySub,
	}

	data := s.newPageData(w, r, sec.Title, sec.Kind)
	data.Data = view
	s.render(w, r, "service_list", data)
}

// handleSectionDelete handles POST {base}/{id}/delete generically.
func (s *Server) handleSectionDelete(w http.ResponseWriter, r *http.Request, sec *section) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := sec.Delete(r.Context(), id); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	setFlash(w, "ok", strings.TrimSuffix(sec.Title, "s")+" deleted.")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", sec.Base)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, sec.Base, http.StatusSeeOther)
}

// handleSectionDetail renders GET {base}/{id} generically.
func (s *Server) handleSectionDetail(w http.ResponseWriter, r *http.Request, sec *section) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	view, err := sec.Detail(r, id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	view.Extras = s.buildExtras(r, id, sec.ServiceType)
	data := s.newPageData(w, r, view.Title, sec.Kind)
	data.Data = view
	s.render(w, r, "service_detail", data)
}

// ---------- Generic detail page ----------

// detailBadge is a header badge on a detail page.
type detailBadge struct {
	Label string
	Class string // "", "badge-ok", "badge-off", "badge-warn"
}

// kvPair is one key/value row of an info card.
type kvPair struct {
	K     string
	V     string
	Mono  bool
	Badge bool
}

// infoCard is one card of key/value pairs.
type infoCard struct {
	Title string
	Pairs []kvPair
	Empty string // shown when Pairs is empty; hidden when ""
}

// detailView is the template payload for generic detail pages.
type detailView struct {
	Title         string
	Mono          bool // render title in mono font
	Badges        []detailBadge
	EditURL       string
	DeleteURL     string
	DeleteConfirm string
	Cards         []infoCard
	Extras        *extrasView
}

// ---------- Shared pricing/detail helpers ----------

// costSumUSDFor sums pricings for one service type, normalized per month in
// USD. Only ACTIVE services with active pricings count. With exchange rates
// available all currencies convert; otherwise it degrades to summing USD
// pricings only (native amounts show in the table regardless). One-time
// pricings are excluded (MonthlyUSD rejects them).
func (s *Server) costSumUSDFor(r *http.Request, serviceType int) float64 {
	rows, err := s.db.QueryContext(r.Context(),
		`SELECT p.currency, p.price, p.term FROM pricings p
		 JOIN `+model.ServiceTable[serviceType]+` svc ON svc.id = p.service_id AND svc.active = 1
		 WHERE p.service_type = ? AND p.active = 1`, serviceType)
	if err != nil {
		return 0
	}
	defer rows.Close()
	rates, _ := s.rates.Get(r.Context())
	sum := 0.0
	for rows.Next() {
		var currency string
		var price float64
		var term int
		if err := rows.Scan(&currency, &price, &term); err != nil {
			return 0
		}
		if v, ok := pricing.MonthlyUSD(&model.Pricing{Currency: currency, Price: price, Term: term}, rates); ok {
			sum += v
		}
	}
	return sum
}

// costPairUSDFor renders both cost cards from ONE aggregation pass.
func (s *Server) costPairUSDFor(r *http.Request, serviceType int) (monthly, yearly string) {
	sum := s.costSumUSDFor(r, serviceType)
	if sum <= 0 {
		return "—", "—"
	}
	return fmt.Sprintf("$%.2f/mo", sum), fmt.Sprintf("$%.2f/yr", sum*12)
}

// pricingPairs renders a pricing as kv pairs for detail cards.
func pricingPairs(p *model.Pricing) []kvPair {
	if p == nil {
		return nil
	}
	pairs := []kvPair{
		{K: "Price", V: priceDisplay(p.Currency, p.Price, p.Term), Mono: true},
		{K: "Term", V: model.TermLabel(p.Term)},
	}
	if p.NextDueDate.Valid {
		pairs = append(pairs, kvPair{K: "Next due", V: p.NextDueDate.String, Mono: true})
	} else {
		pairs = append(pairs, kvPair{K: "Next due", V: "—"})
	}
	return pairs
}

// pricingCell builds the PRICE list cell.
func pricingCell(p *model.Pricing) listCell {
	if p == nil {
		return listCell{Main: "—", Class: "mono"}
	}
	return listCell{Main: priceDisplay(p.Currency, p.Price, p.Term), Class: "mono"}
}

// dueCell builds the DUE list cell with urgency coloring.
func dueCell(p *model.Pricing, dueSoonDays int) listCell {
	if p == nil {
		return listCell{Main: "—", Class: "mono"}
	}
	text, class := dueDisplay(p.NextDueDate, dueSoonDays)
	return listCell{Main: text, Class: ("mono " + class)}
}

// catalogName resolves a catalog entry name by table + id ("" when unset).
func (s *Server) catalogName(r *http.Request, table string, id sql.NullInt64) string {
	if !id.Valid {
		return ""
	}
	var name string
	// Table names are compile-time constants.
	s.db.QueryRowContext(r.Context(), "SELECT name FROM "+table+" WHERE id = ?", id.Int64).Scan(&name)
	return name
}
