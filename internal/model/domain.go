package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Domain mirrors the domains table.
type Domain struct {
	ID         int64
	Domain     string
	Extension  sql.NullString
	Ns1        sql.NullString
	Ns2        sql.NullString
	Ns3        sql.NullString
	ProviderID sql.NullInt64
	Active     bool
	OwnedSince sql.NullString
	CreatedAt  string
	UpdatedAt  string
}

const domainColumns = `id, domain, extension, ns1, ns2, ns3, provider_id,
	active, owned_since, created_at, updated_at`

func scanDomain(row interface{ Scan(...any) error }) (*Domain, error) {
	var d Domain
	var active int
	err := row.Scan(&d.ID, &d.Domain, &d.Extension, &d.Ns1, &d.Ns2, &d.Ns3,
		&d.ProviderID, &active, &d.OwnedSince, &d.CreatedAt, &d.UpdatedAt)
	if err != nil {
		return nil, err
	}
	d.Active = active != 0
	return &d, nil
}

// DomainStore wraps the DB for domain queries.
type DomainStore struct{ DB *sql.DB }

// Create inserts a domain plus optional pricing in one transaction.
func (st *DomainStore) Create(ctx context.Context, d *Domain, pricing *Pricing) (int64, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO domains (domain, extension, ns1, ns2, ns3, provider_id, active, owned_since)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.Domain, d.Extension, d.Ns1, d.Ns2, d.Ns3, d.ProviderID,
		boolToInt(d.Active), d.OwnedSince)
	if err != nil {
		return 0, fmt.Errorf("insert domain: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := upsertPricingTx(ctx, tx, ServiceDomain, id, pricing); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Get returns one domain with its pricing.
func (st *DomainStore) Get(ctx context.Context, id int64) (*Domain, *Pricing, error) {
	d, err := scanDomain(st.DB.QueryRowContext(ctx,
		"SELECT "+domainColumns+" FROM domains WHERE id = ?", id))
	if err != nil {
		return nil, nil, err
	}
	pricing, err := (&PricingStore{DB: st.DB}).Get(ctx, ServiceDomain, id)
	if err != nil {
		return nil, nil, err
	}
	return d, pricing, nil
}

// Update replaces a domain's fields and pricing in one transaction.
func (st *DomainStore) Update(ctx context.Context, d *Domain, pricing *Pricing) error {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE domains SET domain = ?, extension = ?, ns1 = ?, ns2 = ?, ns3 = ?,
			provider_id = ?, active = ?, owned_since = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`,
		d.Domain, d.Extension, d.Ns1, d.Ns2, d.Ns3, d.ProviderID,
		boolToInt(d.Active), d.OwnedSince, d.ID)
	if err != nil {
		return fmt.Errorf("update domain: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := upsertPricingTx(ctx, tx, ServiceDomain, d.ID, pricing); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete removes a domain plus its polymorphic children.
func (st *DomainStore) Delete(ctx context.Context, id int64) error {
	return deleteServiceTx(ctx, st.DB, "domains", ServiceDomain, id)
}

// DomainListItem is one row of the domain list view.
type DomainListItem struct {
	Domain
	ProviderName string
	Pricing      *Pricing
}

var domainSortColumns = map[string]string{
	"domain":   "s.domain COLLATE NOCASE",
	"ext":      "s.extension COLLATE NOCASE",
	"provider": "prov_name COLLATE NOCASE",
	"price":    "pr.price",
	"due":      "pr.next_due_date",
	"owned":    "s.owned_since",
}

// List returns filtered/sorted domains.
func (st *DomainStore) List(ctx context.Context, opts ListOptions) ([]DomainListItem, error) {
	var where []string
	var args []any
	switch opts.Status {
	case "inactive":
		where = append(where, "s.active = 0")
	case "all":
	default:
		where = append(where, "s.active = 1")
	}
	if opts.Q != "" {
		like := "%" + opts.Q + "%"
		where = append(where, "(s.domain LIKE ? OR s.extension LIKE ? OR prov_name LIKE ?)")
		args = append(args, like, like, like)
	}

	orderBy := domainSortColumns["domain"]
	if col, ok := domainSortColumns[opts.Sort]; ok {
		orderBy = col
	}
	if opts.Dir == "desc" {
		orderBy += " DESC"
	}

	query := `
		SELECT ` + prefixedColumns("s", domainColumns) + `,
			COALESCE(prov_name, ''),
			pr.currency, pr.price, pr.term, pr.next_due_date
		FROM (
			SELECT s.*, p.name AS prov_name
			FROM domains s
			LEFT JOIN providers p ON p.id = s.provider_id
		) s
		LEFT JOIN pricings pr ON pr.service_id = s.id AND pr.service_type = 4 AND pr.active = 1`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + orderBy

	rows, err := st.DB.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DomainListItem
	for rows.Next() {
		var it DomainListItem
		var active int
		var currency sql.NullString
		var price sql.NullFloat64
		var term sql.NullInt64
		var due sql.NullString
		err := rows.Scan(&it.ID, &it.Domain.Domain, &it.Extension, &it.Ns1, &it.Ns2,
			&it.Ns3, &it.ProviderID, &active, &it.OwnedSince, &it.CreatedAt,
			&it.UpdatedAt, &it.ProviderName, &currency, &price, &term, &due)
		if err != nil {
			return nil, err
		}
		it.Active = active != 0
		if currency.Valid {
			it.Pricing = &Pricing{
				ServiceID:   it.ID,
				ServiceType: ServiceDomain,
				Currency:    currency.String,
				Price:       price.Float64,
				Term:        int(term.Int64),
				NextDueDate: due,
				Active:      true,
			}
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// StatusCounts returns active and inactive domain counts.
func (st *DomainStore) StatusCounts(ctx context.Context) (active, inactive int, err error) {
	err = st.DB.QueryRowContext(ctx,
		"SELECT COALESCE(SUM(active = 1), 0), COALESCE(SUM(active = 0), 0) FROM domains").
		Scan(&active, &inactive)
	return active, inactive, err
}

// DistinctProviders returns the number of distinct providers used by domains.
func (st *DomainStore) DistinctProviders(ctx context.Context) (int, error) {
	var n int
	err := st.DB.QueryRowContext(ctx,
		"SELECT COUNT(DISTINCT provider_id) FROM domains WHERE provider_id IS NOT NULL").Scan(&n)
	return n, err
}
