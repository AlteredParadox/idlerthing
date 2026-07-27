package model

import (
	"context"
	"database/sql"
)

// Querier is satisfied by both *sql.DB and *sql.Tx, letting read paths run
// either standalone or inside a snapshot transaction.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

type txCtxKey struct{}

// WithTx returns a context carrying tx; read paths honor it (used by the
// export snapshot build).
//
// GUARD: the pool has a single connection, so EVERY read reachable from a
// WithTx caller must go through QuerierFrom — a direct db.Query* would wait
// on the very connection tx holds, deadlocking forever. All model reads are
// routed accordingly; keep it that way when adding new ones.
//
// The same holds for WRITES, doubly so: Exec on st.DB under WithTx would
// deadlock, and Exec on the tx itself would write into a READ-ONLY snapshot
// (failing or, worse, being rolled back). Write methods therefore never
// take a WithTx context. Half-and-half methods like LabelStore.Assign —
// reads via QuerierFrom (safe under a snapshot), writes via s.DB (never
// under one) — are the mandatory shape, not an oversight.
func WithTx(ctx context.Context, tx *sql.Tx) context.Context {
	return context.WithValue(ctx, txCtxKey{}, tx)
}

// QuerierFrom returns the tx from ctx when present, else db.
func QuerierFrom(ctx context.Context, db *sql.DB) Querier {
	if tx, ok := ctx.Value(txCtxKey{}).(*sql.Tx); ok && tx != nil {
		return tx
	}
	return db
}
