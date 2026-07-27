package web

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"idlerthing/internal/model"
)

// extrasView carries the labels/notes/IPs/DNS cards for service detail pages.
type extrasView struct {
	ServiceID   int64
	ServiceType int
	BackURL     string
	Labels      []model.CatalogItem
	AllLabels   []model.CatalogItem
	LabelsFull  bool
	Notes       []model.Note
	IPs         []model.IP
	DNS         []model.DNSListItem
	ShowDNS     bool
}

// buildExtras loads the labels/notes/IPs cards for one service. DNS records
// are only attached for servers and domains. Relation errors propagate —
// a detail page with silently-empty cards looks like data loss.
func (s *Server) buildExtras(r *http.Request, serviceID int64, serviceType int) (*extrasView, error) {
	ctx := r.Context()
	v := &extrasView{
		ServiceID:   serviceID,
		ServiceType: serviceType,
		BackURL:     r.URL.Path,
	}
	var err error
	labels := &model.LabelStore{DB: s.db}
	if v.Labels, err = labels.ListFor(ctx, serviceID, serviceType); err != nil {
		return nil, err
	}
	if v.AllLabels, err = s.catalogs.List(ctx, model.Catalogs["labels"]); err != nil {
		return nil, err
	}
	v.LabelsFull = len(v.Labels) >= model.MaxLabelsPerService

	notes := &model.NoteStore{DB: s.db}
	if v.Notes, err = notes.ListFor(ctx, serviceID, serviceType); err != nil {
		return nil, err
	}

	ips := &model.IPStore{DB: s.db}
	if v.IPs, err = ips.ListFor(ctx, serviceID, serviceType); err != nil {
		return nil, err
	}

	dns := &model.DNSStore{DB: s.db}
	switch serviceType {
	case model.ServiceServer:
		if v.DNS, err = dns.ListForServer(ctx, serviceID); err != nil {
			return nil, err
		}
		v.ShowDNS = true
	case model.ServiceDomain:
		if v.DNS, err = dns.ListForDomain(ctx, serviceID); err != nil {
			return nil, err
		}
		v.ShowDNS = true
	}
	return v, nil
}

// redirectBack redirects to the validated "back" form field. Only
// same-origin absolute paths pass: no scheme, no host, no backslashes
// (browsers normalize /\evil.com to //evil.com).
func redirectBack(w http.ResponseWriter, r *http.Request, fallback string) {
	back := r.FormValue("back")
	if u, err := url.Parse(back); err != nil || u.Scheme != "" || u.Host != "" ||
		strings.Contains(back, "\\") || !strings.HasPrefix(back, "/") {
		back = fallback
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// formInt64 parses a required int64 form field.
func formInt64(r *http.Request, name string) (int64, error) {
	return strconv.ParseInt(r.FormValue(name), 10, 64)
}

// serviceExists verifies an extras target actually exists before attaching
// (crafted POSTs could otherwise create "(deleted)" orphans).
func (s *Server) serviceExists(r *http.Request, serviceType int, serviceID int64) bool {
	table, ok := model.ServiceTable[serviceType]
	if !ok {
		return false
	}
	var n int
	// Table names come from the fixed ServiceTable map.
	s.db.QueryRowContext(r.Context(),
		"SELECT COUNT(*) FROM "+table+" WHERE id = ?", serviceID).Scan(&n)
	return n > 0
}

// ---------- Labels ----------

// handleLabelAssign handles POST /labels/assign.
func (s *Server) handleLabelAssign(w http.ResponseWriter, r *http.Request) {
	serviceID, err1 := formInt64(r, "service_id")
	serviceType, err2 := formInt64(r, "service_type")
	if err1 != nil || err2 != nil || serviceType < 1 || serviceType > 6 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	if !s.serviceExists(r, int(serviceType), serviceID) {
		http.NotFound(w, r)
		return
	}

	labels := &model.LabelStore{DB: s.db}
	var labelID int64
	var err error
	if newName := strings.TrimSpace(r.FormValue("new_label")); newName != "" {
		labelID, err = labels.FindOrCreate(r.Context(), newName)
	} else {
		labelID, err = formInt64(r, "label_id")
	}
	if err != nil || labelID <= 0 {
		s.setFlash(w, r, "err", "Pick or create a label first.")
		redirectBack(w, r, "/")
		return
	}

	if err := labels.Assign(r.Context(), labelID, serviceID, int(serviceType)); err != nil {
		if errors.Is(err, model.ErrTooManyLabels) {
			s.setFlash(w, r, "err", fmt.Sprintf("Maximum of %d labels per service.", model.MaxLabelsPerService))
		} else {
			s.setFlash(w, r, "err", "Could not assign label.")
		}
	} else {
		s.setFlash(w, r, "ok", "Label assigned.")
	}
	s.touchDashboard()
	redirectBack(w, r, "/")
}

// handleLabelUnassign handles POST /labels/unassign.
func (s *Server) handleLabelUnassign(w http.ResponseWriter, r *http.Request) {
	labelID, err1 := formInt64(r, "label_id")
	serviceID, err2 := formInt64(r, "service_id")
	serviceType, err3 := formInt64(r, "service_type")
	if err1 != nil || err2 != nil || err3 != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	labels := &model.LabelStore{DB: s.db}
	if err := labels.Unassign(r.Context(), labelID, serviceID, int(serviceType)); err != nil {
		s.setFlash(w, r, "err", "Could not remove label.")
	} else {
		s.touchDashboard()
	}
	redirectBack(w, r, "/")
}

// ---------- Notes ----------

// handleNoteCreate handles POST /notes.
func (s *Server) handleNoteCreate(w http.ResponseWriter, r *http.Request) {
	serviceID, err1 := formInt64(r, "service_id")
	serviceType, err2 := formInt64(r, "service_type")
	body := strings.TrimSpace(r.FormValue("body"))
	if err1 != nil || err2 != nil || serviceType < 1 || serviceType > 6 || body == "" {
		s.setFlash(w, r, "err", "Note body is required.")
		redirectBack(w, r, "/notes")
		return
	}
	if !s.serviceExists(r, int(serviceType), serviceID) {
		http.NotFound(w, r)
		return
	}
	notes := &model.NoteStore{DB: s.db}
	if _, err := notes.Create(r.Context(), &model.Note{
		ServiceID:   sqlNullInt(serviceID),
		ServiceType: sqlNullInt(serviceType),
		Body:        body,
	}); err != nil {
		s.setFlash(w, r, "err", "Could not save note.")
	} else {
		s.setFlash(w, r, "ok", "Note added.")
		s.touchDashboard()
	}
	redirectBack(w, r, "/notes")
}

// handleNoteDelete handles POST /notes/{id}/delete.
func (s *Server) handleNoteDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	notes := &model.NoteStore{DB: s.db}
	if err := notes.Delete(r.Context(), id); err != nil {
		s.setFlash(w, r, "err", "Could not delete note.")
	} else {
		s.touchDashboard()
	}
	redirectBack(w, r, "/notes")
}

// noteIndexRow is one row of the notes index.
type noteIndexRow struct {
	ID        int64
	Body      string
	Target    string
	TargetURL string
	TypeLabel string
	Created   string
	BackURL   string
}

// handleNotesIndex renders GET /notes.
func (s *Server) handleNotesIndex(w http.ResponseWriter, r *http.Request) {
	notes := &model.NoteStore{DB: s.db}
	all, err := notes.ListAll(r.Context())
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	var rows []noteIndexRow
	for _, n := range all {
		body := n.Body
		// Rune-wise truncation: byte-slicing could split a multibyte
		// character and leak U+FFFD into the page.
		if runes := []rune(body); len(runes) > 120 {
			body = string(runes[:120]) + "…"
		}
		st := int(n.ServiceType.Int64)
		rows = append(rows, noteIndexRow{
			ID:        n.ID,
			Body:      body,
			Target:    n.Target,
			TargetURL: fmt.Sprintf("%s/%d", model.ServiceBasePath(st), n.ServiceID.Int64),
			TypeLabel: model.ServiceTypeLabel(st),
			Created:   dateOnly(n.CreatedAt),
		})
	}
	data := s.newPageData(w, r, "Notes", "notes")
	data.Data = notesIndexView{Rows: rows}
	s.render(w, r, "notes", data)
}

type notesIndexView struct {
	Rows []noteIndexRow
}

// ---------- IPs ----------

// handleIPCreate handles POST /ips.
func (s *Server) handleIPCreate(w http.ResponseWriter, r *http.Request) {
	serviceID, err1 := formInt64(r, "service_id")
	serviceType, err2 := formInt64(r, "service_type")
	if err1 != nil || err2 != nil || serviceType < 1 || serviceType > 6 {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if !s.serviceExists(r, int(serviceType), serviceID) {
		http.NotFound(w, r)
		return
	}
	raw := strings.TrimSpace(r.FormValue("address"))
	addr, err := netip.ParseAddr(raw)
	if err != nil {
		s.setFlash(w, r, "err", "Invalid IP address.")
		redirectBack(w, r, "/ips")
		return
	}
	ips := &model.IPStore{DB: s.db}
	if _, err := ips.Create(r.Context(), &model.IP{
		ServiceID:   serviceID,
		ServiceType: int(serviceType),
		Address:     addr.String(),
		IsIPv4:      addr.Is4(),
	}); err != nil {
		s.setFlash(w, r, "err", "That IP is already attached.")
		redirectBack(w, r, "/ips")
		return
	}
	s.setFlash(w, r, "ok", "IP added.")
	s.touchDashboard()
	redirectBack(w, r, "/ips")
}

// handleIPDelete handles POST /ips/{id}/delete.
func (s *Server) handleIPDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ips := &model.IPStore{DB: s.db}
	if err := ips.Delete(r.Context(), id); err != nil {
		s.setFlash(w, r, "err", "Could not delete IP.")
	} else {
		s.touchDashboard()
	}
	redirectBack(w, r, "/ips")
}

// ---------- WHOIS ----------

// whoisRateLimit serializes lookups to 1 per second per Server.
type whoisRateLimit struct {
	mu   sync.Mutex
	last time.Time
}

// ipwhoIsResp mirrors the ipwho.is response shape.
type ipwhoIsResp struct {
	Success    bool   `json:"success"`
	Country    string `json:"country"`
	Region     string `json:"region"`
	City       string `json:"city"`
	Connection struct {
		ASN int    `json:"asn"`
		Org string `json:"org"`
		ISP string `json:"isp"`
	} `json:"connection"`
}

// fetchWhois looks up one address (rate-limited, 5s timeout).
// errWhoisThrottled means a refresh came in under the 1/s throttle.
var errWhoisThrottled = errors.New("whois refresh throttled")

func (s *Server) fetchWhois(ctx context.Context, address string) (*model.WhoisData, error) {
	// Throttle without sleeping under the lock: too-recent calls bounce
	// immediately instead of queueing goroutines.
	s.whoisRate.mu.Lock()
	tooRecent := time.Since(s.whoisRate.last) < time.Second
	if !tooRecent {
		s.whoisRate.last = time.Now()
	}
	s.whoisRate.mu.Unlock()
	if tooRecent {
		return nil, errWhoisThrottled
	}

	base := s.whoisURL
	if base == "" {
		base = "https://ipwho.is"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/"+address, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("whois service returned %d", resp.StatusCode)
	}
	var body ipwhoIsResp
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); err != nil {
		return nil, err
	}
	if !body.Success {
		return nil, errors.New("whois lookup failed")
	}
	data := &model.WhoisData{
		Country:   body.Country,
		Region:    body.Region,
		City:      body.City,
		Org:       body.Connection.Org,
		Isp:       body.Connection.ISP,
		FetchedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if body.Connection.ASN > 0 {
		data.Asn = "AS" + strconv.Itoa(body.Connection.ASN)
	}
	return data, nil
}

// handleIPWhois handles POST /ips/{id}/whois.
func (s *Server) handleIPWhois(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ips := &model.IPStore{DB: s.db}
	ip, err := ips.Get(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := s.fetchWhois(r.Context(), ip.Address)
	if err != nil {
		if errors.Is(err, errWhoisThrottled) {
			s.setFlash(w, r, "err", "Slow down — one whois refresh per second.")
		} else {
			s.setFlash(w, r, "err", "Whois refresh failed — keeping old data.")
		}
		redirectBack(w, r, "/ips")
		return
	}
	if err := ips.UpdateWhois(r.Context(), id, data); err != nil {
		s.setFlash(w, r, "err", "Could not save whois data.")
		redirectBack(w, r, "/ips")
		return
	}
	s.setFlash(w, r, "ok", "Whois refreshed for "+ip.Address+".")
	redirectBack(w, r, "/ips")
}

// ipIndexRow is one row of the IPs index.
type ipIndexRow struct {
	ID        int64
	Address   string
	TypeBadge string
	Target    string
	TargetURL string
	TypeLabel string
	Country   string
	OrgISP    string
	ASN       string
	Fetched   string
}

// handleIPsIndex renders GET /ips.
func (s *Server) handleIPsIndex(w http.ResponseWriter, r *http.Request) {
	ips := &model.IPStore{DB: s.db}
	all, err := ips.ListAll(r.Context())
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	var rows []ipIndexRow
	for _, ip := range all {
		badge := "IPv6"
		if ip.IsIPv4 {
			badge = "IPv4"
		}
		org := ip.Org.String
		if org == "" {
			org = ip.Isp.String
		}
		fetched := "never"
		if ip.FetchedAt.Valid {
			fetched = dateOnly(ip.FetchedAt.String)
		}
		rows = append(rows, ipIndexRow{
			ID:        ip.ID,
			Address:   ip.Address,
			TypeBadge: badge,
			Target:    ip.Target,
			TargetURL: fmt.Sprintf("%s/%d", model.ServiceBasePath(ip.ServiceType), ip.ServiceID),
			TypeLabel: model.ServiceTypeLabel(ip.ServiceType),
			Country:   dash(ip.Country.String),
			OrgISP:    dash(org),
			ASN:       dash(ip.Asn.String),
			Fetched:   fetched,
		})
	}
	data := s.newPageData(w, r, "IPs", "ips")
	data.Data = ipsIndexView{Rows: rows}
	s.render(w, r, "ips", data)
}

type ipsIndexView struct {
	Rows []ipIndexRow
}

func sqlNullInt(i int64) sql.NullInt64 { return sql.NullInt64{Int64: i, Valid: true} }

func dateOnly(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}
