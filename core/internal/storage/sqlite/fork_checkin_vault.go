package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// Sealed session storage for the check-in extension.
//
// This is a narrow adapter over the existing column sealing, not a new
// credential primitive. It adds no entry to secretColumns, introduces no
// secretstore.Ref and no CredentialRef, and is not reachable through
// secretstore.SecretStore: a caller holding the global interface cannot read a
// check-in session through it. Service API keys keep their own table, their
// own additional data and their own ciphertext, untouched by anything here.
//
// The value is opaque. This package never parses a session, so a future site
// dialect cannot change what has to be stored.

// ForkCheckinVault is the extension's session store for one account each.
//
// It satisfies forkcheckin.Vault. The type is kept separate from Store's own
// credential methods so that the extension's sessions cannot be reached from
// the general credential surface.
type ForkCheckinVault struct {
	store *Store
}

// ForkCheckinSessions returns the vault backed by this store.
//
// The extension schema must already exist; see EnsureForkCheckinSchema. A
// vault on a store without the extension tables reports its sessions as
// unavailable rather than creating anything.
func (store *Store) ForkCheckinSessions() *ForkCheckinVault {
	return &ForkCheckinVault{store: store}
}

var _ forkcheckin.Vault = (*ForkCheckinVault)(nil)

// Put replaces the stored session for one account.
//
// Only ciphertext reaches the database. The envelope is bound to this table
// and this account id, so a row copied into another account fails to open
// instead of decrypting under the new owner.
//
// An empty value is rejected rather than stored: an empty session is not a
// session, and writing one would turn "no session" into a row that exists but
// cannot be used. Callers that mean to forget a session call Delete.
func (vault *ForkCheckinVault) Put(ctx context.Context, id forkcheckin.AccountID, credential []byte) error {
	if err := id.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	if len(credential) == 0 {
		return fmt.Errorf("%w: check-in session is empty", storagecontract.ErrInvalidArgument)
	}
	if len(credential) > forkcheckin.MaxCredentialBytes {
		return fmt.Errorf(
			"%w: check-in session is larger than %d bytes",
			storagecontract.ErrInvalidArgument, forkcheckin.MaxCredentialBytes)
	}
	return putForkCheckinSession(ctx, vault.store.forkCheckinDB(), vault.store, id, credential)
}

type forkCheckinExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// putForkCheckinSession seals and upserts one session through execer, which
// is either the detached pool or an extension transaction.
func putForkCheckinSession(ctx context.Context, execer forkCheckinExecer, store *Store, id forkcheckin.AccountID, credential []byte) error {
	sealed, err := store.keys.sealColumn(forkCheckinCredentials, string(id), credential)
	if err != nil {
		// Nothing is written when sealing fails, so a session that is already
		// stored survives a failed replacement rather than being destroyed by
		// it. The message names the account, never the value.
		return fmt.Errorf("%w: cannot seal check-in session for %q", forkcheckin.ErrCredentialUnavailable, id)
	}
	// Only the envelope is needed past this point.
	defer clear(sealed)
	now := store.now().UTC().Format(time.RFC3339Nano)
	// The account row must exist; the schema's foreign key enforces it, so a
	// session cannot outlive or precede its account.
	if _, err := execer.ExecContext(ctx, fmt.Sprintf(
		`INSERT INTO %s (account_id, sealed_value, updated_at) VALUES (?, ?, ?)
ON CONFLICT(account_id) DO UPDATE SET sealed_value = excluded.sealed_value, updated_at = excluded.updated_at`,
		forkCheckinCredentials), string(id), sealed, now); err != nil {
		return mapForkCheckinVaultWriteError(err, id)
	}
	return nil
}

// Get returns the stored session.
//
// Every reason the plaintext cannot be produced — nothing stored, the local
// key is gone, the key no longer matches, the row was damaged or copied from
// another account — is reported as forkcheckin.ErrCredentialUnavailable. The
// distinction the caller needs is "there is no usable session", and reporting
// which of those happened would describe the keystore's state to whoever asked
// for a session. It never returns a nil value with a nil error, so a missing
// session cannot be mistaken for an empty one.
func (vault *ForkCheckinVault) Get(ctx context.Context, id forkcheckin.AccountID) ([]byte, error) {
	if err := id.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	var sealed []byte
	err := vault.store.forkCheckinDB().QueryRowContext(ctx, fmt.Sprintf(
		`SELECT sealed_value FROM %s WHERE account_id = ?`, forkCheckinCredentials), string(id)).Scan(&sealed)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, fmt.Errorf("%w: no session stored for %q", forkcheckin.ErrCredentialUnavailable, id)
	case err != nil && isForkCheckinMissingTable(err):
		// A store without the extension schema has no sessions. That is the
		// disabled state, not a failure of the main store.
		return nil, fmt.Errorf("%w: check-in storage is not initialized", forkcheckin.ErrCredentialUnavailable)
	case err != nil:
		return nil, fmt.Errorf("read check-in session: %w", err)
	}
	plaintext, err := vault.store.keys.openColumn(forkCheckinCredentials, string(id), sealed)
	// The ciphertext read from the row has served its purpose either way. The
	// stored row itself is never rewritten here: a value that does not open is
	// left exactly as it is, so a key that comes back later still finds it.
	clear(sealed)
	if err != nil {
		// openColumn already reports a missing or mismatched key as
		// secretstore.ErrUnavailable; both become the extension's own
		// unavailable error so the caller has one condition to handle.
		if errors.Is(err, secretstore.ErrUnavailable) {
			return nil, fmt.Errorf("%w: session for %q does not decrypt on this device", forkcheckin.ErrCredentialUnavailable, id)
		}
		return nil, fmt.Errorf("%w: session for %q could not be opened", forkcheckin.ErrCredentialUnavailable, id)
	}
	if len(plaintext) == 0 {
		// Put refuses empty values, so this is a damaged row rather than a
		// stored empty session.
		return nil, fmt.Errorf("%w: session for %q is empty", forkcheckin.ErrCredentialUnavailable, id)
	}
	return plaintext, nil
}

// Delete removes the stored session.
//
// Deleting an absent session succeeds. A retried delete, or a delete for an
// account whose session was already dropped by a redirecting edit, must not
// report an error for work that is already done.
func (vault *ForkCheckinVault) Delete(ctx context.Context, id forkcheckin.AccountID) error {
	if err := id.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	if _, err := vault.store.forkCheckinDB().ExecContext(ctx, fmt.Sprintf(
		`DELETE FROM %s WHERE account_id = ?`, forkCheckinCredentials), string(id)); err != nil {
		if isForkCheckinMissingTable(err) {
			// Nothing is stored where there is no table.
			return nil
		}
		return fmt.Errorf("delete check-in session: %w", err)
	}
	return nil
}

// HasForkCheckinSession reports whether a sealed row exists, without opening
// it.
//
// This answers "does the operator need to sign in" for listings, which must
// not decrypt a session just to render a badge. A row that exists but no
// longer opens still counts as present here; Get is what establishes
// usability.
func (store *Store) HasForkCheckinSession(ctx context.Context, id forkcheckin.AccountID) (bool, error) {
	if err := id.Validate(); err != nil {
		return false, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	var present bool
	err := store.forkCheckinDB().QueryRowContext(ctx, fmt.Sprintf(
		`SELECT EXISTS(SELECT 1 FROM %s WHERE account_id = ?)`, forkCheckinCredentials), string(id)).Scan(&present)
	if err != nil {
		if isForkCheckinMissingTable(err) {
			return false, nil
		}
		return false, fmt.Errorf("read check-in session presence: %w", err)
	}
	return present, nil
}

// mapForkCheckinVaultWriteError translates the schema's own constraints.
func mapForkCheckinVaultWriteError(err error, id forkcheckin.AccountID) error {
	message := err.Error()
	switch {
	case isForkCheckinMissingTable(err):
		return fmt.Errorf("%w: check-in storage is not initialized", forkcheckin.ErrCredentialUnavailable)
	case strings.Contains(message, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("%w: fork check-in account %q", storagecontract.ErrNotFound, id)
	default:
		return fmt.Errorf("write check-in session: %w", err)
	}
}

// isForkCheckinMissingTable reports whether the extension schema is absent.
//
// With the extension off, no fork_checkin_* table exists on disk. Reads of a
// session must then report the extension as having nothing stored, instead of
// surfacing a SQL error that would look like a fault in the main store.
func isForkCheckinMissingTable(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "no such table: "+forkCheckinCredentials)
}
