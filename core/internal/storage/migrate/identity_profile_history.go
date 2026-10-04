package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The fork shipped identity profiles as version 48 before upstream assigned that
// version to answer timing. Reconcile only this known collision, in Up's existing
// transaction. All other migration history mismatches still fail closed.
func (runner *Runner) reconcileIdentityProfileHistory(ctx context.Context, transaction Transaction) error {
	var timing *Migration
	var profiles bool
	for index := range runner.migrations {
		migration := &runner.migrations[index]
		if migration.Version == 48 && migration.Name == "request_first_answer_timing" {
			timing = migration
		}
		if migration.Version == 49 && migration.Name == "service_identity_profiles" {
			profiles = true
		}
	}
	if timing == nil || !profiles {
		return nil
	}
	var name string
	err := transaction.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version = ?`, int64(48)).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // verifyHistory reports the missing version.
	}
	if err != nil {
		return fmt.Errorf("read legacy identity migration: %w", err)
	}
	if name != "service_identity_profiles" {
		return nil
	}
	var columns int
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('service_identity_profiles') WHERE name IN ('id', 'service_id', 'document_json')`).Scan(&columns); err != nil {
		return fmt.Errorf("check legacy identity profile schema: %w", err)
	}
	if columns != 3 {
		return fmt.Errorf("%w: legacy identity migration 48 has an incomplete profile table", ErrMigrationHistory)
	}
	var answerColumn int
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('request_records') WHERE name = 'first_answer_ms'`).Scan(&answerColumn); err != nil {
		return fmt.Errorf("check legacy answer timing schema: %w", err)
	}
	if answerColumn == 0 {
		for _, statement := range timing.Statements {
			if _, err := transaction.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("reconcile legacy answer timing: %w", err)
			}
		}
	}
	// Record version 48 only after its upstream schema is present. Version 49
	// adopts the existing profile table without changing any profile rows.
	if _, err := transaction.ExecContext(ctx, `UPDATE schema_migrations SET name = ? WHERE version = ? AND name = ?`, timing.Name, int64(48), name); err != nil {
		return fmt.Errorf("reconcile legacy identity migration history: %w", err)
	}
	return nil
}
