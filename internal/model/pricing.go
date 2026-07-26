// Package model provides the data layer: store structs wrapping *sql.DB
// with hand-written queries, one file per entity area.
package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Pricing service types (polymorphic service_id + service_type).
const (
	ServiceServer   = 1
	ServiceShared   = 2
	ServiceReseller = 3
	ServiceDomain   = 4
	ServiceMisc     = 5
	ServiceSeedbox  = 6
)

// ServiceTable maps service types to their table names, for joining
// pricings against the parent service. OrderedServiceTypes gives a
// deterministic iteration order.
var ServiceTable = map[int]string{
	ServiceServer:   "servers",
	ServiceShared:   "shared_hosting",
	ServiceReseller: "reseller_hosting",
	ServiceDomain:   "domains",
	ServiceMisc:     "misc_services",
	ServiceSeedbox:  "seedboxes",
}

// OrderedServiceTypes lists all service types in id order.
var OrderedServiceTypes = []int{
	ServiceServer, ServiceShared, ServiceReseller,
	ServiceDomain, ServiceMisc, ServiceSeedbox,
}

// PricingActiveServiceSQL returns a SQL predicate (on pricings alias a)
// that is true only when the parent service row exists and is active.
func PricingActiveServiceSQL() string {
	var parts []string
	for _, st := range OrderedServiceTypes {
		parts = append(parts, fmt.Sprintf(
			"(a.service_type = %d AND EXISTS (SELECT 1 FROM %s svc WHERE svc.id = a.service_id AND svc.active = 1))",
			st, ServiceTable[st]))
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}

// Pricing terms.
const (
	TermMonthly    = 1
	TermQuarterly  = 2
	TermSemiannual = 3
	TermAnnual     = 4
	TermBiennial   = 5
	TermTriennial  = 6
	TermOneTime    = 7
)

// Pricing is a price attached to a service.
type Pricing struct {
	ID          int64
	ServiceID   int64
	ServiceType int
	Currency    string
	Price       float64
	Term        int
	NextDueDate sql.NullString
	Active      bool
}

// TermMonths returns the billing cycle length in months, or 0 for one-time.
func TermMonths(term int) int {
	switch term {
	case TermMonthly:
		return 1
	case TermQuarterly:
		return 3
	case TermSemiannual:
		return 6
	case TermAnnual:
		return 12
	case TermBiennial:
		return 24
	case TermTriennial:
		return 36
	default:
		return 0
	}
}

// TermLabel returns the human label for a term.
func TermLabel(term int) string {
	switch term {
	case TermMonthly:
		return "Monthly"
	case TermQuarterly:
		return "Quarterly"
	case TermSemiannual:
		return "Semi-Annual"
	case TermAnnual:
		return "Annual"
	case TermBiennial:
		return "Biennial"
	case TermTriennial:
		return "Triennial"
	case TermOneTime:
		return "One-time"
	default:
		return "Unknown"
	}
}

// TermAbbrev returns the short suffix for price display, e.g. "/mo".
func TermAbbrev(term int) string {
	switch term {
	case TermMonthly:
		return "/mo"
	case TermQuarterly:
		return "/qtr"
	case TermSemiannual:
		return "/6mo"
	case TermAnnual:
		return "/yr"
	case TermBiennial:
		return "/2yr"
	case TermTriennial:
		return "/3yr"
	case TermOneTime:
		return " once"
	default:
		return ""
	}
}

// PricingStore wraps the DB for pricing queries.
type PricingStore struct {
	DB *sql.DB
}

// Get returns the CURRENT pricing for a service, or nil when none exists.
// Inactive pricings are archives — the app only ever reads/writes current
// pricing, so archived rows are invisible here. Saving (upsert) still
// reactivates per the documented "saving attaches current pricing" rule.
func (s *PricingStore) Get(ctx context.Context, serviceType int, serviceID int64) (*Pricing, error) {
	p := &Pricing{}
	var active int
	err := s.DB.QueryRowContext(ctx, `
		SELECT id, service_id, service_type, currency, price, term, next_due_date, active
		FROM pricings WHERE service_type = ? AND service_id = ? AND active = 1`, serviceType, serviceID).
		Scan(&p.ID, &p.ServiceID, &p.ServiceType, &p.Currency, &p.Price, &p.Term, &p.NextDueDate, &active)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Active = active != 0
	return p, nil
}

// upsertPricingTx inserts or updates the pricing for a service within a
// transaction. A nil pricing removes any existing row instead.
func upsertPricingTx(ctx context.Context, tx *sql.Tx, serviceType int, serviceID int64, p *Pricing) error {
	if p == nil || p.Currency == "" {
		_, err := tx.ExecContext(ctx,
			"DELETE FROM pricings WHERE service_type = ? AND service_id = ?",
			serviceType, serviceID)
		return err
	}
	p.ServiceType = serviceType
	p.ServiceID = serviceID
	if p.Term < TermMonthly || p.Term > TermOneTime {
		return fmt.Errorf("invalid pricing term %d", p.Term)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO pricings (service_id, service_type, currency, price, term, next_due_date)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (service_id, service_type) DO UPDATE SET
			currency = excluded.currency,
			price = excluded.price,
			term = excluded.term,
			next_due_date = excluded.next_due_date,
			active = 1,
			updated_at = CURRENT_TIMESTAMP`,
		p.ServiceID, p.ServiceType, p.Currency, p.Price, p.Term, p.NextDueDate)
	return err
}
