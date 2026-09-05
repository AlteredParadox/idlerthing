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
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"idlerthing/internal/model"
)

// exportTypeNames maps the {type} path value to a service type, or "" for all.
var exportTypes = map[string]int{
	"servers":   model.ServiceServer,
	"shared":    model.ServiceShared,
	"reseller":  model.ServiceReseller,
	"seedboxes": model.ServiceSeedbox,
	"domains":   model.ServiceDomain,
	"misc":      model.ServiceMisc,
}

// snapshot begins a read-only transaction so the many export queries see
// ONE consistent database snapshot (concurrent writes just wait briefly).
// Model read paths honor the tx via model.WithTx.
func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) (context.Context, *sql.Tx, bool) {
	tx, err := s.db.BeginTx(r.Context(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return nil, nil, false
	}
	return model.WithTx(r.Context(), tx), tx, true
}

// handleExportJSON handles GET /export/json and GET /export/json/{type}.
func (s *Server) handleExportJSON(w http.ResponseWriter, r *http.Request) {
	ctx, tx, ok := s.snapshot(w, r)
	if !ok {
		return
	}
	defer tx.Rollback()
	r = r.WithContext(ctx)
	typeName := r.PathValue("type")
	if typeName != "" {
		if _, ok := exportTypes[typeName]; !ok {
			http.NotFound(w, r)
			return
		}
	}

	out, ok := s.buildExportDoc(r, typeName)
	if !ok {
		http.Error(w, "export failed", http.StatusInternalServerError)
		return
	}

	// Close the snapshot BEFORE writing — a slow client must not pin the
	// single SQLite connection for the whole response.
	if err := tx.Commit(); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}

	filename := "idlerthing-export"
	if typeName != "" {
		filename += "-" + typeName
	}
	filename += "-" + time.Now().Format("20060102") + ".json"
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	writeJSON(w, http.StatusOK, out)
}

// buildExportDoc builds the export document inside the snapshot tx.
// Returns false on any query error (a missing section would look like an
// empty table, so the whole export fails).
func (s *Server) buildExportDoc(r *http.Request, typeName string) (map[string]any, bool) {
	out := map[string]any{
		// Envelope version marker — the importer requires it.
		"format": 1,
		// Per-type exports omit the related tables (pricings/ips/dns/notes/
		// labels_assigned/yabs) — the importer warns on these.
		"partial": typeName != "",
	}
	if typeName == "" || typeName == "servers" {
		servers, err := s.exportServers(r)
		if err != nil {
			return nil, false
		}
		out["servers"] = servers
	}
	for name, st := range exportTypes {
		if st == model.ServiceServer || (typeName != "" && typeName != name) {
			continue
		}
		items, err := s.exportService(r, st)
		if err != nil {
			return nil, false
		}
		out[name] = items
	}
	if typeName == "" && !s.addSharedSections(r, out) {
		return nil, false
	}
	return out, true
}

// addSharedSections adds the reference tables to a full export document.
// Returns false on any query error (a missing section would look like an
// empty table, so the whole export fails).
func (s *Server) addSharedSections(r *http.Request, out map[string]any) bool {
	ok := true
	addKey := func(key string, fn func() (any, error)) {
		if err := s.addJSONKey(r, out, key, fn); err != nil {
			ok = false
		}
	}
	addKey("pricings", func() (any, error) { return s.exportPricings(r) })
	addKey("ips", func() (any, error) {
		v, err := (&model.IPStore{DB: s.db}).ListAll(r.Context())
		return flattenSlice(v), err
	})
	addKey("dns", func() (any, error) {
		v, err := (&model.DNSStore{DB: s.db}).List(r.Context())
		return flattenSlice(v), err
	})
	addKey("labels", func() (any, error) {
		v, err := (&model.LabelStore{DB: s.db}).AllWithCounts(r.Context())
		return flattenSlice(v), err
	})
	addKey("yabs", func() (any, error) {
		return s.exportYABS(r)
	})
	addKey("labels_assigned", func() (any, error) { return s.exportLabelsAssigned(r) })
	addKey("notes", func() (any, error) { return s.exportNotes(r) })
	for _, kindStr := range []string{"providers", "locations", "os"} {
		addKey(kindStr, func() (any, error) {
			v, err := s.catalogs.List(r.Context(), model.Catalogs[kindStr])
			return flattenSlice(v), err
		})
	}
	return ok
}

// exportPricings returns the standalone pricings table with timestamps.
func (s *Server) exportPricings(r *http.Request) (any, error) {
	rows, err := model.QuerierFrom(r.Context(), s.db).QueryContext(r.Context(), `
		SELECT id, service_id, service_type, currency, price, term,
			next_due_date, active, created_at, updated_at
		FROM pricings ORDER BY service_type, service_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []any
	for rows.Next() {
		var p model.Pricing
		var active int
		var ca, ua string
		if err := rows.Scan(&p.ID, &p.ServiceID, &p.ServiceType, &p.Currency,
			&p.Price, &p.Term, &p.NextDueDate, &active, &ca, &ua); err != nil {
			return nil, err
		}
		p.Active = active != 0
		m := flatten(p).(map[string]any)
		m["created_at"] = ca
		m["updated_at"] = ua
		items = append(items, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A typed nil slice would marshal as null, not [].
	if items == nil {
		items = []any{}
	}
	return items, nil
}

// exportLabelsAssigned returns the labels_assigned join table.
func (s *Server) exportLabelsAssigned(r *http.Request) (any, error) {
	rows, err := model.QuerierFrom(r.Context(), s.db).QueryContext(r.Context(),
		"SELECT label_id, service_id, service_type FROM labels_assigned ORDER BY service_type, service_id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []any
	for rows.Next() {
		var labelID, serviceID int64
		var serviceType int
		if err := rows.Scan(&labelID, &serviceID, &serviceType); err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"label_id": labelID, "service_id": serviceID, "service_type": serviceType,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if items == nil {
		items = []any{}
	}
	return items, nil
}

// exportNotes returns service notes plus ip-keyed notes in the import's
// {"note": ...} shape.
func (s *Server) exportNotes(r *http.Request) (any, error) {
	notes := &model.NoteStore{DB: s.db}
	v, err := notes.ListAll(r.Context())
	if err != nil {
		return nil, err
	}
	ipNotes, err := notes.ListIPNotes(r.Context())
	if err != nil {
		return nil, err
	}
	out := flattenSlice(v)
	for _, n := range ipNotes {
		// Match the {"note": {...}} shape flatten gives embedded
		// NoteWithTarget (flatten names IPID "ip_id", the import key).
		out = append(out, map[string]any{"note": flatten(n)})
	}
	return out, nil
}

// addJSONKey runs fn and stores the result; a query error aborts the
// whole export (an empty section would look like an empty table).
func (s *Server) addJSONKey(r *http.Request, out map[string]any, key string, fn func() (any, error)) error {
	v, err := fn()
	if err != nil {
		return err
	}
	if v == nil {
		v = []any{}
	}
	out[key] = v
	return nil
}

// exportServers returns servers with disks/pricing/labels/ips inlined.
// Child tables are fetched ONCE each (constant query count regardless of
// server count) and grouped in Go.
func (s *Server) exportServers(r *http.Request) ([]any, error) {
	items, err := s.servers.List(r.Context(), model.ListOptions{Status: "all"})
	if err != nil {
		return nil, err
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	disks, err := s.disksByServer(r, ids)
	if err != nil {
		return nil, err
	}
	labels, err := s.labelsByService(r, ids, model.ServiceServer)
	if err != nil {
		return nil, err
	}
	ips, err := s.ipsByService(r, ids, model.ServiceServer)
	if err != nil {
		return nil, err
	}

	var out []any
	for _, it := range items {
		m := flatten(it).(map[string]any)
		m["disks"] = flattenSlice(disks[it.ID])
		m["labels"] = flattenSlice(labels[it.ID])
		m["ips"] = flattenSlice(ips[it.ID])
		out = append(out, m)
	}
	if out == nil {
		out = []any{}
	}
	return out, nil
}

// queryIDChunks runs fn once per ≤500-id chunk (SQLite variable limit) with
// an " IN (?,…)" clause and matching args, routed through QuerierFrom so the
// export snapshot tx applies.
func (s *Server) queryIDChunks(r *http.Request, ids []int64, fn func(q model.Querier, clause string, args []any) error) error {
	const maxVars = 500
	q := model.QuerierFrom(r.Context(), s.db)
	for i := 0; i < len(ids); i += maxVars {
		chunk := ids[i:min(i+maxVars, len(ids))]
		args := make([]any, len(chunk))
		for j, id := range chunk {
			args[j] = id
		}
		if err := fn(q, " IN (?"+strings.Repeat(",?", len(chunk)-1)+")", args); err != nil {
			return err
		}
	}
	return nil
}

// disksByServer batches server_disks for an export id set.
func (s *Server) disksByServer(r *http.Request, ids []int64) (map[int64][]model.ServerDisk, error) {
	out := map[int64][]model.ServerDisk{}
	err := s.queryIDChunks(r, ids, func(q model.Querier, clause string, args []any) error {
		rows, err := q.QueryContext(r.Context(),
			"SELECT id, server_id, size_as_mb, media FROM server_disks WHERE server_id "+clause+" ORDER BY server_id, id", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d model.ServerDisk
			if err := rows.Scan(&d.ID, &d.ServerID, &d.SizeAsMB, &d.Media); err != nil {
				return err
			}
			out[d.ServerID] = append(out[d.ServerID], d)
		}
		return rows.Err()
	})
	return out, err
}

// labelsByService batches label assignments for an export id set.
func (s *Server) labelsByService(r *http.Request, ids []int64, serviceType int) (map[int64][]model.CatalogItem, error) {
	out := map[int64][]model.CatalogItem{}
	err := s.queryIDChunks(r, ids, func(q model.Querier, clause string, args []any) error {
		rows, err := q.QueryContext(r.Context(), `
			SELECT a.service_id, l.id, l.label FROM labels l
			JOIN labels_assigned a ON a.label_id = l.id
			WHERE a.service_type = ? AND a.service_id `+clause+`
			ORDER BY a.service_id, l.label COLLATE NOCASE`, append([]any{serviceType}, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var serviceID int64
			var it model.CatalogItem
			if err := rows.Scan(&serviceID, &it.ID, &it.Name); err != nil {
				return err
			}
			out[serviceID] = append(out[serviceID], it)
		}
		return rows.Err()
	})
	return out, err
}

// ipsByService batches ips for an export id set (same columns/order as
// IPStore.ListFor).
func (s *Server) ipsByService(r *http.Request, ids []int64, serviceType int) (map[int64][]model.IP, error) {
	out := map[int64][]model.IP{}
	err := s.queryIDChunks(r, ids, func(q model.Querier, clause string, args []any) error {
		rows, err := q.QueryContext(r.Context(), `
			SELECT id, service_id, service_type, address, is_ipv4,
				country, region, city, org, isp, asn, fetched_at, created_at, updated_at
			FROM ips WHERE service_type = ? AND service_id `+clause+`
			ORDER BY service_id, address`, append([]any{serviceType}, args...)...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ip model.IP
			var v4 int
			if err := rows.Scan(&ip.ID, &ip.ServiceID, &ip.ServiceType, &ip.Address, &v4,
				&ip.Country, &ip.Region, &ip.City, &ip.Org, &ip.Isp, &ip.Asn,
				&ip.FetchedAt, &ip.CreatedAt, &ip.UpdatedAt); err != nil {
				return err
			}
			ip.IsIPv4 = v4 != 0
			out[ip.ServiceID] = append(out[ip.ServiceID], ip)
		}
		return rows.Err()
	})
	return out, err
}

// exportService returns one service type's list (pricing already inlined
// by the model list queries).
func (s *Server) exportService(r *http.Request, serviceType int) ([]any, error) {
	opts := model.ListOptions{Status: "all"}
	var out []any
	var err error
	switch serviceType {
	case model.ServiceShared:
		var v []model.HostingListItem
		v, err = (&model.SharedStore{DB: s.db}).List(r.Context(), opts)
		out = flattenSlice(v)
	case model.ServiceReseller:
		var v []model.HostingListItem
		v, err = (&model.ResellerStore{DB: s.db}).List(r.Context(), opts)
		out = flattenSlice(v)
	case model.ServiceSeedbox:
		var v []model.SeedboxListItem
		v, err = (&model.SeedboxStore{DB: s.db}).List(r.Context(), opts)
		out = flattenSlice(v)
	case model.ServiceDomain:
		var v []model.DomainListItem
		v, err = (&model.DomainStore{DB: s.db}).List(r.Context(), opts)
		out = flattenSlice(v)
	case model.ServiceMisc:
		var v []model.MiscListItem
		v, err = (&model.MiscStore{DB: s.db}).List(r.Context(), opts)
		out = flattenSlice(v)
	}
	if out == nil {
		out = []any{}
	}
	return out, err
}

// ---------- CSV export ----------

// handleExportCSV handles GET /export/csv — a zip with one CSV per type.
// Accepted scale: every table is materialized (and zipped) in memory before
// paging — fine for a personal inventory; revisit with LIMIT/OFFSET at
// low-thousands of rows.
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	ctx, tx, ok := s.snapshot(w, r)
	if !ok {
		return
	}
	defer tx.Rollback()
	r = r.WithContext(ctx)

	files := []struct {
		name    string
		headers []string
		rows    func() ([][]string, error)
	}{
		{"servers.csv",
			[]string{"id", "hostname", "type", "os", "provider", "location", "ram_mb", "cpu", "cpu_model", "bandwidth_mb", "network_type", "active", "owned_since", "currency", "price", "term", "next_due_date"},
			func() ([][]string, error) { return s.csvServers(r) }},
		{"shared.csv",
			[]string{"id", "main_domain", "type", "provider", "location", "disk_mb", "bandwidth_mb", "active", "owned_since", "currency", "price", "term", "next_due_date"},
			func() ([][]string, error) { return s.csvHosting(r, model.ServiceShared) }},
		{"reseller.csv",
			[]string{"id", "main_domain", "type", "provider", "location", "disk_mb", "bandwidth_mb", "active", "owned_since", "currency", "price", "term", "next_due_date"},
			func() ([][]string, error) { return s.csvHosting(r, model.ServiceReseller) }},
		{"seedboxes.csv",
			[]string{"id", "title", "hostname", "type", "provider", "location", "port_speed", "disk_mb", "bandwidth_mb", "active", "owned_since", "currency", "price", "term", "next_due_date"},
			func() ([][]string, error) { return s.csvSeedboxes(r) }},
		{"domains.csv",
			[]string{"id", "domain", "extension", "ns1", "ns2", "ns3", "provider", "active", "owned_since", "currency", "price", "term", "next_due_date"},
			func() ([][]string, error) { return s.csvDomains(r) }},
		{"misc.csv",
			[]string{"id", "name", "active", "owned_since", "currency", "price", "term", "next_due_date"},
			func() ([][]string, error) { return s.csvMisc(r) }},
	}

	// Compute all rows BEFORE writing anything — a query error fails the
	// whole export instead of silently skipping a file.
	type csvFile struct {
		name    string
		headers []string
		rows    [][]string
	}
	computed := make([]csvFile, 0, len(files))
	for _, f := range files {
		rows, err := f.rows()
		if err != nil {
			tx.Rollback()
			http.Error(w, "export failed", http.StatusInternalServerError)
			return
		}
		computed = append(computed, csvFile{name: f.name, headers: f.headers, rows: rows})
	}

	// Close the snapshot BEFORE compressing — zip building must not pin the
	// single SQLite connection.
	if err := tx.Commit(); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range computed {
		fw, err := zw.Create(f.name)
		if err != nil {
			http.Error(w, errMsgServerErr, http.StatusInternalServerError)
			return
		}
		cw := csv.NewWriter(fw)
		cw.Write(f.headers)
		for _, row := range f.rows {
			writeCSVRow(cw, row)
		}
		cw.Flush()
	}
	if err := zw.Close(); err != nil {
		http.Error(w, errMsgServerErr, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		"attachment; filename=idlerthing-export-"+time.Now().Format("20060102")+".zip")
	w.Write(buf.Bytes())
}

// csvCell guards against spreadsheet formula injection: values whose
// FIRST SIGNIFICANT char is = + - or @ are prefixed with a single quote
// (leading tabs/spaces/control bytes would otherwise bypass the check).
func csvCell(v string) string {
	t := strings.TrimLeft(v, " \t\r\n\x00\x0b\x0c")
	if t == "" {
		return v
	}
	// ASCII and Unicode full-width formula starters (OWASP).
	switch t[0] {
	case '=', '+', '-', '@':
		return "'" + v
	}
	switch r, _ := utf8.DecodeRuneInString(t); r {
	case '\uFF1D', '\uFF0B', '\uFF0D', '\uFF20':
		return "'" + v
	}
	return v
}

// writeCSVRow writes one row, sanitizing every cell.
func writeCSVRow(cw *csv.Writer, row []string) {
	for i, v := range row {
		row[i] = csvCell(v)
	}
	cw.Write(row)
}

// pricingCSV renders pricing cells shared by all CSV row builders.
func pricingCSV(p *model.Pricing) []string {
	if p == nil {
		return []string{"", "", "", ""}
	}
	due := ""
	if p.NextDueDate.Valid {
		due = p.NextDueDate.String
	}
	return []string{p.Currency, strconv.FormatFloat(p.Price, 'f', 2, 64),
		model.TermLabel(p.Term), due}
}

func boolCSV(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// niCSV renders a nullable integer as a CSV cell.
func niCSV(n sql.NullInt64) string {
	if !n.Valid {
		return ""
	}
	return strconv.FormatInt(n.Int64, 10)
}

func (s *Server) csvServers(r *http.Request) ([][]string, error) {
	items, err := s.servers.List(r.Context(), model.ListOptions{Status: "all"})
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, it := range items {
		row := []string{
			strconv.FormatInt(it.ID, 10), it.Hostname, model.ServerTypeLabel(it.ServerType),
			it.OSName, it.ProviderName, it.LocationName,
			niCSV(it.RamAsMB), niCSV(it.CPU), it.CPUModel.String,
			niCSV(it.BandwidthAsMB), it.NetworkType.String,
			boolCSV(it.Active), it.OwnedSince.String,
		}
		out = append(out, append(row, pricingCSV(it.Pricing)...))
	}
	return out, nil
}

func (s *Server) csvHosting(r *http.Request, serviceType int) ([][]string, error) {
	var items []model.HostingListItem
	var err error
	if serviceType == model.ServiceShared {
		items, err = (&model.SharedStore{DB: s.db}).List(r.Context(), model.ListOptions{Status: "all"})
	} else {
		items, err = (&model.ResellerStore{DB: s.db}).List(r.Context(), model.ListOptions{Status: "all"})
	}
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, it := range items {
		row := []string{
			strconv.FormatInt(it.ID, 10), it.MainDomain, it.SharedType.String,
			it.ProviderName, it.LocationName, niCSV(it.DiskAsMB), niCSV(it.BandwidthAsMB),
			boolCSV(it.Active), it.OwnedSince.String,
		}
		out = append(out, append(row, pricingCSV(it.Pricing)...))
	}
	return out, nil
}

func (s *Server) csvSeedboxes(r *http.Request) ([][]string, error) {
	items, err := (&model.SeedboxStore{DB: s.db}).List(r.Context(), model.ListOptions{Status: "all"})
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, it := range items {
		row := []string{
			strconv.FormatInt(it.ID, 10), it.Title.String, it.Hostname,
			it.SeedBoxType.String, it.ProviderName, it.LocationName,
			niCSV(it.PortSpeed), niCSV(it.DiskAsMB), niCSV(it.BandwidthAsMB),
			boolCSV(it.Active), it.OwnedSince.String,
		}
		out = append(out, append(row, pricingCSV(it.Pricing)...))
	}
	return out, nil
}

func (s *Server) csvDomains(r *http.Request) ([][]string, error) {
	items, err := (&model.DomainStore{DB: s.db}).List(r.Context(), model.ListOptions{Status: "all"})
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, it := range items {
		row := []string{
			strconv.FormatInt(it.ID, 10), it.Domain.Domain, it.Extension.String,
			it.Ns1.String, it.Ns2.String, it.Ns3.String, it.ProviderName,
			boolCSV(it.Active), it.OwnedSince.String,
		}
		out = append(out, append(row, pricingCSV(it.Pricing)...))
	}
	return out, nil
}

func (s *Server) csvMisc(r *http.Request) ([][]string, error) {
	items, err := (&model.MiscStore{DB: s.db}).List(r.Context(), model.ListOptions{Status: "all"})
	if err != nil {
		return nil, err
	}
	var out [][]string
	for _, it := range items {
		row := []string{
			strconv.FormatInt(it.ID, 10), it.Name,
			boolCSV(it.Active), it.OwnedSince.String,
		}
		out = append(out, append(row, pricingCSV(it.Pricing)...))
	}
	return out, nil
}

// exportYABS returns all yabs runs with nested speed rows. Speed tables are
// fetched ONCE each and grouped by run (constant query count). Runs go out
// OLDEST-first (ListAll is newest-first for the views) so a restore lands
// them in id order and "latest run" displays correctly.
func (s *Server) exportYABS(r *http.Request) (any, error) {
	st := &model.YABSStore{DB: s.db}
	items, err := st.ListAll(r.Context())
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	ids := make([]int64, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	disks, err := s.yabsDisksByRun(r, ids)
	if err != nil {
		return nil, err
	}
	network, err := s.yabsNetworkByRun(r, ids)
	if err != nil {
		return nil, err
	}
	var out []any
	for _, it := range items {
		y := it.YABS
		m := flatten(&y).(map[string]any)
		m["disk_speed"] = flattenSlice(disks[it.ID])
		m["network_speed"] = flattenSlice(network[it.ID])
		out = append(out, m)
	}
	if out == nil {
		out = []any{}
	}
	return out, nil
}

// yabsDisksByRun batches yabs_disk_speed for an export id set.
func (s *Server) yabsDisksByRun(r *http.Request, ids []int64) (map[int64][]model.YABSDiskSpeed, error) {
	out := map[int64][]model.YABSDiskSpeed{}
	err := s.queryIDChunks(r, ids, func(q model.Querier, clause string, args []any) error {
		rows, err := q.QueryContext(r.Context(),
			"SELECT id, yabs_id, block_size, read_mbps, write_mbps FROM yabs_disk_speed WHERE yabs_id "+clause+" ORDER BY yabs_id, id", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d model.YABSDiskSpeed
			if err := rows.Scan(&d.ID, &d.YabsID, &d.BlockSize, &d.ReadMbps, &d.WriteMbps); err != nil {
				return err
			}
			out[d.YabsID] = append(out[d.YabsID], d)
		}
		return rows.Err()
	})
	return out, err
}

// yabsNetworkByRun batches yabs_network_speed for an export id set.
func (s *Server) yabsNetworkByRun(r *http.Request, ids []int64) (map[int64][]model.YABSNetworkSpeed, error) {
	out := map[int64][]model.YABSNetworkSpeed{}
	err := s.queryIDChunks(r, ids, func(q model.Querier, clause string, args []any) error {
		rows, err := q.QueryContext(r.Context(),
			"SELECT id, yabs_id, location, provider, COALESCE(mode, ''), send_mbps, recv_mbps, latency_ms FROM yabs_network_speed WHERE yabs_id "+clause+" ORDER BY yabs_id, id", args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n model.YABSNetworkSpeed
			if err := rows.Scan(&n.ID, &n.YabsID, &n.Location, &n.Provider, &n.Mode, &n.SendMbps, &n.RecvMbps, &n.LatencyMs); err != nil {
				return err
			}
			out[n.YabsID] = append(out[n.YabsID], n)
		}
		return rows.Err()
	})
	return out, err
}
