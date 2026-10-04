package migrate

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

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
	seed := append([]Migration(nil), migrations[:47]...)
	switch kind {
	case "upstream":
		seed = append(seed, migrations[47])
	case "fork", "fork_with_timing", "fork_missing_table":
		legacy := migrations[48]
		legacy.Version = 48
		if kind == "fork_missing_table" {
			legacy.Statements = nil
		}
		seed = append(seed, legacy)
	case "unknown":
		seed = append(seed, Migration{Version: 48, Name: "unknown_fork_schema"})
	case "previous":
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
	if kind == "fork" || kind == "fork_with_timing" {
		if _, err := database.Exec(`INSERT INTO services (id, document_json, created_at, updated_at) VALUES ('service_legacy', '{}', '2026-10-04', '2026-10-04')`); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO service_identity_profiles (id, service_id, document_json) VALUES ('identity_legacy', 'service_legacy', '{"preserve":"original profile"}')`); err != nil {
			t.Fatal(err)
		}
	}
	if kind == "fork_with_timing" {
		if _, err := database.Exec(migrations[47].Statements[0]); err != nil {
			t.Fatal(err)
		}
	}
	return database
}

func TestIdentityProfileHistoryConvergesWithoutLosingProfiles(t *testing.T) {
	for _, kind := range []string{"fresh", "previous", "upstream", "fork", "fork_with_timing"} {
		t.Run(kind, func(t *testing.T) {
			database := identityHistoryDatabase(t, kind)
			runner, err := New(SQLDatabase{DB: database}, DefaultMigrations())
			if err != nil {
				t.Fatal(err)
			}
			if kind != "fresh" {
				if err := runner.RequireCurrent(context.Background()); !errors.Is(err, ErrDatabaseOlder) {
					t.Fatalf("read-only check before upgrade = %v", err)
				}
			}
			for attempt := range 2 {
				if err := runner.Up(context.Background()); err != nil {
					t.Fatalf("Up attempt %d: %v", attempt, err)
				}
				if err := runner.RequireCurrent(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var name string
			if err := database.QueryRow(`SELECT name FROM schema_migrations WHERE version = 48`).Scan(&name); err != nil || name != "request_first_answer_timing" {
				t.Fatalf("version 48 = %q, %v", name, err)
			}
			if err := database.QueryRow(`SELECT name FROM schema_migrations WHERE version = 49`).Scan(&name); err != nil || name != "service_identity_profiles" {
				t.Fatalf("version 49 = %q, %v", name, err)
			}
			var columns int
			if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_records') WHERE name = 'first_answer_ms'`).Scan(&columns); err != nil || columns != 1 {
				t.Fatalf("answer timing columns = %d, %v", columns, err)
			}
			if kind == "fork" || kind == "fork_with_timing" {
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
			var before string
			if err := database.QueryRow(`SELECT name FROM schema_migrations WHERE version = 48`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			runner, err := New(SQLDatabase{DB: database}, DefaultMigrations())
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.Up(context.Background()); !errors.Is(err, ErrMigrationHistory) {
				t.Fatalf("Up = %v, want ErrMigrationHistory", err)
			}
			assertLegacyIdentityHistoryUnchanged(t, database, before)
		})
	}
}

func TestIdentityProfileHistoryReconciliationRollsBackWithLaterFailure(t *testing.T) {
	database := identityHistoryDatabase(t, "fork")
	migrations := DefaultMigrations()
	migrations[48].Statements = append(migrations[48].Statements, `INSERT INTO missing_history_test_table VALUES (1)`)
	runner, err := New(SQLDatabase{DB: database}, migrations)
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Up(context.Background()); err == nil {
		t.Fatal("expected a later migration failure")
	}
	assertLegacyIdentityHistoryUnchanged(t, database, "service_identity_profiles")
	var document string
	if err := database.QueryRow(`SELECT document_json FROM service_identity_profiles WHERE id = 'identity_legacy'`).Scan(&document); err != nil || document != `{"preserve":"original profile"}` {
		t.Fatalf("profile after rollback = %q, %v", document, err)
	}
}

func assertLegacyIdentityHistoryUnchanged(t *testing.T, database *sql.DB, wantName string) {
	t.Helper()
	var version int64
	var name string
	if err := database.QueryRow(`SELECT version, name FROM schema_migrations ORDER BY version DESC LIMIT 1`).Scan(&version, &name); err != nil || version != 48 || name != wantName {
		t.Fatalf("history after failure = %d, %q, %v", version, name, err)
	}
	var columns int
	if err := database.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('request_records') WHERE name = 'first_answer_ms'`).Scan(&columns); err != nil || columns != 0 {
		t.Fatalf("answer timing columns after failure = %d, %v", columns, err)
	}
}
