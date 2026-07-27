package model

import (
	"context"
	"fmt"
	"sync"
	"testing"
)

// Batch O #6 — FindOrCreate matches case-insensitively, and a create race
// (lookup miss → INSERT conflict) returns the existing row, never an error.
func TestFindOrCreateCaseAndRace(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &LabelStore{DB: database}

	id1, err := st.FindOrCreate(ctx, "OVH")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.FindOrCreate(ctx, "ovh")
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("OVH/ovh must resolve to one row, got %d and %d", id1, id2)
	}
	var n int
	database.QueryRow("SELECT COUNT(*) FROM labels").Scan(&n)
	if n != 1 {
		t.Fatalf("expected 1 label row, got %d", n)
	}

	// Concurrent creators on the single connection interleave lookup/insert
	// — the conflict path must re-select, not error.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		name := fmt.Sprintf("race-%02d", i)
		for g := 0; g < 4; g++ {
			wg.Add(1)
			go func(name string) {
				defer wg.Done()
				if _, err := st.FindOrCreate(ctx, name); err != nil {
					t.Errorf("FindOrCreate(%q): %v", name, err)
				}
			}(name)
		}
	}
	wg.Wait()
	database.QueryRow("SELECT COUNT(*) FROM labels WHERE label LIKE 'race-%'").Scan(&n)
	if n != 20 {
		t.Fatalf("expected 20 race labels (one per name), got %d", n)
	}
}
