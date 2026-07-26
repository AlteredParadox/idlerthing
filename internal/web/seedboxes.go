package web

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// seedboxSection builds the seedboxes list/detail/delete section.
func (s *Server) seedboxSection() *section {
	st := &model.SeedboxStore{DB: s.db}
	return &section{
		Base:        "/seedboxes",
		Kind:        "seedboxes",
		Title:       "SeedBoxes",
		ServiceType: model.ServiceSeedbox,
		AddLabel:    "＋ Add seedbox",
		SearchHint:  "Search hostname, title, provider…",
		EmptyTitle:  "No seedboxes yet",
		EmptySub:    "Add your first one to start tracking.",
		DefaultSort: "hostname",
		Columns: []listColumn{
			{Key: "title", Label: "Title", Sortable: true},
			{Key: "hostname", Label: "Hostname", Sortable: true},
			{Key: "type", Label: "Type", Sortable: true},
			{Key: "port", Label: "Port", Sortable: true},
			{Key: "disk", Label: "Disk", Sortable: true},
			{Key: "bw", Label: "BW", Sortable: true},
			{Key: "location", Label: "Location", Sortable: true},
			{Key: "provider", Label: "Provider", Sortable: true},
			{Key: "price", Label: "Price", Sortable: true},
			{Key: "due", Label: "Due", Sortable: true},
		},
		List: func(r *http.Request, opts model.ListOptions) ([]listRow, error) {
			items, err := st.List(r.Context(), opts)
			if err != nil {
				return nil, err
			}
			var rows []listRow
			for _, it := range items {
				id := strconv.FormatInt(it.ID, 10)
				dot := "ok"
				if !it.Active {
					dot = "off"
				}
				title := it.Title.String
				if title == "" {
					title = "—"
				}
				rows = append(rows, listRow{
					Link:          "/seedboxes/" + id,
					EditURL:       "/seedboxes/" + id + "/edit",
					DeleteURL:     "/seedboxes/" + id + "/delete",
					DeleteConfirm: "Delete " + it.Hostname + "?",
					Cells: []listCell{
						{Main: title, Dot: dot, Link: "/seedboxes/" + id},
						{Main: it.Hostname, Class: "mono"},
						{Main: dash(it.SeedBoxType.String), Badge: it.SeedBoxType.Valid},
						{Main: nullMbps(it.PortSpeed), Class: "mono"},
						{Main: fmtNullMB(it.DiskAsMB), Class: "mono"},
						bwCell(it.BandwidthAsMB),
						{Main: dash(it.LocationName)},
						{Main: dash(it.ProviderName)},
						pricingCell(it.Pricing),
						dueCell(it.Pricing, s.dueSoonDays(r)),
					},
				})
			}
			return rows, nil
		},
		Counts: st.StatusCounts,
		Cards: func(r *http.Request, active, inactive int) []statCard {
			monthly, yearly := s.costPairUSDFor(r, model.ServiceSeedbox)
			return []statCard{
				{Label: "Total", Value: strconv.Itoa(active + inactive)},
				{Label: "Active", Value: strconv.Itoa(active)},
				{Label: "Monthly cost", Value: monthly},
				{Label: "Yearly cost", Value: yearly},
			}
		},
		Delete: st.Delete,
		Detail: func(r *http.Request, id int64) (*detailView, error) {
			b, pricing, err := st.Get(r.Context(), id)
			if err != nil {
				return nil, err
			}
			badges := []detailBadge{}
			if b.Active {
				badges = append(badges, detailBadge{Label: "Active", Class: "badge-ok"})
			} else {
				badges = append(badges, detailBadge{Label: "Inactive", Class: "badge-off"})
			}
			if b.WasPromo {
				badges = append(badges, detailBadge{Label: "Promo", Class: "badge-warn"})
			}
			if b.ShowPublic {
				badges = append(badges, detailBadge{Label: "Public"})
			}
			kvs := func(k string, v sql.NullString) kvPair {
				if !v.Valid {
					return kvPair{K: k, V: "—"}
				}
				return kvPair{K: k, V: v.String}
			}
			kvn := func(k string, n sql.NullInt64, suffix string) kvPair {
				if !n.Valid {
					return kvPair{K: k, V: "—"}
				}
				return kvPair{K: k, V: strconv.FormatInt(n.Int64, 10) + suffix, Mono: true}
			}
			title := b.Hostname
			if b.Title.Valid && b.Title.String != "" {
				title = b.Title.String
			}
			sid := strconv.FormatInt(b.ID, 10)
			return &detailView{
				Title:         title,
				Mono:          true,
				Badges:        badges,
				EditURL:       "/seedboxes/" + sid + "/edit",
				DeleteURL:     "/seedboxes/" + sid + "/delete",
				DeleteConfirm: "Delete " + b.Hostname + "?",
				Cards: []infoCard{
					{Title: "Service", Pairs: []kvPair{
						kvs("Title", b.Title),
						{K: "Hostname", V: b.Hostname, Mono: true},
						kvs("Type", b.SeedBoxType),
					}},
					{Title: "Resources", Pairs: []kvPair{
						kvn("Port speed", b.PortSpeed, " mbps"),
						{K: "Disk", V: fmtNullMB(b.DiskAsMB), Mono: true},
						{K: "Bandwidth", V: bwDisplay(b.BandwidthAsMB), Mono: true},
					}},
					{Title: "Classification", Pairs: []kvPair{
						{K: "Provider", V: dash(s.catalogName(r, "providers", b.ProviderID))},
						{K: "Location", V: dash(s.catalogName(r, "locations", b.LocationID))},
						kvs("Owned since", b.OwnedSince),
					}},
					{Title: "Pricing", Pairs: pricingPairs(pricing), Empty: "No pricing attached."},
				},
			}, nil
		},
	}
}

func nullMbps(n sql.NullInt64) string {
	if !n.Valid {
		return "—"
	}
	return strconv.FormatInt(n.Int64, 10) + " Mbps"
}

// seedboxFormView is the template payload for the seedbox form.
type seedboxFormView struct {
	Action     string
	CancelURL  string
	IsEdit     bool
	Seedbox    *model.Seedbox
	Pricing    *model.Pricing
	Providers  []model.CatalogItem
	Locations  []model.CatalogItem
	Currencies []string
	Errors     map[string]string
}

func (s *Server) handleSeedboxNew(w http.ResponseWriter, r *http.Request) {
	s.renderSeedboxForm(w, r, seedboxFormView{
		Action:  "/seedboxes",
		Seedbox: &model.Seedbox{Active: true},
	})
}

func (s *Server) handleSeedboxEdit(w http.ResponseWriter, r *http.Request) {
	st := &model.SeedboxStore{DB: s.db}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, pricing, err := st.Get(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.renderSeedboxForm(w, r, seedboxFormView{
		Action:  "/seedboxes/" + strconv.FormatInt(id, 10) + "/update",
		IsEdit:  true,
		Seedbox: b,
		Pricing: pricing,
	})
}

func (s *Server) renderSeedboxForm(w http.ResponseWriter, r *http.Request, view seedboxFormView) {
	if view.Providers == nil {
		view.Providers, _ = s.catalogs.List(r.Context(), model.Catalogs["providers"])
		view.Locations, _ = s.catalogs.List(r.Context(), model.Catalogs["locations"])
	}
	view.Currencies = currencies
	title := "Add seedbox"
	view.CancelURL = "/seedboxes"
	if view.IsEdit {
		title = "Edit " + view.Seedbox.Hostname
		view.CancelURL = "/seedboxes/" + strconv.FormatInt(view.Seedbox.ID, 10)
	}
	data := s.newPageData(w, r, title, "seedboxes")
	data.Data = view
	s.render(w, r, "seedbox_form", data)
}

func (s *Server) handleSeedboxCreate(w http.ResponseWriter, r *http.Request) {
	b, pricing, errs := parseSeedboxForm(r)
	if len(errs) > 0 {
		s.renderSeedboxForm(w, r, seedboxFormView{
			Action: "/seedboxes", Seedbox: b, Pricing: pricing, Errors: errs,
		})
		return
	}
	st := &model.SeedboxStore{DB: s.db}
	id, err := st.Create(r.Context(), b, pricing)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	setFlash(w, "ok", b.Hostname+" added.")
	http.Redirect(w, r, "/seedboxes/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleSeedboxUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	b, pricing, errs := parseSeedboxForm(r)
	b.ID = id
	if len(errs) > 0 {
		s.renderSeedboxForm(w, r, seedboxFormView{
			Action: "/seedboxes/" + strconv.FormatInt(id, 10) + "/update",
			IsEdit: true, Seedbox: b, Pricing: pricing, Errors: errs,
		})
		return
	}
	st := &model.SeedboxStore{DB: s.db}
	if err := st.Update(r.Context(), b, pricing); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	setFlash(w, "ok", b.Hostname+" saved.")
	http.Redirect(w, r, "/seedboxes/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// parseSeedboxForm parses + validates the seedbox form.
func parseSeedboxForm(r *http.Request) (*model.Seedbox, *model.Pricing, map[string]string) {
	errs := map[string]string{}
	b := &model.Seedbox{
		Title:       nullStrFormValue(r, "title"),
		Hostname:    strings.TrimSpace(r.FormValue("hostname")),
		SeedBoxType: nullStrFormValue(r, "seed_box_type"),
		ProviderID:  nullIntFormValue(r, "provider_id"),
		LocationID:  nullIntFormValue(r, "location_id"),
		Active:      r.FormValue("active") != "",
		ShowPublic:  r.FormValue("show_public") != "",
		WasPromo:    r.FormValue("was_promo") != "",
	}
	if b.Hostname == "" {
		errs["hostname"] = "Hostname is required."
	}
	b.PortSpeed = checkedInt(r, errs, "port_speed", 0, 1<<20)
	b.DiskAsMB = sizeFormValue(r, errs, "disk_as_mb", 1<<30)
	b.BandwidthAsMB = bandwidthFormValue(r, errs, "bandwidth_as_mb", 1<<30)
	b.OwnedSince = dateFormValue(r, errs, "owned_since")
	return b, parsePricingForm(r, errs), errs
}
