package migrate

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// Look migrations up by name. The fork's profile step has already been
// renumbered twice by upstream claiming its version, and index-based fixtures
// silently seeded a different migration each time that happened.
func migrationNamed(t *testing.T, migrations []Migration, name string) Migration {
	t.Helper()
	for _, migration := range migrations {
		if migration.Name == name {
			return migration
		}
	}
	t.Fatalf("migration %q is missing", name)
	return Migration{}
}

// migrationsBelow returns every step before the named one, which is where a
// fixture starts from when it replaces that step with a legacy variant.
func migrationsBelow(t *testing.T, migrations []Migration, name string) []Migration {
	t.Helper()
	target := migrationNamed(t, migrations, name)
	var seed []Migration
	for _, migration := range migrations {
		if migration.Version >= target.Version {
			break
		}
		seed = append(seed, migration)
	}
	return seed
}

const (
	profilesStep = "service_identity_profiles"
	timingStep   = "request_first_answer_timing"
	chunksStep   = "privacy_token_kinds_audit_chunks"
)

// identityHistoryDatabase builds a database in one of the historical shapes the
// reconciliation has to converge:
//
//	fresh              empty
//	previous           upstream, stopped before the first claimed version
//	upstream_timing    upstream through 48
//	upstream_chunks    upstream through 49, never carried fork profiles
//	fork_48            fork profiles recorded at 48, upstream's first claim
//	fork_49            fork profiles recorded at 49, upstream's second claim
//	fork_48_applied    profiles at 48, upstream's 48 schema already present
//	fork_49_applied    profiles at 49, upstream's 49 schema already present
//	unknown            an unrecognized fork step at 48; must fail closed
//	fork_missing_table profiles recorded at 48 without their table
func identityHistoryDatabase(t *testing.T, kind string) *sql.DB {
	t.Helper()
	database, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "history.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	database.SetMaxOpenConns(1)
	if kind == "fresh" {
		return database
	}
	migrations := DefaultMigrations()
	profiles := migrationNamed(t, migrations, profilesStep)
	timing := migrationNamed(t, migrations, timingStep)
	chunks := migrationNamed(t, migrations, chunksStep)
	seed := migrationsBelow(t, migrations, timingStep)

	// The legacy step keeps the fork's name and statements at the version
	// upstream later claimed.
	legacyAt := func(version int64, dropStatements bool) Migration {
		legacy := profiles
		legacy.Version = version
		if dropStatements {
			legacy.Statements = nil
		}
		return legacy
	}
	var seeded bool
	switch kind {
	case "previous":
	case "upstream_timing":
		seed = append(seed, timing)
	case "upstream_chunks":
		seed = append(seed, timing, chunks)
	case "fork_48", "fork_48_applied":
		seed = append(seed, legacyAt(timing.Version, false))
		seeded = true
	case "fork_missing_table":
		seed = append(seed, legacyAt(timing.Version, true))
	case "fork_49", "fork_49_applied":
		// The first reconciliation already handed 48 back to upstream.
		seed = append(seed, timing, legacyAt(chunks.Version, false))
		seeded = true
	case "unknown":
		seed = append(seed, Migration{Version: timing.Version, Name: "unknown_fork_schema"})
	default:
		t.Fatalf("unknown fixture %q", kind)
	}
	runner, err := New(SQLDatabase{DB: database}, seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Up(context.Background()); err != nil {
		t.Fatal(err)
	}
	if seeded {
		if _, err := database.Exec(`INSERT INTO services (id, document_json, created_at, updated_at) VALUES ('service_legacy', '{}', '2026-10-04', '2026-10-04')`); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO service_identity_profiles (id, service_id, document_json) VALUES ('identity_legacy', 'service_legacy', '{"preserve":"original profile"}')`); err != nil {
			t.Fatal(err)
		}
	}
	// Apply the reclaimed schema out of band, so reconciliation meets a
	// database where replaying those statements would fail.
	switch kind {
	case "fork_48_applied":
		for _, statement := range timing.Statements {
			if _, err := database.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	case "fork_49_applied":
		for _, statement := range chunks.Statements {
			if _, err := database.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	}
	return database
}

func TestIdentityProfileHistoryConvergesWithoutLosingProfiles(t *testing.T) {
	kinds := []string{
		"fresh", "previous", "upstream_timing", "upstream_chunks",
		"fork_48", "fork_48_applied", "fork_49", "fork_49_applied",
	}
	for _, kind := range kinds {
		t.Run(kind, func(t *testing.T) {
			database := identityHistoryDatabase(t, kind)
			migrations := DefaultMigrations()
			runner, err := New(SQLDatabase{DB: database}, migrations)
			if err != nil {
				t.Fatal(err)
			}
			if kind != "fresh" {
				if err := runner.RequireCurrent(context.Background()); !errors.Is(err, ErrDatabaseOlder) {
					t.Fatalf("read-only check before upgrade = %v", err)
				}
			}
			// Twice: reconciliation must be idempotent, not only convergent.
			for attempt := range 2 {
				if err := runner.Up(context.Background()); err != nil {
					t.Fatalf("Up attempt %d: %v", attempt, err)
				}
				if err := runner.RequireCurrent(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			// Every claimed version ends up recorded under upstream's name,
			// and the fork's step sits at its current version.
			for _, step := range []string{timingStep, chunksStep, profilesStep} {
				want := migrationNamed(t, migrations, step)
				var name string
				if err := database.QueryRow(`SELECT name FROM schema_migrations WHERE version = ?`, want.Version).Scan(&name); err != nil || name != want.Name {
					t.Fatalf("version %d = %q, %v; want %q", want.Version, name, err, want.Name)
				}
			}
			var columns int
			if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_records') WHERE name = 'first_answer_ms'`).Scan(&columns); err != nil || columns != 1 {
				t.Fatalf("answer timing columns = %d, %v", columns, err)
			}
			var chunkTables int
			if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name IN ('audit_chunks', 'audit_part_chunks')`).Scan(&chunkTables); err != nil || chunkTables != 2 {
				t.Fatalf("audit chunk tables = %d, %v", chunkTables, err)
			}
			if kind == "fork_48" || kind == "fork_48_applied" || kind == "fork_49" || kind == "fork_49_applied" {
				var document string
				if err := database.QueryRow(`SELECT document_json FROM service_identity_profiles WHERE id = 'identity_legacy'`).Scan(&document); err != nil || document != `{"preserve":"original profile"}` {
					t.Fatalf("preserved profile = %q, %v", document, err)
				}
			}
		})
	}
}

func TestIdentityProfileHistoryRejectsUnknownOrIncompleteSchema(t *testing.T) {
	for _, kind := range []string{"unknown", "fork_missing_table"} {
		t.Run(kind, func(t *testing.T) {
			database := identityHistoryDatabase(t, kind)
			timing := migrationNamed(t, DefaultMigrations(), timingStep)
			var before string
			if err := database.QueryRow(`SELECT name FROM schema_migrations WHERE version = ?`, timing.Version).Scan(&before); err != nil {
				t.Fatal(err)
			}
			runner, err := New(SQLDatabase{DB: database}, DefaultMigrations())
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.Up(context.Background()); !errors.Is(err, ErrMigrationHistory) {
				t.Fatalf("Up = %v, want ErrMigrationHistory", err)
			}
			assertLegacyIdentityHistoryUnchanged(t, database, timing.Version, before)
		})
	}
}

func TestIdentityProfileHistoryReconciliationRollsBackWithLaterFailure(t *testing.T) {
	for _, kind := range []string{"fork_48", "fork_49"} {
		t.Run(kind, func(t *testing.T) {
			database := identityHistoryDatabase(t, kind)
			migrations := DefaultMigrations()
			profiles := migrationNamed(t, migrations, profilesStep)
			for index := range migrations {
				if migrations[index].Name == profilesStep {
					migrations[index].Statements = append(
						migrations[index].Statements,
						`INSERT INTO missing_history_test_table VALUES (1)`,
					)
				}
			}
			runner, err := New(SQLDatabase{DB: database}, migrations)
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.Up(context.Background()); err == nil {
				t.Fatal("expected a later migration failure")
			}
			// The reconciliation shares Up's transaction, so a later failure
			// must leave the recorded history and the profiles untouched.
			claimed := profiles.Version - 1
			if kind == "fork_48" {
				claimed = migrationNamed(t, migrations, timingStep).Version
			}
			assertLegacyIdentityHistoryUnchanged(t, database, claimed, profilesStep)
			var document string
			if err := database.QueryRow(`SELECT document_json FROM service_identity_profiles WHERE id = 'identity_legacy'`).Scan(&document); err != nil || document != `{"preserve":"original profile"}` {
				t.Fatalf("profile after rollback = %q, %v", document, err)
			}
		})
	}
}

func assertLegacyIdentityHistoryUnchanged(t *testing.T, database *sql.DB, wantVersion int64, wantName string) {
	t.Helper()
	var version int64
	var name string
	if err := database.QueryRow(`SELECT version, name FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &name); err != nil || version != wantVersion || name != wantName {
		t.Fatalf("history after failure = %d, %q, %v; want %d, %q", version, name, err, wantVersion, wantName)
	}
}
