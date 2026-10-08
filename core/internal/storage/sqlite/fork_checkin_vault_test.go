package sqlite

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// forkCheckinSessionMarker is distinctive enough that finding it anywhere in
// the database file proves plaintext was written.
const forkCheckinSessionMarker = "FORKCHECKIN-SESSION-PLAINTEXT-MARKER"

// storeWithForkCheckinAccount returns a store holding one connected account,
// ready to receive a session.
func storeWithForkCheckinAccount(t *testing.T, id forkcheckin.AccountID) *Store {
	t.Helper()
	store := openForkCheckinStore(t)
	account := forkCheckinDraft(id, "https://relay.example/")
	if _, err := store.CreateForkCheckinAccount(context.Background(), account); err != nil {
		t.Fatalf("create account: %v", err)
	}
	return store
}

// readDatabaseBytes returns the database together with its write-ahead log, so
// a value still sitting in the WAL is not missed.
func readDatabaseBytes(t *testing.T, path string) []byte {
	t.Helper()
	var combined []byte
	for _, suffix := range []string{"", "-wal", "-shm"} {
		contents, err := os.ReadFile(path + suffix)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatalf("read %s%s: %v", path, suffix, err)
		}
		combined = append(combined, contents...)
	}
	return combined
}

func TestForkCheckinVaultStoresOnlyCiphertext(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "astrlink.db")
	var logs []string
	store := openWithKey(t, path, testLocalKey(t, 0x61), &logs)
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_seal", "https://relay.example/")); err != nil {
		t.Fatalf("create account: %v", err)
	}

	session := []byte(`{"cookie":"` + forkCheckinSessionMarker + `"}`)
	vault := store.ForkCheckinSessions()
	if err := vault.Put(ctx, "acct_seal", session); err != nil {
		t.Fatalf("Put: %v", err)
	}

	opened, err := vault.Get(ctx, "acct_seal")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(opened, session) {
		t.Fatalf("Get returned %q, want the stored session", opened)
	}

	var stored []byte
	if err := store.db.QueryRowContext(ctx,
		`SELECT sealed_value FROM fork_checkin_credentials WHERE account_id = ?`, "acct_seal",
	).Scan(&stored); err != nil {
		t.Fatalf("read sealed column: %v", err)
	}
	if bytes.Contains(stored, []byte(forkCheckinSessionMarker)) {
		t.Fatal("the stored column contains the session in the clear")
	}

	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if bytes.Contains(readDatabaseBytes(t, path), []byte(forkCheckinSessionMarker)) {
		t.Fatal("the database file contains the session in the clear")
	}
	for _, line := range logs {
		if strings.Contains(line, forkCheckinSessionMarker) {
			t.Fatalf("a log line carried the session: %q", line)
		}
	}
}

func TestForkCheckinVaultRefusesASessionCopiedToAnotherAccount(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	for _, id := range []forkcheckin.AccountID{"acct_owner", "acct_thief"} {
		account := forkCheckinDraft(id, "https://relay-"+string(id)+".example/")
		if _, err := store.CreateForkCheckinAccount(ctx, account); err != nil {
			t.Fatalf("create account %q: %v", id, err)
		}
	}
	vault := store.ForkCheckinSessions()
	if err := vault.Put(ctx, "acct_owner", []byte(`{"cookie":"owner"}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Copy the owner's ciphertext into the other account's row, which is what
	// a stolen database row or a careless migration would do.
	mustExec(t, store, `INSERT INTO fork_checkin_credentials (account_id, sealed_value, updated_at)
SELECT ?, sealed_value, updated_at FROM fork_checkin_credentials WHERE account_id = ?`, "acct_thief", "acct_owner")

	if _, err := vault.Get(ctx, "acct_thief"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("Get on a copied row = %v, want ErrCredentialUnavailable", err)
	}
	// The real owner is unaffected.
	if _, err := vault.Get(ctx, "acct_owner"); err != nil {
		t.Fatalf("the owner lost its session: %v", err)
	}
}

func TestForkCheckinVaultReportsUnavailableAfterTheLocalKeyChanges(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "astrlink.db")
	store := openWithKey(t, path, testLocalKey(t, 0x71), nil)
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}
	if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_key", "https://relay.example/")); err != nil {
		t.Fatalf("create account: %v", err)
	}
	serviceBefore := createForkCheckinService(t, store, "service_alpha")
	var serviceCipherBefore []byte
	if err := store.db.QueryRowContext(ctx,
		`SELECT credential_value FROM service_credentials WHERE service_id = ?`, serviceBefore.Service.ID,
	).Scan(&serviceCipherBefore); err != nil {
		t.Fatalf("read service credential: %v", err)
	}
	if err := store.ForkCheckinSessions().Put(ctx, "acct_key", []byte(`{"cookie":"first"}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	var logs []string
	reopened := openWithKey(t, path, testLocalKey(t, 0x72), &logs)
	t.Cleanup(func() { _ = reopened.Close() })
	vault := reopened.ForkCheckinSessions()

	if _, err := vault.Get(ctx, "acct_key"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("Get under a different key = %v, want ErrCredentialUnavailable", err)
	}
	// The row is still there; only its usability was lost.
	present, err := reopened.HasForkCheckinSession(ctx, "acct_key")
	if err != nil || !present {
		t.Fatalf("HasForkCheckinSession = %v, %v, want true", present, err)
	}
	// The service's own ciphertext is untouched by any of this.
	var serviceCipherAfter []byte
	if err := reopened.db.QueryRowContext(ctx,
		`SELECT credential_value FROM service_credentials WHERE service_id = ?`, serviceBefore.Service.ID,
	).Scan(&serviceCipherAfter); err != nil {
		t.Fatalf("read service credential after reopen: %v", err)
	}
	if !bytes.Equal(serviceCipherBefore, serviceCipherAfter) {
		t.Fatal("the service API key ciphertext changed")
	}

	// Signing in again replaces the unusable session under the current key.
	if err := vault.Put(ctx, "acct_key", []byte(`{"cookie":"second"}`)); err != nil {
		t.Fatalf("Put after key change: %v", err)
	}
	recovered, err := vault.Get(ctx, "acct_key")
	if err != nil {
		t.Fatalf("Get after re-authorizing: %v", err)
	}
	if string(recovered) != `{"cookie":"second"}` {
		t.Fatalf("Get returned %q, want the new session", recovered)
	}
	for _, line := range logs {
		if strings.Contains(line, "cookie") {
			t.Fatalf("a log line carried session material: %q", line)
		}
	}
}

func TestForkCheckinVaultReportsUnavailableForADamagedRow(t *testing.T) {
	ctx := context.Background()
	store := storeWithForkCheckinAccount(t, "acct_damaged")
	vault := store.ForkCheckinSessions()
	if err := vault.Put(ctx, "acct_damaged", []byte(`{"cookie":"intact"}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	mustExec(t, store,
		`UPDATE fork_checkin_credentials SET sealed_value = ? WHERE account_id = ?`,
		[]byte("not a sealed envelope"), "acct_damaged")

	if _, err := vault.Get(ctx, "acct_damaged"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("Get on a damaged row = %v, want ErrCredentialUnavailable", err)
	}
}

func TestForkCheckinVaultDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := storeWithForkCheckinAccount(t, "acct_drop")
	vault := store.ForkCheckinSessions()
	if err := vault.Put(ctx, "acct_drop", []byte(`{"cookie":"gone-soon"}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := vault.Delete(ctx, "acct_drop"); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	// A retried delete must not report a conflict for finished work.
	if err := vault.Delete(ctx, "acct_drop"); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if err := vault.Delete(ctx, "acct_never_existed"); err != nil {
		t.Fatalf("Delete for an unknown account: %v", err)
	}

	if _, err := vault.Get(ctx, "acct_drop"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("Get after Delete = %v, want ErrCredentialUnavailable", err)
	}
	present, err := store.HasForkCheckinSession(ctx, "acct_drop")
	if err != nil || present {
		t.Fatalf("HasForkCheckinSession = %v, %v, want false", present, err)
	}
}

func TestForkCheckinVaultEnforcesItsValueLimits(t *testing.T) {
	ctx := context.Background()
	store := storeWithForkCheckinAccount(t, "acct_limits")
	vault := store.ForkCheckinSessions()

	// An empty session is not a session; Delete is how a caller forgets one.
	for name, value := range map[string][]byte{"nil": nil, "empty": {}} {
		if err := vault.Put(ctx, "acct_limits", value); !errors.Is(err, storagecontract.ErrInvalidArgument) {
			t.Fatalf("Put with a %s value = %v, want ErrInvalidArgument", name, err)
		}
	}

	atLimit := bytes.Repeat([]byte("s"), forkcheckin.MaxCredentialBytes)
	if err := vault.Put(ctx, "acct_limits", atLimit); err != nil {
		t.Fatalf("Put at the limit: %v", err)
	}
	opened, err := vault.Get(ctx, "acct_limits")
	if err != nil || len(opened) != forkcheckin.MaxCredentialBytes {
		t.Fatalf("Get returned %d bytes, %v", len(opened), err)
	}

	tooLarge := bytes.Repeat([]byte("s"), forkcheckin.MaxCredentialBytes+1)
	if err := vault.Put(ctx, "acct_limits", tooLarge); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("Put above the limit = %v, want ErrInvalidArgument", err)
	}
	// The oversized write left the usable session in place.
	if opened, err := vault.Get(ctx, "acct_limits"); err != nil || len(opened) != forkcheckin.MaxCredentialBytes {
		t.Fatalf("the stored session changed: %d bytes, %v", len(opened), err)
	}

	if err := vault.Put(ctx, "Bad ID", []byte("x")); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("Put with an invalid id = %v, want ErrInvalidArgument", err)
	}
	if _, err := vault.Get(ctx, "Bad ID"); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("Get with an invalid id = %v, want ErrInvalidArgument", err)
	}
}

func TestForkCheckinVaultRequiresAnExistingAccount(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	vault := store.ForkCheckinSessions()

	if err := vault.Put(ctx, "acct_absent", []byte(`{"cookie":"orphan"}`)); !errors.Is(err, storagecontract.ErrNotFound) {
		t.Fatalf("Put without an account = %v, want ErrNotFound", err)
	}
	if count := countRows(t, store, "fork_checkin_credentials"); count != 0 {
		t.Fatalf("an orphaned session was written: %d rows", count)
	}
}

func TestForkCheckinVaultSessionGoesWithItsAccount(t *testing.T) {
	ctx := context.Background()
	store := storeWithForkCheckinAccount(t, "acct_cascade")
	vault := store.ForkCheckinSessions()
	if err := vault.Put(ctx, "acct_cascade", []byte(`{"cookie":"bound"}`)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := store.DeleteForkCheckinAccount(ctx, "acct_cascade", 1); err != nil {
		t.Fatalf("delete account: %v", err)
	}
	if count := countRows(t, store, "fork_checkin_credentials"); count != 0 {
		t.Fatalf("the session outlived its account: %d rows", count)
	}
	if _, err := vault.Get(ctx, "acct_cascade"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("Get after the account was deleted = %v, want ErrCredentialUnavailable", err)
	}
}

func TestForkCheckinVaultReportsUnavailableWithoutTheExtensionSchema(t *testing.T) {
	ctx := context.Background()
	// A store that never enabled the extension has no fork_checkin_* table.
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	t.Cleanup(func() { _ = store.Close() })
	vault := store.ForkCheckinSessions()

	if _, err := vault.Get(ctx, "acct_disabled"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("Get with the extension off = %v, want ErrCredentialUnavailable", err)
	}
	if err := vault.Delete(ctx, "acct_disabled"); err != nil {
		t.Fatalf("Delete with the extension off: %v", err)
	}
	present, err := store.HasForkCheckinSession(ctx, "acct_disabled")
	if err != nil || present {
		t.Fatalf("HasForkCheckinSession = %v, %v, want false and no error", present, err)
	}
	if err := vault.Put(ctx, "acct_disabled", []byte("x")); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("Put with the extension off = %v, want ErrCredentialUnavailable", err)
	}
	// Reading a session must not have created the extension schema.
	if present, err := store.ForkCheckinSchemaPresent(ctx); err != nil || present {
		t.Fatalf("ForkCheckinSchemaPresent = %v, %v, want false", present, err)
	}
}
