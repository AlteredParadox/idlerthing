package model

import (
	"context"
	"testing"
)

// Batch K B5 — LIKE wildcards in the search box are escaped: "%" and "_"
// match literally instead of acting as wildcards.
func TestListSearchEscapesLikeWildcards(t *testing.T) {
	database := testDB(t)
	ctx := context.Background()
	st := &ServerStore{DB: database}

	for _, h := range []string{"100%_done", "plain-host", "under_score"} {
		if _, err := st.Create(ctx, &Server{Hostname: h, ServerType: TypeKVM, Active: true}, nil, nil); err != nil {
			t.Fatal(err)
		}
	}

	list := func(q string) []string {
		items, err := st.List(ctx, ListOptions{Status: "all", Q: q})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, it := range items {
			names = append(names, it.Hostname)
		}
		return names
	}

	if got := list("%"); len(got) != 1 || got[0] != "100%_done" {
		t.Fatalf(`search "%%" should match only the literal %% host, got %v`, got)
	}
	got := list("_")
	if len(got) != 2 {
		t.Fatalf(`search "_" should match only literal-underscore hosts, got %v`, got)
	}
	for _, n := range got {
		if n == "plain-host" {
			t.Fatalf(`"_" matched a host without an underscore: %v`, got)
		}
	}
	if got := list("plain"); len(got) != 1 || got[0] != "plain-host" {
		t.Fatalf("ordinary substring search broke: %v", got)
	}
}
