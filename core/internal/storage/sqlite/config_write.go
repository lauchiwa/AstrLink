package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	sqlitedriver "modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"
)

const (
	configWriteMaxAttempts = 8
	configWriteRetryBudget = 5 * time.Second
)

// isSQLiteConstraint reports whether err is a SQLite constraint violation.
func isSQLiteConstraint(err error) bool {
	var failure *sqlitedriver.Error
	return errors.As(err, &failure) && failure.Code()&0xff == sqlite3.SQLITE_CONSTRAINT
}

// retryConfigWrite is only for short, local configuration transactions. In
// WAL mode a deferred read transaction cannot always upgrade to a writer:
// SQLITE_BUSY and SQLITE_BUSY_SNAPSHOT bypass busy_timeout in that case.
// Roll back the whole attempt, then re-read and re-check its CAS on retry.
// apply must not perform network I/O, publish results, or mutate caller-owned
// state. A failed COMMIT is never replayed: its outcome may be ambiguous.
func retryConfigWrite[T any](ctx context.Context, db *sql.DB, apply func(context.Context, *sql.Tx) (T, error)) (T, error) {
	var zero T
	ctx, cancel := context.WithTimeout(ctx, configWriteRetryBudget)
	defer cancel()
	delay := 5 * time.Millisecond
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		tx, err := db.BeginTx(ctx, nil)
		if err == nil {
			// At most eight defers; also release the transaction if apply panics.
			defer tx.Rollback()
			var value T
			value, err = apply(ctx, tx)
			if err == nil && ctx.Err() == nil {
				if err := tx.Commit(); err != nil {
					return zero, fmt.Errorf("commit configuration write: %w", err)
				}
				return value, nil
			}
			rollbackErr := tx.Rollback()
			if ctx.Err() != nil {
				return zero, ctx.Err()
			}
			// Do not retry unless rollback is known to have succeeded.
			if rollbackErr != nil {
				return zero, errors.Join(err, fmt.Errorf("rollback configuration write: %w", rollbackErr))
			}
		} else {
			err = fmt.Errorf("begin configuration write: %w", err)
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		var busy *sqlitedriver.Error
		if attempt >= configWriteMaxAttempts || !errors.As(err, &busy) || busy.Code()&0xff != sqlite3.SQLITE_BUSY {
			return zero, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return zero, ctx.Err()
		case <-timer.C:
		}
		delay = min(delay*2, 100*time.Millisecond)
	}
}
