package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// catalogView is the template payload for catalog list pages.
type catalogView struct {
	Kind       string // url kind, e.g. "providers"
	Title      string
	Items      []model.CatalogItem
	Error      string        // inline error shown above the table (htmx swaps)
	Counts     map[int64]int // usage counts (labels only)
	ShowCounts bool
}

// catalogKind resolves the {kind} path value to a catalog definition.
func catalogKind(r *http.Request) (string, model.CatalogKind, bool) {
	kindStr := r.PathValue("kind")
	kind, ok := model.Catalogs[kindStr]
	return kindStr, kind, ok
}

// handleCatalogList renders GET /catalogs/{kind}.
func (s *Server) handleCatalogList(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	items, err := s.catalogs.List(r.Context(), kind)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	view := catalogView{Kind: kindStr, Title: kind.Title, Items: items}
	if kindStr == "labels" {
		view.ShowCounts = true
		view.Counts = map[int64]int{}
		counts, _ := (&model.LabelStore{DB: s.db}).AllWithCounts(r.Context())
		for _, c := range counts {
			view.Counts[c.ID] = c.Used
		}
	}
	data := s.newPageData(w, r, kind.Title, kindStr)
	data.Data = view
	s.render(w, r, "catalogs", data)
}

// handleCatalogCreate handles POST /catalogs/{kind}.
func (s *Server) handleCatalogCreate(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))

	errMsg := ""
	if name == "" {
		errMsg = "Name is required."
	} else if _, err := s.catalogs.Create(r.Context(), kind, name); err != nil {
		errMsg = "That name already exists."
	}
	s.respondCatalogMutation(w, r, kindStr, kind, errMsg)
}

// handleCatalogUpdate handles POST /catalogs/{kind}/{id}/update.
func (s *Server) handleCatalogUpdate(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))

	errMsg := ""
	if name == "" {
		errMsg = "Name is required."
	} else if err := s.catalogs.Update(r.Context(), kind, id, name); err != nil {
		errMsg = "That name already exists."
	}
	s.respondCatalogMutation(w, r, kindStr, kind, errMsg)
}

// handleCatalogDelete handles POST /catalogs/{kind}/{id}/delete. Deleting an
// entry still referenced by services is refused with an inline message.
func (s *Server) handleCatalogDelete(w http.ResponseWriter, r *http.Request) {
	kindStr, kind, ok := catalogKind(r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	errMsg := ""
	// UsageCount up front so the refusal message has the count without
	// re-running it after the failed delete.
	n, _ := s.catalogs.UsageCount(r.Context(), kind, id)
	if err := s.catalogs.Delete(r.Context(), kind, id); err != nil {
		if errors.Is(err, model.ErrInUse) {
			errMsg = "In use by " + strconv.Itoa(n) + " service(s) — remove those first."
		} else {
			errMsg = "Delete failed."
		}
	}
	s.respondCatalogMutation(w, r, kindStr, kind, errMsg)
}

// respondCatalogMutation re-renders the list partial for htmx requests, or
// redirects back to the list (with flash on error) for plain posts.
func (s *Server) respondCatalogMutation(w http.ResponseWriter, r *http.Request, kindStr string, kind model.CatalogKind, errMsg string) {
	if r.Header.Get("HX-Request") != "true" {
		if errMsg != "" {
			setFlash(w, "err", errMsg)
		} else {
			s.touchDashboard()
			setFlash(w, "ok", "Saved.")
		}
		http.Redirect(w, r, "/catalogs/"+kindStr, http.StatusSeeOther)
		return
	}
	if errMsg == "" {
		s.touchDashboard() // successful htmx mutations invalidate too
	}
	items, err := s.catalogs.List(r.Context(), kind)
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	data := s.newPageData(w, r, kind.Title, kindStr)
	data.Data = catalogView{Kind: kindStr, Title: kind.Title, Items: items, Error: errMsg}
	s.renderNamed(w, "catalogs", "catalog_list", data)
}
