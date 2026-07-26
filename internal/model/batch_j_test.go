package model

import (
	"context"
	"testing"
)

// Batch J #2 — a nil pricing on update removes only the ACTIVE row;
// archived (inactive) rows survive unrelated edits.
func TestUpdateNilPricingPreservesArchive(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &ServerStore{DB: database}

	id, err := st.Create(ctx, &Server{Hostname: "arch-01", ServerType: TypeKVM, Active: true},
		nil, &Pricing{Currency: "USD", Price: 10, Term: TermMonthly})
	if err != nil {
		t.Fatal(err)
	}
	// Archive the pricing (as an old batch-I row would look).
	if _, err := database.Exec(
		"UPDATE pricings SET active = 0 WHERE service_id = ? AND service_type = 1", id); err != nil {
		t.Fatal(err)
	}

	// Unrelated edit (hostname only, nil pricing): archive survives.
	if err := st.Update(ctx, &Server{ID: id, Hostname: "arch-02", ServerType: TypeKVM, Active: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	var n, active int
	if err := database.QueryRow(
		"SELECT COUNT(*), COALESCE(SUM(active), 0) FROM pricings WHERE service_id = ? AND service_type = 1", id).
		Scan(&n, &active); err != nil {
		t.Fatal(err)
	}
	if n != 1 || active != 0 {
		t.Fatalf("archived pricing must survive an unrelated edit: rows=%d active=%d", n, active)
	}

	// Clearing an ACTIVE pricing removes exactly that row.
	if _, err := database.Exec(
		"UPDATE pricings SET active = 1 WHERE service_id = ? AND service_type = 1", id); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(ctx, &Server{ID: id, Hostname: "arch-03", ServerType: TypeKVM, Active: true}, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := database.QueryRow(
		"SELECT COUNT(*) FROM pricings WHERE service_id = ? AND service_type = 1", id).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("active pricing should be cleared: %d rows left", n)
	}
}
