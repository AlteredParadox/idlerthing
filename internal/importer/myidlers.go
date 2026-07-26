package importer

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
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
		s := MyServer{
			Hostname:      strings.TrimSpace(j.Hostname),
			ServerType:    1,
			NetworkType:   mapNetworkType(j.NetworkType),
			WasPromo:      intBool(j.WasPromo),
			Transferrable: intBool(j.Transferrable),
			Active:        j.Active == nil || *j.Active != 0, // default active
			ShowPublic:    intBool(j.ShowPublic),
			CPU:           j.CPU,
			RamAsMB:       j.RamAsMB,
			LinkSpeed:     j.LinkSpeed,
			SSHPort:       j.SSH,
		}
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
				warnings = append(warnings, fmt.Sprintf("%s: invalid owned_since %q — storing NULL", s.Hostname, *j.OwnedSince))
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

		s.BandwidthAsMB = convertBandwidth(j.Bandwidth)

		for _, d := range j.Disks {
			if mb := diskToMB(d.Size, d.Unit); mb > 0 {
				s.Disks = append(s.Disks, myDisk{SizeMB: mb, Media: diskMedia(d.Media)})
			}
		}
		if len(s.Disks) == 0 && j.DiskAsGB != nil && *j.DiskAsGB > 0 {
			// Legacy single-disk fallback.
			s.Disks = append(s.Disks, myDisk{SizeMB: int64(*j.DiskAsGB * 1024), Media: "SSD"})
		}

		for _, ip := range j.IPs {
			if ip.Address != "" {
				s.IPs = append(s.IPs, myIP{Address: ip.Address, IsIPv4: ip.IsIPv4 != 0})
			}
		}
		s.Labels = parseLabels(j.Labels)

		if j.Pricing != nil && j.Pricing.Price > 0 {
			p := &myPricing{
				Currency:    j.Pricing.Currency,
				Price:       j.Pricing.Price,
				Term:        j.Pricing.Term,
				NextDueDate: j.Pricing.NextDueDate,
			}
			if d, ok := normDate(p.NextDueDate); ok {
				p.NextDueDate = d
			} else {
				warnings = append(warnings, fmt.Sprintf("%s: invalid next_due_date %q — storing NULL", s.Hostname, p.NextDueDate))
				p.NextDueDate = ""
			}
			s.Pricing = p
		}
		out = append(out, s)
	}
	return out, warnings, nil
}

// parseLabels handles labels as ["a","b"] or [{"label":"a"},...] (and null).
func parseLabels(raw json.RawMessage) []string {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var strings_ []string
	if err := json.Unmarshal(raw, &strings_); err == nil {
		return strings_
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
		return out
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
	header := map[string]int{}
	for i, col := range rows[0] {
		header[col] = i
	}
	cell := func(row []string, name string) string {
		if i, ok := header[name]; ok && i < len(row) {
			return strings.TrimSpace(row[i])
		}
		return ""
	}
	atoi := func(v string) *int64 {
		if v == "" {
			return nil
		}
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil
		}
		return &n
	}
	atof := func(v string) *float64 {
		if v == "" {
			return nil
		}
		f, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return nil
		}
		return &f
	}
	csvBool := func(v string) bool { return v == "1" }

	var out []MyServer
	var warnings []string
	for i, row := range rows[1:] {
		owned, ownedOK := normDate(cell(row, "owned_since"))
		if !ownedOK {
			warnings = append(warnings, fmt.Sprintf("row %d: invalid owned_since %q — storing NULL", i+1, cell(row, "owned_since")))
		}
		s := MyServer{
			Hostname:      cell(row, "hostname"),
			Ns1:           cell(row, "ns1"),
			Ns2:           cell(row, "ns2"),
			ServerType:    1,
			CPU:           atoi(cell(row, "cpu")),
			CPUModel:      cell(row, "cpu_model"),
			RamAsMB:       atoi(cell(row, "ram_as_mb")),
			NetworkType:   mapNetworkTypeStr(cell(row, "network_type")),
			SSHPort:       atoi(cell(row, "ssh")),
			LinkSpeed:     atoi(cell(row, "link_speed")),
			WasPromo:      csvBool(cell(row, "was_promo")),
			Transferrable: csvBool(cell(row, "transferrable")),
			Active:        cell(row, "active") != "0",
			ShowPublic:    csvBool(cell(row, "show_public")),
			OwnedSince:    owned,
			OSName:        cell(row, "os_name"),
			LocationName:  cell(row, "location_name"),
			ProviderName:  cell(row, "provider_name"),
		}
		if st := atoi(cell(row, "server_type")); st != nil && *st >= 1 && *st <= 7 {
			s.ServerType = int(*st)
		}
		s.BandwidthAsMB = convertBandwidth(atof(cell(row, "bandwidth")))

		var disks []miJSONDisk
		if raw := cell(row, "disks"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &disks); err != nil {
				warnings = append(warnings, fmt.Sprintf("row %d (%s): bad disks JSON", i+1, s.Hostname))
			}
		}
		for _, d := range disks {
			if mb := diskToMB(d.Size, d.Unit); mb > 0 {
				s.Disks = append(s.Disks, myDisk{SizeMB: mb, Media: diskMedia(d.Media)})
			}
		}
		if len(s.Disks) == 0 {
			if gb := atof(cell(row, "disk_as_gb")); gb != nil && *gb > 0 {
				s.Disks = append(s.Disks, myDisk{SizeMB: int64(*gb * 1024), Media: "SSD"})
			}
		}

		var ips []miJSONIP
		if raw := cell(row, "ips"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &ips); err != nil {
				warnings = append(warnings, fmt.Sprintf("row %d (%s): bad ips JSON", i+1, s.Hostname))
			}
		}
		for _, ip := range ips {
			if ip.Address != "" {
				s.IPs = append(s.IPs, myIP{Address: ip.Address, IsIPv4: ip.IsIPv4 != 0})
			}
		}
		s.Labels = parseLabels(json.RawMessage(cell(row, "labels")))

		if price := atof(cell(row, "pricing_price")); price != nil && *price > 0 {
			term := 1
			if t := atoi(cell(row, "pricing_term")); t != nil {
				term = int(*t)
			}
			due, dueOK := normDate(cell(row, "pricing_next_due_date"))
			if !dueOK {
				warnings = append(warnings, fmt.Sprintf("row %d: invalid next_due_date %q — storing NULL", i+1, cell(row, "pricing_next_due_date")))
			}
			s.Pricing = &myPricing{
				Currency:    cell(row, "pricing_currency"),
				Price:       *price,
				Term:        term,
				NextDueDate: due,
			}
		}
		out = append(out, s)
	}
	return out, warnings, nil
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
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return "", false
	}
	return t.Format("2006-01-02"), true
}

func intBool(v *int) bool { return v != nil && *v != 0 }

// convertBandwidth: my-idlers stores GB → MB (1024-based); 0/nil → NULL (unlimited).
func convertBandwidth(gb *float64) *int64 {
	if gb == nil || *gb <= 0 {
		return nil
	}
	mb := int64(*gb * 1024)
	return &mb
}

// diskToMB converts a disk size+unit to whole MB (1024-based).
func diskToMB(size float64, unit string) int64 {
	switch strings.ToUpper(unit) {
	case "TB":
		return int64(size * 1024 * 1024)
	case "MB":
		return int64(size)
	default: // GB and anything else
		return int64(size * 1024)
	}
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

// networkTypes is our target set.
var networkTypes = map[string]bool{
	"IPv4": true, "IPv6": true, "IPv4+IPv6": true,
	"IPv4 NAT": true, "IPv4 NAT + IPv6": true,
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
		seen[key] = true

		// Per-row savepoint: a failing row rolls back cleanly without
		// poisoning the transaction for the rest.
		if _, err := tx.ExecContext(ctx, "SAVEPOINT row"); err != nil {
			tx.Rollback()
			return nil, err
		}
		if err := importMyServer(ctx, tx, rec, sum); err != nil {
			tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT row")
			tx.ExecContext(ctx, "RELEASE SAVEPOINT row")
			sum.Warnings = append(sum.Warnings, fmt.Sprintf("%s: %v", rec.Hostname, err))
			continue
		}
		if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT row"); err != nil {
			tx.Rollback()
			return nil, err
		}
		sum.Imported++
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return sum, nil
}

// importMyServer inserts one record (server + relations) inside tx.
func importMyServer(ctx context.Context, tx *sql.Tx, rec MyServer, sum *MySummary) error {
	var osID, locID, provID sql.NullInt64
	var created bool
	var err error
	if rec.OSName != "" {
		if osID.Int64, created, err = getOrCreateCatalogTx(ctx, tx, "os", "name", rec.OSName); err != nil {
			return err
		}
		osID.Valid = true
		if created {
			sum.OS++
		}
	}
	if rec.LocationName != "" {
		if locID.Int64, created, err = getOrCreateCatalogTx(ctx, tx, "locations", "name", rec.LocationName); err != nil {
			return err
		}
		locID.Valid = true
		if created {
			sum.Locations++
		}
	}
	if rec.ProviderName != "" {
		if provID.Int64, created, err = getOrCreateCatalogTx(ctx, tx, "providers", "name", rec.ProviderName); err != nil {
			return err
		}
		provID.Valid = true
		if created {
			sum.Providers++
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
		sum.Disks++
	}
	for _, ip := range rec.IPs {
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO ips (service_id, service_type, address, is_ipv4) VALUES (?, 1, ?, ?)",
			serverID, ip.Address, bint(ip.IsIPv4)); err != nil {
			return err
		}
		sum.IPs++
	}
	for i, name := range rec.Labels {
		if i >= model.MaxLabelsPerService {
			break
		}
		labelID, created, err := getOrCreateCatalogTx(ctx, tx, "labels", "label", name)
		if err != nil {
			return err
		}
		if created {
			sum.Labels++
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO labels_assigned (label_id, service_id, service_type) VALUES (?, ?, 1)",
			labelID, serverID); err != nil {
			return err
		}
	}
	if rec.Pricing != nil {
		term := rec.Pricing.Term
		if term < 1 || term > 7 {
			term = 1
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date)
			VALUES (?, 1, ?, ?, ?, ?)`,
			serverID, rec.Pricing.Currency, rec.Pricing.Price, term,
			nullStr(rec.Pricing.NextDueDate)); err != nil {
			return err
		}
		sum.Pricings++
	}
	return nil
}

// getOrCreateCatalogTx is the tx-scoped catalog get-or-create.
func getOrCreateCatalogTx(ctx context.Context, tx *sql.Tx, table, nameCol, name string) (int64, bool, error) {
	var id int64
	err := tx.QueryRowContext(ctx,
		"SELECT id FROM "+table+" WHERE "+nameCol+" = ?", name).Scan(&id)
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
