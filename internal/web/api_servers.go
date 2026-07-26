package web

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
)

// serverJSON is the API DTO for servers (mirrors the web form fields).
type serverJSON struct {
	Hostname      string       `json:"hostname"`
	ServerType    int          `json:"server_type"`
	OsID          *int64       `json:"os_id"`
	ProviderID    *int64       `json:"provider_id"`
	LocationID    *int64       `json:"location_id"`
	RamAsMB       *int64       `json:"ram_as_mb"`
	CPU           *int64       `json:"cpu"`
	CPUModel      string       `json:"cpu_model"`
	BandwidthAsMB *int64       `json:"bandwidth_as_mb"`
	LinkSpeed     *int64       `json:"link_speed"`
	NetworkType   string       `json:"network_type"`
	Ns1           string       `json:"ns1"`
	Ns2           string       `json:"ns2"`
	SSHPort       *int64       `json:"ssh_port"`
	Active        *bool        `json:"active"`
	ShowPublic    *bool        `json:"show_public"`
	WasPromo      *bool        `json:"was_promo"`
	Transferrable *bool        `json:"transferrable"`
	OwnedSince    string       `json:"owned_since"`
	Disks         []diskJSON   `json:"disks"`
	Pricing       *pricingJSON `json:"pricing"`
}

type diskJSON struct {
	SizeAsMB int64  `json:"size_as_mb"`
	Media    string `json:"media"`
}

type pricingJSON struct {
	Currency    string  `json:"currency"`
	Price       float64 `json:"price"`
	Term        int     `json:"term"`
	NextDueDate string  `json:"next_due_date"`
}

// toModel converts + validates the DTO (same rules as the web form).
func (j *serverJSON) toModel() (*model.Server, []model.ServerDisk, *model.Pricing, map[string]string) {
	errs := map[string]string{}
	srv := &model.Server{
		Hostname:      strings.TrimSpace(j.Hostname),
		ServerType:    j.ServerType,
		OsID:          ptrToNull(j.OsID),
		ProviderID:    ptrToNull(j.ProviderID),
		LocationID:    ptrToNull(j.LocationID),
		RamAsMB:       ptrToNull(j.RamAsMB),
		CPU:           ptrToNull(j.CPU),
		BandwidthAsMB: ptrToNull(j.BandwidthAsMB),
		LinkSpeed:     ptrToNull(j.LinkSpeed),
		SSHPort:       ptrToNull(j.SSHPort),
		Active:        j.Active == nil || *j.Active, // default true
		ShowPublic:    j.ShowPublic != nil && *j.ShowPublic,
		WasPromo:      j.WasPromo != nil && *j.WasPromo,
		Transferrable: j.Transferrable != nil && *j.Transferrable,
	}
	if srv.Hostname == "" {
		errs["hostname"] = "hostname is required"
	}
	if srv.ServerType < model.TypeKVM || srv.ServerType > model.TypeNAT {
		srv.ServerType = model.TypeKVM
	}
	if j.CPUModel != "" {
		srv.CPUModel = sql.NullString{String: j.CPUModel, Valid: true}
	}
	if j.NetworkType != "" {
		srv.NetworkType = sql.NullString{String: j.NetworkType, Valid: true}
	}
	if j.Ns1 != "" {
		srv.Ns1 = sql.NullString{String: j.Ns1, Valid: true}
	}
	if j.Ns2 != "" {
		srv.Ns2 = sql.NullString{String: j.Ns2, Valid: true}
	}
	if j.OwnedSince != "" {
		if _, err := time.Parse("2006-01-02", j.OwnedSince); err != nil {
			errs["owned_since"] = "invalid date (want yyyy-mm-dd)"
		} else {
			srv.OwnedSince = sql.NullString{String: j.OwnedSince, Valid: true}
		}
	}
	for _, v := range []struct {
		name string
		p    *int64
	}{
		{"ram_as_mb", j.RamAsMB}, {"cpu", j.CPU}, {"bandwidth_as_mb", j.BandwidthAsMB},
		{"link_speed", j.LinkSpeed}, {"ssh_port", j.SSHPort},
	} {
		if v.p != nil && *v.p < 0 {
			errs[v.name] = "must be >= 0"
		}
	}

	var disks []model.ServerDisk
	for _, d := range j.Disks {
		if d.SizeAsMB <= 0 {
			continue
		}
		media := d.Media
		switch media {
		case "SSD", "HDD", "NVMe":
		default:
			media = "SSD"
		}
		disks = append(disks, model.ServerDisk{SizeAsMB: d.SizeAsMB, Media: media})
	}

	var pricing *model.Pricing
	if j.Pricing != nil {
		if j.Pricing.Price <= 0 {
			errs["price"] = "price must be > 0"
		} else {
			currency := validCurrency(j.Pricing.Currency)
			term := j.Pricing.Term
			if term < model.TermMonthly || term > model.TermOneTime {
				term = model.TermMonthly
			}
			pricing = &model.Pricing{Currency: currency, Price: j.Pricing.Price, Term: term}
			if j.Pricing.NextDueDate != "" {
				if _, err := time.Parse("2006-01-02", j.Pricing.NextDueDate); err != nil {
					errs["next_due_date"] = "invalid date (want yyyy-mm-dd)"
				} else {
					pricing.NextDueDate = sql.NullString{String: j.Pricing.NextDueDate, Valid: true}
				}
			}
		}
	}
	return srv, disks, pricing, errs
}

func ptrToNull(p *int64) sql.NullInt64 {
	if p == nil || *p <= 0 {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// decodeServerJSON reads the request body as a serverJSON (1 MB cap).
func decodeServerJSON(w http.ResponseWriter, r *http.Request) (*serverJSON, bool) {
	var j serverJSON
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&j)
	if err != nil {
		if strings.Contains(err.Error(), "too large") {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "body too large")
		} else {
			writeAPIError(w, http.StatusBadRequest, "invalid JSON body")
		}
		return nil, false
	}
	return &j, true
}

// apiServerPayload builds the GET response for one server.
func (s *Server) apiServerPayload(r *http.Request, id int64) (map[string]any, error) {
	srv, disks, pricing, err := s.servers.Get(r.Context(), id)
	if err != nil {
		return nil, err
	}
	labels, _ := (&model.LabelStore{DB: s.db}).ListFor(r.Context(), id, model.ServiceServer)
	ips, _ := (&model.IPStore{DB: s.db}).ListFor(r.Context(), id, model.ServiceServer)
	return map[string]any{
		"data":    flatten(srv),
		"disks":   flattenSlice(disks),
		"pricing": flatten(pricing),
		"labels":  flattenSlice(labels),
		"ips":     flattenSlice(ips),
	}, nil
}

// handleAPIServers returns the paginated server list.
func (s *Server) handleAPIServers(w http.ResponseWriter, r *http.Request) {
	items, err := s.servers.List(r.Context(), model.ListOptions{Status: "all"})
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.writeList(w, r, flattenSlice(items))
}

// handleAPIServerGet returns one server with relations.
func (s *Server) handleAPIServerGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	payload, err := s.apiServerPayload(r, id)
	if err == sql.ErrNoRows {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleAPIServerCreate handles POST /api/servers.
func (s *Server) handleAPIServerCreate(w http.ResponseWriter, r *http.Request) {
	j, ok := decodeServerJSON(w, r)
	if !ok {
		return
	}
	srv, disks, pricing, errs := j.toModel()
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "fields": errs})
		return
	}
	id, err := s.servers.Create(r.Context(), srv, disks, pricing)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.touchDashboard()
	payload, err := s.apiServerPayload(r, id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusCreated, payload)
}

// handleAPIServerUpdate handles PUT /api/servers/{id}.
func (s *Server) handleAPIServerUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	j, ok := decodeServerJSON(w, r)
	if !ok {
		return
	}
	srv, disks, pricing, errs := j.toModel()
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "fields": errs})
		return
	}
	srv.ID = id
	if err := s.servers.Update(r.Context(), srv, disks, pricing); err == sql.ErrNoRows {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.touchDashboard()
	payload, err := s.apiServerPayload(r, id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleAPIServerDelete handles DELETE /api/servers/{id}.
func (s *Server) handleAPIServerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	if err := s.servers.Delete(r.Context(), id); err == sql.ErrNoRows {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	} else if err != nil {
		writeAPIError(w, http.StatusInternalServerError, "internal error")
		return
	}
	s.touchDashboard()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
