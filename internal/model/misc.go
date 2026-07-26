package model

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// MiscService mirrors the misc_services table.
type MiscService struct {
	ID         int64
	Name       string
	Active     bool
	OwnedSince sql.NullString
	CreatedAt  string
	UpdatedAt  string
}

const miscColumns = `id, name, active, owned_since, created_at, updated_at`

// MiscStore wraps the DB for misc service queries.
type MiscStore struct{ DB *sql.DB }

// Create inserts a misc service plus optional pricing in one transaction.
func (st *MiscStore) Create(ctx context.Context, m *MiscService, pricing *Pricing) (int64, error) {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO misc_services (name, active, owned_since) VALUES (?, ?, ?)`,
		m.Name, boolToInt(m.Active), m.OwnedSince)
	if err != nil {
		return 0, fmt.Errorf("insert misc service: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if err := upsertPricingTx(ctx, tx, ServiceMisc, id, pricing); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// Get returns one misc service with its pricing.
func (st *MiscStore) Get(ctx context.Context, id int64) (*MiscService, *Pricing, error) {
	var m MiscService
	var active int
	err := QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT "+miscColumns+" FROM misc_services WHERE id = ?", id).
		Scan(&m.ID, &m.Name, &active, &m.OwnedSince, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, nil, err
	}
	m.Active = active != 0
	pricing, err := (&PricingStore{DB: st.DB}).Get(ctx, ServiceMisc, id)
	if err != nil {
		return nil, nil, err
	}
	return &m, pricing, nil
}

// Update replaces a misc service's fields and pricing in one transaction.
func (st *MiscStore) Update(ctx context.Context, m *MiscService, pricing *Pricing) error {
	tx, err := st.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		UPDATE misc_services SET name = ?, active = ?, owned_since = ?,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, m.Name, boolToInt(m.Active), m.OwnedSince, m.ID)
	if err != nil {
		return fmt.Errorf("update misc service: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := upsertPricingTx(ctx, tx, ServiceMisc, m.ID, pricing); err != nil {
		return err
	}
	return tx.Commit()
}

// Delete removes a misc service plus its polymorphic children.
func (st *MiscStore) Delete(ctx context.Context, id int64) error {
	return deleteServiceTx(ctx, st.DB, "misc_services", ServiceMisc, id)
}

// MiscListItem is one row of the misc list view.
type MiscListItem struct {
	MiscService
	Pricing *Pricing
}

var miscSortColumns = map[string]string{
	"name":  "s.name COLLATE NOCASE",
	"price": "pr.price",
	"due":   "pr.next_due_date",
	"owned": "s.owned_since",
}

// List returns filtered/sorted misc services.
func (st *MiscStore) List(ctx context.Context, opts ListOptions) ([]MiscListItem, error) {
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
		where = append(where, "s.name LIKE ? ESCAPE '\\'")
		args = append(args, likePattern(opts.Q))
	}

	orderBy := miscSortColumns["name"]
	if col, ok := miscSortColumns[opts.Sort]; ok {
		orderBy = col
	}
	if opts.Dir == "desc" {
		orderBy += " DESC"
	}

	query := `
		SELECT ` + prefixedColumns("s", miscColumns) + `,
			pr.currency, pr.price, pr.term, pr.next_due_date
		FROM misc_services s
		LEFT JOIN pricings pr ON pr.service_id = s.id AND pr.service_type = 5 AND pr.active = 1`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY " + orderBy

	rows, err := QuerierFrom(ctx, st.DB).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []MiscListItem
	for rows.Next() {
		var it MiscListItem
		var active int
		var currency sql.NullString
		var price sql.NullFloat64
		var term sql.NullInt64
		var due sql.NullString
		err := rows.Scan(&it.ID, &it.Name, &active, &it.OwnedSince,
			&it.CreatedAt, &it.UpdatedAt, &currency, &price, &term, &due)
		if err != nil {
			return nil, err
		}
		it.Active = active != 0
		if currency.Valid {
			it.Pricing = &Pricing{
				ServiceID:   it.ID,
				ServiceType: ServiceMisc,
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

// StatusCounts returns active and inactive misc service counts.
func (st *MiscStore) StatusCounts(ctx context.Context) (active, inactive int, err error) {
	err = QuerierFrom(ctx, st.DB).QueryRowContext(ctx,
		"SELECT COALESCE(SUM(active = 1), 0), COALESCE(SUM(active = 0), 0) FROM misc_services").
		Scan(&active, &inactive)
	return active, inactive, err
}
