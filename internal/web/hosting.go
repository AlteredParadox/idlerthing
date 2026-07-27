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
	"context"
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// hostingStorer is satisfied by both model.SharedStore and model.ResellerStore.
type hostingStorer interface {
	Create(ctx context.Context, h *model.SharedHosting, p *model.Pricing) (int64, error)
	Get(ctx context.Context, id int64) (*model.SharedHosting, *model.Pricing, error)
	Update(ctx context.Context, h *model.SharedHosting, p *model.Pricing) error
	Delete(ctx context.Context, id int64) error
	List(ctx context.Context, opts model.ListOptions) ([]model.HostingListItem, error)
	StatusCounts(ctx context.Context) (int, int, error)
	DistinctProviders(ctx context.Context) (int, error)
}

// hostingConfig parameterizes the shared/reseller sections.
type hostingConfig struct {
	base      string // "/shared" or "/reseller"
	kind      string // nav key
	title     string
	typeLabel string // "Shared type" / "Reseller type"
	addLabel  string
	store     hostingStorer
}

func (s *Server) sharedConfig() *hostingConfig {
	return &hostingConfig{
		base: "/shared", kind: "shared", title: "Shared Hosting",
		typeLabel: "Shared type", addLabel: "＋ Add shared hosting",
		store: &model.SharedStore{DB: s.db},
	}
}

func (s *Server) resellerConfig() *hostingConfig {
	return &hostingConfig{
		base: "/reseller", kind: "reseller", title: "Reseller Hosting",
		typeLabel: "Reseller type", addLabel: "＋ Add reseller hosting",
		store: &model.ResellerStore{DB: s.db},
	}
}

// hostingSection builds the generic list/detail/delete section.
func (s *Server) hostingSection(cfg *hostingConfig) *section {
	return &section{
		Base:        cfg.base,
		Kind:        cfg.kind,
		Title:       cfg.title,
		ServiceType: serviceTypeOf(cfg.base),
		AddLabel:    cfg.addLabel,
		SearchHint:  "Search domain, type, provider…",
		EmptyTitle:  "No " + strings.ToLower(cfg.title) + " yet",
		EmptySub:    "Add your first one to start tracking.",
		DefaultSort: "domain",
		Columns: []listColumn{
			{Key: "domain", Label: "Domain", Sortable: true},
			{Key: "type", Label: "Type", Sortable: true},
			{Label: "Limits"},
			{Key: "disk", Label: "Disk", Sortable: true},
			{Key: "bw", Label: "BW", Sortable: true},
			{Key: "location", Label: "Location", Sortable: true},
			{Key: "provider", Label: "Provider", Sortable: true},
			{Key: "price", Label: "Price", Sortable: true},
			{Key: "due", Label: "Due", Sortable: true},
		},
		List: func(r *http.Request, opts model.ListOptions) ([]listRow, error) {
			items, err := cfg.store.List(r.Context(), opts)
			if err != nil {
				return nil, err
			}
			return s.hostingRows(cfg.base, items, s.dueSoonDays(r)), nil
		},
		Counts: cfg.store.StatusCounts,
		Cards: func(r *http.Request, active, inactive int) []statCard {
			providers, _ := cfg.store.DistinctProviders(r.Context())
			monthly, yearly := s.costPairUSDFor(r, serviceTypeOf(cfg.base))
			return []statCard{
				{Label: "Total", Value: strconv.Itoa(active + inactive)},
				{Label: "Active", Value: strconv.Itoa(active)},
				{Label: "Monthly cost", Value: monthly},
				{Label: "Yearly cost", Value: yearly},
				{Label: "Providers", Value: strconv.Itoa(providers)},
			}
		},
		Delete: cfg.store.Delete,
		Detail: func(r *http.Request, id int64) (*detailView, error) {
			h, pricing, err := cfg.store.Get(r.Context(), id)
			if err != nil {
				return nil, err
			}
			return s.hostingDetail(r, cfg, h, pricing), nil
		},
	}
}

// serviceTypeOf maps a section base path to its pricings service_type.
func serviceTypeOf(base string) int {
	switch base {
	case "/shared":
		return model.ServiceShared
	case "/reseller":
		return model.ServiceReseller
	case "/domains":
		return model.ServiceDomain
	case "/misc":
		return model.ServiceMisc
	case "/seedboxes":
		return model.ServiceSeedbox
	default:
		return model.ServiceServer
	}
}

// hostingRows builds list rows for shared/reseller items.
func (s *Server) hostingRows(base string, items []model.HostingListItem, dueSoon int) []listRow {
	var rows []listRow
	for _, it := range items {
		id := strconv.FormatInt(it.ID, 10)
		dot := "ok"
		if !it.Active {
			dot = "off"
		}
		row := listRow{
			Link:          base + "/" + id,
			EditURL:       base + "/" + id + "/edit",
			DeleteURL:     base + "/" + id + "/delete",
			DeleteConfirm: "Delete " + it.MainDomain + "?",
			Cells: []listCell{
				{Main: it.MainDomain, Dot: dot, Link: base + "/" + id, Class: "mono"},
				{Main: dash(it.SharedType.String), Badge: it.SharedType.Valid},
				{Main: limitsSummary(it)},
				{Main: fmtNullMB(it.DiskAsMB), Class: "mono"},
				bwCell(it.BandwidthAsMB),
				{Main: dash(it.LocationName)},
				{Main: dash(it.ProviderName)},
				pricingCell(it.Pricing),
				dueCell(it.Pricing, dueSoon),
			},
		}
		rows = append(rows, row)
	}
	return rows
}

// limitsSummary renders e.g. "10 dom · 50 sub · 5 db".
func limitsSummary(it model.HostingListItem) string {
	var parts []string
	add := func(n sql.NullInt64, label string) {
		if n.Valid {
			parts = append(parts, strconv.FormatInt(n.Int64, 10)+" "+label)
		}
	}
	add(it.DomainsLimit, "dom")
	add(it.SubdomainsLimit, "sub")
	add(it.FtpLimit, "ftp")
	add(it.EmailLimit, "mail")
	add(it.DbLimit, "db")
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " · ")
}

// hostingDetail builds the generic detail view for shared/reseller.
func (s *Server) hostingDetail(r *http.Request, cfg *hostingConfig, h *model.SharedHosting, pricing *model.Pricing) *detailView {
	badges := []detailBadge{}
	if h.Active {
		badges = append(badges, detailBadge{Label: "Active", Class: "badge-ok"})
	} else {
		badges = append(badges, detailBadge{Label: "Inactive", Class: "badge-off"})
	}
	if h.WasPromo {
		badges = append(badges, detailBadge{Label: "Promo", Class: "badge-warn"})
	}
	if h.ShowPublic {
		badges = append(badges, detailBadge{Label: "Public"})
	}
	if h.HasDedicatedIP {
		badges = append(badges, detailBadge{Label: "Dedicated IP"})
	}

	kv := func(k, v string) kvPair { return kvPair{K: k, V: v} }
	kvn := func(k string, n sql.NullInt64, suffix string) kvPair {
		if !n.Valid {
			return kvPair{K: k, V: "—"}
		}
		return kvPair{K: k, V: strconv.FormatInt(n.Int64, 10) + suffix, Mono: true}
	}
	kvs := func(k string, v sql.NullString) kvPair {
		if !v.Valid {
			return kvPair{K: k, V: "—"}
		}
		return kvPair{K: k, V: v.String}
	}

	id := strconv.FormatInt(h.ID, 10)
	return &detailView{
		Title:         h.MainDomain,
		Badges:        badges,
		EditURL:       cfg.base + "/" + id + "/edit",
		DeleteURL:     cfg.base + "/" + id + "/delete",
		DeleteConfirm: "Delete " + h.MainDomain + "?",
		Cards: []infoCard{
			{Title: "Plan", Pairs: []kvPair{
				kvs(cfg.typeLabel, h.SharedType),
				kvn("Domains", h.DomainsLimit, ""),
				kvn("Subdomains", h.SubdomainsLimit, ""),
				kvn("FTP accounts", h.FtpLimit, ""),
				kvn("Email accounts", h.EmailLimit, ""),
				kvn("Databases", h.DbLimit, ""),
			}},
			{Title: "Resources", Pairs: []kvPair{
				{K: "Disk", V: fmtNullMB(h.DiskAsMB), Mono: true},
				{K: "Bandwidth", V: bwDisplay(h.BandwidthAsMB), Mono: true},
				kv("Dedicated IP", yesNo(h.HasDedicatedIP)),
				kvs("IP", h.IP),
			}},
			{Title: "Classification", Pairs: []kvPair{
				kv("Provider", dash(s.catalogName(r, "providers", h.ProviderID))),
				kv("Location", dash(s.catalogName(r, "locations", h.LocationID))),
				kvs("Owned since", h.OwnedSince),
			}},
			{Title: "Pricing", Pairs: pricingPairs(pricing), Empty: "No pricing attached."},
		},
	}
}

// hostingFormView is the template payload for shared/reseller forms.
type hostingFormView struct {
	Action     string
	CancelURL  string
	IsEdit     bool
	TypeLabel  string
	Hosting    *model.SharedHosting
	Pricing    *model.Pricing
	Providers  []model.CatalogItem
	Locations  []model.CatalogItem
	Currencies []string
	Errors     map[string]string
}

// handleHostingNew renders GET {base}/new.
func (s *Server) handleHostingNew(w http.ResponseWriter, r *http.Request, cfg *hostingConfig) {
	s.renderHostingForm(w, r, cfg, hostingFormView{
		Action:    cfg.base,
		TypeLabel: cfg.typeLabel,
		Hosting:   &model.SharedHosting{Active: true},
	})
}

// handleHostingEdit renders GET {base}/{id}/edit.
func (s *Server) handleHostingEdit(w http.ResponseWriter, r *http.Request, cfg *hostingConfig) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h, pricing, err := cfg.store.Get(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.renderHostingForm(w, r, cfg, hostingFormView{
		Action:    cfg.base + "/" + strconv.FormatInt(id, 10) + "/update",
		IsEdit:    true,
		TypeLabel: cfg.typeLabel,
		Hosting:   h,
		Pricing:   pricing,
	})
}

func (s *Server) renderHostingForm(w http.ResponseWriter, r *http.Request, cfg *hostingConfig, view hostingFormView) {
	if view.Providers == nil {
		view.Providers, _ = s.catalogs.List(r.Context(), model.Catalogs["providers"])
		view.Locations, _ = s.catalogs.List(r.Context(), model.Catalogs["locations"])
	}
	view.Currencies = currencies
	title := strings.TrimPrefix(cfg.addLabel, "＋ ")
	view.CancelURL = cfg.base
	if view.IsEdit {
		title = "Edit " + view.Hosting.MainDomain
		view.CancelURL = cfg.base + "/" + strconv.FormatInt(view.Hosting.ID, 10)
	}
	data := s.newPageData(w, r, title, cfg.kind)
	data.Data = view
	s.render(w, r, "hosting_form", data)
}

// handleHostingCreate handles POST {base}.
func (s *Server) handleHostingCreate(w http.ResponseWriter, r *http.Request, cfg *hostingConfig) {
	h, pricing, errs := parseHostingForm(r)
	if len(errs) > 0 {
		s.renderHostingForm(w, r, cfg, hostingFormView{
			Action: cfg.base, TypeLabel: cfg.typeLabel,
			Hosting: h, Pricing: pricing, Errors: errs,
		})
		return
	}
	id, err := cfg.store.Create(r.Context(), h, pricing)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", h.MainDomain+" added.")
	http.Redirect(w, r, cfg.base+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// handleHostingUpdate handles POST {base}/{id}/update.
func (s *Server) handleHostingUpdate(w http.ResponseWriter, r *http.Request, cfg *hostingConfig) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h, pricing, errs := parseHostingForm(r)
	h.ID = id
	if len(errs) > 0 {
		s.renderHostingForm(w, r, cfg, hostingFormView{
			Action: cfg.base + "/" + strconv.FormatInt(id, 10) + "/update",
			IsEdit: true, TypeLabel: cfg.typeLabel,
			Hosting: h, Pricing: pricing, Errors: errs,
		})
		return
	}
	if err := cfg.store.Update(r.Context(), h, pricing); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", h.MainDomain+" saved.")
	http.Redirect(w, r, cfg.base+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// parseHostingForm parses + validates the shared/reseller form.
func parseHostingForm(r *http.Request) (*model.SharedHosting, *model.Pricing, map[string]string) {
	errs := map[string]string{}
	h := &model.SharedHosting{
		MainDomain:     strings.TrimSpace(r.FormValue("main_domain")),
		SharedType:     nullStrFormValue(r, "svc_type"),
		ProviderID:     nullIntFormValue(r, "provider_id"),
		LocationID:     nullIntFormValue(r, "location_id"),
		HasDedicatedIP: r.FormValue("has_dedicated_ip") != "",
		Active:         r.FormValue("active") != "",
		ShowPublic:     r.FormValue("show_public") != "",
		WasPromo:       r.FormValue("was_promo") != "",
	}
	if h.MainDomain == "" {
		errs["main_domain"] = "Main domain is required."
	}
	h.DomainsLimit = checkedInt(r, errs, "domains_limit", 0, 1<<20)
	h.SubdomainsLimit = checkedInt(r, errs, "subdomains_limit", 0, 1<<20)
	h.FtpLimit = checkedInt(r, errs, "ftp_limit", 0, 1<<20)
	h.EmailLimit = checkedInt(r, errs, "email_limit", 0, 1<<20)
	h.DbLimit = checkedInt(r, errs, "db_limit", 0, 1<<20)
	h.DiskAsMB = sizeFormValue(r, errs, "disk_as_mb", 1<<30)
	h.BandwidthAsMB = bandwidthFormValue(r, errs, "bandwidth_as_mb", 1<<30)
	h.IP = nullStrFormValue(r, "ip")
	h.OwnedSince = dateFormValue(r, errs, "owned_since")
	return h, parsePricingForm(r, errs), errs
}

func yesNo(b bool) string {
	if b {
		return "Yes"
	}
	return "No"
}
