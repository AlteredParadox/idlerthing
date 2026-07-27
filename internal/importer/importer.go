// Package importer restores idlerthing's own JSON export format.
//
// Semantics: catalogs (providers/locations/os/labels) are get-or-created by
// name; services are always inserted fresh, so importing into a non-empty
// database duplicates services — Import therefore requires force=true when
// any service table has rows. Everything runs in ONE transaction.
package importer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/netip"
	"strings"

	"idlerthing/internal/model"
)

// Summary reports per-type inserted counts.
type Summary struct {
	Warnings  []string
	YABS      int
	Providers int
	Locations int
	OS        int
	Labels    int
	Servers   int
	Shared    int
	Reseller  int
	Seedboxes int
	Domains   int
	Misc      int
	Pricings  int
	Disks     int
	IPs       int
	DNS       int
	Notes     int
}

// exportSections are the array-valued document keys (validated on import).
var exportSections = []string{
	"providers", "locations", "os", "labels",
	"servers", "shared", "reseller", "seedboxes", "domains", "misc",
	"pricings", "ips", "dns", "labels_assigned", "notes", "yabs",
}

// Import decodes an export document from r and inserts it into db.
// When force is false and any content table is non-empty, it refuses.
func Import(ctx context.Context, db *sql.DB, r io.Reader, force bool) (*Summary, error) {
	dec := json.NewDecoder(r)
	var doc map[string]any
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode export JSON: %w", err)
	}
	// Strict envelope: one document, nothing after it. The format marker
	// arrived in the same release as this check, so an ABSENT key is a
	// legacy (pre-marker) backup — restore it with a warning. A present
	// but unrecognized value is rejected.
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("trailing garbage after the JSON document")
	}
	legacyFormat := false
	if raw, present := doc["format"]; present {
		if v, _ := raw.(float64); v != 1 {
			return nil, fmt.Errorf("unrecognized \"format\" marker — expected an idlerthing JSON export")
		}
	} else {
		legacyFormat = true
	}
	for _, key := range exportSections {
		if raw, present := doc[key]; present {
			if _, isArr := raw.([]any); !isArr {
				return nil, fmt.Errorf("section %q must be an array", key)
			}
		}
	}

	// BEGIN IMMEDIATE takes SQLite's write lock up front: the emptiness
	// check and the restore are ONE atomic unit (a deferred begin would let
	// a running server write in between). A server writing during the
	// restore busy-times-out (5s) and fails that request.
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		return nil, fmt.Errorf("begin restore: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	if !force {
		var blocking []string
		for _, table := range []string{
			"servers", "shared_hosting", "reseller_hosting", "seedboxes", "domains", "misc_services",
			// Content tables too — importing duplicates them with NULL parents.
			"dns", "notes", "ips",
		} {
			var n int
			if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				blocking = append(blocking, fmt.Sprintf("%s: %d rows", table, n))
			}
		}
		if len(blocking) > 0 {
			return nil, fmt.Errorf("refusing import: database not empty (%s) — re-run with --force to import anyway, duplicating records",
				strings.Join(blocking, ", "))
		}
	}

	imp := &importer{tx: conn, seenIPs: map[string]bool{}}
	if legacyFormat {
		imp.sum.Warnings = append(imp.sum.Warnings,
			"backup predates the format marker — restoring as legacy format")
	}
	if err := imp.run(ctx, doc); err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		return nil, err
	}
	committed = true
	return &imp.sum, nil
}

// dbtx is satisfied by *sql.Conn (manual BEGIN IMMEDIATE) and *sql.Tx.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type importer struct {
	tx        dbtx
	sum       Summary
	catMisses int             // catalog refs the document couldn't resolve (partial export)
	ipMaps    map[int64]int64 // old ip id → new ip id
	// idMaps maps service_type → old id → new id.
	idMaps  [7]map[int64]int64
	catMaps map[string]map[int64]int64 // catalog kind → old id → new id
	seenIPs map[string]bool
}

func (imp *importer) run(ctx context.Context, doc map[string]any) error {
	imp.catMaps = map[string]map[int64]int64{"labels": {}}
	imp.ipMaps = map[int64]int64{}
	for i := range imp.idMaps {
		imp.idMaps[i] = map[int64]int64{}
	}

	// Partial (per-type) exports omit the related tables — say so loudly.
	if partial, _ := doc["partial"].(bool); partial {
		imp.sum.Warnings = append(imp.sum.Warnings,
			"partial export: related records (pricings/ips/dns/notes/labels/yabs) were not included and cannot be restored")
	}

	// Catalogs first (dependency order).
	if err := imp.catalog(ctx, doc, "providers", "providers", &imp.sum.Providers); err != nil {
		return err
	}
	if err := imp.catalog(ctx, doc, "locations", "locations", &imp.sum.Locations); err != nil {
		return err
	}
	if err := imp.catalog(ctx, doc, "os", "os", &imp.sum.OS); err != nil {
		return err
	}

	// Services (with pricing/disks/labels/ips inlined).
	if err := imp.servers(ctx, doc); err != nil {
		return err
	}
	if err := imp.hosting(ctx, doc, "shared", "shared_hosting", "shared_type", 2, &imp.sum.Shared); err != nil {
		return err
	}
	if err := imp.hosting(ctx, doc, "reseller", "reseller_hosting", "reseller_type", 3, &imp.sum.Reseller); err != nil {
		return err
	}
	if err := imp.seedboxes(ctx, doc); err != nil {
		return err
	}
	if err := imp.domains(ctx, doc); err != nil {
		return err
	}
	if err := imp.misc(ctx, doc); err != nil {
		return err
	}

	// Labels catalog (assignments come from inlined server labels).
	if err := imp.labels(ctx, doc); err != nil {
		return err
	}
	// Remaining relations.
	if err := imp.ips(ctx, doc); err != nil {
		return err
	}
	if err := imp.dns(ctx, doc); err != nil {
		return err
	}
	if err := imp.notes(ctx, doc); err != nil {
		return err
	}
	if err := imp.yabs(ctx, doc); err != nil {
		return err
	}
	if err := imp.labelsAssigned(ctx, doc); err != nil {
		return err
	}
	if err := imp.topLevelPricings(ctx, doc); err != nil {
		return err
	}
	if imp.catMisses > 0 {
		imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
			"partial export: catalog associations could not be restored for %d rows", imp.catMisses))
	}
	return nil
}

// yabs imports runs (with speeds) remapped through the server id map, then
// recomputes servers.has_yabs.
func (imp *importer) yabs(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "yabs") {
		y, _ := item.(map[string]any)
		oldServer := int64(fget(y, "server_id"))
		newServer, ok := imp.idMaps[1][oldServer]
		if !ok {
			continue // server not in this document
		}
		// INSERT OR IGNORE: pre-0009/0010 backups may contain duplicate
		// payload_hash/gb_url rows — keep the first, skip the rest (the
		// unique indexes do the deduping).
		res, err := imp.tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO yabs (server_id, run_at, cpu, cpu_cores, ram, swap, distro,
				kernel, uptime, geekbench_version, gb_single, gb_multi, gb_url, payload_hash)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			newServer, sgetN(y, "run_at"), sgetN(y, "cpu"), nget(y, "cpu_cores"),
			sgetN(y, "ram"), sgetN(y, "swap"), sgetN(y, "distro"), sgetN(y, "kernel"),
			sgetN(y, "uptime"), nget(y, "geekbench_version"), nget(y, "gb_single"),
			nget(y, "gb_multi"), sgetN(y, "gb_url"), sgetN(y, "payload_hash"))
		if err != nil {
			return fmt.Errorf("import yabs for server %d: %w", oldServer, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			continue // duplicate payload_hash/gb_url — already restored
		}
		yabsID, _ := res.LastInsertId()
		for _, d := range arr(y, "disk_speed") {
			dm, _ := d.(map[string]any)
			if _, err := imp.tx.ExecContext(ctx,
				"INSERT INTO yabs_disk_speed (yabs_id, block_size, read_mbps, write_mbps) VALUES (?, ?, ?, ?)",
				yabsID, sget(dm, "block_size"), fget(dm, "read_mbps"), fget(dm, "write_mbps")); err != nil {
				return err
			}
		}
		for _, n := range arr(y, "network_speed") {
			nm, _ := n.(map[string]any)
			if _, err := imp.tx.ExecContext(ctx,
				"INSERT INTO yabs_network_speed (yabs_id, location, provider, send_mbps, recv_mbps, latency_ms) VALUES (?, ?, ?, ?, ?, ?)",
				yabsID, sget(nm, "location"), sget(nm, "provider"),
				fget(nm, "send_mbps"), fget(nm, "recv_mbps"), fget(nm, "latency_ms")); err != nil {
				return err
			}
		}
		imp.sum.YABS++
		if err := imp.fixTimestamps(ctx, "yabs", yabsID, y); err != nil {
			return err
		}
	}
	if imp.sum.YABS > 0 {
		if _, err := imp.tx.ExecContext(ctx, `
			UPDATE servers SET has_yabs = (SELECT COUNT(*) > 0 FROM yabs WHERE server_id = servers.id)`); err != nil {
			return err
		}
	}
	return nil
}

// fixTimestamps preserves exported created_at/updated_at when present
// (schema defaults apply when absent).
func (imp *importer) fixTimestamps(ctx context.Context, table string, id int64, m map[string]any) error {
	created, updated := sget(m, "created_at"), sget(m, "updated_at")
	if created == "" && updated == "" {
		return nil
	}
	// Table names are compile-time constants from the importer.
	if _, err := imp.tx.ExecContext(ctx,
		"UPDATE "+table+" SET created_at = COALESCE(?, created_at), updated_at = COALESCE(?, updated_at) WHERE id = ?",
		nullStr(created), nullStr(updated), id); err != nil {
		return fmt.Errorf("preserve timestamps on %s %d: %w", table, id, err)
	}
	return nil
}

// topLevelPricings imports the standalone pricings table (all rows,
// including inactive ones, with the exported active flag preserved).
// INSERT OR IGNORE because inlined service pricing may already exist under
// the same UNIQUE(service_id, service_type). serviceType comes from the
// document and is validated against the valid pricing terms per batch G.
func (imp *importer) topLevelPricings(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "pricings") {
		pm, _ := item.(map[string]any)
		serviceType := int(fget(pm, "service_type"))
		if serviceType < 1 || serviceType > 6 {
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"pricing: service_type %d out of range, skipped", serviceType))
			continue
		}
		oldService := int64(fget(pm, "service_id"))
		newService, ok := imp.idMaps[serviceType][oldService]
		if !ok {
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"pricing: service %d/%d not in document, skipped", serviceType, oldService))
			continue
		}
		if !validImportPrice(fget(pm, "price")) {
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"pricing: invalid price for service %d, skipped", newService))
			continue
		}
		cur, ok := normCurrency(sget(pm, "currency"))
		if !ok {
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"pricing: invalid currency %q, skipped", sget(pm, "currency")))
			continue
		}
		if term := int64(fget(pm, "term")); term < 1 || term > 7 {
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"pricing: term %d out of range for service %d, skipped", term, newService))
			continue
		}
		due := sgetN(pm, "next_due_date")
		if d, ok := normDate(due.String); !ok {
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"pricing: invalid next_due_date %q — storing NULL", due.String))
			due = sql.NullString{}
		} else if d != "" {
			due = sql.NullString{String: d, Valid: true}
		}
		res, err := imp.tx.ExecContext(ctx, `
			INSERT OR IGNORE INTO pricings (service_id, service_type, currency, price, term, next_due_date, active)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			newService, serviceType, cur, fget(pm, "price"),
			int64(fget(pm, "term")), due, bint(bget(pm, "active")))
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			imp.sum.Pricings++
		}
		// Timestamps apply even when the insert was IGNOREd (the inlined
		// service pricing created the row first, without timestamps).
		var pid int64
		err = imp.tx.QueryRowContext(ctx,
			"SELECT id FROM pricings WHERE service_id = ? AND service_type = ?",
			newService, serviceType).Scan(&pid)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if pid > 0 {
			if err := imp.fixTimestamps(ctx, "pricings", pid, pm); err != nil {
				return err
			}
		}
	}
	return nil
}

// labelsAssigned imports the labels_assigned table, remapped through the
// labels catalog map and the service id maps, capped per service.
func (imp *importer) labelsAssigned(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "labels_assigned") {
		a, _ := item.(map[string]any)
		oldLabel := int64(fget(a, "label_id"))
		serviceType := int(fget(a, "service_type"))
		oldService := int64(fget(a, "service_id"))

		labelID, ok := imp.catMaps["labels"][oldLabel]
		if !ok {
			continue // label catalog row not in this document
		}
		if serviceType < 1 || serviceType > 6 {
			imp.sum.Warnings = append(imp.sum.Warnings,
				fmt.Sprintf("label assignment: service_type %d out of range, skipped", serviceType))
			continue
		}
		newService, ok := imp.idMaps[serviceType][oldService]
		if !ok {
			continue
		}
		var n int
		if err := imp.tx.QueryRowContext(ctx,
			"SELECT COUNT(*) FROM labels_assigned WHERE service_id = ? AND service_type = ?",
			newService, serviceType).Scan(&n); err != nil {
			return err
		}
		if n >= model.MaxLabelsPerService {
			continue
		}
		if _, err := imp.tx.ExecContext(ctx,
			"INSERT OR IGNORE INTO labels_assigned (label_id, service_id, service_type) VALUES (?, ?, ?)",
			labelID, newService, serviceType); err != nil {
			return err
		}
	}
	return nil
}

// ---------- decode helpers ----------

func arr(doc map[string]any, key string) []any {
	v, _ := doc[key].([]any)
	return v
}

func mget(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func sget(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func sgetN(m map[string]any, key string) sql.NullString {
	v := sget(m, key)
	if v == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: v, Valid: true}
}

func nget(m map[string]any, key string) sql.NullInt64 {
	v, ok := m[key].(float64)
	if !ok {
		return sql.NullInt64{}
	}
	// JSON itself can't carry NaN/Inf, but out-of-range float→int64 is
	// implementation-defined — clamp to NULL instead.
	if math.IsNaN(v) || math.IsInf(v, 0) || v > 1<<62 || v < -(1<<62) {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(v), Valid: true}
}

func bget(m map[string]any, key string) bool {
	v, _ := m[key].(bool)
	return v
}

func fget(m map[string]any, key string) float64 {
	v, _ := m[key].(float64)
	return v
}

func bint(b bool) int {
	if b {
		return 1
	}
	return 0
}

// oldID extracts the old numeric id of an entity map (0 when absent).
func oldID(m map[string]any) int64 {
	v, _ := m["id"].(float64)
	return int64(v)
}

// ---------- catalogs ----------

// catalog get-or-creates entries by name and builds the old→new id map.
func (imp *importer) catalog(ctx context.Context, doc map[string]any, key, table string, count *int) error {
	nameCol := "name"
	if table == "labels" {
		nameCol = "label"
	}
	imp.catMaps[table] = map[int64]int64{}
	for _, item := range arr(doc, key) {
		em, _ := item.(map[string]any)
		name := sget(em, "name")
		if name == "" {
			continue
		}
		// Case-distinct names merge into the existing row (NOCASE lookup) —
		// say so, since the document's casing is lost.
		var existing string
		if err := imp.tx.QueryRowContext(ctx,
			"SELECT "+nameCol+" FROM "+table+" WHERE "+nameCol+" = ? COLLATE NOCASE", name).Scan(&existing); err == nil && existing != name {
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"catalog %s: %q merged into existing %q", table, name, existing))
		}
		newID, created, err := getOrCreateCatalogTx(ctx, imp.tx, table, nameCol, name)
		if err != nil {
			return err
		}
		if old := oldID(em); old > 0 {
			imp.catMaps[table][old] = newID
		}
		if created {
			*count++
		}
	}
	return nil
}

// remapCatalog returns the new catalog id for an old one (NULL when unknown).
func (imp *importer) remapCatalog(table string, old sql.NullInt64) sql.NullInt64 {
	if !old.Valid {
		return sql.NullInt64{}
	}
	if newID, ok := imp.catMaps[table][old.Int64]; ok {
		return sql.NullInt64{Int64: newID, Valid: true}
	}
	imp.catMisses++
	return sql.NullInt64{}
}

// ---------- pricing ----------

// validImportPrice mirrors the app's finite-price rule for JSON numbers.
func validImportPrice(f float64) bool {
	return !math.IsNaN(f) && !math.IsInf(f, 0) && f > 0 && f <= 1e9
}

// normCurrency validates + uppercases a currency code (^3 ASCII letters).
// Deliberately a FORMAT check, not membership in model.Currencies: imports
// (especially my-idlers) may carry codes outside the app's select list, and
// keeping them loses nothing — displays fall back to a "CODE " prefix.
func normCurrency(c string) (string, bool) {
	c = strings.ToUpper(strings.TrimSpace(c))
	if len(c) != 3 {
		return "", false
	}
	for _, r := range c {
		if r < 'A' || r > 'Z' {
			return "", false
		}
	}
	return c, true
}

// normOwned normalizes an owned_since value from the document.
func (imp *importer) normOwned(v sql.NullString, serviceName string) sql.NullString {
	if !v.Valid {
		return v
	}
	if d, ok := normDate(v.String); ok {
		return sql.NullString{String: d, Valid: d != ""}
	}
	imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
		"%s: invalid owned_since %q — storing NULL", serviceName, v.String))
	return sql.NullString{}
}

// insertPricing inserts the inlined pricing map for a new service id.
func (imp *importer) insertPricing(ctx context.Context, pm map[string]any, serviceID int64, serviceType int) error {
	if pm == nil || sget(pm, "currency") == "" {
		return nil
	}
	cur, ok := normCurrency(sget(pm, "currency"))
	if !ok {
		imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
			"pricing: invalid currency %q for service %d, skipped", sget(pm, "currency"), serviceID))
		return nil
	}
	if !validImportPrice(fget(pm, "price")) {
		imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
			"pricing: invalid price for service %d, skipped", serviceID))
		return nil
	}
	if term := int64(fget(pm, "term")); term < 1 || term > 7 {
		imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
			"pricing: term %d out of range for service %d, skipped", term, serviceID))
		return nil
	}
	due := sgetN(pm, "next_due_date")
	if d, ok := normDate(due.String); !ok {
		imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
			"pricing: invalid next_due_date %q for service %d — storing NULL", due.String, serviceID))
		due = sql.NullString{}
	} else if d != "" {
		due = sql.NullString{String: d, Valid: true}
	}
	_, err := imp.tx.ExecContext(ctx, `
		INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date, active)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		serviceID, serviceType, cur, fget(pm, "price"),
		int64(fget(pm, "term")), due, bint(bget(pm, "active")))
	if err == nil {
		imp.sum.Pricings++
	}
	return err
}

// boundedInt re-validates a nullable numeric against the same plausibility
// caps the web form + JSON API use; out-of-range → NULL + warning.
func (imp *importer) boundedInt(v sql.NullInt64, max int64, field, host string) sql.NullInt64 {
	if !v.Valid {
		return v
	}
	if v.Int64 < 0 || v.Int64 > max {
		imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
			"server %q: %s %d out of range — storing NULL", host, field, v.Int64))
		return sql.NullInt64{}
	}
	return v
}

// ---------- servers ----------

func (imp *importer) servers(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "servers") {
		it, _ := item.(map[string]any)
		s := mget(it, "server")
		if s == nil {
			continue
		}
		serverType := int64(fget(s, "server_type"))
		if serverType < model.TypeKVM || serverType > model.TypeNAT {
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"server %q: server_type %d out of range — storing %d (KVM)", sget(s, "hostname"), serverType, model.TypeKVM))
			serverType = model.TypeKVM
		}
		res, err := imp.tx.ExecContext(ctx, `
			INSERT INTO servers (hostname, server_type, os_id, provider_id, location_id,
				ram_as_mb, cpu, cpu_model, bandwidth_as_mb, link_speed, network_type, ns1, ns2,
				ssh_port, active, show_public, was_promo, transferrable, owned_since)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sget(s, "hostname"), serverType,
			imp.remapCatalog("os", nget(s, "os_id")),
			imp.remapCatalog("providers", nget(s, "provider_id")),
			imp.remapCatalog("locations", nget(s, "location_id")),
			imp.boundedInt(nget(s, "ram_as_mb"), 1<<30, "ram_as_mb", sget(s, "hostname")), imp.boundedInt(nget(s, "cpu"), 1024, "cpu", sget(s, "hostname")), sgetN(s, "cpu_model"),
			imp.boundedInt(nget(s, "bandwidth_as_mb"), 1<<30, "bandwidth_as_mb", sget(s, "hostname")), imp.boundedInt(nget(s, "link_speed"), 1<<20, "link_speed", sget(s, "hostname")), sgetN(s, "network_type"),
			sgetN(s, "ns1"), sgetN(s, "ns2"), imp.boundedInt(nget(s, "ssh_port"), 65535, "ssh_port", sget(s, "hostname")),
			bint(bget(s, "active")), bint(bget(s, "show_public")),
			bint(bget(s, "was_promo")), bint(bget(s, "transferrable")),
			imp.normOwned(sgetN(s, "owned_since"), sget(s, "hostname")))
		if err != nil {
			return fmt.Errorf("import server %q: %w", sget(s, "hostname"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(s); old > 0 {
			imp.idMaps[1][old] = newID
		}
		imp.sum.Servers++
		if err := imp.fixTimestamps(ctx, "servers", newID, s); err != nil {
			return err
		}

		for _, d := range arr(it, "disks") {
			dm, _ := d.(map[string]any)
			size := imp.boundedInt(nget(dm, "size_as_mb"), 1<<30, "disk size_as_mb", sget(s, "hostname"))
			if !size.Valid || size.Int64 <= 0 {
				continue
			}
			media := sget(dm, "media")
			switch media {
			case "SSD", "HDD", "NVMe":
			case "":
				media = "SSD"
			default:
				imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
					"server %q: invalid disk media %q — storing SSD", sget(s, "hostname"), media))
				media = "SSD"
			}
			if _, err := imp.tx.ExecContext(ctx,
				"INSERT INTO server_disks (server_id, size_as_mb, media) VALUES (?, ?, ?)",
				newID, size.Int64, media); err != nil {
				return err
			}
			imp.sum.Disks++
		}

		// Inlined labels → assign (get-or-create label, cap 4).
		for _, l := range arr(it, "labels") {
			lm, _ := l.(map[string]any)
			name := sget(lm, "name")
			if name == "" {
				continue
			}
			labelID, _, err := getOrCreateCatalogTx(ctx, imp.tx, "labels", "label", name)
			if err != nil {
				return err
			}
			var n int
			if err := imp.tx.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM labels_assigned WHERE service_id = ? AND service_type = 1", newID).Scan(&n); err != nil {
				return err
			}
			if n >= model.MaxLabelsPerService {
				continue
			}
			if _, err := imp.tx.ExecContext(ctx,
				"INSERT OR IGNORE INTO labels_assigned (label_id, service_id, service_type) VALUES (?, ?, 1)",
				labelID, newID); err != nil {
				return err
			}
		}

		// Inlined IPs.
		for _, ip := range arr(it, "ips") {
			im, _ := ip.(map[string]any)
			if err := imp.insertIP(ctx, im, newID, 1); err != nil {
				return err
			}
		}

		if err := imp.insertPricing(ctx, mget(it, "pricing"), newID, 1); err != nil {
			return err
		}
	}
	return nil
}

// insertIP inserts an IP map for a service, deduping within the import.
// The address is stored in CANONICAL form and is_ipv4 derived from it (the
// file's flag is ignored when it contradicts).
func (imp *importer) insertIP(ctx context.Context, im map[string]any, serviceID int64, serviceType int) error {
	raw := sget(im, "address")
	if raw == "" {
		return nil
	}
	parsed, err := netip.ParseAddr(raw)
	if err != nil {
		imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
			"ip %q: invalid address, skipped", raw))
		return nil
	}
	addr := parsed.String()
	key := fmt.Sprintf("%d:%d:%s", serviceType, serviceID, addr)
	if imp.seenIPs[key] {
		return nil
	}
	imp.seenIPs[key] = true
	v4 := 0
	if parsed.Is4() {
		v4 = 1
	}
	res, err := imp.tx.ExecContext(ctx, `
		INSERT INTO ips (service_id, service_type, address, is_ipv4, country, region, city, org, isp, asn, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		serviceID, serviceType, addr, v4,
		sgetN(im, "country"), sgetN(im, "region"), sgetN(im, "city"),
		sgetN(im, "org"), sgetN(im, "isp"), sgetN(im, "asn"), sgetN(im, "fetched_at"))
	if err == nil {
		imp.sum.IPs++
		ipID, _ := res.LastInsertId()
		if old := oldID(im); old > 0 {
			imp.ipMaps[old] = ipID
		}
		return imp.fixTimestamps(ctx, "ips", ipID, im)
	}
	return err
}

// ---------- shared/reseller ----------

func (imp *importer) hosting(ctx context.Context, doc map[string]any, key, table, typeCol string, serviceType int, count *int) error {
	entityKey := "shared_hosting"
	for _, item := range arr(doc, key) {
		it, _ := item.(map[string]any)
		h := mget(it, entityKey)
		if h == nil {
			continue
		}
		res, err := imp.tx.ExecContext(ctx, `
			INSERT INTO `+table+` (main_domain, `+typeCol+`, provider_id, location_id,
				domains_limit, subdomains_limit, ftp_limit, email_limit, db_limit,
				disk_as_mb, bandwidth_as_mb, has_dedicated_ip, ip,
				active, show_public, was_promo, owned_since)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sget(h, "main_domain"), sgetN(h, "shared_type"),
			imp.remapCatalog("providers", nget(h, "provider_id")),
			imp.remapCatalog("locations", nget(h, "location_id")),
			imp.boundedInt(nget(h, "domains_limit"), 1<<20, "domains_limit", sget(h, "main_domain")),
			imp.boundedInt(nget(h, "subdomains_limit"), 1<<20, "subdomains_limit", sget(h, "main_domain")),
			imp.boundedInt(nget(h, "ftp_limit"), 1<<20, "ftp_limit", sget(h, "main_domain")),
			imp.boundedInt(nget(h, "email_limit"), 1<<20, "email_limit", sget(h, "main_domain")),
			imp.boundedInt(nget(h, "db_limit"), 1<<20, "db_limit", sget(h, "main_domain")),
			imp.boundedInt(nget(h, "disk_as_mb"), 1<<30, "disk_as_mb", sget(h, "main_domain")),
			imp.boundedInt(nget(h, "bandwidth_as_mb"), 1<<30, "bandwidth_as_mb", sget(h, "main_domain")),
			bint(bget(h, "has_dedicated_ip")), sgetN(h, "ip"),
			bint(bget(h, "active")), bint(bget(h, "show_public")),
			bint(bget(h, "was_promo")), imp.normOwned(sgetN(h, "owned_since"), sget(h, "main_domain")))
		if err != nil {
			return fmt.Errorf("import %s %q: %w", table, sget(h, "main_domain"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(h); old > 0 {
			imp.idMaps[serviceType][old] = newID
		}
		*count++
		if err := imp.fixTimestamps(ctx, table, newID, h); err != nil {
			return err
		}
		if err := imp.insertPricing(ctx, mget(it, "pricing"), newID, serviceType); err != nil {
			return err
		}
	}
	return nil
}

// ---------- seedboxes ----------

func (imp *importer) seedboxes(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "seedboxes") {
		it, _ := item.(map[string]any)
		b := mget(it, "seedbox")
		if b == nil {
			continue
		}
		res, err := imp.tx.ExecContext(ctx, `
			INSERT INTO seedboxes (title, hostname, seed_box_type, provider_id, location_id,
				port_speed, disk_as_mb, bandwidth_as_mb, active, show_public, was_promo, owned_since)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sgetN(b, "title"), sget(b, "hostname"), sgetN(b, "seed_box_type"),
			imp.remapCatalog("providers", nget(b, "provider_id")),
			imp.remapCatalog("locations", nget(b, "location_id")),
			imp.boundedInt(nget(b, "port_speed"), 1<<20, "port_speed", sget(b, "hostname")),
			imp.boundedInt(nget(b, "disk_as_mb"), 1<<30, "disk_as_mb", sget(b, "hostname")),
			imp.boundedInt(nget(b, "bandwidth_as_mb"), 1<<30, "bandwidth_as_mb", sget(b, "hostname")),
			bint(bget(b, "active")), bint(bget(b, "show_public")),
			bint(bget(b, "was_promo")), imp.normOwned(sgetN(b, "owned_since"), sget(b, "hostname")))
		if err != nil {
			return fmt.Errorf("import seedbox %q: %w", sget(b, "hostname"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(b); old > 0 {
			imp.idMaps[6][old] = newID
		}
		imp.sum.Seedboxes++
		if err := imp.fixTimestamps(ctx, "seedboxes", newID, b); err != nil {
			return err
		}
		if err := imp.insertPricing(ctx, mget(it, "pricing"), newID, 6); err != nil {
			return err
		}
	}
	return nil
}

// ---------- domains ----------

func (imp *importer) domains(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "domains") {
		it, _ := item.(map[string]any)
		d := mget(it, "domain")
		if d == nil {
			continue
		}
		res, err := imp.tx.ExecContext(ctx, `
			INSERT INTO domains (domain, extension, ns1, ns2, ns3, provider_id, active, owned_since)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			sget(d, "domain"), sgetN(d, "extension"), sgetN(d, "ns1"), sgetN(d, "ns2"), sgetN(d, "ns3"),
			imp.remapCatalog("providers", nget(d, "provider_id")),
			bint(bget(d, "active")), imp.normOwned(sgetN(d, "owned_since"), sget(d, "domain")))
		if err != nil {
			return fmt.Errorf("import domain %q: %w", sget(d, "domain"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(d); old > 0 {
			imp.idMaps[4][old] = newID
		}
		imp.sum.Domains++
		if err := imp.fixTimestamps(ctx, "domains", newID, d); err != nil {
			return err
		}
		if err := imp.insertPricing(ctx, mget(it, "pricing"), newID, 4); err != nil {
			return err
		}
	}
	return nil
}

// ---------- misc ----------

func (imp *importer) misc(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "misc") {
		it, _ := item.(map[string]any)
		m := mget(it, "misc_service")
		if m == nil {
			continue
		}
		res, err := imp.tx.ExecContext(ctx,
			"INSERT INTO misc_services (name, active, owned_since) VALUES (?, ?, ?)",
			sget(m, "name"), bint(bget(m, "active")), imp.normOwned(sgetN(m, "owned_since"), sget(m, "name")))
		if err != nil {
			return fmt.Errorf("import misc %q: %w", sget(m, "name"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(m); old > 0 {
			imp.idMaps[5][old] = newID
		}
		imp.sum.Misc++
		if err := imp.fixTimestamps(ctx, "misc_services", newID, m); err != nil {
			return err
		}
		if err := imp.insertPricing(ctx, mget(it, "pricing"), newID, 5); err != nil {
			return err
		}
	}
	return nil
}

// ---------- labels / ips / dns / notes ----------

func (imp *importer) labels(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "labels") {
		it, _ := item.(map[string]any)
		l := mget(it, "catalog_item")
		if l == nil {
			l = it
		}
		name := sget(l, "name")
		if name == "" {
			continue
		}
		newID, created, err := getOrCreateCatalogTx(ctx, imp.tx, "labels", "label", name)
		if err != nil {
			return err
		}
		if created {
			imp.sum.Labels++
		}
		if old := oldID(l); old > 0 {
			imp.catMaps["labels"][old] = newID
		}
	}
	return nil
}

// ips imports the top-level ips array (server IPs already came in inline;
// dedupe keeps the unique constraint intact).
func (imp *importer) ips(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "ips") {
		it, _ := item.(map[string]any)
		im := mget(it, "ip")
		if im == nil {
			continue
		}
		serviceType := int(fget(im, "service_type"))
		if serviceType < 1 || serviceType > 6 {
			imp.sum.Warnings = append(imp.sum.Warnings,
				fmt.Sprintf("ip %q: service_type %d out of range, skipped", sget(im, "address"), serviceType))
			continue
		}
		oldService := int64(fget(im, "service_id"))
		newService, ok := imp.idMaps[serviceType][oldService]
		if !ok {
			continue // service not imported (e.g. partial export)
		}
		if err := imp.insertIP(ctx, im, newService, serviceType); err != nil {
			return err
		}
	}
	return nil
}

func (imp *importer) dns(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "dns") {
		it, _ := item.(map[string]any)
		d := mget(it, "dns_record")
		if d == nil {
			continue
		}
		remap := func(key string, serviceType int) sql.NullInt64 {
			old := nget(d, key)
			if !old.Valid {
				return sql.NullInt64{}
			}
			if newID, ok := imp.idMaps[serviceType][old.Int64]; ok {
				return sql.NullInt64{Int64: newID, Valid: true}
			}
			imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
				"dns %q: parent %s=%d not in document — storing NULL", sget(d, "hostname"), key, old.Int64))
			return sql.NullInt64{}
		}
		dnsType := sget(d, "dns_type")
		if dnsType == "" {
			dnsType = "A"
		} else {
			valid := false
			for _, t := range model.DNSTypes {
				if dnsType == t {
					valid = true
					break
				}
			}
			if !valid {
				imp.sum.Warnings = append(imp.sum.Warnings, fmt.Sprintf(
					"dns %q: invalid dns_type %q — storing A", sget(d, "hostname"), dnsType))
				dnsType = "A"
			}
		}
		dres, err := imp.tx.ExecContext(ctx, `
			INSERT INTO dns (hostname, dns_type, address, server_id, domain_id, shared_id, reseller_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sget(d, "hostname"), dnsType, sget(d, "address"),
			remap("server_id", 1), remap("domain_id", 4), remap("shared_id", 2), remap("reseller_id", 3))
		if err != nil {
			return fmt.Errorf("import dns %q: %w", sget(d, "hostname"), err)
		}
		imp.sum.DNS++
		if err := func() error { dnsID, _ := dres.LastInsertId(); return imp.fixTimestamps(ctx, "dns", dnsID, d) }(); err != nil {
			return err
		}
	}
	return nil
}

func (imp *importer) notes(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "notes") {
		it, _ := item.(map[string]any)
		n := mget(it, "note")
		if n == nil {
			continue
		}
		// IP-keyed note: remap ip_id through the ip id map (service fields
		// are NULL for these — check BEFORE the service guards).
		if ipOld := nget(n, "ip_id"); ipOld.Valid {
			newIP, ok := imp.ipMaps[ipOld.Int64]
			if !ok {
				continue // parent ip not in this document
			}
			body := sget(n, "body")
			if body == "" {
				continue
			}
			res, err := imp.tx.ExecContext(ctx,
				"INSERT INTO notes (ip_id, body) VALUES (?, ?)", newIP, body)
			if err != nil {
				return err
			}
			imp.sum.Notes++
			if noteID, _ := res.LastInsertId(); noteID > 0 {
				if err := imp.fixTimestamps(ctx, "notes", noteID, n); err != nil {
					return err
				}
			}
			continue
		}
		serviceType := int(fget(n, "service_type"))
		if serviceType < 1 || serviceType > 6 {
			imp.sum.Warnings = append(imp.sum.Warnings,
				fmt.Sprintf("note: service_type %d out of range, skipped", serviceType))
			continue
		}
		oldService := nget(n, "service_id")
		if !oldService.Valid {
			continue
		}

		newService, ok := imp.idMaps[serviceType][oldService.Int64]
		if !ok {
			continue
		}
		body := sget(n, "body")
		if body == "" {
			continue
		}
		nres, err := imp.tx.ExecContext(ctx,
			"INSERT INTO notes (service_id, service_type, body) VALUES (?, ?, ?)",
			newService, serviceType, body)
		if err != nil {
			return err
		}
		imp.sum.Notes++
		if noteID, _ := nres.LastInsertId(); noteID > 0 {
			if err := imp.fixTimestamps(ctx, "notes", noteID, n); err != nil {
				return err
			}
		}
	}
	return nil
}
