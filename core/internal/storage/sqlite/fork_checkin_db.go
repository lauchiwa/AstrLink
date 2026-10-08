package sqlite

import (
	"context"
	"database/sql"
)

// forkCheckinDB is the only way extension code reaches the shared pool.
//
// modernc.org/sqlite v1.38.2 drops a statement when the query's context is
// cancelled just after the first row is produced: stmt.query discards the rows
// that own the statement without finalizing it. The statement stays active, so
// SQLite cannot reset its interrupt flag, ROLLBACK fails with SQLITE_INTERRUPT,
// and database/sql discards the connection. sqlite3_close_v2 then leaves a
// zombie handle that keeps its transaction, and with it the writer lock on the
// shared store.db, until the process exits. Main Core writes would then wait
// out busy_timeout and fail.
//
// Extension statements are short local work, so they run without
// cancellation. A context that is already done is passed through unchanged:
// database/sql rejects it before the driver sees it, so callers still fail
// fast. Network calls are not affected and remain cancellable.
type forkCheckinDB struct{ pool *sql.DB }

func (store *Store) forkCheckinDB() forkCheckinDB { return forkCheckinDB{pool: store.db} }

func forkCheckinStatementContext(ctx context.Context) context.Context {
	if ctx.Err() != nil {
		return ctx
	}
	return context.WithoutCancel(ctx)
}

func (db forkCheckinDB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.pool.ExecContext(forkCheckinStatementContext(ctx), query, args...)
}

func (db forkCheckinDB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.pool.QueryContext(forkCheckinStatementContext(ctx), query, args...)
}

func (db forkCheckinDB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.pool.QueryRowContext(forkCheckinStatementContext(ctx), query, args...)
}

// BeginTx also returns the context that every statement in the transaction
// must use. Callers rebind their ctx to it, including in closures.
func (db forkCheckinDB) BeginTx(ctx context.Context) (*sql.Tx, context.Context, error) {
	ctx = forkCheckinStatementContext(ctx)
	transaction, err := db.pool.BeginTx(ctx, nil)
	return transaction, ctx, err
}
