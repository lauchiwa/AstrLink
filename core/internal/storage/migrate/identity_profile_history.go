package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// The fork shipped identity profiles before upstream assigned the same versions
// to its own steps, twice: 48 went to answer timing, then 49 went to privacy
// token kinds and audit chunks. Profiles therefore moved 48 -> 49 -> 50.
//
// A database carrying either earlier numbering records a name this Core no
// longer expects at that version, which verifyHistory would reject. Reconcile
// only these two known collisions, in Up's existing transaction: apply the
// upstream schema that version is now supposed to carry, then rename the
// recorded name. Version 50 afterwards adopts the existing profile table
// without changing any profile row. All other history mismatches still fail
// closed.
func (runner *Runner) reconcileIdentityProfileHistory(ctx context.Context, transaction Transaction) error {
	var profiles bool
	claimed := map[int64]*Migration{}
	for index := range runner.migrations {
		migration := &runner.migrations[index]
		switch {
		case migration.Version == 48 && migration.Name == "request_first_answer_timing":
			claimed[48] = migration
		case migration.Version == 49 && migration.Name == "privacy_token_kinds_audit_chunks":
			claimed[49] = migration
		case migration.Version == 50 && migration.Name == "service_identity_profiles":
			profiles = true
		}
	}
	if !profiles {
		return nil
	}
	// Ascending: a database at 49 was reconciled through 48 first, so 48 must
	// hold its upstream schema before 49 is touched.
	for _, version := range []int64{48, 49} {
		upstream := claimed[version]
		if upstream == nil {
			continue
		}
		if err := runner.adoptClaimedProfileVersion(ctx, transaction, version, upstream); err != nil {
			return err
		}
	}
	return nil
}

// adoptClaimedProfileVersion hands one version back to upstream when the fork's
// profile step is what is recorded there.
func (runner *Runner) adoptClaimedProfileVersion(
	ctx context.Context,
	transaction Transaction,
	version int64,
	upstream *Migration,
) error {
	var name string
	err := transaction.QueryRowContext(ctx, `SELECT name FROM schema_migrations WHERE version = ?`, version).Scan(&name)
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
		return fmt.Errorf("%w: legacy identity migration %d has an incomplete profile table", ErrMigrationHistory, version)
	}
	applied, err := runner.claimedVersionApplied(ctx, transaction, version)
	if err != nil {
		return err
	}
	if !applied {
		for _, statement := range upstream.Statements {
			if _, err := transaction.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("reconcile legacy migration %d: %w", version, err)
			}
		}
	}
	// Record the version only after its upstream schema is present.
	if _, err := transaction.ExecContext(
		ctx,
		`UPDATE schema_migrations SET name = ? WHERE version = ? AND name = ?`,
		upstream.Name,
		version,
		name,
	); err != nil {
		return fmt.Errorf("reconcile legacy identity migration history: %w", err)
	}
	return nil
}

// claimedVersionApplied reports whether the upstream schema for a reclaimed
// version is already present, so its statements are not replayed. Each probe
// targets a step that is all-or-nothing within its own transaction.
func (runner *Runner) claimedVersionApplied(
	ctx context.Context,
	transaction Transaction,
	version int64,
) (bool, error) {
	var query string
	switch version {
	case 48:
		query = `SELECT COUNT(*) FROM pragma_table_info('request_records') WHERE name = 'first_answer_ms'`
	case 49:
		query = `SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'audit_chunks'`
	default:
		return false, fmt.Errorf("%w: no reconciliation probe for version %d", ErrMigrationHistory, version)
	}
	var present int
	if err := transaction.QueryRowContext(ctx, query).Scan(&present); err != nil {
		return false, fmt.Errorf("check reclaimed schema for version %d: %w", version, err)
	}
	return present > 0, nil
}
