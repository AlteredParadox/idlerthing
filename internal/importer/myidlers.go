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

package importer

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"idlerthing/internal/model"
)

// Format identifies an import file's shape.
type Format int

const (
	FormatUnknown Format = iota
	FormatNative         // our own export (top-level object)
	FormatMyJSON         // my-idlers JSON array
	FormatMyCSV          // my-idlers CSV
)

// DetectFormat sniffs the file: "[" → my-idlers JSON array, "{" → our
// export, otherwise a CSV whose header must contain hostname+server_type_name.
func DetectFormat(r io.Reader) (Format, io.Reader, error) {
	br := bufio.NewReader(r)
	for {
		b, err := br.Peek(1)
		if err != nil {
			return FormatUnknown, br, fmt.Errorf("empty import file")
		}
		if b[0] == ' ' || b[0] == '\t' || b[0] == '\n' || b[0] == '\r' {
			br.ReadByte()
			continue
		}
		switch b[0] {
		case '[':
			return FormatMyJSON, br, nil
		case '{':
			return FormatNative, br, nil
		}
		break
	}
	header, err := br.ReadString('\n')
	if err != nil {
		return FormatUnknown, br, fmt.Errorf("read header: %w", err)
	}
	if strings.Contains(header, "hostname") && strings.Contains(header, "server_type_name") {
		// Hand the header line back so the CSV parser sees the full file.
		return FormatMyCSV, io.MultiReader(strings.NewReader(header), br), nil
	}
	return FormatUnknown, br, fmt.Errorf("unrecognized import format (expected idlerthing export JSON, my-idlers JSON array, or my-idlers CSV)")
}

// ---------- intermediate record ----------

// MyServer is the normalized my-idlers record (from JSON or CSV).
type MyServer struct {
	Hostname      string
	Ns1, Ns2      string
	ServerType    int
	CPU           *int64
	CPUModel      string
	RamAsMB       *int64
	Disks         []myDisk
	BandwidthAsMB *int64 // converted from GB; nil = unlimited
	LinkSpeed     *int64
	NetworkType   string
	SSHPort       *int64
	WasPromo      bool
	Transferrable bool
	Active        bool
	ShowPublic    bool
	OwnedSince    string
	OSName        string
	LocationName  string
	ProviderName  string
	IPs           []myIP
	Labels        []string
	Pricing       *myPricing
}

type myDisk struct {
	SizeMB int64
	Media  string
}

type myIP struct {
	Address string
	IsIPv4  bool
}

type myPricing struct {
	Currency    string
	Price       float64
	Term        int
	NextDueDate string
}

// ---------- JSON decoding ----------

type miJSONDisk struct {
	Size  float64 `json:"disk_size"`
	Unit  string  `json:"disk_unit"`
	Media string  `json:"disk_media"`
}

type miJSONIP struct {
	Address string `json:"address"`
	IsIPv4  int    `json:"is_ipv4"`
}

type miJSONNamed struct {
	Name string `json:"name"`
}

type miJSONPricing struct {
	Price       float64 `json:"price"`
	Currency    string  `json:"currency"`
	Term        int     `json:"term"`
	NextDueDate string  `json:"next_due_date"`
}

type miJSON struct {
	Hostname      string          `json:"hostname"`
	Ns1           *string         `json:"ns1"`
	Ns2           *string         `json:"ns2"`
	ServerType    *int            `json:"server_type"`
	CPU           *int64          `json:"cpu"`
	CPUModel      *string         `json:"cpu_model"`
	RamAsMB       *int64          `json:"ram_as_mb"`
	DiskAsGB      *float64        `json:"disk_as_gb"`
	Disks         []miJSONDisk    `json:"disks"`
	Bandwidth     *float64        `json:"bandwidth"`
	LinkSpeed     *int64          `json:"link_speed"`
	NetworkType   *string         `json:"network_type"`
	SSH           *int64          `json:"ssh"`
	WasPromo      *int            `json:"was_promo"`
	Transferrable *int            `json:"transferrable"`
	Active        *int            `json:"active"`
	ShowPublic    *int            `json:"show_public"`
	OwnedSince    *string         `json:"owned_since"`
	OS            *miJSONNamed    `json:"os"`
	Location      *miJSONNamed    `json:"location"`
	Provider      *miJSONNamed    `json:"provider"`
	IPs           []miJSONIP      `json:"ips"`
	Labels        json.RawMessage `json:"labels"`
	Pricing       *miJSONPricing  `json:"pricing"`
}

// ParseMyJSON decodes the my-idlers JSON array into normalized records.
func ParseMyJSON(r io.Reader) ([]MyServer, []string, error) {
	var rows []miJSON
	if err := json.NewDecoder(r).Decode(&rows); err != nil {
		return nil, nil, fmt.Errorf("decode my-idlers JSON: %w", err)
	}
	var out []MyServer
	var warnings []string
	for _, j := range rows {
		out = append(out, myServerFromJSON(j, &warnings))
	}
	return out, warnings, nil
}

// myServerFromJSON normalizes one my-idlers JSON record.
func myServerFromJSON(j miJSON, warnings *[]string) MyServer {
	s := MyServer{
		Hostname:      strings.TrimSpace(j.Hostname),
		ServerType:    1,
		NetworkType:   mapNetworkType(j.NetworkType),
		WasPromo:      intBool(j.WasPromo),
		Transferrable: intBool(j.Transferrable),
		Active:        j.Active == nil || *j.Active != 0, // default active
		ShowPublic:    intBool(j.ShowPublic),
	}
	s.CPU = capMy(j.CPU, 1024, "cpu", s.Hostname, warnings)
	s.RamAsMB = capMy(j.RamAsMB, 1<<30, "ram_as_mb", s.Hostname, warnings)
	s.LinkSpeed = capMy(j.LinkSpeed, 1<<20, "link_speed", s.Hostname, warnings)
	s.SSHPort = capMy(j.SSH, 65535, "ssh_port", s.Hostname, warnings)
	myJSONNames(&s, j, warnings)

	if bw, bad := convertBandwidth(j.Bandwidth); bad {
		*warnings = append(*warnings, fmt.Sprintf("%s: implausible bandwidth — storing NULL", s.Hostname))
	} else {
		s.BandwidthAsMB = bw
	}

	s.Disks = myJSONDisks(j, s.Hostname, warnings)
	s.IPs = myJSONIPs(j.IPs, s.Hostname, warnings)
	s.Labels = parseLabels(j.Labels)
	s.Pricing = myJSONPricing(j.Pricing, s.Hostname, warnings)
	return s
}

// myJSONNames maps the optional string/named fields of one record.
func myJSONNames(s *MyServer, j miJSON, warnings *[]string) {
	if j.Ns1 != nil {
		s.Ns1 = *j.Ns1
	}
	if j.Ns2 != nil {
		s.Ns2 = *j.Ns2
	}
	if j.CPUModel != nil {
		s.CPUModel = *j.CPUModel
	}
	if j.OwnedSince != nil {
		if d, ok := normDate(*j.OwnedSince); ok {
			s.OwnedSince = d
		} else {
			*warnings = append(*warnings, fmt.Sprintf("%s: invalid owned_since %q — storing NULL", s.Hostname, *j.OwnedSince))
		}
	}
	if j.ServerType != nil && *j.ServerType >= 1 && *j.ServerType <= 7 {
		s.ServerType = *j.ServerType
	}
	if j.OS != nil {
		s.OSName = j.OS.Name
	}
	if j.Location != nil {
		s.LocationName = j.Location.Name
	}
	if j.Provider != nil {
		s.ProviderName = j.Provider.Name
	}
}

// myJSONDisks converts the disk list, with the legacy single-disk fallback.
func myJSONDisks(j miJSON, host string, warnings *[]string) []myDisk {
	var disks []myDisk
	for _, d := range j.Disks {
		if mb := diskToMB(d.Size, d.Unit); mb > 0 {
			disks = append(disks, myDisk{SizeMB: mb, Media: diskMedia(d.Media)})
		} else if d.Size > 0 {
			*warnings = append(*warnings, fmt.Sprintf("%s: implausible disk size — skipped", host))
		}
	}
	if len(disks) == 0 && j.DiskAsGB != nil && *j.DiskAsGB > 0 {
		// Legacy single-disk fallback.
		if mb := diskToMB(*j.DiskAsGB, "GB"); mb > 0 {
			disks = append(disks, myDisk{SizeMB: mb, Media: "SSD"})
		} else {
			*warnings = append(*warnings, fmt.Sprintf("%s: implausible disk size — skipped", host))
		}
	}
	return disks
}

// myJSONIPs converts + dedupes the IP list (is_ipv4 derived from the
// address itself — the file's flag is ignored when it contradicts).
func myJSONIPs(ips []miJSONIP, host string, warnings *[]string) []myIP {
	var out []myIP
	seen := map[string]bool{}
	for _, ip := range ips {
		if ip.Address == "" {
			continue
		}
		addr, err := netip.ParseAddr(ip.Address)
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("%s: invalid ip %q — skipped", host, ip.Address))
			continue
		}
		// A duplicated address would fail the whole row on
		// UNIQUE(service_id, service_type, address) — keep it once.
		if seen[ip.Address] {
			*warnings = append(*warnings, fmt.Sprintf("%s: duplicate IP %s listed twice — kept once", host, ip.Address))
			continue
		}
		seen[ip.Address] = true
		out = append(out, myIP{Address: ip.Address, IsIPv4: addr.Is4()})
	}
	return out
}

// myJSONPricing converts the optional pricing block (nil when skipped).
func myJSONPricing(jp *miJSONPricing, host string, warnings *[]string) *myPricing {
	if jp == nil || !validImportPrice(jp.Price) {
		return nil
	}
	cur, ok := normCurrency(jp.Currency)
	if !ok {
		*warnings = append(*warnings, fmt.Sprintf("%s: invalid currency %q — pricing skipped", host, jp.Currency))
		return nil
	}
	if jp.Term < 1 || jp.Term > 7 {
		*warnings = append(*warnings, fmt.Sprintf("%s: term %d out of range — pricing skipped", host, jp.Term))
		return nil
	}
	p := &myPricing{
		Currency:    cur,
		Price:       jp.Price,
		Term:        jp.Term,
		NextDueDate: jp.NextDueDate,
	}
	if d, ok := normDate(p.NextDueDate); ok {
		p.NextDueDate = d
	} else {
		*warnings = append(*warnings, fmt.Sprintf("%s: invalid next_due_date %q — storing NULL", host, p.NextDueDate))
		p.NextDueDate = ""
	}
	return p
}

// parseLabels handles labels as ["a","b"] or [{"label":"a"},...] (and null).
func parseLabels(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// Empty/whitespace names are dropped — they would create an
	// empty-string catalog row.
	clean := func(in []string) []string {
		var out []string
		for _, l := range in {
			if l = strings.TrimSpace(l); l != "" {
				out = append(out, l)
			}
		}
		return out
	}
	var names []string
	if err := json.Unmarshal(raw, &names); err == nil {
		return clean(names)
	}
	var objs []struct {
		Label string `json:"label"`
		Name  string `json:"name"`
	}
	if err := json.Unmarshal(raw, &objs); err == nil {
		var out []string
		for _, o := range objs {
			if o.Label != "" {
				out = append(out, o.Label)
			} else if o.Name != "" {
				out = append(out, o.Name)
			}
		}
		return clean(out)
	}
	return nil
}

// ---------- CSV decoding ----------

// ParseMyCSV decodes the my-idlers CSV (nested JSON cells) into records.
func ParseMyCSV(r io.Reader) ([]MyServer, []string, error) {
	rd := csv.NewReader(r)
	rd.FieldsPerRecord = -1
	rows, err := rd.ReadAll()
	if err != nil {
		return nil, nil, fmt.Errorf("decode my-idlers CSV: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil, fmt.Errorf("empty CSV")
	}
	c := &csvCells{header: map[string]int{}}
	for i, col := range rows[0] {
		c.header[col] = i
	}

	var out []MyServer
	var warnings []string
	for i, row := range rows[1:] {
		out = append(out, parseCSVRow(c, row, i+2, &warnings)) // header is line 1
	}
	return out, warnings, nil
}

// csvCells resolves column names against the CSV header.
type csvCells struct {
	header map[string]int
}

func (c *csvCells) cell(row []string, name string) string {
	if i, ok := c.header[name]; ok && i < len(row) {
		return strings.TrimSpace(row[i])
	}
	return ""
}

func csvInt(v string) *int64 {
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func csvFloat(v string) *float64 {
	if v == "" {
		return nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return nil
	}
	return &f
}

func csvBool(v string) bool { return v == "1" }

// parseCSVRow normalizes one CSV data row (line is the 1-based file line).
func parseCSVRow(c *csvCells, row []string, line int, warnings *[]string) MyServer {
	owned, ownedOK := normDate(c.cell(row, "owned_since"))
	if !ownedOK {
		*warnings = append(*warnings, fmt.Sprintf("row %d: invalid owned_since %q — storing NULL", line, c.cell(row, "owned_since")))
	}
	s := MyServer{
		Hostname:      c.cell(row, "hostname"),
		Ns1:           c.cell(row, "ns1"),
		Ns2:           c.cell(row, "ns2"),
		ServerType:    1,
		CPUModel:      c.cell(row, "cpu_model"),
		NetworkType:   mapNetworkTypeStr(c.cell(row, "network_type")),
		WasPromo:      csvBool(c.cell(row, "was_promo")),
		Transferrable: csvBool(c.cell(row, "transferrable")),
		Active:        c.cell(row, "active") != "0",
		ShowPublic:    csvBool(c.cell(row, "show_public")),
		OwnedSince:    owned,
		OSName:        c.cell(row, "os_name"),
		LocationName:  c.cell(row, "location_name"),
		ProviderName:  c.cell(row, "provider_name"),
	}
	if st := csvInt(c.cell(row, "server_type")); st != nil && *st >= 1 && *st <= 7 {
		s.ServerType = int(*st)
	}
	rowLabel := fmt.Sprintf("row %d", line)
	s.CPU = capMy(csvInt(c.cell(row, "cpu")), 1024, "cpu", rowLabel, warnings)
	s.RamAsMB = capMy(csvInt(c.cell(row, "ram_as_mb")), 1<<30, "ram_as_mb", rowLabel, warnings)
	s.LinkSpeed = capMy(csvInt(c.cell(row, "link_speed")), 1<<20, "link_speed", rowLabel, warnings)
	s.SSHPort = capMy(csvInt(c.cell(row, "ssh")), 65535, "ssh_port", rowLabel, warnings)
	if bw, bad := convertBandwidth(csvFloat(c.cell(row, "bandwidth"))); bad {
		*warnings = append(*warnings, fmt.Sprintf("row %d: implausible bandwidth — storing NULL", line))
	} else {
		s.BandwidthAsMB = bw
	}

	s.Disks = csvDisks(c, row, line, s.Hostname, warnings)
	s.IPs = csvIPs(c, row, line, s.Hostname, warnings)
	s.Labels = parseLabels(json.RawMessage(c.cell(row, "labels")))
	s.Pricing = csvPricing(c, row, line, warnings)
	return s
}

// csvDisks converts the nested disks JSON cell, with the legacy
// disk_as_gb fallback.
func csvDisks(c *csvCells, row []string, line int, host string, warnings *[]string) []myDisk {
	var out []myDisk
	var disks []miJSONDisk
	if raw := c.cell(row, "disks"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &disks); err != nil {
			*warnings = append(*warnings, fmt.Sprintf("row %d (%s): bad disks JSON", line, host))
		}
	}
	for _, d := range disks {
		if mb := diskToMB(d.Size, d.Unit); mb > 0 {
			out = append(out, myDisk{SizeMB: mb, Media: diskMedia(d.Media)})
		} else if d.Size > 0 {
			*warnings = append(*warnings, fmt.Sprintf("row %d: implausible disk size — skipped", line))
		}
	}
	if len(out) == 0 {
		if gb := csvFloat(c.cell(row, "disk_as_gb")); gb != nil && *gb > 0 {
			if mb := diskToMB(*gb, "GB"); mb > 0 {
				out = append(out, myDisk{SizeMB: mb, Media: "SSD"})
			} else {
				*warnings = append(*warnings, fmt.Sprintf("row %d: implausible disk size — skipped", line))
			}
		}
	}
	return out
}

// csvIPs converts + dedupes the nested ips JSON cell (is_ipv4 derived from
// the address, not the file's flag).
func csvIPs(c *csvCells, row []string, line int, host string, warnings *[]string) []myIP {
	var out []myIP
	var ips []miJSONIP
	if raw := c.cell(row, "ips"); raw != "" {
		if err := json.Unmarshal([]byte(raw), &ips); err != nil {
			*warnings = append(*warnings, fmt.Sprintf("row %d (%s): bad ips JSON", line, host))
		}
	}
	seen := map[string]bool{}
	for _, ip := range ips {
		if ip.Address == "" {
			continue
		}
		addr, err := netip.ParseAddr(ip.Address)
		if err != nil {
			*warnings = append(*warnings, fmt.Sprintf("row %d: invalid ip %q — skipped", line, ip.Address))
			continue
		}
		if seen[ip.Address] {
			*warnings = append(*warnings, fmt.Sprintf("row %d: duplicate IP %s listed twice — kept once", line, ip.Address))
			continue
		}
		seen[ip.Address] = true
		out = append(out, myIP{Address: ip.Address, IsIPv4: addr.Is4()})
	}
	return out
}

// csvPricing converts the pricing_* columns (nil when skipped).
func csvPricing(c *csvCells, row []string, line int, warnings *[]string) *myPricing {
	price := csvFloat(c.cell(row, "pricing_price"))
	if price == nil || !validImportPrice(*price) {
		return nil
	}
	term := 1
	if t := csvInt(c.cell(row, "pricing_term")); t != nil {
		term = int(*t)
	}
	due, dueOK := normDate(c.cell(row, "pricing_next_due_date"))
	if !dueOK {
		*warnings = append(*warnings, fmt.Sprintf("row %d: invalid next_due_date %q — storing NULL", line, c.cell(row, "pricing_next_due_date")))
	}
	if term < 1 || term > 7 {
		*warnings = append(*warnings, fmt.Sprintf("row %d: term %d out of range — pricing skipped", line, term))
		return nil
	}
	cur, ok := normCurrency(c.cell(row, "pricing_currency"))
	if !ok {
		*warnings = append(*warnings, fmt.Sprintf("row %d: invalid currency %q — pricing skipped", line, c.cell(row, "pricing_currency")))
		return nil
	}
	return &myPricing{
		Currency:    cur,
		Price:       *price,
		Term:        term,
		NextDueDate: due,
	}
}

// ---------- shared mapping helpers ----------

// normDate normalizes a date to yyyy-mm-dd (strict; RFC3339 date prefix
// "2006-01-02T..." accepted by trimming at 'T'). ok=false means the input
// was non-empty but unparseable → caller warns and stores NULL.
func normDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	if i := strings.IndexByte(s, 'T'); i > 0 {
		s = s[:i]
	}
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return "", false
	}
	return t.Format(time.DateOnly), true
}

func intBool(v *int) bool { return v != nil && *v != 0 }

// maxImportMB is the plausibility ceiling for float→MB conversions — the
// same 1<<30 cap the web form + JSON API use (1 PB in MB); beyond it the
// value is corrupt, not big.
const maxImportMB = int64(1) << 30

// safeInt converts a JSON/CSV float to an int64 with bounds: non-finite,
// negative, or over-max values are rejected (ok=false) instead of hitting
// implementation-defined float→int conversion. Valid values are rounded.
func safeInt(f float64, max int64) (int64, bool) {
	if math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > float64(max) {
		return 0, false
	}
	return int64(f + 0.5), true
}

// capMy bounds an optional integer to the same plausibility caps the web
// form + JSON API use; out-of-range → nil + warning (label is the hostname
// for JSON rows, "row N" for CSV rows).
func capMy(v *int64, max int64, field, label string, warnings *[]string) *int64 {
	if v == nil {
		return nil
	}
	if *v < 0 || *v > max {
		*warnings = append(*warnings, fmt.Sprintf("%s: %s %d out of range — storing NULL", label, field, *v))
		return nil
	}
	return v
}

// convertBandwidth: my-idlers stores GB → MB (1024-based); 0/nil → NULL
// (unlimited). bad=true means the input was present but implausible
// (NaN/Inf/huge) — caller warns and stores NULL.
func convertBandwidth(gb *float64) (mb *int64, bad bool) {
	if gb == nil || *gb == 0 {
		return nil, false // absent / unlimited — not an error
	}
	if *gb < 0 {
		return nil, true // negative is corrupt: warn + NULL
	}
	v, ok := safeInt(*gb*1024, maxImportMB)
	if !ok || v == 0 {
		return nil, true
	}
	return &v, false
}

// diskToMB converts a disk size+unit to whole MB (1024-based); implausible
// values collapse to 0 (callers skip non-positive sizes).
func diskToMB(size float64, unit string) int64 {
	factor := float64(1024) // GB and anything else
	switch strings.ToUpper(unit) {
	case "TB":
		factor = 1024 * 1024
	case "MB":
		factor = 1
	}
	mb, ok := safeInt(size*factor, maxImportMB)
	if !ok {
		return 0
	}
	return mb
}

func diskMedia(media string) string {
	switch strings.ToUpper(media) {
	case "HDD":
		return "HDD"
	case "NVMe", "NVME":
		return "NVMe"
	default:
		return "SSD"
	}
}

// mapNetworkType maps fork variants onto our set; unknown free text passes through.
func mapNetworkType(v *string) string {
	if v == nil {
		return ""
	}
	return mapNetworkTypeStr(*v)
}

func mapNetworkTypeStr(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	switch v {
	case "NAT+IPv4", "IPv4 (shared)", "NAT":
		return "IPv4 NAT"
	}
	return v // known set members and unknown free text pass through
}

// ---------- the import itself ----------

// MySummary reports a my-idlers import.
type MySummary struct {
	Imported   int
	SkippedDup int
	Warnings   []string
	Providers  int
	Locations  int
	OS         int
	Labels     int
	Disks      int
	IPs        int
	Pricings   int
}

// ImportMyIdlers inserts normalized my-idlers records in ONE transaction.
// Duplicate hostnames (existing or earlier in the file) are skipped;
// per-row failures are warnings, never aborts.
func ImportMyIdlers(ctx context.Context, db *sql.DB, records []MyServer, warnings []string) (*MySummary, error) {
	sum := &MySummary{Warnings: warnings}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	seen := map[string]bool{}
	for _, rec := range records {
		if rec.Hostname == "" {
			sum.Warnings = append(sum.Warnings, "row with empty hostname skipped")
			continue
		}
		key := strings.ToLower(rec.Hostname)
		if seen[key] {
			sum.SkippedDup++
			continue
		}
		var existing int
		if err := tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM servers WHERE hostname = ? COLLATE NOCASE",
			rec.Hostname).Scan(&existing); err != nil {
			tx.Rollback()
			return nil, err
		}
		if existing > 0 {
			seen[key] = true
			sum.SkippedDup++
			continue
		}
		// Per-row savepoint: a failing row rolls back cleanly without
		// poisoning the transaction for the rest. Counters accumulate into a
		// row-local delta merged only on success, and the seen mark lands
		// AFTER the row actually commits (a late failure must not suppress a
		// later valid row with the same hostname).
		if _, err := tx.ExecContext(ctx, "SAVEPOINT row"); err != nil {
			tx.Rollback()
			return nil, err
		}
		var delta MySummary
		if err := importMyServer(ctx, tx, rec, &delta); err != nil {
			tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT row")
			tx.ExecContext(ctx, "RELEASE SAVEPOINT row")
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("%s: %v", rec.Hostname, err))
			continue
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT row"); err != nil {
			tx.Rollback()
			return nil, err
		}
		seen[key] = true
		sum.OS += delta.OS
		sum.Locations += delta.Locations
		sum.Providers += delta.Providers
		sum.Labels += delta.Labels
		sum.Disks += delta.Disks
		sum.IPs += delta.IPs
		sum.Pricings += delta.Pricings
		sum.Imported++
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sum, nil
}

// importMyServer inserts one record (server + relations) inside tx.
// Counts accumulate into delta, merged by the caller only on success.
func importMyServer(ctx context.Context, tx *sql.Tx, rec MyServer, delta *MySummary) error {
	var osID, locID, provID sql.NullInt64
	var created bool
	var err error
	if rec.OSName != "" {
		if osID.Int64, created, err = getOrCreateCatalogTx(ctx, tx, "os", "name", rec.OSName); err != nil {
			return err
		}
		osID.Valid = true
		if created {
			delta.OS++
		}
	}
	if rec.LocationName != "" {
		if locID.Int64, created, err = getOrCreateCatalogTx(ctx, tx, "locations", "name", rec.LocationName); err != nil {
			return err
		}
		locID.Valid = true
		if created {
			delta.Locations++
		}
	}
	if rec.ProviderName != "" {
		if provID.Int64, created, err = getOrCreateCatalogTx(ctx, tx, "providers", "name", rec.ProviderName); err != nil {
			return err
		}
		provID.Valid = true
		if created {
			delta.Providers++
		}
	}

	var networkType sql.NullString
	if rec.NetworkType != "" {
		networkType = sql.NullString{String: rec.NetworkType, Valid: true}
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO servers (hostname, server_type, os_id, provider_id, location_id,
			ram_as_mb, cpu, cpu_model, bandwidth_as_mb, link_speed, network_type,
			ns1, ns2, ssh_port, active, show_public, was_promo, transferrable, owned_since)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.Hostname, rec.ServerType, osID, provID, locID,
		nullInt64(rec.RamAsMB), nullInt64(rec.CPU), nullStr(rec.CPUModel),
		nullInt64(rec.BandwidthAsMB), nullInt64(rec.LinkSpeed), networkType,
		nullStr(rec.Ns1), nullStr(rec.Ns2), nullInt64(rec.SSHPort),
		bint(rec.Active), bint(rec.ShowPublic), bint(rec.WasPromo),
		bint(rec.Transferrable), nullStr(rec.OwnedSince))
	if err != nil {
		return err
	}
	serverID, err := res.LastInsertId()
	if err != nil {
		return err
	}

	for _, d := range rec.Disks {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO server_disks (server_id, size_as_mb, media) VALUES (?, ?, ?)",
			serverID, d.SizeMB, d.Media); err != nil {
			return err
		}
		delta.Disks++
	}
	for _, ip := range rec.IPs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO ips (service_id, service_type, address, is_ipv4) VALUES (?, 1, ?, ?)",
			serverID, ip.Address, bint(ip.IsIPv4)); err != nil {
			return err
		}
		delta.IPs++
	}
	// Cap DISTINCT successful assignments (the source may repeat names or
	// vary case); get-or-create matches case-insensitively.
	assigned := 0
	seenLabels := map[string]bool{}
	for _, name := range rec.Labels {
		if assigned >= model.MaxLabelsPerService {
			break
		}
		key := strings.ToLower(name)
		if seenLabels[key] {
			continue
		}
		seenLabels[key] = true
		labelID, created, err := getOrCreateCatalogTx(ctx, tx, "labels", "label", name)
		if err != nil {
			return err
		}
		if created {
			delta.Labels++
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO labels_assigned (label_id, service_id, service_type) VALUES (?, ?, 1)",
			labelID, serverID); err != nil {
			return err
		}
		assigned++
	}
	if rec.Pricing != nil {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date)
			VALUES (?, 1, ?, ?, ?, ?)`,
			serverID, rec.Pricing.Currency, rec.Pricing.Price, rec.Pricing.Term,
			nullStr(rec.Pricing.NextDueDate)); err != nil {
			return err
		}
		delta.Pricings++
	}
	return nil
}

// getOrCreateCatalogTx is the tx-scoped catalog get-or-create.
func getOrCreateCatalogTx(ctx context.Context, tx dbtx, table, nameCol, name string) (int64, bool, error) {
	var id int64
	// Case-insensitive lookup: "OVH" and "ovh" from a source file are one
	// provider (the row's first-seen casing is kept).
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM "+table+" WHERE "+nameCol+" = ? COLLATE NOCASE", name).Scan(&id)
	if err == nil {
		return id, false, nil
	}
	if err != sql.ErrNoRows {
		return 0, false, err
	}
	res, err := tx.ExecContext(ctx,
		"INSERT INTO "+table+" ("+nameCol+") VALUES (?)", name)
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, true, err
}

func nullInt64(v *int64) sql.NullInt64 {
	if v == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: *v, Valid: true}
}

func nullStr(s string) sql.NullString {
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}
