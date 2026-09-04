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
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// miscSection builds the misc services list/detail/delete section.
func (s *Server) miscSection() *section {
	st := &model.MiscStore{DB: s.db}
	return &section{
		Base:        routeMisc,
		Kind:        "misc",
		Title:       "Misc Services",
		Singular:    "Misc service",
		ServiceType: model.ServiceMisc,
		AddLabel:    "＋ Add service",
		SearchHint:  "Search name…",
		EmptyTitle:  "No misc services yet",
		EmptySub:    "Add your first one to start tracking.",
		DefaultSort: "name",
		Columns: []listColumn{
			{Key: "name", Label: "Name", Sortable: true},
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
				rows = append(rows, listRow{
					Link:          routeMisc + "/" + id,
					EditURL:       routeMisc + "/" + id + "/edit",
					DeleteURL:     routeMisc + "/" + id + "/delete",
					DeleteConfirm: "Delete " + it.Name + "?",
					Cells: []listCell{
						{Main: it.Name, Dot: dot, Link: routeMisc + "/" + id},
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
			monthly, yearly := s.costPairUSDFor(r, model.ServiceMisc)
			return []statCard{
				{Label: "Total", Value: strconv.Itoa(active + inactive)},
				{Label: "Active", Value: strconv.Itoa(active)},
				{Label: "Monthly cost", Value: monthly},
				{Label: "Yearly cost", Value: yearly},
			}
		},
		Delete: st.Delete,
		Detail: func(r *http.Request, id int64) (*detailView, error) {
			m, pricing, err := st.Get(r.Context(), id)
			if err != nil {
				return nil, err
			}
			badges := []detailBadge{}
			if m.Active {
				badges = append(badges, detailBadge{Label: "Active", Class: "badge-ok"})
			} else {
				badges = append(badges, detailBadge{Label: "Inactive", Class: "badge-off"})
			}
			owned := "—"
			if m.OwnedSince.Valid {
				owned = m.OwnedSince.String
			}
			sid := strconv.FormatInt(m.ID, 10)
			return &detailView{
				Title:         m.Name,
				Badges:        badges,
				EditURL:       routeMisc + "/" + sid + "/edit",
				DeleteURL:     routeMisc + "/" + sid + "/delete",
				DeleteConfirm: "Delete " + m.Name + "?",
				Cards: []infoCard{
					{Title: "Service", Pairs: []kvPair{
						{K: "Name", V: m.Name},
						{K: "Owned since", V: owned, Mono: true},
					}},
					{Title: "Pricing", Pairs: pricingPairs(pricing), Empty: "No pricing attached."},
				},
			}, nil
		},
	}
}

// miscFormView is the template payload for the misc form.
type miscFormView struct {
	Action     string
	CancelURL  string
	IsEdit     bool
	Misc       *model.MiscService
	Pricing    *model.Pricing
	Currencies []string
	Errors     map[string]string
}

func (s *Server) handleMiscNew(w http.ResponseWriter, r *http.Request) {
	s.renderMiscForm(w, r, miscFormView{
		Action: routeMisc,
		Misc:   &model.MiscService{Active: true},
	})
}

func (s *Server) handleMiscEdit(w http.ResponseWriter, r *http.Request) {
	st := &model.MiscStore{DB: s.db}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, pricing, err := st.Get(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.renderMiscForm(w, r, miscFormView{
		Action:  routeMisc + "/" + strconv.FormatInt(id, 10) + "/update",
		IsEdit:  true,
		Misc:    m,
		Pricing: pricing,
	})
}

func (s *Server) renderMiscForm(w http.ResponseWriter, r *http.Request, view miscFormView) {
	view.Currencies = currencies
	title := "Add service"
	view.CancelURL = routeMisc
	if view.IsEdit {
		title = "Edit " + view.Misc.Name
		view.CancelURL = routeMisc + "/" + strconv.FormatInt(view.Misc.ID, 10)
	}
	data := s.newPageData(w, r, title, "misc")
	data.Data = view
	s.render(w, r, "misc_form", data)
}

func (s *Server) handleMiscCreate(w http.ResponseWriter, r *http.Request) {
	m, pricing, errs := parseMiscForm(r)
	if len(errs) > 0 {
		s.renderMiscForm(w, r, miscFormView{
			Action: routeMisc, Misc: m, Pricing: pricing, Errors: errs,
		})
		return
	}
	st := &model.MiscStore{DB: s.db}
	id, err := st.Create(r.Context(), m, pricing)
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", m.Name+" added.")
	http.Redirect(w, r, routeMisc+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (s *Server) handleMiscUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	m, pricing, errs := parseMiscForm(r)
	m.ID = id
	if len(errs) > 0 {
		s.renderMiscForm(w, r, miscFormView{
			Action: routeMisc + "/" + strconv.FormatInt(id, 10) + "/update",
			IsEdit: true, Misc: m, Pricing: pricing, Errors: errs,
		})
		return
	}
	st := &model.MiscStore{DB: s.db}
	if err := st.Update(r.Context(), m, pricing); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	s.touchDashboard()
	s.setFlash(w, r, "ok", m.Name+" saved.")
	http.Redirect(w, r, routeMisc+"/"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

// parseMiscForm parses + validates the misc form.
func parseMiscForm(r *http.Request) (*model.MiscService, *model.Pricing, map[string]string) {
	errs := map[string]string{}
	m := &model.MiscService{
		Name:   strings.TrimSpace(r.FormValue("name")),
		Active: r.FormValue("active") != "",
	}
	if m.Name == "" {
		errs["name"] = "Name is required."
	}
	m.OwnedSince = dateFormValue(r, errs, "owned_since")
	return m, parsePricingForm(r, errs), errs
}
