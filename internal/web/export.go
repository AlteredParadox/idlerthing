package web

import (
	"archive/zip"
	"database/sql"
	"encoding/csv"
	"net/http"
	"strconv"
	"time"

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

// handleExportJSON handles GET /export/json and GET /export/json/{type}.
func (s *Server) handleExportJSON(w http.ResponseWriter, r *http.Request) {
	typeName := r.PathValue("type")
	if typeName != "" {
		if _, ok := exportTypes[typeName]; !ok {
			http.NotFound(w, r)
			return
		}
	}

	out := map[string]any{}
	if typeName == "" || typeName == "servers" {
		servers, err := s.exportServers(r)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		out["servers"] = servers
	}
	for name, st := range exportTypes {
		if st == model.ServiceServer || (typeName != "" && typeName != name) {
			continue
		}
		items, err := s.exportService(r, st)
		if err != nil {
			http.Error(w, "internal server error", http.StatusInternalServerError)
			return
		}
		out[name] = items
	}

	if typeName == "" {
		// Full export adds the shared/reference tables.
		exportErr := false
		addKey := func(key string, fn func() (any, error)) {
			if err := s.addJSONKey(r, out, key, fn); err != nil {
				exportErr = true
			}
		}
		addKey("pricings", func() (any, error) {
			rows, err := s.db.QueryContext(r.Context(), `
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
			return items, nil
		})
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
		addKey("notes", func() (any, error) {
			v, err := (&model.NoteStore{DB: s.db}).ListAll(r.Context())
			return flattenSlice(v), err
		})
		for _, kindStr := range []string{"providers", "locations", "os"} {
			addKey(kindStr, func() (any, error) {
				v, err := s.catalogs.List(r.Context(), model.Catalogs[kindStr])
				return flattenSlice(v), err
			})
		}
		if exportErr {
			http.Error(w, "export failed", http.StatusInternalServerError)
			return
		}
	}

	filename := "idlerthing-export"
	if typeName != "" {
		filename += "-" + typeName
	}
	filename += "-" + time.Now().Format("20060102") + ".json"
	w.Header().Set("Content-Disposition", "attachment; filename="+filename)
	writeJSON(w, http.StatusOK, out)
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
func (s *Server) exportServers(r *http.Request) ([]any, error) {
	items, err := s.servers.List(r.Context(), model.ListOptions{Status: "all"})
	if err != nil {
		return nil, err
	}
	var out []any
	for _, it := range items {
		m := flatten(it).(map[string]any)
		disks, err := s.servers.Disks(r.Context(), it.ID)
		if err != nil {
			return nil, err
		}
		m["disks"] = flattenSlice(disks)
		labels, _ := (&model.LabelStore{DB: s.db}).ListFor(r.Context(), it.ID, model.ServiceServer)
		m["labels"] = flattenSlice(labels)
		ips, _ := (&model.IPStore{DB: s.db}).ListFor(r.Context(), it.ID, model.ServiceServer)
		m["ips"] = flattenSlice(ips)
		out = append(out, m)
	}
	if out == nil {
		out = []any{}
	}
	return out, nil
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
func (s *Server) handleExportCSV(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition",
		"attachment; filename=idlerthing-export-"+time.Now().Format("20060102")+".zip")

	zw := zip.NewWriter(w)
	defer zw.Close()

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
			http.Error(w, "export failed", http.StatusInternalServerError)
			return
		}
		computed = append(computed, csvFile{name: f.name, headers: f.headers, rows: rows})
	}

	for _, f := range computed {
		fw, err := zw.Create(f.name)
		if err != nil {
			return
		}
		cw := csv.NewWriter(fw)
		cw.Write(f.headers)
		for _, row := range f.rows {
			writeCSVRow(cw, row)
		}
		cw.Flush()
	}
}

// csvCell guards against spreadsheet formula injection: values starting
// with = + - or @ are prefixed with a single quote.
func csvCell(v string) string {
	if v == "" {
		return v
	}
	switch v[0] {
	case '=', '+', '-', '@':
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
