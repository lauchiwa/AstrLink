package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// forkCheckinSchemaTables lists every table this build's extension history
// creates. The tests assert on the whole set so a new migration cannot add a
// table that Open silently starts creating.
var forkCheckinSchemaTables = []string{
	forkCheckinLedger,
	forkCheckinAccounts,
	forkCheckinBindings,
	forkCheckinCredentials,
	forkCheckinJobs,
	forkCheckinAttempts,
	forkCheckinReceipts,
	forkCheckinBatches,
	forkCheckinBatchJobs,
}

func forkCheckinLedgerHistory(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(fmt.Sprintf(`SELECT version, name FROM %s ORDER BY version`, forkCheckinLedger))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var history []string
	for rows.Next() {
		var version int64
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			t.Fatal(err)
		}
		history = append(history, fmt.Sprintf("%d:%s", version, name))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return history
}

func expectedForkCheckinHistory() []string {
	var history []string
	for _, migration := range forkCheckinMigrations() {
		history = append(history, fmt.Sprintf("%d:%s", migration.version, migration.name))
	}
	return history
}

// TestForkCheckinEnsureSchemaIsExplicitAndIdempotent covers the fresh-install
// and repeated-enable cases: a second call must not re-run a step or append a
// duplicate ledger row.
func TestForkCheckinEnsureSchemaIsExplicitAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()

	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatalf("first enable: %v", err)
	}
	for _, table := range forkCheckinSchemaTables {
		if !forkCheckinTableExists(t, store, table) {
			t.Fatalf("table %s was not created", table)
		}
	}
	first := forkCheckinLedgerHistory(t, store)
	if want := expectedForkCheckinHistory(); !equalStrings(first, want) {
		t.Fatalf("ledger = %v, want %v", first, want)
	}

	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatalf("second enable: %v", err)
	}
	if second := forkCheckinLedgerHistory(t, store); !equalStrings(second, first) {
		t.Fatalf("repeated enable changed the ledger: %v -> %v", first, second)
	}
	if err := store.RequireForkCheckinSchema(ctx); err != nil {
		t.Fatalf("RequireForkCheckinSchema after enable: %v", err)
	}
	present, err := store.ForkCheckinSchemaPresent(ctx)
	if err != nil || !present {
		t.Fatalf("schema presence = %v, %v", present, err)
	}
}

// TestForkCheckinSchemaIsNotCreatedByOpen is the disabled-extension case: a
// plain Open must leave no extension table, so a core without the extension
// still reads the database.
func TestForkCheckinSchemaIsNotCreatedByOpen(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()

	for _, table := range forkCheckinSchemaTables {
		if forkCheckinTableExists(t, store, table) {
			t.Fatalf("Open created extension table %s", table)
		}
	}
	present, err := store.ForkCheckinSchemaPresent(ctx)
	if err != nil {
		t.Fatalf("schema presence on a disabled extension: %v", err)
	}
	if present {
		t.Fatal("schema reported present without an explicit enable")
	}
	// The status route uses this while disabled; it must report absence
	// rather than creating the schema as a side effect.
	if err := store.RequireForkCheckinSchema(ctx); !errors.Is(err, storagecontract.ErrNotFound) {
		t.Fatalf("RequireForkCheckinSchema = %v, want ErrNotFound", err)
	}
	for _, table := range forkCheckinSchemaTables {
		if forkCheckinTableExists(t, store, table) {
			t.Fatalf("a read created extension table %s", table)
		}
	}
}

// TestForkCheckinPartialMigrationFailureRollsBack proves the whole history is
// one transaction: a step that fails must leave no table and no ledger.
func TestForkCheckinPartialMigrationFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	mainBefore := mainMigrationHistory(t, store)

	broken := forkCheckinMigrations()
	if len(broken) < 3 {
		t.Fatalf("history is too short to test a partial failure: %d", len(broken))
	}
	// Break the third step so the first two have already succeeded inside the
	// transaction when the failure happens.
	broken[2].statements = append([]string{"CREATE TABLE fork_checkin_invalid ("}, broken[2].statements...)

	if err := store.applyForkCheckinMigrations(ctx, broken); err == nil {
		t.Fatal("a broken history was applied without an error")
	}
	for _, table := range forkCheckinSchemaTables {
		if forkCheckinTableExists(t, store, table) {
			t.Fatalf("rolled-back migration left %s behind", table)
		}
	}
	if got := mainMigrationHistory(t, store); !equalStrings(got, mainBefore) {
		t.Fatalf("failed extension migration changed the main history: %v -> %v", mainBefore, got)
	}
	// The main store must still serve after an extension failure.
	if _, err := store.ListServices(ctx, storagecontract.ServiceListOptions{}); err != nil {
		t.Fatalf("main store stopped serving after an extension failure: %v", err)
	}
	// Recovery: the real history still applies cleanly afterwards.
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatalf("enable after a rolled-back failure: %v", err)
	}
	if got := forkCheckinLedgerHistory(t, store); !equalStrings(got, expectedForkCheckinHistory()) {
		t.Fatalf("ledger after recovery = %v", got)
	}
}

// TestForkCheckinRejectsANewerExtensionLedger covers a database written by a
// newer build: only the extension is refused, and the main store is untouched.
func TestForkCheckinRejectsANewerExtensionLedger(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	mainBefore := mainMigrationHistory(t, store)
	mustExec(t, store, fmt.Sprintf(
		`INSERT INTO %s (version, name, applied_at) VALUES (?, 'future_step', 'now')`, forkCheckinLedger),
		ForkCheckinSchemaVersion()+1)

	if err := store.EnsureForkCheckinSchema(ctx); !errors.Is(err, ErrForkCheckinSchemaNewer) {
		t.Fatalf("enable against a newer ledger = %v, want ErrForkCheckinSchemaNewer", err)
	}
	if err := store.RequireForkCheckinSchema(ctx); !errors.Is(err, ErrForkCheckinSchemaNewer) {
		t.Fatalf("check against a newer ledger = %v, want ErrForkCheckinSchemaNewer", err)
	}
	if got := mainMigrationHistory(t, store); !equalStrings(got, mainBefore) {
		t.Fatalf("a newer extension ledger changed the main history: %v -> %v", mainBefore, got)
	}
	if _, err := store.ListServices(ctx, storagecontract.ServiceListOptions{}); err != nil {
		t.Fatalf("main store refused service after an extension version refusal: %v", err)
	}
}

// TestForkCheckinRejectsADivergedExtensionHistory covers a version recorded
// under another name: skipping by version alone would leave the step this
// build owns unapplied.
func TestForkCheckinRejectsADivergedExtensionHistory(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, fmt.Sprintf(`UPDATE %s SET name = 'another_build' WHERE version = 1`, forkCheckinLedger))

	err := store.RequireForkCheckinSchema(ctx)
	if !errors.Is(err, ErrForkCheckinSchemaHistory) {
		t.Fatalf("diverged history = %v, want ErrForkCheckinSchemaHistory", err)
	}
	if !strings.Contains(err.Error(), "another_build") {
		t.Fatalf("error does not name the recorded step: %v", err)
	}
	if err := store.EnsureForkCheckinSchema(ctx); !errors.Is(err, ErrForkCheckinSchemaHistory) {
		t.Fatalf("enable against a diverged history = %v", err)
	}

	// An extra recorded row that this build does not own is also a divergence.
	mustExec(t, store, fmt.Sprintf(`UPDATE %s SET name = 'accounts' WHERE version = 1`, forkCheckinLedger))
	if err := store.RequireForkCheckinSchema(ctx); err != nil {
		t.Fatalf("restored history still refused: %v", err)
	}
	mustExec(t, store, fmt.Sprintf(
		`INSERT INTO %s (version, name, applied_at) VALUES (?, 'stray', 'now')`, forkCheckinLedger), 0)
	if err := store.RequireForkCheckinSchema(ctx); !errors.Is(err, ErrForkCheckinSchemaHistory) {
		t.Fatalf("stray ledger row = %v, want ErrForkCheckinSchemaHistory", err)
	}
}

// TestForkCheckinSchemaRefusesAReadOnlyStore proves a reader cannot create the
// schema: the read-only CLI path must keep working without writing.
func TestForkCheckinSchemaRefusesAReadOnlyStore(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "astrlink.db")
	localKey := testLocalKey(t, 0x71)
	store := openWithKey(t, path, localKey, nil)
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := Open(ctx, path, WithLocalKey(localKey), WithReadOnly())
	if err != nil {
		t.Fatalf("read-only open with extension tables present: %v", err)
	}
	defer reader.Close()
	if err := reader.EnsureForkCheckinSchema(ctx); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("read-only enable = %v, want ErrInvalidArgument", err)
	}
	// Reading the schema state is still allowed.
	if err := reader.RequireForkCheckinSchema(ctx); err != nil {
		t.Fatalf("read-only schema check: %v", err)
	}
	present, err := reader.ForkCheckinSchemaPresent(ctx)
	if err != nil || !present {
		t.Fatalf("read-only presence = %v, %v", present, err)
	}
}

// TestForkCheckinSchemaKeepsMainMigrationHistory proves enabling the extension
// writes nothing to schema_migrations.
func TestForkCheckinSchemaKeepsMainMigrationHistory(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	before := mainMigrationHistory(t, store)

	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if after := mainMigrationHistory(t, store); !equalStrings(after, before) {
		t.Fatalf("main history changed: %v -> %v", before, after)
	}
	// The two ledgers are separate tables; no extension step may appear in
	// the main one.
	for _, entry := range mainMigrationHistory(t, store) {
		if strings.Contains(entry, "fork_checkin") {
			t.Fatalf("extension step recorded in the main history: %s", entry)
		}
	}
}

// TestForkCheckinForeignKeysStayInsideTheExtension proves no extension table
// references an upstream table, so a service delete never cascades here and
// the extension tables can be dropped on their own.
func TestForkCheckinForeignKeysStayInsideTheExtension(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}

	for _, table := range forkCheckinSchemaTables {
		rows, err := store.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%s)`, table))
		if err != nil {
			t.Fatal(err)
		}
		var targets []string
		for rows.Next() {
			var (
				id, sequence                         int
				target, from, to, onUpdate, onDelete string
				match                                string
			)
			if err := rows.Scan(&id, &sequence, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				rows.Close()
				t.Fatal(err)
			}
			targets = append(targets, target)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		rows.Close()
		for _, target := range targets {
			if !strings.HasPrefix(target, "fork_checkin_") {
				t.Fatalf("table %s references %s outside the extension", table, target)
			}
		}
	}

	// No upstream table may reference an extension table either.
	rows, err := store.db.QueryContext(ctx,
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'fork_checkin_%'`)
	if err != nil {
		t.Fatal(err)
	}
	var upstream []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		upstream = append(upstream, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	for _, table := range upstream {
		if strings.HasPrefix(table, "sqlite_") {
			continue
		}
		keys, err := store.db.QueryContext(ctx, fmt.Sprintf(`PRAGMA foreign_key_list(%q)`, table))
		if err != nil {
			t.Fatal(err)
		}
		for keys.Next() {
			var (
				id, sequence                         int
				target, from, to, onUpdate, onDelete string
				match                                string
			)
			if err := keys.Scan(&id, &sequence, &target, &from, &to, &onUpdate, &onDelete, &match); err != nil {
				keys.Close()
				t.Fatal(err)
			}
			if strings.HasPrefix(target, "fork_checkin_") {
				keys.Close()
				t.Fatalf("upstream table %s references extension table %s", table, target)
			}
		}
		if err := keys.Err(); err != nil {
			keys.Close()
			t.Fatal(err)
		}
		keys.Close()
	}
}
