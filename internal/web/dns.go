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

// dnsParentError enforces the link rules: at most ONE parent service, and
// it must exist. Returns "" when the links are valid.
func (s *Server) dnsParentError(r *http.Request, d *model.DNSRecord) string {
	// Table names are compile-time constants here.
	parents := []struct {
		table string
		id    sql.NullInt64
	}{
		{"servers", d.ServerID},
		{"domains", d.DomainID},
		{"shared_hosting", d.SharedID},
		{"reseller_hosting", d.ResellerID},
	}
	var set int
	var table string
	var id int64
	for _, p := range parents {
		if p.id.Valid {
			set++
			table, id = p.table, p.id.Int64
		}
	}
	if set > 1 {
		return "Link at most one service."
	}
	if set == 1 {
		var n int
		if err := s.db.QueryRowContext(r.Context(),
			"SELECT COUNT(*) FROM "+table+" WHERE id = ?", id).Scan(&n); err != nil || n == 0 {
			return "Linked service does not exist."
		}
	}
	return ""
}

// dnsFormError flashes the first form error and redirects back to /dns.
func (s *Server) dnsFormError(w http.ResponseWriter, r *http.Request, errs map[string]string) {
	msg := "Hostname and address are required."
	if e, ok := errs["link"]; ok {
		msg = e
	}
	s.setFlash(w, r, "err", msg)
	redirectBack(w, r, "/dns")
}

// handleDNSCreate handles POST /dns.
func (s *Server) handleDNSCreate(w http.ResponseWriter, r *http.Request) {
	rec, errs := parseDNSForm(r)
	if msg := s.dnsParentError(r, rec); msg != "" {
		errs["link"] = msg
	}
	if len(errs) > 0 {
		s.dnsFormError(w, r, errs)
		return
	}
	st := &model.DNSStore{DB: s.db}
	if _, err := st.Create(r.Context(), rec); err != nil {
		if err == sql.ErrNoRows {
			// Parent vanished between validation and insert.
			s.setFlash(w, r, "err", "Linked service does not exist.")
			redirectBack(w, r, "/dns")
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.setFlash(w, r, "ok", "DNS record added.")
	s.touchDashboard()
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
	if msg := s.dnsParentError(r, rec); msg != "" {
		errs["link"] = msg
	}
	if len(errs) > 0 {
		s.dnsFormError(w, r, errs)
		return
	}
	st := &model.DNSStore{DB: s.db}
	if err := st.Update(r.Context(), rec); err != nil {
		if err == sql.ErrNoRows {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	s.setFlash(w, r, "ok", "DNS record saved.")
	s.touchDashboard()
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
		s.setFlash(w, r, "err", "Could not delete record.")
	} else {
		s.touchDashboard()
	}
	redirectBack(w, r, "/dns")
}
