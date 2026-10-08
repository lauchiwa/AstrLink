package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

const (
	forkCheckinLedgerTable     = "fork_checkin_schema_migrations"
	forkCheckinAccountTable    = "fork_checkin_accounts"
	forkCheckinCredentialTable = "fork_checkin_credentials"
	forkCheckinLatestVersion   = int64(2)
)

type forkCheckinProbeMigration struct {
	version    int64
	name       string
	statements []string
}

var forkCheckinProbeMigrations = []forkCheckinProbeMigration{
	{
		version: 1,
		name:    "accounts",
		statements: []string{
			fmt.Sprintf(`CREATE TABLE %s (
    id TEXT PRIMARY KEY,
    dashboard_base_url TEXT NOT NULL,
    remote_user_id TEXT,
    revision INTEGER NOT NULL DEFAULT 1
)`, forkCheckinAccountTable),
		},
	},
	{
		version: 2,
		name:    "credentials",
		statements: []string{
			fmt.Sprintf(`CREATE TABLE %s (
    account_id TEXT PRIMARY KEY REFERENCES %s(id) ON DELETE CASCADE,
    credential_value BLOB NOT NULL,
    sealed INTEGER NOT NULL CHECK(sealed = 1)
)`, forkCheckinCredentialTable, forkCheckinAccountTable),
		},
	},
}

func applyForkCheckinProbeMigrations(ctx context.Context, store *Store, failVersion int64) error {
	transaction, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin fork check-in migration: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()

	if _, err := transaction.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`, forkCheckinLedgerTable)); err != nil {
		return fmt.Errorf("create fork check-in ledger: %w", err)
	}
	var current int64
	if err := transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT COALESCE(MAX(version), 0) FROM %s`, forkCheckinLedgerTable)).Scan(&current); err != nil {
		return fmt.Errorf("read fork check-in version: %w", err)
	}
	if current > forkCheckinLatestVersion {
		return fmt.Errorf("fork check-in database version %d is newer than %d", current, forkCheckinLatestVersion)
	}

	for _, migration := range forkCheckinProbeMigrations {
		if migration.version <= current {
			continue
		}
		for index, statement := range migration.statements {
			if migration.version == failVersion && index == 0 {
				statement = "CREATE TABLE fork_checkin_invalid ("
			}
			if _, err := transaction.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply fork check-in migration %d (%s): %w", migration.version, migration.name, err)
			}
		}
		if _, err := transaction.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s (version, name, applied_at) VALUES (?, ?, ?)`, forkCheckinLedgerTable),
			migration.version, migration.name, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("record fork check-in migration %d: %w", migration.version, err)
		}
	}
	if err := transaction.Commit(); err != nil {
		return fmt.Errorf("commit fork check-in migration: %w", err)
	}
	return nil
}

func forkCheckinProbeHistory(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT version, name FROM fork_checkin_schema_migrations ORDER BY version`)
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

func mainMigrationHistory(t *testing.T, store *Store) []string {
	t.Helper()
	rows, err := store.db.Query(`SELECT version, name FROM schema_migrations ORDER BY version`)
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

func forkCheckinTableExists(t *testing.T, store *Store, table string) bool {
	t.Helper()
	var exists bool
	if err := store.db.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, table,
	).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	return exists
}

func TestForkCheckinProbeKeepsMainSchemaAndSealsCredential(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "astrlink.db")
	localKey := testLocalKey(t, 0x61)
	store := openWithKey(t, path, localKey, nil)
	mainBefore := mainMigrationHistory(t, store)
	service := contract.ServiceFromEndpoint(testEndpoint("service_fork_checkin_probe"))
	serviceRecord, err := store.CreateService(ctx, service, storagecontract.CredentialMutation{
		Present: true,
		Secret:  []byte("original-service-credential"),
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := applyForkCheckinProbeMigrations(ctx, store, 0); err != nil {
		t.Fatal(err)
	}
	if got := forkCheckinProbeHistory(t, store); !strings.EqualFold(strings.Join(got, ","), "1:accounts,2:credentials") {
		t.Fatalf("fork check-in history = %v", got)
	}
	accountID := "account_probe"
	credential := []byte(`{"session":"probe-secret","user_id":7}`)
	sealed, err := store.keys.sealColumn(forkCheckinCredentialTable, accountID, credential)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO fork_checkin_accounts (id, dashboard_base_url, remote_user_id) VALUES (?, ?, ?)`,
		accountID, "https://relay.example/base", "7"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx,
		`INSERT INTO fork_checkin_credentials (account_id, credential_value, sealed) VALUES (?, ?, 1)`,
		accountID, sealed); err != nil {
		t.Fatal(err)
	}
	if fileContains(t, path, credential) {
		t.Fatal("fork check-in credential was written in plaintext")
	}
	opened, err := store.keys.openColumn(forkCheckinCredentialTable, accountID, sealed)
	if err != nil || !bytes.Equal(opened, credential) {
		t.Fatalf("open fork check-in credential failed: %v", err)
	}
	clear(opened)
	if _, err := store.keys.openColumn(forkCheckinCredentialTable, "other-account", sealed); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("copied credential opened for another account: %v", err)
	}
	if got := mainMigrationHistory(t, store); !equalStrings(got, mainBefore) {
		t.Fatalf("main migration history changed: before=%v after=%v", mainBefore, got)
	}
	loadedService, err := store.GetService(ctx, service.ID)
	if err != nil || loadedService.ETag != serviceRecord.ETag {
		t.Fatalf("service record changed: %#v, %v", loadedService, err)
	}
	loadedSecret, err := store.Get(ctx, secretstore.Ref("local://service/"+string(serviceRecord.Service.ID)))
	if err != nil || !bytes.Equal(loadedSecret, []byte("original-service-credential")) {
		t.Fatalf("original service credential changed: %q, %v", loadedSecret, err)
	}
	clear(loadedSecret)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reader, err := Open(ctx, path, WithLocalKey(localKey), WithReadOnly())
	if err != nil {
		t.Fatalf("read-only open with fork tables: %v", err)
	}
	if !forkCheckinTableExists(t, reader, forkCheckinCredentialTable) || countRows(t, reader, forkCheckinCredentialTable) != 1 {
		t.Fatal("read-only store could not read fork check-in tables")
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}

	if _, err := Open(ctx, path, WithLocalKey(testLocalKey(t, 0x62)), WithExistingDatabase()); !errors.Is(err, ErrLocalKeyMismatch) {
		t.Fatalf("wrong local key error = %v, want ErrLocalKeyMismatch", err)
	}
	store = openWithKey(t, path, localKey, nil)
	defer store.Close()
	if countRows(t, store, forkCheckinCredentialTable) != 1 {
		t.Fatal("fork check-in data did not survive wrong-key attempt")
	}
}

func equalStrings(left, right []string) bool {
	return strings.Join(left, "\x00") == strings.Join(right, "\x00")
}

func TestForkCheckinProbeRollbackDoesNotTouchMainStore(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	mainBefore := mainMigrationHistory(t, store)
	if err := applyForkCheckinProbeMigrations(ctx, store, 2); err == nil {
		t.Fatal("failed fork check-in migration succeeded")
	}
	if forkCheckinTableExists(t, store, forkCheckinLedgerTable) || forkCheckinTableExists(t, store, forkCheckinAccountTable) {
		t.Fatal("failed fork check-in migration left schema behind")
	}
	if got := mainMigrationHistory(t, store); !equalStrings(got, mainBefore) {
		t.Fatalf("failed fork migration changed main history: before=%v after=%v", mainBefore, got)
	}
	if _, err := store.GetService(ctx, "missing-service"); err == nil {
		t.Fatal("main store stopped serving after fork migration failure")
	}
}

func TestForkCheckinProbeRejectsNewerExtensionWithoutChangingMainStore(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	if err := applyForkCheckinProbeMigrations(ctx, store, 0); err != nil {
		t.Fatal(err)
	}
	mainBefore := mainMigrationHistory(t, store)
	mustExec(t, store, `INSERT INTO fork_checkin_schema_migrations (version, name, applied_at) VALUES (99, 'future', 'now')`)
	if err := applyForkCheckinProbeMigrations(ctx, store, 0); err == nil || !strings.Contains(err.Error(), "newer") {
		t.Fatalf("newer fork migration error = %v", err)
	}
	if got := mainMigrationHistory(t, store); !equalStrings(got, mainBefore) {
		t.Fatalf("newer fork migration changed main history: before=%v after=%v", mainBefore, got)
	}
}
