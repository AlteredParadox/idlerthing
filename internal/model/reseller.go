package model

import (
	"context"
	"database/sql"
)

// ResellerHosting mirrors the reseller_hosting table (same shape as
// SharedHosting, with reseller_type instead of shared_type).
type ResellerHosting = SharedHosting

// ResellerStore wraps the DB for reseller hosting queries.
type ResellerStore struct{ DB *sql.DB }

func (s *ResellerStore) impl() *hostingStore {
	return &hostingStore{DB: s.DB, table: "reseller_hosting", typeCol: "reseller_type", serviceType: ServiceReseller}
}

func (s *ResellerStore) Create(ctx context.Context, h *ResellerHosting, p *Pricing) (int64, error) {
	return s.impl().create(ctx, h, p)
}
func (s *ResellerStore) Get(ctx context.Context, id int64) (*ResellerHosting, *Pricing, error) {
	return s.impl().get(ctx, id)
}
func (s *ResellerStore) Update(ctx context.Context, h *ResellerHosting, p *Pricing) error {
	return s.impl().update(ctx, h, p)
}
func (s *ResellerStore) Delete(ctx context.Context, id int64) error { return s.impl().delete(ctx, id) }
func (s *ResellerStore) List(ctx context.Context, opts ListOptions) ([]HostingListItem, error) {
	return s.impl().list(ctx, opts)
}
func (s *ResellerStore) StatusCounts(ctx context.Context) (int, int, error) {
	return s.impl().statusCounts(ctx)
}
func (s *ResellerStore) DistinctProviders(ctx context.Context) (int, error) {
	return s.impl().distinctProviders(ctx)
}
