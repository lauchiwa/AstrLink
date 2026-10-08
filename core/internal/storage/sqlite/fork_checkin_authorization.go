package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

var _ forkcheckin.AuthorizationStore = (*Store)(nil)

// ReadForkCheckinAuthorizationAccount reads the account an authorization is
// for and whether a job holds a live lease on it. It reads local state only.
func (store *Store) ReadForkCheckinAuthorizationAccount(ctx context.Context, id forkcheckin.AccountID) (forkcheckin.Account, bool, error) {
	account, err := store.GetForkCheckinAccount(ctx, id)
	if err != nil {
		return forkcheckin.Account{}, false, err
	}
	busy, err := forkCheckinAccountLeased(ctx, store.forkCheckinDB(), store, id)
	if err != nil {
		return forkcheckin.Account{}, false, err
	}
	return account, busy, nil
}

// LookupForkCheckinAuthorizationReceipt returns the receipt stored under a
// request id, whatever route stored it, so the caller can tell a replay from
// a reused id.
func (store *Store) LookupForkCheckinAuthorizationReceipt(ctx context.Context, requestID string) (forkcheckin.AuthorizationReceipt, error) {
	if err := forkcheckin.ValidateRequestID(requestID); err != nil {
		return forkcheckin.AuthorizationReceipt{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	stored, digest, _, err := readForkCheckinReceipt(ctx, store.forkCheckinDB(), requestID)
	if err != nil {
		return forkcheckin.AuthorizationReceipt{}, err
	}
	return forkcheckin.AuthorizationReceipt{Route: stored.Route, Fingerprint: digest, Status: stored.Status, Body: stored.Body}, nil
}

// CompleteForkCheckinAuthorization stores a session the site has confirmed.
// The account change, the sealed session and the receipt commit together, so
// a failure at any step leaves none of them. The account is re-checked under
// the writer lock: an edit, delete or new job since verification refuses the
// commit rather than storing a session for a changed account.
func (store *Store) CompleteForkCheckinAuthorization(ctx context.Context, commit forkcheckin.AuthorizationCommit) (forkcheckin.AccountWriteResult, error) {
	if err := commit.AccountID.Validate(); err != nil {
		return forkcheckin.AccountWriteResult{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	if commit.Revision < 1 || commit.RemoteUserID == "" || len(commit.Credential) == 0 || len(commit.Credential) > forkcheckin.MaxCredentialBytes {
		return forkcheckin.AccountWriteResult{}, fmt.Errorf("%w: incomplete check-in authorization", storagecontract.ErrInvalidArgument)
	}
	return store.writeForkCheckinOnce(ctx, commit.RequestID, forkcheckin.WriteRouteAuthorizationComplete, commit.Fingerprint, commit.Revision,
		func(ctx context.Context, transaction *sql.Tx) (int, any, error) {
			current, err := readForkCheckinAccountTx(ctx, transaction, commit.AccountID)
			if err != nil {
				return 0, nil, err
			}
			if current.Revision != commit.Revision || current.ConfigFingerprint() != commit.ConfigFingerprint {
				return 0, nil, forkcheckin.ErrRevisionChanged
			}
			if current.RemoteUserID != "" && current.RemoteUserID != commit.RemoteUserID {
				// A site and user change is a new account, never an overwrite.
				return 0, nil, forkcheckin.ErrIdentityMismatch
			}
			if err := requireForkCheckinAccountIdle(ctx, transaction, store, commit.AccountID); err != nil {
				return 0, nil, err
			}
			next := current
			next.State = forkcheckin.AccountStateConnected
			next.RemoteUserID = commit.RemoteUserID
			stored, err := store.updateForkCheckinAccountTx(ctx, transaction, next, current.Revision)
			if err != nil {
				return 0, nil, err
			}
			if err := putForkCheckinSession(ctx, transaction, store, stored.ID, commit.Credential); err != nil {
				return 0, nil, err
			}
			return http.StatusOK, stored.View(), nil
		})
}
