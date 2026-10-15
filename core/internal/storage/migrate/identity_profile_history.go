package migrate

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// The fork shipped identity profiles before upstream assigned the same versions
// to its own steps, three times: 48 went to answer timing, 49 to privacy token
// kinds and audit chunks, and 50 to config sync. Profiles therefore moved
// 48 -> 49 -> 50 -> 53.
//
// A database carrying any earlier numbering records a name this Core no longer
// expects at that version, which verifyHistory would reject. Reconcile only
// these three known collisions, in Up's existing transaction: apply the
// upstream schema that version is now supposed to carry, then rename the
// recorded name. Version 53 afterwards adopts the existing profile table
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
		case migration.Version == 50 && migration.Name == "config_sync":
			claimed[50] = migration
		case migration.Version == 53 && migration.Name == "service_identity_profiles":
			profiles = true
		}
	}
	if !profiles {
		return nil
	}
	// Ascending: a database at 49 or 50 was reconciled through the lower
	// versions first, so each must hold its upstream schema before the next
	// one is touched.
	for _, version := range []int64{48, 49, 50} {
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
	// Every fork build recorded profiles as its newest step. A later row means
	// a history no known build produced, so it is not a collision to repair.
	var later int
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version > ?`, version).Scan(&later); err != nil {
		return fmt.Errorf("check legacy identity migration successors: %w", err)
	}
	if later != 0 {
		return fmt.Errorf("%w: legacy identity migration %d is followed by later versions", ErrMigrationHistory, version)
	}
	var columns int
	if err := transaction.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('service_identity_profiles') WHERE name IN ('id', 'service_id', 'document_json')`).Scan(&columns); err != nil {
		return fmt.Errorf("check legacy identity profile schema: %w", err)
	}
	if columns != 3 {
		return fmt.Errorf("%w: legacy identity migration %d has an incomplete profile table", ErrMigrationHistory, version)
	}
	applied, err := runner.claimedVersionApplied(ctx, transaction, version, upstream)
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
	upstream *Migration,
) (bool, error) {
	var query string
	switch version {
	case 50:
		return createdObjectsApplied(ctx, transaction, upstream)
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

// createdObjectPattern matches a plain CREATE of a named schema object. A
// statement written with IF NOT EXISTS, or any other statement, does not match,
// so it cannot silently drop out of the probe.
var createdObjectPattern = regexp.MustCompile(`^CREATE (?:TABLE|INDEX|TRIGGER) (\w+)`)

// createdObjectsApplied probes a reclaimed step made only of plain CREATE
// statements, such as config_sync. The expected objects come from the step's
// own statements, so the probe cannot drift from what it would replay. None
// present means replay, all present means only rename, and anything in between
// is an out-of-band partial state that fails closed: replaying would stop at
// the first existing object, and renaming would skip the missing ones.
func createdObjectsApplied(ctx context.Context, transaction Transaction, upstream *Migration) (bool, error) {
	names := make([]string, 0, len(upstream.Statements))
	for _, statement := range upstream.Statements {
		match := createdObjectPattern.FindStringSubmatch(strings.TrimSpace(statement))
		if match == nil {
			return false, fmt.Errorf("%w: migration %d (%s) has a statement the reconciliation cannot probe", ErrMigrationHistory, upstream.Version, upstream.Name)
		}
		names = append(names, match[1])
	}
	if len(names) == 0 {
		return false, fmt.Errorf("%w: migration %d (%s) has no statements to probe", ErrMigrationHistory, upstream.Version, upstream.Name)
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(names)), ", ")
	arguments := make([]any, len(names))
	for index, name := range names {
		arguments[index] = name
	}
	var present int
	if err := transaction.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE name IN (`+placeholders+`)`,
		arguments...,
	).Scan(&present); err != nil {
		return false, fmt.Errorf("check reclaimed schema for version %d: %w", upstream.Version, err)
	}
	switch present {
	case 0:
		return false, nil
	case len(names):
		return true, nil
	default:
		return false, fmt.Errorf("%w: migration %d (%s) is partially applied: %d of %d objects present", ErrMigrationHistory, upstream.Version, upstream.Name, present, len(names))
	}
}
