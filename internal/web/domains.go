package web

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// domainSection builds the domains list/detail/delete section.
func (s *Server) domainSection() *section {
	st := &model.DomainStore{DB: s.db}
	return &section{
		Base:        "/domains",
		Kind:        "domains",
		Title:       "Domains",
		ServiceType: model.ServiceDomain,
		AddLabel:    "＋ Add domain",
		SearchHint:  "Search domain, extension, provider…",
		EmptyTitle:  "No domains yet",
		EmptySub:    "Add your first one to start tracking.",
		DefaultSort: "domain",
		Columns: []listColumn{
			{Key: "domain", Label: "Domain", Sortable: true},
			{Key: "ext", Label: "Ext", Sortable: true},
			{Label: "NS"},
			{Key: "provider", Label: "Provider", Sortable: true},
			{Key: "owned", Label: "Owned", Sortable: true},
			{Key: "price", Label: "Price", Sortable: true},
			{Key: "due", Label: "Due", Sortable: true},
		},
		List: func(r *http.Request, opts model.ListOptions) ([]listRow, error) {
			items, err := st.List(r.Context(), opts)
			if err != nil {
				return nil, err
			}
			dueSoon := s.dueSoonDays(r)
			var rows []listRow
			for _, it := range items {
				id := strconv.FormatInt(it.ID, 10)
				dot := "ok"
				if !it.Active {
					dot = "off"
				}
				ns := it.Ns1.String
				var nsSub string
				if it.Ns2.Valid {
					nsSub = it.Ns2.String
				}
				if ns == "" {
					ns = "—"
				}
				rows = append(rows, listRow{
					Link:          "/domains/" + id,
					EditURL:       "/domains/" + id + "/edit",
					DeleteURL:     "/domains/" + id + "/delete",
					DeleteConfirm: "Delete " + it.Domain.Domain + "?",
					Cells: []listCell{
						{Main: it.Domain.Domain, Dot: dot, Link: "/domains/" + id, Class: "mono"},
						{Main: dash(it.Extension.String)},
						{Main: ns, Sub: nsSub, Class: "mono"},
						{Main: dash(it.ProviderName)},
						{Main: dash(it.OwnedSince.String), Class: "mono"},
						pricingCell(it.Pricing),
						dueCell(it.Pricing, dueSoon),
					},
				})
			}
			return rows, nil
		},
		Counts: st.StatusCounts,
		Cards: func(r *http.Request, active, inactive int) []statCard {
			providers, _ := st.DistinctProviders(r.Context())
			monthly, yearly := s.costPairUSDFor(r, model.ServiceDomain)
			return []statCard{
				{Label: "Total", Value: strconv.Itoa(active + inactive)},
				{Label: "Active", Value: strconv.Itoa(active)},
				{Label: "Monthly cost", Value: monthly},
				{Label: "Yearly cost", Value: yearly},
				{Label: "Providers", Value: strconv.Itoa(providers)},
			}
		},
		Delete: st.Delete,
		Detail: func(r *http.Request, id int64) (*detailView, error) {
			d, pricing, err := st.Get(r.Context(), id)
			if err != nil {
				return nil, err
			}
			badges := []detailBadge{}
			if d.Active {
				badges = append(badges, detailBadge{Label: "Active", Class: "badge-ok"})
			} else {
				badges = append(badges, detailBadge{Label: "Inactive", Class: "badge-off"})
			}
			kvs := func(k string, v sql.NullString) kvPair {
				if !v.Valid {
					return kvPair{K: k, V: "—"}
				}
				return kvPair{K: k, V: v.String}
			}
			sid := strconv.FormatInt(d.ID, 10)
			return &detailView{
				Title:         d.Domain,
				Mono:          true,
				Badges:        badges,
				EditURL:       "/domains/" + sid + "/edit",
				DeleteURL:     "/domains/" + sid + "/delete",
				DeleteConfirm: "Delete " + d.Domain + "?",
				Cards: []infoCard{
					{Title: "Domain", Pairs: []kvPair{
						{K: "Domain", V: d.Domain, Mono: true},
						kvs("Extension", d.Extension),
						kvs("NS1", d.Ns1),
						kvs("NS2", d.Ns2),
						kvs("NS3", d.Ns3),
					}},
					{Title: "Classification", Pairs: []kvPair{
						{K: "Provider", V: dash(s.catalogName(r, "providers", d.ProviderID))},
						kvs("Owned since", d.OwnedSince),
					}},
					{Title: "Pricing", Pairs: pricingPairs(pricing), Empty: "No pricing attached."},
				},
			}, nil
		},
	}
}

// domainFormView is the template payload for the domain form.
type domainFormView struct {
	Action     string
	CancelURL  string
	IsEdit     bool
	Domain     *model.Domain
	Pricing    *model.Pricing
	Providers  []model.CatalogItem
	Currencies []string
	Errors     map[string]string
}

func (s *Server) handleDomainNew(w http.ResponseWriter, r *http.Request) {
	s.renderDomainForm(w, r, domainFormView{
		Action: "/domains",
		Domain: &model.Domain{Active: true},
	})
}

func (s *Server) handleDomainEdit(w http.ResponseWriter, r *http.Request) {
	st := &model.DomainStore{DB: s.db}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, pricing, err := st.Get(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.renderDomainForm(w, r, domainFormView{
		Action:  "/domains/" + strconv.FormatInt(id, 10) + "/update",
		IsEdit:  true,
		Domain:  d,
		Pricing: pricing,
	})
}

func (s *Server) renderDomainForm(w http.ResponseWriter, r *http.Request, view domainFormView) {
	if view.Providers == nil {
		view.Providers, _ = s.catalogs.List(r.Context(), model.Catalogs["providers"])
	}
	view.Currencies = currencies
	title := "Add domain"
	view.CancelURL = "/domains"
	if view.IsEdit {
		title = "Edit " + view.Domain.Domain
		view.CancelURL = "/domains/" + strconv.FormatInt(view.Domain.ID, 10)
	}
	data := s.newPageData(w, r, title, "domains")
	data.Data = view
	s.render(w, r, "domain_form", data)
}

func (s *Server) handleDomainCreate(w http.ResponseWriter, r *http.Request) {
	d, pricing, errs := parseDomainForm(r)
	if len(errs) > 0 {
		s.renderDomainForm(w, r, domainFormView{
			Action: "/domains", Domain: d, Pricing: pricing, Errors: errs,
		})
		return
	}
	st := &model.DomainStore{DB: s.db}
	id, err := st.Create(r.Context(), d, pricing)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", d.Domain+" added.")
	http.Redirect(w, r, "/domains/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleDomainUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	d, pricing, errs := parseDomainForm(r)
	d.ID = id
	if len(errs) > 0 {
		s.renderDomainForm(w, r, domainFormView{
			Action: "/domains/" + strconv.FormatInt(id, 10) + "/update",
			IsEdit: true, Domain: d, Pricing: pricing, Errors: errs,
		})
		return
	}
	st := &model.DomainStore{DB: s.db}
	if err := st.Update(r.Context(), d, pricing); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", d.Domain+" saved.")
	http.Redirect(w, r, "/domains/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// parseDomainForm parses + validates the domain form.
func parseDomainForm(r *http.Request) (*model.Domain, *model.Pricing, map[string]string) {
	errs := map[string]string{}
	d := &model.Domain{
		Domain:     strings.TrimSpace(r.FormValue("domain")),
		Extension:  nullStrFormValue(r, "extension"),
		Ns1:        nullStrFormValue(r, "ns1"),
		Ns2:        nullStrFormValue(r, "ns2"),
		Ns3:        nullStrFormValue(r, "ns3"),
		ProviderID: nullIntFormValue(r, "provider_id"),
		Active:     r.FormValue("active") != "",
	}
	if d.Domain == "" {
		errs["domain"] = "Domain is required."
	}
	d.OwnedSince = dateFormValue(r, errs, "owned_since")
	return d, parsePricingForm(r, errs), errs
}
