package web

import (
	"database/sql"
	"net/http"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// dnsView is the template payload for the DNS index page.
type dnsView struct {
	Rows    []dnsRow
	Servers []model.CatalogItem
	Domains []model.CatalogItem
}

// dnsRow is one row of the DNS index.
type dnsRow struct {
	ID       int64
	Hostname string
	Type     string
	Address  string
	LinkName string
	LinkURL  string
}

// handleDNSIndex renders GET /dns.
func (s *Server) handleDNSIndex(w http.ResponseWriter, r *http.Request) {
	st := &model.DNSStore{DB: s.db}
	items, err := st.List(r.Context())
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	var rows []dnsRow
	for _, it := range items {
		rows = append(rows, dnsRow{
			ID:       it.ID,
			Hostname: it.Hostname,
			Type:     it.DNSType,
			Address:  it.Address,
			LinkName: dnsLinkName(it),
			LinkURL:  dnsLinkURL(it),
		})
	}

	servers, _ := s.serverOptions(r)
	domains, _ := s.domainOptions(r)
	data := s.newPageData(w, r, "DNS", "dns")
	data.Data = dnsView{Rows: rows, Servers: servers, Domains: domains}
	s.render(w, r, "dns", data)
}

// dnsLinkName/URL resolve the linked entity, if any.
func dnsLinkName(it model.DNSListItem) string {
	switch {
	case it.ServerName != "":
		return it.ServerName
	case it.DomainName != "":
		return it.DomainName
	case it.SharedName != "":
		return it.SharedName
	case it.ResellerName != "":
		return it.ResellerName
	default:
		return "—"
	}
}

func dnsLinkURL(it model.DNSListItem) string {
	switch {
	case it.ServerID.Valid:
		return "/servers/" + strconv.FormatInt(it.ServerID.Int64, 10)
	case it.DomainID.Valid:
		return "/domains/" + strconv.FormatInt(it.DomainID.Int64, 10)
	case it.SharedID.Valid:
		return "/shared/" + strconv.FormatInt(it.SharedID.Int64, 10)
	case it.ResellerID.Valid:
		return "/reseller/" + strconv.FormatInt(it.ResellerID.Int64, 10)
	default:
		return ""
	}
}

// serverOptions/domainOptions feed the link selects in the DNS form.
func (s *Server) serverOptions(r *http.Request) ([]model.CatalogItem, error) {
	rows, err := s.db.QueryContext(r.Context(),
		"SELECT id, hostname FROM servers ORDER BY hostname COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CatalogItem
	for rows.Next() {
		var it model.CatalogItem
		if err := rows.Scan(&it.ID, &it.Name); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

func (s *Server) domainOptions(r *http.Request) ([]model.CatalogItem, error) {
	rows, err := s.db.QueryContext(r.Context(),
		"SELECT id, domain FROM domains ORDER BY domain COLLATE NOCASE")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.CatalogItem
	for rows.Next() {
		var it model.CatalogItem
		if err := rows.Scan(&it.ID, &it.Name); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// parseDNSForm parses + validates the DNS form.
func parseDNSForm(r *http.Request) (*model.DNSRecord, map[string]string) {
	errs := map[string]string{}
	d := &model.DNSRecord{
		Hostname:   strings.TrimSpace(r.FormValue("hostname")),
		DNSType:    r.FormValue("dns_type"),
		Address:    strings.TrimSpace(r.FormValue("address")),
		ServerID:   nullIntFormValue(r, "server_id"),
		DomainID:   nullIntFormValue(r, "domain_id"),
		SharedID:   nullIntFormValue(r, "shared_id"),
		ResellerID: nullIntFormValue(r, "reseller_id"),
	}
	if d.Hostname == "" {
		errs["hostname"] = "Hostname is required."
	}
	if d.Address == "" {
		errs["address"] = "Address is required."
	}
	valid := false
	for _, t := range model.DNSTypes {
		if d.DNSType == t {
			valid = true
			break
		}
	}
	if !valid {
		d.DNSType = "A"
	}
	return d, errs
}

// dnsFormView is the template payload for the DNS edit form.
type dnsFormView struct {
	Record  *model.DNSRecord
	Servers []model.CatalogItem
	Domains []model.CatalogItem
	Errors  map[string]string
}

// handleDNSCreate handles POST /dns.
func (s *Server) handleDNSCreate(w http.ResponseWriter, r *http.Request) {
	rec, errs := parseDNSForm(r)
	if len(errs) > 0 {
		setFlash(w, "err", "Hostname and address are required.")
		redirectBack(w, r, "/dns")
		return
	}
	st := &model.DNSStore{DB: s.db}
	if _, err := st.Create(r.Context(), rec); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	setFlash(w, "ok", "DNS record added.")
	http.Redirect(w, r, "/dns", http.StatusSeeOther)
}

// handleDNSEdit renders GET /dns/{id}/edit.
func (s *Server) handleDNSEdit(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st := &model.DNSStore{DB: s.db}
	rec, err := st.Get(r.Context(), id)
	if err == sql.ErrNoRows {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	servers, _ := s.serverOptions(r)
	domains, _ := s.domainOptions(r)
	data := s.newPageData(w, r, "Edit "+rec.Hostname, "dns")
	data.Data = dnsFormView{Record: rec, Servers: servers, Domains: domains}
	s.render(w, r, "dns_form", data)
}

// handleDNSUpdate handles POST /dns/{id}/update.
func (s *Server) handleDNSUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	rec, errs := parseDNSForm(r)
	rec.ID = id
	if len(errs) > 0 {
		setFlash(w, "err", "Hostname and address are required.")
		redirectBack(w, r, "/dns")
		return
	}
	st := &model.DNSStore{DB: s.db}
	if err := st.Update(r.Context(), rec); err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	setFlash(w, "ok", "DNS record saved.")
	http.Redirect(w, r, "/dns", http.StatusSeeOther)
}

// handleDNSDelete handles POST /dns/{id}/delete.
func (s *Server) handleDNSDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	st := &model.DNSStore{DB: s.db}
	if err := st.Delete(r.Context(), id); err != nil {
		setFlash(w, "err", "Could not delete record.")
	}
	redirectBack(w, r, "/dns")
}
