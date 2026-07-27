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
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"reflect"
	"strconv"
	"strings"

	"idlerthing/internal/model"
)

// apiMux returns the /api router. Token auth only — no session or CSRF.
// (The public YABS ingest route mounts outside this, directly on the main mux.)
func (s *Server) apiMux() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/servers", s.handleAPIServers)
	mux.HandleFunc("POST /api/servers", s.handleAPIServerCreate)
	mux.HandleFunc("GET /api/servers/{id}", s.handleAPIServerGet)
	mux.HandleFunc("PUT /api/servers/{id}", s.handleAPIServerUpdate)
	mux.HandleFunc("DELETE /api/servers/{id}", s.handleAPIServerDelete)

	mux.HandleFunc("GET /api/shared", s.handleAPIServiceList(model.ServiceShared))
	mux.HandleFunc("GET /api/shared/{id}", s.handleAPIServiceGet(model.ServiceShared))
	mux.HandleFunc("GET /api/reseller", s.handleAPIServiceList(model.ServiceReseller))
	mux.HandleFunc("GET /api/reseller/{id}", s.handleAPIServiceGet(model.ServiceReseller))
	mux.HandleFunc("GET /api/seedboxes", s.handleAPIServiceList(model.ServiceSeedbox))
	mux.HandleFunc("GET /api/seedboxes/{id}", s.handleAPIServiceGet(model.ServiceSeedbox))
	mux.HandleFunc("GET /api/domains", s.handleAPIServiceList(model.ServiceDomain))
	mux.HandleFunc("GET /api/domains/{id}", s.handleAPIServiceGet(model.ServiceDomain))
	mux.HandleFunc("GET /api/misc", s.handleAPIServiceList(model.ServiceMisc))
	mux.HandleFunc("GET /api/misc/{id}", s.handleAPIServiceGet(model.ServiceMisc))

	mux.HandleFunc("GET /api/pricings", s.handleAPIPricings)
	mux.HandleFunc("GET /api/ips", s.handleAPIIPs)
	mux.HandleFunc("GET /api/dns", s.handleAPIDNS)
	mux.HandleFunc("GET /api/labels", s.handleAPILabels)
	mux.HandleFunc("GET /api/notes", s.handleAPINotes)

	mux.HandleFunc("GET /api/providers", s.handleAPICatalog("providers"))
	mux.HandleFunc("GET /api/locations", s.handleAPICatalog("locations"))
	mux.HandleFunc("GET /api/os", s.handleAPICatalog("os"))

	return s.apiAuth(mux)
}

// apiAuth enforces Bearer-token auth against users.api_token_hash.
// Note: only the first token-bearing user row is checked — idlerthing is a
// single-user app (one admin), so a per-user token lookup is unnecessary.
func (s *Server) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" {
			writeAPIError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		sum := sha256.Sum256([]byte(token))
		given := hex.EncodeToString(sum[:])

		var stored *string
		err := s.db.QueryRowContext(r.Context(),
			"SELECT api_token_hash FROM users WHERE api_token_hash IS NOT NULL LIMIT 1").Scan(&stored)
		switch {
		case err != nil && err != sql.ErrNoRows:
			// A DB error must surface as 500 — masking it as 401 would send
			// operators chasing tokens while the database is broken.
			writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
			return
		// No token row, no stored hash, or a mismatch are all "unauthorized"
		// and must stay indistinguishable. The || short-circuits before
		// *stored on the ErrNoRows path.
		case err == sql.ErrNoRows || stored == nil ||
			subtle.ConstantTimeCompare([]byte(given), []byte(*stored)) != 1:
			writeAPIError(w, http.StatusUnauthorized, "unauthorized")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- JSON helpers ----------

// writeJSON sends v as JSON with the given status. Encoding happens into
// a buffer FIRST so a serialization failure (e.g. NaN) yields a clean 500
// instead of a partial 200.
func writeJSON(w http.ResponseWriter, status int, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	w.Write(buf.Bytes())
}

// writeAPIError sends the error envelope.
func writeAPIError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// apiPagination parses ?page=&per= (default 1/50, max 200).
type apiPagination struct {
	Page int
	Per  int
}

func parsePagination(r *http.Request) apiPagination {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	per, _ := strconv.Atoi(r.URL.Query().Get("per"))
	if page < 1 {
		page = 1
	}
	if per < 1 {
		per = 50
	}
	if per > 200 {
		per = 200
	}
	return apiPagination{Page: page, Per: per}
}

// pageWindow computes safe slice bounds. start is computed WITHOUT
// overflow-prone multiplication: (page-1)*per can wrap for huge pages.
func pageWindow(page, per, total int) (int, int, int, int) {
	start := 0
	if page > 1 {
		// Equivalent to (page-1)*per clamped to total, without overflow:
		// any page beyond the last yields an empty window.
		if page-1 > total/per+1 {
			start = total
		} else {
			start = (page - 1) * per
			if start > total {
				start = total
			}
		}
	}
	end := start + per
	if end > total {
		end = total
	}
	return page, per, start, end
}

// flatten converts model structs (incl. sql.Null* fields) into plain
// JSON-friendly values: Null* unwraps to the value or nil, structs and
// slices recurse, field names become snake_case.
func flatten(v any) any {
	rv := reflect.ValueOf(v)
	return flattenValue(rv)
}

func flattenValue(rv reflect.Value) any {
	if !rv.IsValid() {
		return nil
	}
	if rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil
		}
		return flattenValue(rv.Elem())
	}
	// sql.Null* types: unwrap via their Valid flag + value field.
	t := rv.Type()
	if t.PkgPath() == "database/sql" && strings.HasPrefix(t.Name(), "Null") {
		if !rv.FieldByName("Valid").Bool() {
			return nil
		}
		for _, name := range []string{"String", "Int64", "Int32", "Int16", "Byte", "Float64", "Bool", "Time"} {
			if f := rv.FieldByName(name); f.IsValid() {
				return flattenValue(f)
			}
		}
		return nil
	}
	switch rv.Kind() {
	case reflect.Struct:
		out := map[string]any{}
		for i := 0; i < rv.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			out[camelToSnake(f.Name)] = flattenValue(rv.Field(i))
		}
		return out
	case reflect.Slice, reflect.Array:
		out := make([]any, rv.Len())
		for i := 0; i < rv.Len(); i++ {
			out[i] = flattenValue(rv.Index(i))
		}
		return out
	case reflect.Map:
		return nil // not used by model structs
	default:
		return rv.Interface()
	}
}

// camelToSnake converts "ServiceID" → "service_id", "CPUModel" → "cpu_model"
// (acronym-aware: runs of capitals stay together).
func camelToSnake(s string) string {
	var b strings.Builder
	runes := []rune(s)
	for i, c := range runes {
		if c >= 'A' && c <= 'Z' {
			if i > 0 {
				prev := runes[i-1]
				prevLower := prev >= 'a' && prev <= 'z'
				nextLower := i+1 < len(runes) && runes[i+1] >= 'a' && runes[i+1] <= 'z'
				if prevLower || nextLower {
					b.WriteByte('_')
				}
			}
			b.WriteRune(c + 32)
		} else {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// flattenSlice flattens a typed slice (already page-windowed by callers).
func flattenSlice[T any](items []T) []any {
	out := make([]any, len(items))
	for i, it := range items {
		out[i] = flatten(it)
	}
	return out
}

// writeTypedList pages typed rows (sliced BEFORE flattening, so the
// reflection only processes the returned page) and writes the envelope.
// Accepted scale: the whole table is materialized before paging — fine for
// a personal inventory; revisit with LIMIT/OFFSET at low-thousands of rows.
func writeTypedList[T any](w http.ResponseWriter, r *http.Request, items []T) {
	p := parsePagination(r)
	_, _, start, end := pageWindow(p.Page, p.Per, len(items))
	writeJSON(w, http.StatusOK, map[string]any{
		"data":  flattenSlice(items[start:end]),
		"page":  p.Page,
		"per":   p.Per,
		"total": len(items),
	})
}

// ---------- Read endpoints ----------

// handleAPIServiceList returns a paginated list for one service type.
func (s *Server) handleAPIServiceList(serviceType int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		opts := model.ListOptions{Status: "all"}
		switch serviceType {
		case model.ServiceShared:
			v, err := (&model.SharedStore{DB: s.db}).List(r.Context(), opts)
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
				return
			}
			writeTypedList(w, r, v)
		case model.ServiceReseller:
			v, err := (&model.ResellerStore{DB: s.db}).List(r.Context(), opts)
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
				return
			}
			writeTypedList(w, r, v)
		case model.ServiceSeedbox:
			v, err := (&model.SeedboxStore{DB: s.db}).List(r.Context(), opts)
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
				return
			}
			writeTypedList(w, r, v)
		case model.ServiceDomain:
			v, err := (&model.DomainStore{DB: s.db}).List(r.Context(), opts)
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
				return
			}
			writeTypedList(w, r, v)
		case model.ServiceMisc:
			v, err := (&model.MiscStore{DB: s.db}).List(r.Context(), opts)
			if err != nil {
				writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
				return
			}
			writeTypedList(w, r, v)
		}
	}
}

// handleAPIServiceGet returns one service with its pricing.
func (s *Server) handleAPIServiceGet(serviceType int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil {
			writeAPIError(w, http.StatusNotFound, errMsgNotFound)
			return
		}
		var entity any
		var pricing *model.Pricing
		switch serviceType {
		case model.ServiceShared:
			entity, pricing, err = (&model.SharedStore{DB: s.db}).Get(r.Context(), id)
		case model.ServiceReseller:
			entity, pricing, err = (&model.ResellerStore{DB: s.db}).Get(r.Context(), id)
		case model.ServiceSeedbox:
			entity, pricing, err = (&model.SeedboxStore{DB: s.db}).Get(r.Context(), id)
		case model.ServiceDomain:
			entity, pricing, err = (&model.DomainStore{DB: s.db}).Get(r.Context(), id)
		case model.ServiceMisc:
			entity, pricing, err = (&model.MiscStore{DB: s.db}).Get(r.Context(), id)
		}
		if err == sql.ErrNoRows {
			writeAPIError(w, http.StatusNotFound, errMsgNotFound)
			return
		}
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"data":    flatten(entity),
			"pricing": flatten(pricing),
		})
	}
}

// handleAPIPricings returns all pricings with service refs.
func (s *Server) handleAPIPricings(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.QueryContext(r.Context(), `
		SELECT a.id, a.service_id, a.service_type, a.currency, a.price, a.term, a.next_due_date,
			a.active, a.created_at, a.updated_at, 
			`+model.TargetNameSQL+` AS service_name
		FROM pricings a ORDER BY a.service_type, a.service_id`)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	defer rows.Close()
	var pricings []model.Pricing
	var timestamps [][2]string
	var names []string
	for rows.Next() {
		var p model.Pricing
		var active int
		var createdAt, updatedAt, serviceName string
		if err := rows.Scan(&p.ID, &p.ServiceID, &p.ServiceType, &p.Currency,
			&p.Price, &p.Term, &p.NextDueDate, &active, &createdAt, &updatedAt,
			&serviceName); err != nil {
			writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
			return
		}
		p.Active = active != 0
		pricings = append(pricings, p)
		timestamps = append(timestamps, [2]string{createdAt, updatedAt})
		names = append(names, serviceName)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}

	// Names came along in the main query (no per-row N+1). Slice BEFORE
	// flattening so only the returned page goes through reflection.
	p := parsePagination(r)
	page, per, start, end := pageWindow(p.Page, p.Per, len(pricings))
	items := make([]any, 0, end-start)
	for i := start; i < end; i++ {
		m := flatten(pricings[i]).(map[string]any)
		m["service_name"] = names[i]
		m["created_at"] = timestamps[i][0]
		m["updated_at"] = timestamps[i][1]
		items = append(items, m)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"data":  items,
		"page":  page,
		"per":   per,
		"total": len(pricings),
	})
}

// handleAPIIPs returns all IPs with targets.
func (s *Server) handleAPIIPs(w http.ResponseWriter, r *http.Request) {
	items, err := (&model.IPStore{DB: s.db}).ListAll(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	writeTypedList(w, r, items)
}

// handleAPIDNS returns all DNS records.
func (s *Server) handleAPIDNS(w http.ResponseWriter, r *http.Request) {
	items, err := (&model.DNSStore{DB: s.db}).List(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	writeTypedList(w, r, items)
}

// handleAPILabels returns all labels with usage counts.
func (s *Server) handleAPILabels(w http.ResponseWriter, r *http.Request) {
	items, err := (&model.LabelStore{DB: s.db}).AllWithCounts(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	writeTypedList(w, r, items)
}

// handleAPINotes returns all notes.
func (s *Server) handleAPINotes(w http.ResponseWriter, r *http.Request) {
	items, err := (&model.NoteStore{DB: s.db}).ListAll(r.Context())
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
		return
	}
	writeTypedList(w, r, items)
}

// handleAPICatalog returns a catalog list.
func (s *Server) handleAPICatalog(kindStr string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		kind, ok := model.Catalogs[kindStr]
		if !ok {
			writeAPIError(w, http.StatusNotFound, errMsgNotFound)
			return
		}
		items, err := s.catalogs.List(r.Context(), kind)
		if err != nil {
			writeAPIError(w, http.StatusInternalServerError, errMsgInternal)
			return
		}
		writeTypedList(w, r, items)
	}
}
