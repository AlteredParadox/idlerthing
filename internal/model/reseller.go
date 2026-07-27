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
