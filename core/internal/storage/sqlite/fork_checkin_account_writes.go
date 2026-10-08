package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// forkCheckinAccountWriter commits each operator write together with its
// request receipt, so a retried request_id replays the stored response
// instead of applying the change twice. It writes only extension tables; a
// bound service is read to confirm it exists and is never modified.
type forkCheckinAccountWriter struct{ store *Store }

func (writer forkCheckinAccountWriter) CreateAccount(ctx context.Context, request forkcheckin.AccountDraftRequest) (forkcheckin.AccountWriteResult, error) {
	return writer.store.writeForkCheckinOnce(ctx, request.RequestID, forkcheckin.WriteRouteAccountCreate, request.Fingerprint(), 0,
		func(ctx context.Context, transaction *sql.Tx) (int, any, error) {
			id, err := newForkCheckinToken("acct_")
			if err != nil {
				return 0, nil, err
			}
			network := forkcheckin.Network{Mode: forkcheckin.NetworkModeDirect}
			if request.Network != nil {
				network = *request.Network
			}
			stored, err := writer.store.createForkCheckinAccountTx(ctx, transaction, forkcheckin.Account{
				ID: forkcheckin.AccountID(id), DashboardBaseURL: request.DashboardBaseURL,
				State: forkcheckin.AccountStateDraft, Network: network, TimeZone: request.TimeZone,
			})
			if err != nil {
				return 0, nil, err
			}
			return http.StatusCreated, stored.View(), nil
		})
}

func (writer forkCheckinAccountWriter) UpdateAccount(ctx context.Context, id forkcheckin.AccountID, request forkcheckin.AccountUpdateRequest) (forkcheckin.AccountWriteResult, error) {
	return writer.store.writeForkCheckinOnce(ctx, request.RequestID, forkcheckin.WriteRouteAccountUpdate, request.Fingerprint(id), request.ExpectedRevision,
		func(ctx context.Context, transaction *sql.Tx) (int, any, error) {
			if err := id.Validate(); err != nil {
				return 0, nil, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
			}
			current, err := readForkCheckinAccountTx(ctx, transaction, id)
			if err != nil {
				return 0, nil, err
			}
			next := request.Apply(current)
			if err := requireForkCheckinServicesExist(ctx, transaction, current.BoundServices, next.BoundServices); err != nil {
				return 0, nil, err
			}
			stored, err := writer.store.updateForkCheckinAccountTx(ctx, transaction, next, request.ExpectedRevision)
			if err != nil {
				return 0, nil, err
			}
			return http.StatusOK, stored.View(), nil
		})
}

func (writer forkCheckinAccountWriter) DeleteAccount(ctx context.Context, id forkcheckin.AccountID, request forkcheckin.AccountDeleteRequest) (forkcheckin.AccountWriteResult, error) {
	return writer.store.writeForkCheckinOnce(ctx, request.RequestID, forkcheckin.WriteRouteAccountDelete, request.Fingerprint(id), request.ExpectedRevision,
		func(ctx context.Context, transaction *sql.Tx) (int, any, error) {
			if err := requireForkCheckinAccountIdle(ctx, transaction, writer.store, id); err != nil {
				return 0, nil, err
			}
			if err := deleteForkCheckinAccountTx(ctx, transaction, id, request.ExpectedRevision); err != nil {
				return 0, nil, err
			}
			return http.StatusNoContent, struct{}{}, nil
		})
}

// writeForkCheckinOnce runs apply and stores its response under requestID in
// one transaction. Only a successful write leaves a receipt: a refused write
// changes nothing, so retrying it is evaluated afresh.
func (store *Store) writeForkCheckinOnce(
	ctx context.Context,
	requestID, route, fingerprint string,
	expectedRevision int64,
	apply func(context.Context, *sql.Tx) (int, any, error),
) (result forkcheckin.AccountWriteResult, err error) {
	if err := forkcheckin.ValidateRequestID(requestID); err != nil {
		return result, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return result, fmt.Errorf("begin fork check-in write: %w", err)
	}
	defer rollbackOnError(transaction, &err)
	if err = lockForkCheckinWrites(ctx, transaction); err != nil {
		return result, err
	}
	stored, digest, revision, lookupErr := readForkCheckinReceipt(ctx, transaction, requestID)
	switch {
	case lookupErr == nil:
		if stored.Route != route || digest != fingerprint || revision != expectedRevision {
			return result, forkcheckin.ErrRequestIDReused
		}
		if err = transaction.Commit(); err != nil {
			return result, err
		}
		return forkcheckin.AccountWriteResult{Status: stored.Status, Body: stored.Body, Replayed: true}, nil
	case !errors.Is(lookupErr, storagecontract.ErrNotFound):
		return result, lookupErr
	}
	status, view, err := apply(ctx, transaction)
	if err != nil {
		return result, mapForkCheckinAccountWriteError(err)
	}
	body, err := json.Marshal(view)
	if err != nil {
		return result, fmt.Errorf("marshal fork check-in receipt: %w", err)
	}
	receipt := ForkCheckinStoredReceipt{RequestID: requestID, Route: route, Status: status, Body: body, CreatedAt: store.now().UTC()}
	if err = insertForkCheckinReceipt(ctx, transaction, receipt, fingerprint, expectedRevision); err != nil {
		return result, err
	}
	if err = transaction.Commit(); err != nil {
		return result, fmt.Errorf("commit fork check-in write: %w", err)
	}
	return forkcheckin.AccountWriteResult{Status: status, Body: body}, nil
}

// An identity collision is the only conflict an account write can raise;
// it is reported as such rather than as a request id reuse.
func mapForkCheckinAccountWriteError(err error) error {
	if errors.Is(err, storagecontract.ErrConflict) && !errors.Is(err, forkcheckin.ErrRequestIDReused) {
		return fmt.Errorf("%w: %w", forkcheckin.ErrAccountConflict, err)
	}
	return err
}

// requireForkCheckinServicesExist checks only bindings the write adds. A
// binding that is already stored may outlive its service and is reported
// stale by ResolveForkCheckinBindings; it is not a reason to refuse an
// unrelated edit.
func requireForkCheckinServicesExist(ctx context.Context, transaction *sql.Tx, current, next []contract.ServiceID) error {
	stored := make(map[contract.ServiceID]bool, len(current))
	for _, id := range current {
		stored[id] = true
	}
	for _, id := range next {
		if stored[id] {
			continue
		}
		if err := id.Validate(); err != nil {
			return fmt.Errorf("%w: %v", storagecontract.ErrInvalidRecord, err)
		}
		var exists bool
		if err := transaction.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM services WHERE id = ?)`, string(id)).Scan(&exists); err != nil {
			return fmt.Errorf("read bound service: %w", err)
		}
		if !exists {
			return forkcheckin.ErrUnknownService
		}
	}
	return nil
}

// requireForkCheckinAccountIdle refuses a delete while a worker holds a live
// lease: the cascade would erase the record of a submission in flight.
func requireForkCheckinAccountIdle(ctx context.Context, transaction *sql.Tx, store *Store, id forkcheckin.AccountID) error {
	busy, err := forkCheckinAccountLeased(ctx, transaction, store, id)
	if err != nil {
		return err
	}
	if busy {
		return forkcheckin.ErrAccountBusy
	}
	return nil
}

// forkCheckinAccountLeased reports whether a job holds a live lease.
func forkCheckinAccountLeased(ctx context.Context, reader forkCheckinReceiptReader, store *Store, id forkcheckin.AccountID) (bool, error) {
	var busy bool
	if err := reader.QueryRowContext(ctx, `SELECT EXISTS(
    SELECT 1 FROM fork_checkin_job_attempts AS a JOIN fork_checkin_jobs AS j ON j.id = a.job_id
    WHERE j.account_id = ? AND a.ended_at = '' AND a.lease_expires_at > ?)`,
		id, formatForkCheckinTime(store.now())).Scan(&busy); err != nil {
		return false, fmt.Errorf("read fork check-in account leases: %w", err)
	}
	return busy, nil
}
