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
	"encoding/json"
	"fmt"
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
	srv := j.serverFields(errs)
	j.checkBounds(errs)
	return srv, j.disksToModel(), j.pricingToModel(errs), errs
}

// serverFields maps the scalar fields and validates them.
func (j *serverJSON) serverFields(errs map[string]string) *model.Server {
	srv := &model.Server{
		Hostname:      strings.TrimSpace(j.Hostname),
		ServerType:    j.ServerType,
		OsID:          ptrToRef(j.OsID),
		ProviderID:    ptrToRef(j.ProviderID),
		LocationID:    ptrToRef(j.LocationID),
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
	srv.CPUModel = nullStrIf(j.CPUModel)
	srv.NetworkType = nullStrIf(j.NetworkType)
	srv.Ns1 = nullStrIf(j.Ns1)
	srv.Ns2 = nullStrIf(j.Ns2)
	srv.OwnedSince = jsonDate(errs, "owned_since", j.OwnedSince)
	return srv
}

// nullStrIf wraps a non-blank string as a valid NullString (trimmed, like
// the web form's nullStrFormValue — "  " must not become a non-NULL value).
func nullStrIf(s string) sql.NullString {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// jsonDate validates a yyyy-mm-dd DTO field, recording an error when invalid.
func jsonDate(errs map[string]string, field, value string) sql.NullString {
	if value == "" {
		return sql.NullString{}
	}
	if _, err := time.Parse(time.DateOnly, value); err != nil {
		errs[field] = "invalid date (want yyyy-mm-dd)"
		return sql.NullString{}
	}
	return sql.NullString{String: value, Valid: true}
}

// checkBounds applies the shared numeric caps (aligned with the web form).
func (j *serverJSON) checkBounds(errs map[string]string) {
	for _, v := range []struct {
		name string
		p    *int64
		max  int64
	}{
		{"ram_as_mb", j.RamAsMB, 1 << 30},
		{"cpu", j.CPU, 1024},
		{"bandwidth_as_mb", j.BandwidthAsMB, 1 << 30},
		{"link_speed", j.LinkSpeed, 1 << 20},
		{"ssh_port", j.SSHPort, 65535},
	} {
		if v.p != nil && (*v.p < 0 || *v.p > v.max) {
			errs[v.name] = "out of range"
		}
	}
	// Same cap as the web form's fixed disk rows: the edit form only shows
	// (and on save re-inserts) four, so a fifth disk stored via the API would
	// be silently dropped by the next form edit.
	if len(j.Disks) > maxServerDisks {
		errs["disks"] = fmt.Sprintf("at most %d disks", maxServerDisks)
	}
	for i, d := range j.Disks {
		if d.SizeAsMB < 0 || d.SizeAsMB > 1<<30 {
			errs[fmt.Sprintf("disks[%d].size_as_mb", i)] = "out of range"
		}
	}
}

// maxServerDisks is the number of disk rows the server form offers.
const maxServerDisks = 4

// checkCatalogRefs turns dangling os/provider/location ids into 422 field
// errors, instead of letting the FOREIGN KEY violation surface as a 500.
func (s *Server) checkCatalogRefs(ctx context.Context, srv *model.Server, errs map[string]string) error {
	for _, ref := range []struct {
		field, kind string
		id          sql.NullInt64
	}{
		{"os_id", "os", srv.OsID},
		{"provider_id", "providers", srv.ProviderID},
		{"location_id", "locations", srv.LocationID},
	} {
		if !ref.id.Valid {
			continue
		}
		ok, err := s.catalogs.Exists(ctx, model.Catalogs[ref.kind], ref.id.Int64)
		if err != nil {
			return err
		}
		if !ok {
			errs[ref.field] = "unknown " + ref.kind + " id"
		}
	}
	return nil
}

// disksToModel keeps positive sizes, normalizing media.
func (j *serverJSON) disksToModel() []model.ServerDisk {
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
	return disks
}

// pricingToModel converts the optional pricing block.
func (j *serverJSON) pricingToModel(errs map[string]string) *model.Pricing {
	if j.Pricing == nil {
		return nil
	}
	if !validPrice(j.Pricing.Price) {
		errs["price"] = "price must be finite and > 0"
		return nil
	}
	term := j.Pricing.Term
	if term < model.TermMonthly || term > model.TermOneTime {
		term = model.TermMonthly
	}
	return &model.Pricing{
		Currency:    validCurrency(j.Pricing.Currency),
		Price:       j.Pricing.Price,
		Term:        term,
		NextDueDate: jsonDate(errs, "next_due_date", j.Pricing.NextDueDate),
	}
}

// ptrToNull maps an absent numeric field to NULL and keeps any present
// value — including 0, which the web form also stores as a valid 0 (for
// bandwidth NULL means UNLIMITED, so 0 → NULL on a GET/PUT round-trip
// silently turned a metered plan into an unlimited one). Range checks
// happen in checkBounds.
func ptrToNull(p *int64) sql.NullInt64 {
	if p == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *p, Valid: true}
}

// ptrToRef maps a catalog reference: absent or non-positive → NULL (no
// entry), matching the form's nullIntFormValue.
func ptrToRef(p *int64) sql.NullInt64 {
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
	// Relation errors propagate — a partial 200 would look like "no labels".
	labels, err := (&model.LabelStore{DB: s.db}).ListFor(r.Context(), id, model.ServiceServer)
	if err != nil {
		return nil, err
	}
	ips, err := (&model.IPStore{DB: s.db}).ListFor(r.Context(), id, model.ServiceServer)
	if err != nil {
		return nil, err
	}
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
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	writeTypedList(w, r, items)
}

// handleAPIServerGet returns one server with relations.
func (s *Server) handleAPIServerGet(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, errMsgNotFound)
		return
	}
	payload, err := s.apiServerPayload(r, id)
	if err == sql.ErrNoRows {
		writeAPIError(w, http.StatusNotFound, errMsgNotFound)
		return
	}
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
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
	if err := s.checkCatalogRefs(r.Context(), srv, errs); err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "fields": errs})
		return
	}
	id, err := s.servers.Create(r.Context(), srv, disks, pricing)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	s.touchDashboard()
	payload, err := s.apiServerPayload(r, id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	writeJSON(w, http.StatusCreated, payload)
}

// handleAPIServerUpdate handles PUT /api/servers/{id}.
func (s *Server) handleAPIServerUpdate(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, errMsgNotFound)
		return
	}
	j, ok := decodeServerJSON(w, r)
	if !ok {
		return
	}
	srv, disks, pricing, errs := j.toModel()
	if err := s.checkCatalogRefs(r.Context(), srv, errs); err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	if len(errs) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "validation failed", "fields": errs})
		return
	}
	srv.ID = id
	if err := s.servers.Update(r.Context(), srv, disks, pricing); err == sql.ErrNoRows {
		writeAPIError(w, http.StatusNotFound, errMsgNotFound)
		return
	} else if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	s.touchDashboard()
	payload, err := s.apiServerPayload(r, id)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleAPIServerDelete handles DELETE /api/servers/{id}.
func (s *Server) handleAPIServerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeAPIError(w, http.StatusNotFound, errMsgNotFound)
		return
	}
	if err := s.servers.Delete(r.Context(), id); err == sql.ErrNoRows {
		writeAPIError(w, http.StatusNotFound, errMsgNotFound)
		return
	} else if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	s.touchDashboard()
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}
