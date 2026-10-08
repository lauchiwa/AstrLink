package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// forkCheckinJobWriter accepts and cancels operator jobs. Acceptance reuses
// the job DAO's request-id receipts, so a retried request returns the jobs
// it created instead of queueing more. Nothing here contacts a site.
type forkCheckinJobWriter struct{ store *Store }

func (writer forkCheckinJobWriter) CreateJobs(ctx context.Context, create forkcheckin.JobCreateRequest) (forkcheckin.JobWriteResult, error) {
	request := create.JobRequest()
	// One account may omit its revision: it is pinned below from the receipt
	// or the account itself. Validate everything else before reading storage.
	probe := request
	if len(probe.AccountIDs) == 1 && probe.ExpectedRevision == 0 {
		probe.ExpectedRevision = 1
	}
	if err := probe.Validate(); err != nil {
		return forkcheckin.JobWriteResult{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	route := forkcheckin.WriteRouteJobCreate
	if len(request.AccountIDs) > 1 {
		route = forkcheckin.WriteRouteJobBatch
	}
	// A request id seen before is answered from its receipt, even when the
	// account has since changed or gone. Its stored revision completes a
	// single request that omitted one, so the digest matches on replay.
	stored, digest, revision, err := readForkCheckinReceipt(ctx, writer.store.forkCheckinDB(), request.RequestID)
	replay := err == nil
	switch {
	case replay && stored.Route != route:
		return forkcheckin.JobWriteResult{}, forkcheckin.ErrRequestIDReused
	case replay && route == forkcheckin.WriteRouteJobCreate && request.ExpectedRevision == 0:
		request.ExpectedRevision = revision
	case !replay && !errors.Is(err, storagecontract.ErrNotFound):
		return forkcheckin.JobWriteResult{}, err
	}
	if replay && digest != request.Digest() {
		return forkcheckin.JobWriteResult{}, forkcheckin.ErrRequestIDReused
	}
	days := make(map[forkcheckin.AccountID]string, len(request.AccountIDs))
	for _, id := range request.AccountIDs {
		day, current, dayErr := writer.siteDay(ctx, id, replay)
		if dayErr != nil {
			return forkcheckin.JobWriteResult{}, dayErr
		}
		days[id] = day
		if route == forkcheckin.WriteRouteJobCreate && request.ExpectedRevision == 0 {
			request.ExpectedRevision = current
		}
	}
	if route == forkcheckin.WriteRouteJobBatch {
		result, batchErr := writer.store.CreateForkCheckinBatchOnce(ctx, request, days)
		if batchErr != nil {
			return forkcheckin.JobWriteResult{}, mapForkCheckinJobCreateError(batchErr)
		}
		launch := make([]forkcheckin.JobID, 0, len(result.Batch.Children))
		for _, child := range result.Batch.Children {
			launch = append(launch, child.ID)
		}
		return jobWriteResult(result.Receipt, result.Outcome, launch), nil
	}
	id := request.AccountIDs[0]
	result, err := writer.store.CreateForkCheckinJobOnce(ctx, forkcheckin.Job{
		AccountID: id, Action: request.Action, Trigger: request.Trigger, RequestID: request.RequestID,
		InputDigest: request.Digest(), ExpectedRevision: request.ExpectedRevision, SiteDay: days[id],
		Status: forkcheckin.JobStatusQueued, ProofSource: forkcheckin.ProofSourceNone,
	})
	if err != nil {
		return forkcheckin.JobWriteResult{}, mapForkCheckinJobCreateError(err)
	}
	return jobWriteResult(result.Receipt, result.Outcome, []forkcheckin.JobID{result.Job.ID}), nil
}

// siteDay is the account's local date now. The runner rebinds it if the site
// reports its own date before dispatch. On replay the stored day wins and the
// account may be gone, so a placeholder is enough.
func (writer forkCheckinJobWriter) siteDay(ctx context.Context, id forkcheckin.AccountID, replay bool) (string, int64, error) {
	account, err := writer.store.GetForkCheckinAccount(ctx, id)
	if err != nil {
		if replay {
			return writer.store.now().UTC().Format(time.DateOnly), 0, nil
		}
		return "", 0, err
	}
	location, err := time.LoadLocation(account.TimeZone)
	if err != nil {
		return "", 0, storagecontract.ErrInvalidRecord
	}
	return writer.store.now().In(location).Format(time.DateOnly), account.Revision, nil
}

func jobWriteResult(receipt ForkCheckinStoredReceipt, outcome forkcheckin.IdempotencyOutcome, launch []forkcheckin.JobID) forkcheckin.JobWriteResult {
	return forkcheckin.JobWriteResult{
		AccountWriteResult: forkcheckin.AccountWriteResult{
			Status: receipt.Status, Body: receipt.Body, Replayed: outcome == forkcheckin.IdempotencyReplayed,
		},
		Launch: launch,
	}
}

// The job DAO reports a reused id as a bare conflict; at this boundary every
// conflict on acceptance is exactly that.
func mapForkCheckinJobCreateError(err error) error {
	switch {
	case errors.Is(err, forkcheckin.ErrRevisionChanged):
		return fmt.Errorf("%w: %w", storagecontract.ErrPrecondition, err)
	case errors.Is(err, storagecontract.ErrConflict):
		return forkcheckin.ErrRequestIDReused
	default:
		return err
	}
}

// CancelJob stops work that has not been dispatched. An undispatched job is
// settled as cancelled in the same transaction as the receipt; a worker
// still holding its lease is then refused by the dispatch and completion
// fences. A dispatched job is left exactly as it is: the submission may have
// reached the site, so only a status read can resolve it.
func (writer forkCheckinJobWriter) CancelJob(ctx context.Context, id forkcheckin.JobID, request forkcheckin.JobCancelRequest) (forkcheckin.JobWriteResult, error) {
	var interrupt []forkcheckin.JobID
	result, err := writer.store.writeForkCheckinOnce(ctx, request.RequestID, forkcheckin.WriteRouteJobCancel, request.Fingerprint(id), 0,
		func(ctx context.Context, transaction *sql.Tx) (int, any, error) {
			if strings.HasPrefix(string(id), forkcheckin.BatchIDPrefix) {
				return writer.cancelBatch(ctx, transaction, id, &interrupt)
			}
			job, cancelled, err := writer.cancelOne(ctx, transaction, id)
			if err != nil {
				return 0, nil, err
			}
			if job.Status.Terminal() && !cancelled {
				return 0, nil, forkcheckin.ErrJobNotCancellable
			}
			if cancelled {
				interrupt = append(interrupt, id)
			}
			return http.StatusOK, job.Receipt().Public(), nil
		})
	if err != nil {
		return forkcheckin.JobWriteResult{}, err
	}
	return forkcheckin.JobWriteResult{AccountWriteResult: result, Interrupt: interrupt}, nil
}

func (writer forkCheckinJobWriter) cancelBatch(ctx context.Context, transaction *sql.Tx, id forkcheckin.JobID, interrupt *[]forkcheckin.JobID) (int, any, error) {
	ids, err := forkCheckinBatchChildIDs(ctx, transaction, id)
	if err != nil {
		return 0, nil, err
	}
	children := make([]forkcheckin.JobReceipt, 0, len(ids))
	open := false
	for _, child := range ids {
		job, cancelled, err := writer.cancelOne(ctx, transaction, child)
		if err != nil {
			return 0, nil, err
		}
		if cancelled {
			*interrupt = append(*interrupt, child)
		}
		open = open || cancelled || !job.Status.Terminal()
		children = append(children, job.Receipt())
	}
	if !open {
		return 0, nil, forkcheckin.ErrJobNotCancellable
	}
	return http.StatusOK, forkcheckin.AggregateBatch(id, children), nil
}

// cancelOne settles one undispatched, unfinished job as cancelled and ends
// its open attempt. It reports the job as it stands afterwards.
func (writer forkCheckinJobWriter) cancelOne(ctx context.Context, transaction *sql.Tx, id forkcheckin.JobID) (forkcheckin.Job, bool, error) {
	job, err := readForkCheckinJobTx(ctx, transaction, id)
	if err != nil {
		return job, false, err
	}
	if job.Status.Terminal() || job.Dispatched {
		return job, false, nil
	}
	now := writer.store.now().UTC()
	if now.Before(job.CreatedAt) {
		now = job.CreatedAt
	}
	nowText := formatForkCheckinTime(now)
	result, err := transaction.ExecContext(ctx, `UPDATE fork_checkin_jobs
SET status = 'cancelled', proof_source = 'none', failure_code = 'cancelled', finished_at = ?
WHERE id = ? AND dispatched = 0 AND status IN ('queued', 'running')`, nowText, id)
	if err != nil {
		return job, false, fmt.Errorf("cancel fork check-in job: %w", err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		return job, false, fmt.Errorf("%w: job %q changed while cancelling", storagecontract.ErrPrecondition, id)
	}
	if _, err = transaction.ExecContext(ctx, `UPDATE fork_checkin_job_attempts
SET outcome = 'cancelled', failure_code = 'cancelled', ended_at = ?, lease_owner = '', lease_expires_at = ''
WHERE job_id = ? AND ended_at = ''`, nowText, id); err != nil {
		return job, false, fmt.Errorf("end cancelled fork check-in attempt: %w", err)
	}
	job, err = readForkCheckinJobTx(ctx, transaction, id)
	return job, err == nil, err
}

func readForkCheckinJobTx(ctx context.Context, transaction *sql.Tx, id forkcheckin.JobID) (forkcheckin.Job, error) {
	job, err := scanForkCheckinJob(transaction.QueryRowContext(ctx, forkCheckinJobSelect+` WHERE j.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return job, fmt.Errorf("%w: fork check-in job %q", storagecontract.ErrNotFound, id)
	}
	return job, err
}

type forkCheckinQueryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// forkCheckinBatchChildIDs lists a batch's children in id order. A batch
// whose children were all removed with their accounts is reported missing.
func forkCheckinBatchChildIDs(ctx context.Context, queryer forkCheckinQueryer, id forkcheckin.JobID) ([]forkcheckin.JobID, error) {
	rows, err := queryer.QueryContext(ctx, `SELECT job_id FROM fork_checkin_batch_jobs WHERE batch_id = ? ORDER BY job_id LIMIT ?`, id, forkcheckin.MaxBatchAccounts)
	if err != nil {
		return nil, fmt.Errorf("read fork check-in batch: %w", err)
	}
	defer rows.Close()
	var ids []forkcheckin.JobID
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return nil, fmt.Errorf("scan fork check-in batch: %w", err)
		}
		ids = append(ids, forkcheckin.JobID(child))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fork check-in batch: %w", err)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("%w: fork check-in batch %q", storagecontract.ErrNotFound, id)
	}
	return ids, nil
}

// GetForkCheckinBatch reads a batch's children in their current state.
func (store *Store) GetForkCheckinBatch(ctx context.Context, id forkcheckin.JobID) ([]forkcheckin.JobReceipt, error) {
	ids, err := forkCheckinBatchChildIDs(ctx, store.forkCheckinDB(), id)
	if err != nil {
		return nil, err
	}
	children := make([]forkcheckin.JobReceipt, 0, len(ids))
	for _, child := range ids {
		job, err := store.GetForkCheckinJob(ctx, child)
		if err != nil {
			return nil, err
		}
		children = append(children, job.Receipt())
	}
	return children, nil
}
