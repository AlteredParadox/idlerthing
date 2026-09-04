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
	"database/sql"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
	"idlerthing/internal/pricing"
	"idlerthing/internal/prom"
)

// currencySymbols maps currency codes to display symbols; keys mirror
// model.Currencies (unknown codes fall back to "CODE " in priceDisplay).
var currencySymbols = map[string]string{
	"USD": "$", "EUR": "€", "GBP": "£", "CAD": "CA$",
	"AUD": "A$", "JPY": "¥", "CNY": "CN¥",
}

// currencies lists the options offered in pricing forms.
var currencies = model.Currencies

// networkTypes lists common network type options (free-ish select).
var networkTypes = []string{"IPv4", "IPv6", "IPv4+IPv6", "IPv4 NAT", "IPv4 NAT + IPv6"}

// diskMedia lists disk media options.
var diskMediaTypes = []string{"SSD", "HDD", "NVMe"}

// serverRow is one display row of the servers list.
type serverRow struct {
	ID            int64
	Hostname      string
	HostnameTitle string // full hostname for the title attr when shortened
	PortNote      string // e.g. ":2222" when non-default
	Active        bool
	Type          string
	OS            string
	CPU           string
	CPUModel      string
	RAM           string
	Disk          string
	DiskMedia     string
	BW            string
	Net           string
	Location      string
	Provider      string
	Price         string
	Due           string
	DueClass      string // "", "due-warn", "due-over"
	// Live prometheus data (nil/"" when unmatched or disabled).
	Live       int // 0 unknown, 1 up, 2 down
	CPUMeter   *meterView
	RAMMeter   *meterView
	DiskMeter  *meterView
	CPUPct     string // compact-mode inline pct
	RAMPct     string
	DiskPct    string
	Throughput string
	// Optional columns.
	LinkSpeed   string
	LinkMeter   *meterView
	LinkUtilPct string // compact-mode inline utilization
	PriceYr     string
	Since       string
	Uptime      string
}

// serversListView is the template payload for GET /servers.
type serversListView struct {
	listNav
	Rows           []serverRow
	ActiveCount    int
	InactiveCount  int
	Total          int
	Locations      int
	MonthlyUSD     string // "$12.34/mo" or "—"
	YearlyUSD      string // "$148.08/yr" or "—"
	RowCount       int
	ShortHostnames bool
	HiddenCols     map[string]bool
}

// handleServerList renders GET /servers (full page or htmx table partial).
func (s *Server) handleServerList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	opts := model.ListOptions{
		Status: normStatus(q.Get("status")),
		Q:      strings.TrimSpace(q.Get("q")),
	}
	// Sort/dir come from the per-user pref (listSort) — never from the query.
	opts.Sort, opts.Dir = s.listSort(r, "servers", "hostname")

	items, err := s.servers.List(r.Context(), opts)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}

	// htmx swaps re-render only the table partial — skip the
	// counts/locations/cost queries the full page would need.
	isHX := r.Header.Get("HX-Request") == "true"

	var active, inactive, locations int
	if !isHX {
		active, inactive, err = s.servers.StatusCounts(r.Context())
		if err != nil {
			http.Error(w, errMsgServerErr, http.StatusInternalServerError)
			return
		}
		locations, err = s.servers.DistinctLocations(r.Context())
		if err != nil {
			http.Error(w, errMsgServerErr, http.StatusInternalServerError)
			return
		}
	}

	var monthlyCost, yearlyCost string
	if !isHX {
		monthlyCost, yearlyCost = s.costPairUSDFor(r, model.ServiceServer)
	}
	view := serversListView{
		listNav:        listNav{Base: routeServers, Status: opts.Status, Q: opts.Q, Sort: opts.Sort, Dir: opts.Dir},
		ActiveCount:    active,
		InactiveCount:  inactive,
		Total:          active + inactive,
		Locations:      locations,
		MonthlyUSD:     monthlyCost,
		YearlyUSD:      yearlyCost,
		RowCount:       len(items),
		ShortHostnames: s.shortHostnames(r),
		HiddenCols:     s.hiddenCols(r),
	}
	dueSoon := s.dueSoonDays(r)
	metrics := s.liveMetrics(r)
	rates, _ := s.rates.Get(r.Context())
	for _, it := range items {
		row := makeServerRow(it, dueSoon)
		if view.ShortHostnames {
			row.Hostname, row.HostnameTitle = shortHostname(it.Hostname), it.Hostname
		}
		row.PriceYr = priceYrDisplay(it.Pricing, rates)
		applyLiveToRow(&row, matchLive(metrics, it.Hostname), it.LinkSpeed)
		view.Rows = append(view.Rows, row)
	}

	data := s.newPageData(w, r, "Servers", "servers")
	data.Data = view
	if isHX {
		s.renderNamed(w, "servers", "server_table", data)
		return
	}
	s.render(w, r, "servers", data)
}

// dueSoonDays reads settings.due_soon_amount.
func (s *Server) dueSoonDays(r *http.Request) int {
	if n := s.memoSettings(r).DueSoon; n > 0 {
		return n
	}
	return 14
}

// makeServerRow converts a list item into display-ready strings.
func makeServerRow(it model.ServerListItem, dueSoonDays int) serverRow {
	row := serverRow{
		ID:       it.ID,
		Hostname: it.Hostname,
		Active:   it.Active,
		Type:     model.ServerTypeLabel(it.ServerType),
		OS:       dash(it.OSName),
		Provider: dash(it.ProviderName),
		Location: dash(it.LocationName),
		Net:      it.NetworkType.String,
	}
	if it.SSHPort.Valid && it.SSHPort.Int64 != 22 {
		row.PortNote = ":" + strconv.FormatInt(it.SSHPort.Int64, 10)
	}
	if it.CPU.Valid {
		row.CPU = strconv.FormatInt(it.CPU.Int64, 10) + "×"
	} else {
		row.CPU = "—"
	}
	row.CPUModel = it.CPUModel.String
	row.RAM = fmtNullMB(it.RamAsMB)
	if it.DiskMB > 0 {
		row.Disk = fmtMB(it.DiskMB)
	} else {
		row.Disk = "—"
	}
	row.DiskMedia = it.DiskMedia
	if it.BandwidthAsMB.Valid {
		row.BW = fmtMB(it.BandwidthAsMB.Int64)
	} else {
		row.BW = "∞"
	}
	if it.LinkSpeed.Valid {
		row.LinkSpeed = linkSpeedDisplay(it.LinkSpeed.Int64)
	} else {
		row.LinkSpeed = "—"
	}
	row.Since = dash(it.OwnedSince.String)
	if it.Pricing != nil {
		row.Price = priceDisplay(it.Pricing.Currency, it.Pricing.Price, it.Pricing.Term)
		row.Due, row.DueClass = dueDisplay(it.Pricing.NextDueDate, dueSoonDays)
	} else {
		row.Price = "—"
		row.Due = "—"
	}
	return row
}

// priceYrDisplay renders a pricing as USD per year (one-time → "—").
func priceYrDisplay(p *model.Pricing, rates map[string]float64) string {
	if p == nil {
		return "—"
	}
	monthly, ok := pricing.MonthlyUSDRaw(p, rates)
	if !ok {
		return "—"
	}
	return fmt.Sprintf("$%.2f/yr", monthly*12)
}

func dash(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// shortHostname strips a hostname to its first DNS label.
func shortHostname(hostname string) string {
	if i := strings.IndexByte(hostname, '.'); i > 0 {
		return hostname[:i]
	}
	return hostname
}

// priceDisplay renders e.g. "$12.00/mo".
func priceDisplay(currency string, price float64, term int) string {
	sym := currencySymbols[currency]
	if sym == "" {
		sym = currency + " "
	}
	return fmt.Sprintf("%s%.2f%s", sym, price, model.TermAbbrev(term))
}

// dueDisplay renders the next due date and its urgency class.
func dueDisplay(next sql.NullString, dueSoonDays int) (string, string) {
	if !next.Valid || next.String == "" {
		return "—", ""
	}
	date := next.String
	if len(date) > 10 {
		date = date[:10]
	}
	t, err := time.ParseInLocation(time.DateOnly, date, time.Local)
	if err != nil {
		return date, ""
	}
	// Local midnight (the rest of the app compares local dates).
	now := time.Now()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	switch {
	case t.Before(today):
		return date, "due-over"
	case !t.After(today.AddDate(0, 0, dueSoonDays)):
		return date, "due-warn"
	default:
		return date, ""
	}
}

// handleServerDetail renders GET /servers/{id}.
func (s *Server) handleServerDetail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	srv, disks, pricing, err := s.servers.Get(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}

	names := s.lookupNames(r, srv)
	extras, err := s.buildExtras(r, srv.ID, model.ServiceServer)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	// One live-metrics fetch for both the Live card and the LiveMon section.
	metrics := s.liveMetrics(r)
	view := serverDetailView{
		Server:      srv,
		Disks:       disks,
		Pricing:     pricing,
		OS:          names[0],
		Provider:    names[1],
		Location:    names[2],
		TypeLabel:   model.ServerTypeLabel(srv.ServerType),
		Extras:      extras,
		Live:        s.buildLive(r, metrics, srv.Hostname),
		YABSCommand: s.yabsCommand(r, srv.ID),
	}
	if h := matchLive(metrics, srv.Hostname); h != nil {
		view.LiveMon = s.liveMonEntry(r, h.Instance)
	}
	runs, err := (&model.YABSStore{DB: s.db}).ListFor(r.Context(), srv.ID)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	view.YABSRuns = yabsRows(runs)
	if len(view.YABSRuns) > 0 {
		view.LatestSingle = view.YABSRuns[0].GbSingle
		view.LatestMulti = view.YABSRuns[0].GbMulti
	}
	data := s.newPageData(w, r, srv.Hostname, "servers")
	data.Data = view
	s.render(w, r, "server_detail", data)
}

// serverDetailView is the template payload for the detail page.
type serverDetailView struct {
	Server       *model.Server
	Disks        []model.ServerDisk
	Pricing      *model.Pricing
	OS           string
	Provider     string
	Location     string
	TypeLabel    string
	Extras       *extrasView
	Live         *liveView
	LiveMon      *liveMonView
	YABSCommand  string
	YABSRuns     []yabsRow
	LatestSingle string
	LatestMulti  string
}

// lookupNames resolves os/provider/display names for a server.
func (s *Server) lookupNames(r *http.Request, srv *model.Server) [3]string {
	var names [3]string
	lookups := []struct {
		id    sql.NullInt64
		table string
		out   *string
	}{
		{srv.OsID, "os", &names[0]},
		{srv.ProviderID, "providers", &names[1]},
		{srv.LocationID, "locations", &names[2]},
	}
	for _, l := range lookups {
		if !l.id.Valid {
			continue
		}
		// Table names are compile-time constants.
		s.db.QueryRowContext(r.Context(),
			"SELECT name FROM "+l.table+" WHERE id = ?", l.id.Int64).Scan(l.out)
	}
	return names
}

// serverFormView is the template payload for new/edit forms.
type serverFormView struct {
	Action       string // form action URL
	IsEdit       bool
	Server       *model.Server
	Disks        []model.ServerDisk // padded to 4 rows in the template
	Pricing      *model.Pricing
	Providers    []model.CatalogItem
	Locations    []model.CatalogItem
	OSes         []model.CatalogItem
	Currencies   []string
	NetworkTypes []string
	DiskMedia    []string
	Errors       map[string]string
}

// handleServerNew renders GET /servers/new.
func (s *Server) handleServerNew(w http.ResponseWriter, r *http.Request) {
	s.renderServerForm(w, r, serverFormView{
		Action: routeServers,
		Server: &model.Server{Active: true, ServerType: model.TypeKVM, SSHPort: sql.NullInt64{Int64: 22, Valid: true}},
	})
}

// handleServerEdit renders GET /servers/{id}/edit.
func (s *Server) handleServerEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	srv, disks, pricing, err := s.servers.Get(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.renderServerForm(w, r, serverFormView{
		Action:  routeServers + "/" + strconv.FormatInt(id, 10) + "/update",
		IsEdit:  true,
		Server:  srv,
		Disks:   disks,
		Pricing: pricing,
	})
}

// renderServerForm renders the shared new/edit form.
func (s *Server) renderServerForm(w http.ResponseWriter, r *http.Request, view serverFormView) {
	if view.Providers == nil {
		view.Providers, _ = s.catalogs.List(r.Context(), model.Catalogs["providers"])
		view.Locations, _ = s.catalogs.List(r.Context(), model.Catalogs["locations"])
		view.OSes, _ = s.catalogs.List(r.Context(), model.Catalogs["os"])
	}
	view.Currencies = currencies
	view.NetworkTypes = networkTypes
	view.DiskMedia = diskMediaTypes

	title := "Add server"
	if view.IsEdit {
		title = "Edit " + view.Server.Hostname
	}
	data := s.newPageData(w, r, title, "servers")
	data.Data = view
	s.render(w, r, "server_form", data)
}

// handleServerCreate handles POST /servers.
func (s *Server) handleServerCreate(w http.ResponseWriter, r *http.Request) {
	srv, disks, pricing, errs := parseServerForm(r)
	if len(errs) > 0 {
		s.renderServerForm(w, r, serverFormView{
			Action: routeServers, Server: srv, Disks: disks, Pricing: pricing, Errors: errs,
		})
		return
	}
	id, err := s.servers.Create(r.Context(), srv, disks, pricing)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", "Server "+srv.Hostname+" added.")
	http.Redirect(w, r, routeServers+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleServerUpdate handles POST /servers/{id}/update.
func (s *Server) handleServerUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	srv, disks, pricing, errs := parseServerForm(r)
	srv.ID = id
	if len(errs) > 0 {
		s.renderServerForm(w, r, serverFormView{
			Action: routeServers + "/" + strconv.FormatInt(id, 10) + "/update",
			IsEdit: true, Server: srv, Disks: disks, Pricing: pricing, Errors: errs,
		})
		return
	}
	if err := s.servers.Update(r.Context(), srv, disks, pricing); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", "Server "+srv.Hostname+" saved.")
	http.Redirect(w, r, routeServers+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleServerDelete handles POST /servers/{id}/delete.
func (s *Server) handleServerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if err := s.servers.Delete(r.Context(), id); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", "Server deleted.")
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", routeServers)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, routeServers, http.StatusSeeOther)
}

// parseServerForm parses + validates the server form. Returned errors are
// keyed by field name for inline display.
func parseServerForm(r *http.Request) (*model.Server, []model.ServerDisk, *model.Pricing, map[string]string) {
	errs := map[string]string{}
	srv := &model.Server{
		Hostname:      strings.TrimSpace(r.FormValue("hostname")),
		ServerType:    intFormValue(r, "server_type", model.TypeKVM),
		Active:        r.FormValue("active") != "",
		ShowPublic:    r.FormValue("show_public") != "",
		WasPromo:      r.FormValue("was_promo") != "",
		Transferrable: r.FormValue("transferrable") != "",
	}
	if srv.Hostname == "" {
		errs["hostname"] = "Hostname is required."
	}
	if srv.ServerType < model.TypeKVM || srv.ServerType > model.TypeNAT {
		srv.ServerType = model.TypeKVM
	}

	srv.OsID = nullIntFormValue(r, "os_id")
	srv.ProviderID = nullIntFormValue(r, "provider_id")
	srv.LocationID = nullIntFormValue(r, "location_id")
	srv.CPU = checkedInt(r, errs, "cpu", 0, 1024)
	srv.RamAsMB = sizeFormValue(r, errs, "ram_as_mb", 1<<30)
	srv.BandwidthAsMB = bandwidthFormValue(r, errs, "bandwidth_as_mb", 1<<30)
	srv.LinkSpeed = checkedInt(r, errs, "link_speed", 0, 1<<20)
	srv.SSHPort = checkedInt(r, errs, "ssh_port", 0, 65535)
	srv.CPUModel = nullStrFormValue(r, "cpu_model")
	srv.NetworkType = nullStrFormValue(r, "network_type")
	srv.Ns1 = nullStrFormValue(r, "ns1")
	srv.Ns2 = nullStrFormValue(r, "ns2")
	srv.OwnedSince = dateFormValue(r, errs, "owned_since")

	var disks []model.ServerDisk
	for i := 1; i <= 4; i++ {
		name := fmt.Sprintf("disk%d_size", i)
		if strings.TrimSpace(r.FormValue(name)) == "" {
			continue
		}
		size := sizeFormValue(r, errs, name, 1<<30)
		if !size.Valid || size.Int64 == 0 {
			continue
		}
		media := r.FormValue(fmt.Sprintf("disk%d_media", i))
		switch media {
		case "SSD", "HDD", "NVMe":
		default:
			media = "SSD"
		}
		disks = append(disks, model.ServerDisk{SizeAsMB: size.Int64, Media: media})
	}

	pricing := parsePricingForm(r, errs)
	return srv, disks, pricing, errs
}

// validPrice reports whether a price is finite and plausible (ParseFloat
// accepts NaN/+Inf/-Inf; a bare `price <= 0` check lets NaN through).
func validPrice(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 && f <= 1e9
}

// parsePricingForm parses the optional pricing section. Pricing is only
// present when a price is entered; then it must be > 0.
func parsePricingForm(r *http.Request, errs map[string]string) *model.Pricing {
	priceStr := strings.TrimSpace(r.FormValue("price"))
	if priceStr == "" {
		return nil
	}
	price, err := strconv.ParseFloat(priceStr, 64)
	if err != nil || !validPrice(price) {
		errs["price"] = "Price must be a number greater than 0."
		return nil
	}
	currency := validCurrency(r.FormValue("currency"))
	term := intFormValue(r, "term", model.TermMonthly)
	if term < model.TermMonthly || term > model.TermOneTime {
		term = model.TermMonthly
	}
	return &model.Pricing{
		Currency:    currency,
		Price:       price,
		Term:        term,
		NextDueDate: dateFormValue(r, errs, "next_due_date"),
	}
}

func intFormValue(r *http.Request, name string, fallback int) int {
	n, err := strconv.Atoi(strings.TrimSpace(r.FormValue(name)))
	if err != nil {
		return fallback
	}
	return n
}

// checkedInt parses an optional non-negative integer field within [min, max].
func checkedInt(r *http.Request, errs map[string]string, name string, minV, maxV int64) sql.NullInt64 {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return sql.NullInt64{}
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < minV || n > maxV {
		errs[name] = "Must be a number between " + strconv.FormatInt(minV, 10) + " and " + strconv.FormatInt(maxV, 10) + "."
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: n, Valid: true}
}

func nullIntFormValue(r *http.Request, name string) sql.NullInt64 {
	n, err := strconv.ParseInt(strings.TrimSpace(r.FormValue(name)), 10, 64)
	if err != nil || n <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: n, Valid: true}
}

func nullStrFormValue(r *http.Request, name string) sql.NullString {
	v := strings.TrimSpace(r.FormValue(name))
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

// dateFormValue parses an optional yyyy-mm-dd date field.
func dateFormValue(r *http.Request, errs map[string]string, name string) sql.NullString {
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		return sql.NullString{}
	}
	if _, err := time.Parse(time.DateOnly, raw); err != nil {
		errs[name] = "Invalid date."
		return sql.NullString{}
	}
	return sql.NullString{String: raw, Valid: true}
}

// applyLiveToRow attaches live prometheus data to a list row.
func applyLiveToRow(row *serverRow, h *prom.HostMetrics, linkSpeed sql.NullInt64) {
	if h == nil {
		return
	}
	// Online is only meaningful when the `up` query succeeded — otherwise
	// the row keeps the neutral grey dot (Live 0) while meters still render.
	if h.OnlineKnown {
		row.Live = 2
		if h.Online {
			row.Live = 1
		}
	}
	row.CPUMeter = meter(h.CPUPct)
	row.RAMMeter = meter(h.RAMPct)
	row.DiskMeter = meter(h.DiskPct)
	row.CPUPct = pctInline(h.CPUPct)
	row.RAMPct = pctInline(h.RAMPct)
	row.DiskPct = pctInline(h.DiskPct)
	row.Throughput = throughput(h)
	if h.Uptime30dValid {
		row.Uptime = fmt.Sprintf("%.1f%%", h.Uptime30d)
	} else {
		row.Uptime = "—"
	}
	// Link utilization = live bits/sec vs negotiated link speed.
	if linkSpeed.Valid && linkSpeed.Int64 > 0 {
		util := (h.NetRxBps + h.NetTxBps) * 8 / (float64(linkSpeed.Int64) * 1e6) * 100
		if util > 100 {
			util = 100
		}
		row.LinkMeter = meter(util)
		row.LinkUtilPct = pctInline(util)
	}
}
