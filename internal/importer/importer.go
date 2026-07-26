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

	"idlerthing/internal/model"
)

// Summary reports per-type inserted counts.
type Summary struct {
	Warnings  []string
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

// Import decodes an export document from r and inserts it into db.
// When force is false and any service table is non-empty, it refuses.
func Import(ctx context.Context, db *sql.DB, r io.Reader, force bool) (*Summary, error) {
	var doc map[string]any
	if err := json.NewDecoder(r).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode export JSON: %w", err)
	}

	if !force {
		for _, table := range []string{"servers", "shared_hosting", "reseller_hosting", "seedboxes", "domains", "misc_services"} {
			var n int
			if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
				return nil, err
			}
			if n > 0 {
				return nil, fmt.Errorf("%s is not empty (%d rows) — importing duplicates services; re-run with --force", table, n)
			}
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	imp := &importer{tx: tx, seenIPs: map[string]bool{}}
	if err := imp.run(ctx, doc); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &imp.sum, nil
}

type importer struct {
	tx  *sql.Tx
	sum Summary
	// idMaps maps service_type → old id → new id.
	idMaps  [7]map[int64]int64
	catMaps map[string]map[int64]int64 // catalog kind → old id → new id
	seenIPs map[string]bool
}

func (imp *importer) run(ctx context.Context, doc map[string]any) error {
	imp.catMaps = map[string]map[int64]int64{}
	for i := range imp.idMaps {
		imp.idMaps[i] = map[int64]int64{}
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
	return sql.NullInt64{}
}

// ---------- pricing ----------

// insertPricing inserts the inlined pricing map for a new service id.
func (imp *importer) insertPricing(ctx context.Context, pm map[string]any, serviceID int64, serviceType int) error {
	if pm == nil || sget(pm, "currency") == "" {
		return nil
	}
	_, err := imp.tx.ExecContext(ctx, `
		INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date, active)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		serviceID, serviceType, sget(pm, "currency"), fget(pm, "price"),
		int64(fget(pm, "term")), sgetN(pm, "next_due_date"), bint(bget(pm, "active")))
	if err == nil {
		imp.sum.Pricings++
	}
	return err
}

// ---------- servers ----------

func (imp *importer) servers(ctx context.Context, doc map[string]any) error {
	for _, item := range arr(doc, "servers") {
		it, _ := item.(map[string]any)
		s := mget(it, "server")
		if s == nil {
			continue
		}
		res, err := imp.tx.ExecContext(ctx, `
			INSERT INTO servers (hostname, server_type, os_id, provider_id, location_id,
				ram_as_mb, cpu, cpu_model, bandwidth_as_mb, link_speed, network_type, ns1, ns2,
				ssh_port, active, show_public, was_promo, transferrable, owned_since)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			sget(s, "hostname"), int64(fget(s, "server_type")),
			imp.remapCatalog("os", nget(s, "os_id")),
			imp.remapCatalog("providers", nget(s, "provider_id")),
			imp.remapCatalog("locations", nget(s, "location_id")),
			nget(s, "ram_as_mb"), nget(s, "cpu"), sgetN(s, "cpu_model"),
			nget(s, "bandwidth_as_mb"), nget(s, "link_speed"), sgetN(s, "network_type"),
			sgetN(s, "ns1"), sgetN(s, "ns2"), nget(s, "ssh_port"),
			bint(bget(s, "active")), bint(bget(s, "show_public")),
			bint(bget(s, "was_promo")), bint(bget(s, "transferrable")),
			sgetN(s, "owned_since"))
		if err != nil {
			return fmt.Errorf("import server %q: %w", sget(s, "hostname"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(s); old > 0 {
			imp.idMaps[1][old] = newID
		}
		imp.sum.Servers++

		for _, d := range arr(it, "disks") {
			dm, _ := d.(map[string]any)
			size := int64(fget(dm, "size_as_mb"))
			if size <= 0 {
				continue
			}
			media := sget(dm, "media")
			if media == "" {
				media = "SSD"
			}
			if _, err := imp.tx.ExecContext(ctx,
				"INSERT INTO server_disks (server_id, size_as_mb, media) VALUES (?, ?, ?)",
				newID, size, media); err != nil {
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
func (imp *importer) insertIP(ctx context.Context, im map[string]any, serviceID int64, serviceType int) error {
	addr := sget(im, "address")
	if addr == "" {
		return nil
	}
	key := fmt.Sprintf("%d:%d:%s", serviceType, serviceID, addr)
	if imp.seenIPs[key] {
		return nil
	}
	imp.seenIPs[key] = true
	v4 := 1
	// "is_i_pv4" is the legacy export's key (camelToSnake quirk from an
	// older exporter); "is_ipv4" is the current one. Accept both.
	if b, ok := im["is_i_pv4"].(bool); ok && !b {
		v4 = 0
	}
	if b, ok := im["is_ipv4"].(bool); ok && !b {
		v4 = 0
	}
	_, err := imp.tx.ExecContext(ctx, `
		INSERT INTO ips (service_id, service_type, address, is_ipv4, country, region, city, org, isp, asn, fetched_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		serviceID, serviceType, addr, v4,
		sgetN(im, "country"), sgetN(im, "region"), sgetN(im, "city"),
		sgetN(im, "org"), sgetN(im, "isp"), sgetN(im, "asn"), sgetN(im, "fetched_at"))
	if err == nil {
		imp.sum.IPs++
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
			nget(h, "domains_limit"), nget(h, "subdomains_limit"), nget(h, "ftp_limit"),
			nget(h, "email_limit"), nget(h, "db_limit"), nget(h, "disk_as_mb"),
			nget(h, "bandwidth_as_mb"), bint(bget(h, "has_dedicated_ip")), sgetN(h, "ip"),
			bint(bget(h, "active")), bint(bget(h, "show_public")),
			bint(bget(h, "was_promo")), sgetN(h, "owned_since"))
		if err != nil {
			return fmt.Errorf("import %s %q: %w", table, sget(h, "main_domain"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(h); old > 0 {
			imp.idMaps[serviceType][old] = newID
		}
		*count++
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
			nget(b, "port_speed"), nget(b, "disk_as_mb"), nget(b, "bandwidth_as_mb"),
			bint(bget(b, "active")), bint(bget(b, "show_public")),
			bint(bget(b, "was_promo")), sgetN(b, "owned_since"))
		if err != nil {
			return fmt.Errorf("import seedbox %q: %w", sget(b, "hostname"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(b); old > 0 {
			imp.idMaps[6][old] = newID
		}
		imp.sum.Seedboxes++
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
			bint(bget(d, "active")), sgetN(d, "owned_since"))
		if err != nil {
			return fmt.Errorf("import domain %q: %w", sget(d, "domain"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(d); old > 0 {
			imp.idMaps[4][old] = newID
		}
		imp.sum.Domains++
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
			sget(m, "name"), bint(bget(m, "active")), sgetN(m, "owned_since"))
		if err != nil {
			return fmt.Errorf("import misc %q: %w", sget(m, "name"), err)
		}
		newID, _ := res.LastInsertId()
		if old := oldID(m); old > 0 {
			imp.idMaps[5][old] = newID
		}
		imp.sum.Misc++
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
		if _, created, err := getOrCreateCatalogTx(ctx, imp.tx, "labels", "label", name); err != nil {
			return err
		} else if created {
			imp.sum.Labels++
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
			return sql.NullInt64{}
		}
		dnsType := sget(d, "dns_type")
		if dnsType == "" {
			dnsType = "A"
		}
		if _, err := imp.tx.ExecContext(ctx, `
			INSERT INTO dns (hostname, dns_type, address, server_id, domain_id, shared_id, reseller_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sget(d, "hostname"), dnsType, sget(d, "address"),
			remap("server_id", 1), remap("domain_id", 4), remap("shared_id", 2), remap("reseller_id", 3)); err != nil {
			return fmt.Errorf("import dns %q: %w", sget(d, "hostname"), err)
		}
		imp.sum.DNS++
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
		if _, err := imp.tx.ExecContext(ctx,
			"INSERT INTO notes (service_id, service_type, body) VALUES (?, ?, ?)",
			newService, serviceType, body); err != nil {
			return err
		}
		imp.sum.Notes++
	}
	return nil
}
