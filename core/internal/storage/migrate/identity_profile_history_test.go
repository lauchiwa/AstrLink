package migrate

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Look migrations up by name. The fork's profile step has already been
// renumbered three times by upstream claiming its version, and index-based fixtures
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
	profilesStep   = "service_identity_profiles"
	timingStep     = "request_first_answer_timing"
	chunksStep     = "privacy_token_kinds_audit_chunks"
	configSyncStep = "config_sync"
	ignoredStep    = "request_privacy_ignored"
	standInsStep   = "privacy_stand_ins"
)

// legacyProfileVersionStep names the upstream step each fork fixture recorded
// its profiles in place of. Rollback assertions look it up by name, because
// the profile step's own version says nothing about where it used to sit.
var legacyProfileVersionStep = map[string]string{
	"fork_48": timingStep,
	"fork_49": chunksStep,
	"fork_50": configSyncStep,
}

// configSyncObjects are the 27 schema objects upstream's config_sync creates:
// five tables, one index, and 21 change-log triggers.
var configSyncObjects = []string{
	"config_change_log", "sync_state", "sync_key_material", "sync_tombstone", "sync_baseline",
	"config_change_log_changed_idx",
	"config_change_services", "config_change_services_update", "config_change_services_delete",
	"config_change_service_credentials", "config_change_service_credentials_update", "config_change_service_credentials_delete",
	"config_change_service_proxy_credentials", "config_change_service_proxy_credentials_update", "config_change_service_proxy_credentials_delete",
	"config_change_builtin_tool_credentials", "config_change_builtin_tool_credentials_update", "config_change_builtin_tool_credentials_delete",
	"config_change_policies", "config_change_policies_update", "config_change_policies_delete",
	"config_change_privacy_model_installations", "config_change_privacy_model_installations_update", "config_change_privacy_model_installations_delete",
	"config_change_pricing_configs", "config_change_pricing_configs_update", "config_change_pricing_configs_delete",
}

func countConfigSyncObjects(t *testing.T, database *sql.DB) int {
	t.Helper()
	placeholders := strings.TrimSuffix(strings.Repeat("?, ", len(configSyncObjects)), ", ")
	arguments := make([]any, len(configSyncObjects))
	for index, name := range configSyncObjects {
		arguments[index] = name
	}
	var present int
	if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN (`+placeholders+`)`, arguments...).Scan(&present); err != nil {
		t.Fatal(err)
	}
	return present
}

// identityHistoryDatabase builds a database in one of the historical shapes the
// reconciliation has to converge:
//
//	fresh              empty
//	previous           upstream, stopped before the first claimed version
//	upstream_timing    upstream through 48
//	upstream_chunks    upstream through 49, never carried fork profiles
//	upstream_sync      upstream through 50
//	upstream_stand_ins upstream through 52, an upstream user switching over
//	fork_48            fork profiles recorded at 48, upstream's first claim
//	fork_49            fork profiles recorded at 49, upstream's second claim
//	fork_50            fork profiles recorded at 50, upstream's third claim
//	fork_48_applied    profiles at 48, upstream's 48 schema already present
//	fork_49_applied    profiles at 49, upstream's 49 schema already present
//	fork_50_applied    profiles at 50, upstream's 50 schema already present
//	fork_50_partial    profiles at 50, one config_sync object out of band;
//	                   must fail closed
//	fork_50_successor  profiles at 50 followed by a later version no build
//	                   produced; must fail closed
//	unknown            an unrecognized fork step at 48; must fail closed
//	unknown_50         an unrecognized fork step at 50; must fail closed
//	fork_missing_table profiles recorded at 48 without their table
//	fork_50_missing_table
//	                   profiles recorded at 50 without their table
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
	configSync := migrationNamed(t, migrations, configSyncStep)
	ignored := migrationNamed(t, migrations, ignoredStep)
	standIns := migrationNamed(t, migrations, standInsStep)
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
	case "upstream_sync":
		seed = append(seed, timing, chunks, configSync)
	case "upstream_stand_ins":
		seed = append(seed, timing, chunks, configSync, ignored, standIns)
	case "fork_48", "fork_48_applied":
		seed = append(seed, legacyAt(timing.Version, false))
		seeded = true
	case "fork_missing_table":
		seed = append(seed, legacyAt(timing.Version, true))
	case "fork_49", "fork_49_applied":
		// The first reconciliation already handed 48 back to upstream.
		seed = append(seed, timing, legacyAt(chunks.Version, false))
		seeded = true
	case "fork_50", "fork_50_applied", "fork_50_partial":
		// The earlier reconciliations already handed 48 and 49 back.
		seed = append(seed, timing, chunks, legacyAt(configSync.Version, false))
		seeded = true
	case "fork_50_successor":
		seed = append(seed, timing, chunks, legacyAt(configSync.Version, false), Migration{Version: ignored.Version, Name: ignored.Name})
	case "fork_50_missing_table":
		seed = append(seed, timing, chunks, legacyAt(configSync.Version, true))
	case "unknown":
		seed = append(seed, Migration{Version: timing.Version, Name: "unknown_fork_schema"})
	case "unknown_50":
		seed = append(seed, timing, chunks, Migration{Version: configSync.Version, Name: "unknown_fork_schema"})
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
	case "fork_50_applied":
		for _, statement := range configSync.Statements {
			if _, err := database.Exec(statement); err != nil {
				t.Fatal(err)
			}
		}
	case "fork_50_partial":
		// Only the first config_sync object, the shape a single-table probe
		// would mistake for a complete step.
		if _, err := database.Exec(configSync.Statements[0]); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func TestIdentityProfileHistoryConvergesWithoutLosingProfiles(t *testing.T) {
	kinds := []string{
		"fresh", "previous", "upstream_timing", "upstream_chunks",
		"upstream_sync", "upstream_stand_ins",
		"fork_48", "fork_48_applied", "fork_49", "fork_49_applied",
		"fork_50", "fork_50_applied",
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
			for _, step := range []string{timingStep, chunksStep, configSyncStep, ignoredStep, standInsStep, profilesStep} {
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
			if present := countConfigSyncObjects(t, database); present != len(configSyncObjects) {
				t.Fatalf("config_sync objects = %d, want %d", present, len(configSyncObjects))
			}
			var ignoredColumns int
			if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_records') WHERE name = 'privacy_ignored_json'`).Scan(&ignoredColumns); err != nil || ignoredColumns != 1 {
				t.Fatalf("privacy ignored columns = %d, %v", ignoredColumns, err)
			}
			var standInTables int
			if err := database.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = 'privacy_stand_ins'`).Scan(&standInTables); err != nil || standInTables != 1 {
				t.Fatalf("privacy stand-in tables = %d, %v", standInTables, err)
			}
			if strings.HasPrefix(kind, "fork_") {
				var document string
				if err := database.QueryRow(`SELECT document_json FROM service_identity_profiles WHERE id = 'identity_legacy'`).Scan(&document); err != nil || document != `{"preserve":"original profile"}` {
					t.Fatalf("preserved profile = %q, %v", document, err)
				}
				// The reconciled config_sync triggers must be live: a service
				// edit schedules a sync and leaves the profile in place.
				if _, err := database.Exec(`UPDATE services SET updated_at = updated_at WHERE id = 'service_legacy'`); err != nil {
					t.Fatal(err)
				}
				var changes int
				if err := database.QueryRow(`SELECT COUNT(*) FROM config_change_log WHERE entity_kind = 'service' AND entity_id = 'service_legacy'`).Scan(&changes); err != nil || changes == 0 {
					t.Fatalf("service change log rows = %d, %v", changes, err)
				}
				var profiles int
				if err := database.QueryRow(`SELECT COUNT(*) FROM service_identity_profiles WHERE id = 'identity_legacy'`).Scan(&profiles); err != nil || profiles != 1 {
					t.Fatalf("profiles after service edit = %d, %v", profiles, err)
				}
			}
		})
	}
}

func TestIdentityProfileHistoryRejectsUnknownOrIncompleteSchema(t *testing.T) {
	// Each kind names the step whose version holds the rejected record, and
	// the step whose version the history must still end at.
	cases := map[string]struct{ affected, latest string }{
		"unknown":               {timingStep, timingStep},
		"fork_missing_table":    {timingStep, timingStep},
		"unknown_50":            {configSyncStep, configSyncStep},
		"fork_50_missing_table": {configSyncStep, configSyncStep},
		"fork_50_partial":       {configSyncStep, configSyncStep},
		"fork_50_successor":     {configSyncStep, ignoredStep},
	}
	for kind, want := range cases {
		t.Run(kind, func(t *testing.T) {
			database := identityHistoryDatabase(t, kind)
			migrations := DefaultMigrations()
			affected := migrationNamed(t, migrations, want.affected)
			latest := migrationNamed(t, migrations, want.latest)
			var before, latestName string
			if err := database.QueryRow(`SELECT name FROM schema_migrations WHERE version = ?`, affected.Version).Scan(&before); err != nil {
				t.Fatal(err)
			}
			if err := database.QueryRow(`SELECT name FROM schema_migrations WHERE version = ?`, latest.Version).Scan(&latestName); err != nil {
				t.Fatal(err)
			}
			objectsBefore := countConfigSyncObjects(t, database)
			runner, err := New(SQLDatabase{DB: database}, migrations)
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.Up(context.Background()); !errors.Is(err, ErrMigrationHistory) {
				t.Fatalf("Up = %v, want ErrMigrationHistory", err)
			}
			var after string
			if err := database.QueryRow(`SELECT name FROM schema_migrations WHERE version = ?`, affected.Version).Scan(&after); err != nil || after != before {
				t.Fatalf("version %d after rejection = %q, %v; want %q", affected.Version, after, err, before)
			}
			assertLegacyIdentityHistoryUnchanged(t, database, latest.Version, latestName)
			// Nothing was replayed: a partial config_sync stays partial.
			if objectsAfter := countConfigSyncObjects(t, database); objectsAfter != objectsBefore {
				t.Fatalf("config_sync objects after rejection = %d, want %d", objectsAfter, objectsBefore)
			}
		})
	}
}

func TestIdentityProfileHistoryReconciliationRollsBackWithLaterFailure(t *testing.T) {
	for _, kind := range []string{"fork_48", "fork_49", "fork_50"} {
		t.Run(kind, func(t *testing.T) {
			database := identityHistoryDatabase(t, kind)
			migrations := DefaultMigrations()
			objectsBefore := countConfigSyncObjects(t, database)
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
			claimed := migrationNamed(t, migrations, legacyProfileVersionStep[kind]).Version
			assertLegacyIdentityHistoryUnchanged(t, database, claimed, profilesStep)
			// fork_50 had config_sync replayed during reconciliation; the
			// rollback must remove it again.
			if objectsAfter := countConfigSyncObjects(t, database); objectsAfter != objectsBefore {
				t.Fatalf("config_sync objects after rollback = %d, want %d", objectsAfter, objectsBefore)
			}
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
