package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

const (
	DefaultForkCheckinLeaseDuration = 30 * time.Second
	MaxForkCheckinLeaseDuration     = 15 * time.Minute
)

var (
	ErrForkCheckinLeaseHeld    = forkcheckin.ErrExecutionBusy
	ErrForkCheckinLeaseExpired = errors.New("check-in lease expired")
)

// ForkCheckinCreateResult reports whether a job was created or replayed.
type ForkCheckinCreateResult struct {
	Job     forkcheckin.Job
	Receipt ForkCheckinStoredReceipt
	Outcome forkcheckin.IdempotencyOutcome
}

// ForkCheckinLease is an internal capability for one execution attempt. Token
// is never included in a public receipt.
type ForkCheckinLease = forkcheckin.ExecutionLease

// ForkCheckinLeaseRequest describes an account-level claim.
type ForkCheckinLeaseRequest struct {
	AccountID        forkcheckin.AccountID
	Action           forkcheckin.JobAction
	SiteDay          string
	ExpectedRevision int64
	Duration         time.Duration
}

// ForkCheckinCompletion is the site result persisted by CompleteForkCheckinJob.
// ReadOnly identifies a recovery status read, which can never claim to have
// read a submission response. Both submission and recovery leases expire.
type ForkCheckinCompletion = forkcheckin.ExecutionCompletion

var _ forkcheckin.ExecutionStore = (*Store)(nil)

// ForkCheckinStoredReceipt is the exact public response saved for an
// idempotency key. Body contains no credential or request secret.
type ForkCheckinStoredReceipt struct {
	RequestID string
	Route     string
	Status    int
	Body      []byte
	CreatedAt time.Time
	snapshot  []byte // Internal, bounded job metadata; never returned as the HTTP body.
}

// CreateForkCheckinJobOnce creates one queued job and its durable public
// receipt. Reusing requestID with the same input digest returns the original
// job; reusing it with another digest is a conflict.
func (store *Store) CreateForkCheckinJobOnce(ctx context.Context, job forkcheckin.Job) (result ForkCheckinCreateResult, err error) {
	job, err = prepareForkCheckinJob(store, job)
	if err != nil {
		return result, err
	}
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return result, fmt.Errorf("begin fork check-in job create: %w", err)
	}
	defer rollbackOnError(transaction, &err)

	if err = lockForkCheckinWrites(ctx, transaction); err != nil {
		return result, err
	}
	stored, digest, revision, lookupErr := readForkCheckinReceipt(ctx, transaction, job.RequestID)
	if lookupErr == nil {
		if stored.Route != "jobs" || digest != job.InputDigest || revision != job.ExpectedRevision {
			return result, fmt.Errorf("%w: request id was used for different content", storagecontract.ErrConflict)
		}
		var original forkcheckin.JobReceipt
		if err = json.Unmarshal(stored.snapshot, &original); err != nil {
			return result, storagecontract.ErrInvalidRecord
		}
		if original.AccountID != job.AccountID || original.Action != job.Action || original.Trigger != job.Trigger || original.ParentID != job.ParentID {
			return result, storagecontract.ErrConflict
		}
		// SiteDay and generated IDs/times are server-owned. Midnight, account
		// deletion or a new revision must not change an accepted response.
		job.ID, job.SiteDay, job.CreatedAt = original.ID, original.SiteDay, original.CreatedAt
		if err = transaction.Commit(); err != nil {
			return result, err
		}
		return ForkCheckinCreateResult{Job: job, Receipt: stored, Outcome: forkcheckin.IdempotencyReplayed}, nil
	}
	if !errors.Is(lookupErr, storagecontract.ErrNotFound) {
		return result, lookupErr
	}
	currentRevision, err := lockForkCheckinAccount(ctx, transaction, job.AccountID)
	if err != nil {
		return result, err
	}
	if currentRevision != job.ExpectedRevision {
		return result, forkcheckin.ErrRevisionChanged
	}
	if job.ParentID != "" {
		return result, fmt.Errorf("%w: batch children must be created atomically with their batch", storagecontract.ErrInvalidArgument)
	}
	if err = insertForkCheckinJob(ctx, transaction, job); err != nil {
		return result, err
	}
	body, marshalErr := json.Marshal(job.Receipt().Public())
	if marshalErr != nil {
		return result, fmt.Errorf("marshal fork check-in receipt: %w", marshalErr)
	}
	snapshot, marshalErr := json.Marshal(job.Receipt())
	if marshalErr != nil {
		return result, marshalErr
	}
	stored = ForkCheckinStoredReceipt{RequestID: job.RequestID, Route: "jobs", Status: 202, Body: body, CreatedAt: job.CreatedAt, snapshot: snapshot}
	if err = insertForkCheckinReceipt(ctx, transaction, stored, job.InputDigest, job.ExpectedRevision); err != nil {
		return result, err
	}
	if err = transaction.Commit(); err != nil {
		return result, fmt.Errorf("commit fork check-in job create: %w", err)
	}
	return ForkCheckinCreateResult{Job: job, Receipt: stored, Outcome: forkcheckin.IdempotencyCreated}, nil
}

// GetForkCheckinJob loads a private job record, including its attempt count.
func (store *Store) GetForkCheckinJob(ctx context.Context, id forkcheckin.JobID) (forkcheckin.Job, error) {
	if id == "" {
		return forkcheckin.Job{}, fmt.Errorf("%w: job id is required", storagecontract.ErrInvalidArgument)
	}
	job, err := scanForkCheckinJob(store.forkCheckinDB().QueryRowContext(ctx,
		fmt.Sprintf(`%s WHERE j.id = ?`, forkCheckinJobSelect), id))
	if errors.Is(err, sql.ErrNoRows) {
		return forkcheckin.Job{}, fmt.Errorf("%w: job %q", storagecontract.ErrNotFound, id)
	}
	return job, err
}

// LookupForkCheckinReceipt returns the exact public response saved for a
// request ID. It does not reconstruct a response from the mutable job row.
func (store *Store) LookupForkCheckinReceipt(ctx context.Context, requestID string) (ForkCheckinStoredReceipt, error) {
	if err := forkcheckin.ValidateRequestID(requestID); err != nil {
		return ForkCheckinStoredReceipt{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	receipt, _, _, err := readForkCheckinReceipt(ctx, store.forkCheckinDB(), requestID)
	return receipt, err
}

type forkCheckinReceiptReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readForkCheckinReceipt(ctx context.Context, reader forkCheckinReceiptReader, requestID string) (ForkCheckinStoredReceipt, string, int64, error) {
	var receipt ForkCheckinStoredReceipt
	var body, createdAt, digest, snapshot string
	var revision int64
	err := reader.QueryRowContext(ctx, `SELECT request_id, route, response_status, response_body, created_at, request_fingerprint, expected_revision, job_snapshot
FROM fork_checkin_request_receipts WHERE request_id = ?`, requestID).Scan(
		&receipt.RequestID, &receipt.Route, &receipt.Status, &body, &createdAt, &digest, &revision, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, "", 0, storagecontract.ErrNotFound
	}
	if err != nil {
		return receipt, "", 0, fmt.Errorf("read fork check-in receipt: %w", err)
	}
	receipt.CreatedAt, err = parseForkCheckinTime(createdAt)
	if err != nil {
		return receipt, "", 0, err
	}
	if !json.Valid([]byte(body)) || body == "null" {
		return receipt, "", 0, storagecontract.ErrInvalidRecord
	}
	receipt.Body = []byte(body)
	receipt.snapshot = []byte(snapshot)
	return receipt, digest, revision, nil
}

// LookupForkCheckinJobReceipt decodes a single-job public receipt. Batch
// receipts remain available through LookupForkCheckinReceipt as their exact
// stored JSON body.
func (store *Store) LookupForkCheckinJobReceipt(ctx context.Context, requestID string) (forkcheckin.JobReceipt, error) {
	receipt, err := store.LookupForkCheckinReceipt(ctx, requestID)
	if err != nil {
		return forkcheckin.JobReceipt{}, err
	}
	if receipt.Route != "jobs" {
		return forkcheckin.JobReceipt{}, storagecontract.ErrInvalidArgument
	}
	var public forkcheckin.JobReceipt
	if err := json.Unmarshal(receipt.snapshot, &public); err != nil {
		return forkcheckin.JobReceipt{}, fmt.Errorf("%w: decode fork check-in receipt: %v", storagecontract.ErrInvalidRecord, err)
	}
	return public, nil
}

// ClaimForkCheckinJob claims the next eligible job for an account.
func (store *Store) ClaimForkCheckinJob(ctx context.Context, request ForkCheckinLeaseRequest) (ForkCheckinLease, error) {
	return store.ClaimForkCheckin(ctx, request.AccountID, request.Action, request.SiteDay, request.ExpectedRevision, request.Duration)
}

// ClaimForkCheckin claims an account-level lease. SQLite's writer lock is
// acquired by the no-op account update before any lease check, so two Core
// processes cannot both observe an unleased account and insert attempts.
func (store *Store) ClaimForkCheckin(
	ctx context.Context,
	accountID forkcheckin.AccountID,
	action forkcheckin.JobAction,
	siteDay string,
	expectedRevision int64,
	duration time.Duration,
) (lease ForkCheckinLease, err error) {
	return store.claimForkCheckin(ctx, accountID, action, siteDay, expectedRevision, duration, "")
}

// ClaimForkCheckinJobID claims exactly the requested job. It must not silently
// execute an earlier queued sibling when a runner is invoked with a job ID.
// Obsolete revisions may be claimed solely to record their local cancellation;
// Mark still requires the current revision and can never authorize their I/O.
func (store *Store) ClaimForkCheckinJobID(ctx context.Context, id forkcheckin.JobID, duration time.Duration) (ForkCheckinLease, error) {
	job, err := store.GetForkCheckinJob(ctx, id)
	if err != nil {
		return ForkCheckinLease{}, err
	}
	return store.claimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, job.ExpectedRevision, duration, id)
}

func (store *Store) claimForkCheckin(ctx context.Context, accountID forkcheckin.AccountID, action forkcheckin.JobAction, siteDay string, expectedRevision int64, duration time.Duration, requestedID forkcheckin.JobID) (lease ForkCheckinLease, err error) {
	if err := validateForkCheckinClaim(accountID, action, siteDay, expectedRevision, duration); err != nil {
		return lease, err
	}
	if duration == 0 {
		duration = DefaultForkCheckinLeaseDuration
	}
	token, err := newForkCheckinToken("lease_")
	if err != nil {
		return lease, fmt.Errorf("create fork check-in lease token: %w", err)
	}
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return lease, fmt.Errorf("begin fork check-in claim: %w", err)
	}
	defer rollbackOnError(transaction, &err)

	currentRevision, err := lockForkCheckinAccount(ctx, transaction, accountID)
	if err != nil {
		return lease, err
	}
	if currentRevision != expectedRevision && requestedID == "" {
		return lease, fmt.Errorf("%w: account %q is at revision %d, not %d", forkcheckin.ErrRevisionChanged, accountID, currentRevision, expectedRevision)
	}
	now := store.now().UTC()
	nowText := formatForkCheckinTime(now)
	if err = expireForkCheckinAttempts(ctx, transaction, accountID, nowText); err != nil {
		return lease, err
	}
	var activeJob string
	err = transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT j.id
FROM %s AS j JOIN %s AS a ON a.job_id = j.id
WHERE j.account_id = ? AND a.lease_owner <> '' AND a.lease_expires_at > ? AND a.ended_at = ''
LIMIT 1`, forkCheckinJobs, forkCheckinAttempts), accountID, nowText).Scan(&activeJob)
	if err == nil {
		return lease, fmt.Errorf("%w: account %q is leased by job %q", ErrForkCheckinLeaseHeld, accountID, activeJob)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return lease, fmt.Errorf("check fork check-in account lease: %w", err)
	}
	var dispatchedJob string
	err = transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT id FROM %s
WHERE account_id = ? AND status = 'running' AND dispatched = 1
LIMIT 1`, forkCheckinJobs), accountID).Scan(&dispatchedJob)
	if err == nil {
		return lease, fmt.Errorf("%w: dispatched job %q requires a read-only status recovery", ErrForkCheckinLeaseHeld, dispatchedJob)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return lease, fmt.Errorf("check dispatched fork check-in job: %w", err)
	}

	if action == forkcheckin.JobActionCheckIn && currentRevision == expectedRevision {
		var resolvedStatus string
		err = transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT status FROM %s
WHERE account_id = ? AND site_date = ?
  AND (status IN ('success', 'already_checked')
       OR (action = 'check_in' AND dispatched = 1))
ORDER BY created_at DESC LIMIT 1`, forkCheckinJobs), accountID, siteDay).Scan(&resolvedStatus)
		if err == nil {
			if resolvedStatus == string(forkcheckin.JobStatusSuccess) || resolvedStatus == string(forkcheckin.JobStatusAlreadyChecked) {
				return lease, fmt.Errorf("%w: %w: account %q already has a resolved check-in for %s", forkcheckin.ErrDayResolved, storagecontract.ErrConflict, accountID, siteDay)
			}
			return lease, fmt.Errorf("%w: prior dispatched result %s requires a read-only status refresh", ErrForkCheckinLeaseHeld, resolvedStatus)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return lease, fmt.Errorf("check same-day fork check-in result: %w", err)
		}
	}

	var jobID string
	err = transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT j.id FROM %s AS j
WHERE j.account_id = ? AND j.action = ? AND j.site_date = ? AND j.expected_revision = ?
  AND (? = '' OR j.id = ?)
  AND (j.status = 'queued' OR (j.status = 'running' AND j.dispatched = 0))
  AND NOT EXISTS (SELECT 1 FROM %s AS child WHERE child.parent_id = j.id)
ORDER BY j.created_at, j.id LIMIT 1`, forkCheckinJobs, forkCheckinJobs),
		accountID, string(action), siteDay, expectedRevision, requestedID, requestedID).Scan(&jobID)
	if errors.Is(err, sql.ErrNoRows) {
		return lease, fmt.Errorf("%w: no queued job for account %q", storagecontract.ErrNotFound, accountID)
	}
	if err != nil {
		return lease, fmt.Errorf("find fork check-in job to claim: %w", err)
	}
	if err = checkForkCheckinScheduledClaim(ctx, transaction, forkcheckin.JobID(jobID), now); err != nil {
		return lease, err
	}
	var attempt int
	if err = transaction.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT COALESCE(MAX(attempt), 0) + 1 FROM %s WHERE job_id = ?`, forkCheckinAttempts), jobID).Scan(&attempt); err != nil {
		return lease, fmt.Errorf("allocate fork check-in attempt: %w", err)
	}
	expiresAt := now.Add(duration)
	if _, err = transaction.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s
    (job_id, attempt, phase, dispatched, outcome, failure_code, lease_owner, lease_expires_at, started_at, ended_at)
VALUES (?, ?, ?, 0, ?, '', ?, ?, ?, '')`, forkCheckinAttempts),
		jobID, attempt, "claim", "claimed", token, formatForkCheckinTime(expiresAt), nowText); err != nil {
		return lease, fmt.Errorf("insert fork check-in attempt: %w", err)
	}
	result, err := transaction.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET status = 'running' WHERE id = ? AND status IN ('queued', 'running') AND dispatched = 0`, forkCheckinJobs), jobID)
	if err != nil {
		return lease, fmt.Errorf("mark fork check-in job running: %w", err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return lease, fmt.Errorf("confirm fork check-in claim: %w", rowsErr)
		}
		return lease, fmt.Errorf("%w: job %q changed while claiming", storagecontract.ErrPrecondition, jobID)
	}
	if err = transaction.Commit(); err != nil {
		return lease, fmt.Errorf("commit fork check-in claim: %w", err)
	}
	return ForkCheckinLease{
		JobID:     forkcheckin.JobID(jobID),
		AccountID: accountID,
		Revision:  expectedRevision,
		Attempt:   attempt,
		Token:     token,
		ExpiresAt: expiresAt,
	}, nil
}

// MarkForkCheckinDispatched must commit BEFORE the caller sends a submission.
// It is a one-shot permission: a repeated call is refused. A crash after this
// mark is conservatively treated as possibly dispatched, even if no byte left
// the machine. Recovery can only read status; it cannot obtain another send.
func (store *Store) MarkForkCheckinDispatched(ctx context.Context, lease ForkCheckinLease) (err error) {
	if err := validateForkCheckinLease(lease); err != nil {
		return err
	}
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin fork check-in dispatch mark: %w", err)
	}
	defer rollbackOnError(transaction, &err)
	currentRevision, err := lockForkCheckinAccount(ctx, transaction, lease.AccountID)
	if err != nil {
		return err
	}
	if currentRevision != lease.Revision {
		return fmt.Errorf("%w: account %q changed during dispatch", forkcheckin.ErrRevisionChanged, lease.AccountID)
	}
	var (
		jobRevision                      int64
		jobStatus, jobAction             string
		jobDispatched, attemptDispatched int
		owner, expiresAt, endedAt        string
	)
	err = transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT j.expected_revision, j.status, j.action, j.dispatched,
    a.dispatched, a.lease_owner, a.lease_expires_at, a.ended_at
FROM %s AS j JOIN %s AS a ON a.job_id = j.id
WHERE j.id = ? AND j.account_id = ? AND a.attempt = ?`, forkCheckinJobs, forkCheckinAttempts),
		lease.JobID, lease.AccountID, lease.Attempt).Scan(&jobRevision, &jobStatus, &jobAction, &jobDispatched,
		&attemptDispatched, &owner, &expiresAt, &endedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: lease attempt not found", storagecontract.ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("read fork check-in lease: %w", err)
	}
	if jobAction != string(forkcheckin.JobActionCheckIn) {
		return fmt.Errorf("%w: a status refresh cannot dispatch a submission", storagecontract.ErrPrecondition)
	}
	if jobRevision != lease.Revision {
		return fmt.Errorf("%w: job revision changed", forkcheckin.ErrRevisionChanged)
	}
	if owner != lease.Token {
		return fmt.Errorf("%w: lease token does not match", storagecontract.ErrPrecondition)
	}
	if endedAt != "" {
		return fmt.Errorf("%w: lease attempt already ended", storagecontract.ErrPrecondition)
	}
	if jobStatus != string(forkcheckin.JobStatusRunning) {
		return fmt.Errorf("%w: job is %s", storagecontract.ErrPrecondition, jobStatus)
	}
	if jobDispatched != 0 || attemptDispatched != 0 {
		return fmt.Errorf("%w: submission permission was already consumed", storagecontract.ErrPrecondition)
	}
	expiry, err := parseForkCheckinTime(expiresAt)
	if err != nil {
		return err
	}
	if !expiry.After(store.now().UTC()) {
		return ErrForkCheckinLeaseExpired
	}
	if err = checkForkCheckinDispatchDay(ctx, transaction, lease.JobID); err != nil {
		return err
	}
	if _, err = transaction.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET dispatched = 1 WHERE id = ? AND status = 'running' AND dispatched = 0`, forkCheckinJobs), lease.JobID); err != nil {
		return fmt.Errorf("mark fork check-in job dispatched: %w", err)
	}
	result, err := transaction.ExecContext(ctx, fmt.Sprintf(`UPDATE %s
SET phase = 'submission', dispatched = 1, outcome = 'dispatched'
WHERE job_id = ? AND attempt = ? AND lease_owner = ? AND ended_at = '' AND dispatched = 0`, forkCheckinAttempts),
		lease.JobID, lease.Attempt, lease.Token)
	if err != nil {
		return fmt.Errorf("record fork check-in dispatch: %w", err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return fmt.Errorf("confirm fork check-in dispatch: %w", rowsErr)
		}
		return fmt.Errorf("%w: dispatch state changed concurrently", storagecontract.ErrPrecondition)
	}
	if err = transaction.Commit(); err != nil {
		return fmt.Errorf("commit fork check-in dispatch mark: %w", err)
	}
	return nil
}

// RecoverForkCheckinDispatch creates a persisted read-only lease for a
// dispatched, unresolved job. It is the restart path: no submission permission
// is restored, only the ability to perform and record a status recheck.
func (store *Store) RecoverForkCheckinDispatch(ctx context.Context, jobID forkcheckin.JobID, accountID forkcheckin.AccountID, revision int64, duration time.Duration) (lease ForkCheckinLease, err error) {
	if jobID == "" || accountID.Validate() != nil || revision < 1 {
		return lease, fmt.Errorf("%w: invalid dispatched-job recovery request", storagecontract.ErrInvalidArgument)
	}
	if duration == 0 {
		duration = DefaultForkCheckinLeaseDuration
	}
	if duration < 0 || duration > MaxForkCheckinLeaseDuration {
		return lease, fmt.Errorf("%w: invalid recovery lease duration", storagecontract.ErrInvalidArgument)
	}
	token, err := newForkCheckinToken("recovery_")
	if err != nil {
		return lease, fmt.Errorf("create fork check-in recovery token: %w", err)
	}
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return lease, fmt.Errorf("begin fork check-in recovery: %w", err)
	}
	defer rollbackOnError(transaction, &err)
	// An obsolete snapshot can only settle as uncertain/account_changed. The
	// runner must not open its Vault or contact the site; Complete enforces
	// that it cannot publish evidence from the superseded account revision.
	if _, err = lockForkCheckinAccount(ctx, transaction, accountID); err != nil {
		return lease, err
	}
	now := store.now().UTC()
	nowText := formatForkCheckinTime(now)
	if err = expireForkCheckinAttempts(ctx, transaction, accountID, nowText); err != nil {
		return lease, err
	}
	var activeJob string
	err = transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT j.id FROM %s AS j
JOIN %s AS a ON a.job_id = j.id
WHERE j.account_id = ? AND a.lease_owner <> '' AND a.lease_expires_at > ? AND a.ended_at = ''
LIMIT 1`, forkCheckinJobs, forkCheckinAttempts), accountID, nowText).Scan(&activeJob)
	if err == nil {
		return lease, fmt.Errorf("%w: account %q has active job %q", ErrForkCheckinLeaseHeld, accountID, activeJob)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return lease, fmt.Errorf("check active recovery lease: %w", err)
	}
	var (
		jobExpectedRevision                           int64
		jobStatus, jobAction, phase, owner, expiresAt string
		jobDispatched, attemptDispatched              int
		jobAttempt                                    int
		endedAt                                       string
	)
	err = transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT j.expected_revision, j.status, j.action, j.dispatched,
    a.attempt, a.phase, a.dispatched, a.ended_at, a.lease_owner, a.lease_expires_at
FROM %s AS j JOIN %s AS a ON a.job_id = j.id
WHERE j.id = ? AND j.account_id = ?
ORDER BY a.attempt DESC LIMIT 1`, forkCheckinJobs, forkCheckinAttempts), jobID, accountID).Scan(
		&jobExpectedRevision, &jobStatus, &jobAction, &jobDispatched, &jobAttempt, &phase, &attemptDispatched, &endedAt, &owner, &expiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return lease, fmt.Errorf("%w: dispatched job %q", storagecontract.ErrNotFound, jobID)
	}
	if err != nil {
		return lease, fmt.Errorf("read dispatched fork check-in job: %w", err)
	}
	if jobExpectedRevision != revision || jobStatus != string(forkcheckin.JobStatusRunning) || jobDispatched == 0 || jobAction != string(forkcheckin.JobActionCheckIn) {
		return lease, fmt.Errorf("%w: job is not an unresolved dispatched check-in", storagecontract.ErrPrecondition)
	}
	if phase != "submission" && phase != "status_recheck" {
		return lease, fmt.Errorf("%w: dispatched attempt has invalid phase %q", storagecontract.ErrInvalidRecord, phase)
	}
	if phase == "submission" {
		if attemptDispatched == 0 {
			return lease, fmt.Errorf("%w: dispatched submission attempt is inconsistent", storagecontract.ErrInvalidRecord)
		}
	} else {
		if attemptDispatched != 0 {
			return lease, fmt.Errorf("%w: read-only recovery attempt is marked dispatched", storagecontract.ErrInvalidRecord)
		}
		if endedAt == "" {
			if owner == "" || expiresAt == "" {
				return lease, fmt.Errorf("%w: recovery lease is incomplete", storagecontract.ErrInvalidRecord)
			}
			expires, parseErr := parseForkCheckinTime(expiresAt)
			if parseErr != nil {
				return lease, parseErr
			}
			if expires.After(now) {
				return lease, fmt.Errorf("%w: job already has a recovery lease", ErrForkCheckinLeaseHeld)
			}
		}
	}
	// End the prior attempt only if it is still open. An expired read-only
	// recovery attempt is already ended by expireForkCheckinAttempts.
	if endedAt == "" {
		if _, err = transaction.ExecContext(ctx, fmt.Sprintf(`UPDATE %s
SET outcome = 'status_recovery', ended_at = ?, lease_owner = '', lease_expires_at = ''
WHERE job_id = ? AND attempt = ? AND ended_at = ''`, forkCheckinAttempts), nowText, jobID, jobAttempt); err != nil {
			return lease, fmt.Errorf("close dispatched fork check-in attempt: %w", err)
		}
	}
	newAttempt := jobAttempt + 1
	recoveryExpiresAt := now.Add(duration)
	if _, err = transaction.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s
    (job_id, attempt, phase, dispatched, outcome, failure_code, lease_owner, lease_expires_at, started_at, ended_at)
VALUES (?, ?, 'status_recheck', 0, 'status_recheck_claimed', '', ?, ?, ?, '')`, forkCheckinAttempts),
		jobID, newAttempt, token, formatForkCheckinTime(recoveryExpiresAt), nowText); err != nil {
		return lease, fmt.Errorf("insert fork check-in recovery attempt: %w", err)
	}
	if err = transaction.Commit(); err != nil {
		return lease, fmt.Errorf("commit fork check-in recovery: %w", err)
	}
	return ForkCheckinLease{
		JobID: jobID, AccountID: accountID, Revision: revision, Attempt: newAttempt,
		Token: token, ExpiresAt: recoveryExpiresAt,
	}, nil
}

// CompleteForkCheckinJob stores a terminal result under the lease and account
// revision. After an edit, only a local cancelled/uncertain account_changed
// result is accepted. The original live lease can report its submission
// response; an expired or replaced lease cannot complete. Recovery is read-only.
func (store *Store) CompleteForkCheckinJob(
	ctx context.Context,
	lease ForkCheckinLease,
	completion ForkCheckinCompletion,
) (receipt forkcheckin.JobReceipt, err error) {
	if err := validateForkCheckinLease(lease); err != nil {
		return receipt, err
	}
	if completion.FailureCode != "" && !validForkCheckinCode(completion.FailureCode, 64) {
		return receipt, fmt.Errorf("%w: failure code is not a stable identifier", storagecontract.ErrInvalidArgument)
	}
	if completion.Reward.Known && !validForkCheckinCode(completion.Reward.Unit, 32) {
		return receipt, fmt.Errorf("%w: reward unit is not a recognized identifier", storagecontract.ErrInvalidArgument)
	}
	if !completion.Status.Terminal() {
		return receipt, fmt.Errorf("%w: completion status must be terminal", storagecontract.ErrInvalidArgument)
	}
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return receipt, fmt.Errorf("begin fork check-in completion: %w", err)
	}
	defer rollbackOnError(transaction, &err)
	currentRevision, err := lockForkCheckinAccount(ctx, transaction, lease.AccountID)
	if err != nil {
		return receipt, err
	}
	job, err := scanForkCheckinJob(transaction.QueryRowContext(ctx,
		fmt.Sprintf(`%s WHERE j.id = ?`, forkCheckinJobSelect), lease.JobID))
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, fmt.Errorf("%w: job %q", storagecontract.ErrNotFound, lease.JobID)
	}
	if err != nil {
		return receipt, err
	}
	if job.AccountID != lease.AccountID || job.ExpectedRevision != lease.Revision {
		return receipt, fmt.Errorf("%w: job revision or account changed", forkcheckin.ErrRevisionChanged)
	}
	if currentRevision != lease.Revision {
		// Keep the original lease fencing, but permit a local no-proof result
		// to release obsolete work. This never clears durable dispatch intent.
		status, proof := forkcheckin.JobStatusCancelled, forkcheckin.ProofSourceNone
		if job.Dispatched {
			status, proof = forkcheckin.JobStatusUncertain, forkcheckin.ProofSourceTransportError
		}
		if completion.Status != status || completion.ProofSource != proof || completion.FailureCode != "account_changed" || !completion.ReadOnly || completion.Reward != (forkcheckin.Reward{}) {
			return receipt, fmt.Errorf("%w: account %q changed before completion", forkcheckin.ErrRevisionChanged, lease.AccountID)
		}
	}
	var attemptDispatched int
	var owner, expiresAt, endedAt, phase string
	err = transaction.QueryRowContext(ctx, fmt.Sprintf(`SELECT dispatched, lease_owner, lease_expires_at, ended_at, phase
FROM %s WHERE job_id = ? AND attempt = ?`, forkCheckinAttempts), lease.JobID, lease.Attempt).Scan(
		&attemptDispatched, &owner, &expiresAt, &endedAt, &phase)
	if errors.Is(err, sql.ErrNoRows) {
		return receipt, fmt.Errorf("%w: lease attempt not found", storagecontract.ErrNotFound)
	}
	if err != nil {
		return receipt, fmt.Errorf("read fork check-in completion lease: %w", err)
	}
	if owner != lease.Token || endedAt != "" {
		return receipt, fmt.Errorf("%w: lease token is no longer current", storagecontract.ErrPrecondition)
	}
	now := store.now().UTC()
	expiry, parseErr := parseForkCheckinTime(expiresAt)
	if parseErr != nil {
		return receipt, parseErr
	}
	if !expiry.After(now) {
		return receipt, ErrForkCheckinLeaseExpired
	}
	if job.Status != forkcheckin.JobStatusRunning {
		return receipt, storagecontract.ErrPrecondition
	}
	switch phase {
	case "claim":
		if job.Dispatched || attemptDispatched != 0 || completion.ProofSource == forkcheckin.ProofSourceSubmitResponse || completion.ProofSource == forkcheckin.ProofSourceStatusRecheck {
			return receipt, storagecontract.ErrPrecondition
		}
	case "submission":
		if !job.Dispatched || attemptDispatched != 1 ||
			(completion.ReadOnly && completion.ProofSource == forkcheckin.ProofSourceSubmitResponse) ||
			(!completion.ReadOnly && completion.ProofSource == forkcheckin.ProofSourceStatusRecheck) {
			return receipt, storagecontract.ErrInvalidRecord
		}
	case "status_recheck":
		if !job.Dispatched || attemptDispatched != 0 || !completion.ReadOnly || completion.ProofSource == forkcheckin.ProofSourceSubmitResponse {
			return receipt, storagecontract.ErrPrecondition
		}
	default:
		return receipt, storagecontract.ErrInvalidRecord
	}
	job.Status = completion.Status
	job.ProofSource = completion.ProofSource
	job.Reward = completion.Reward
	job.FailureCode = completion.FailureCode
	// A backwards wall-clock adjustment must not strand a valid lease just
	// because its completion appears earlier than its creation timestamp.
	if now.Before(job.CreatedAt) {
		now = job.CreatedAt
	}
	job.CompletedAt = &now
	job.RetryNotBefore = completion.RetryNotBefore
	if err := job.Validate(); err != nil {
		return receipt, fmt.Errorf("%w: invalid fork check-in completion: %v", storagecontract.ErrInvalidArgument, err)
	}
	quota := any(nil)
	unit := ""
	if job.Reward.Known {
		quota = job.Reward.Quota
		unit = job.Reward.Unit
	}
	jobResult, err := transaction.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET status = ?, proof_source = ?,
    reward_known = ?, reward_quota = ?, reward_unit = ?, failure_code = ?, finished_at = ?, retry_not_before = ?
WHERE id = ? AND status = 'running'`, forkCheckinJobs),
		string(job.Status), string(job.ProofSource), boolToInt(job.Reward.Known), quota, unit,
		job.FailureCode, formatForkCheckinTime(now), optionalForkCheckinTime(job.RetryNotBefore), lease.JobID)
	if err != nil {
		return receipt, fmt.Errorf("complete fork check-in job: %w", err)
	}
	if changed, rowsErr := jobResult.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return receipt, fmt.Errorf("confirm fork check-in job completion: %w", rowsErr)
		}
		return receipt, fmt.Errorf("%w: job is no longer running", storagecontract.ErrPrecondition)
	}
	attemptResult, err := transaction.ExecContext(ctx, fmt.Sprintf(`UPDATE %s
SET outcome = ?, failure_code = ?, ended_at = ?, lease_owner = '', lease_expires_at = ''
WHERE job_id = ? AND attempt = ? AND lease_owner = ? AND ended_at = ''`, forkCheckinAttempts),
		string(job.Status), job.FailureCode, formatForkCheckinTime(now), lease.JobID, lease.Attempt, lease.Token)
	if err != nil {
		return receipt, fmt.Errorf("complete fork check-in attempt: %w", err)
	}
	if changed, rowsErr := attemptResult.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return receipt, fmt.Errorf("confirm fork check-in attempt completion: %w", rowsErr)
		}
		return receipt, fmt.Errorf("%w: lease attempt is no longer current", storagecontract.ErrPrecondition)
	}
	if err = transaction.Commit(); err != nil {
		return receipt, fmt.Errorf("commit fork check-in completion: %w", err)
	}
	return job.Receipt(), nil
}

// ListForkCheckinChildren returns public child receipts in stable id order.
func (store *Store) ListForkCheckinChildren(ctx context.Context, parentID forkcheckin.JobID, options forkcheckin.ListOptions) (forkcheckin.JobPage, error) {
	if parentID == "" {
		return forkcheckin.JobPage{}, fmt.Errorf("%w: parent job id is required", storagecontract.ErrInvalidArgument)
	}
	if err := options.Validate(); err != nil {
		return forkcheckin.JobPage{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	after, err := decodeForkCheckinJobCursor(options.Cursor)
	if err != nil {
		return forkcheckin.JobPage{}, err
	}
	var exists bool
	if err := store.forkCheckinDB().QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fork_checkin_batches WHERE id = ?)`, parentID).Scan(&exists); err != nil {
		return forkcheckin.JobPage{}, err
	}
	if !exists {
		return forkcheckin.JobPage{}, storagecontract.ErrNotFound
	}
	limit := options.EffectiveLimit()
	rows, err := store.forkCheckinDB().QueryContext(ctx, fmt.Sprintf(`%s
WHERE j.id IN (SELECT job_id FROM fork_checkin_batch_jobs WHERE batch_id = ?) AND j.id > ? ORDER BY j.id LIMIT ?`, forkCheckinJobSelect), parentID, after, limit+1)
	if err != nil {
		return forkcheckin.JobPage{}, fmt.Errorf("list fork check-in children: %w", err)
	}
	defer rows.Close()
	items := make([]forkcheckin.JobReceipt, 0, limit+1)
	for rows.Next() {
		job, scanErr := scanForkCheckinJob(rows)
		if scanErr != nil {
			return forkcheckin.JobPage{}, scanErr
		}
		items = append(items, job.Receipt())
	}
	if err := rows.Err(); err != nil {
		return forkcheckin.JobPage{}, fmt.Errorf("iterate fork check-in children: %w", err)
	}
	page := forkcheckin.JobPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeForkCheckinJobCursor(page.Items[len(page.Items)-1].ID)
	}
	return page, nil
}

const forkCheckinJobSelect = `SELECT j.id, j.account_id,
    COALESCE(j.parent_id, (SELECT batch_id FROM fork_checkin_batch_jobs WHERE job_id = j.id)), j.action, j.trigger,
    j.request_id, j.request_fingerprint, j.expected_revision, j.site_date, j.status,
    j.dispatched, j.proof_source, j.reward_known, j.reward_quota, j.reward_unit,
    j.failure_code, j.created_at, j.finished_at,
    (SELECT COUNT(*) FROM fork_checkin_job_attempts AS ac WHERE ac.job_id = j.id) AS attempts,
    j.schedule_day, j.retry_not_before
FROM fork_checkin_jobs AS j`

func prepareForkCheckinJob(store *Store, job forkcheckin.Job) (forkcheckin.Job, error) {
	if job.SiteDay == "" {
		return forkcheckin.Job{}, fmt.Errorf("%w: site day is required", storagecontract.ErrInvalidArgument)
	}
	parsedDay, err := time.Parse(time.DateOnly, job.SiteDay)
	if err != nil || parsedDay.Format(time.DateOnly) != job.SiteDay {
		return forkcheckin.Job{}, fmt.Errorf("%w: site day must be canonical YYYY-MM-DD", storagecontract.ErrInvalidArgument)
	}
	if job.Status == "" {
		job.Status = forkcheckin.JobStatusQueued
	}
	if job.Status != forkcheckin.JobStatusQueued || job.Dispatched || job.ProofSource != forkcheckin.ProofSourceNone || job.CompletedAt != nil || job.Attempts != 0 || job.RetryNotBefore != nil {
		return forkcheckin.Job{}, fmt.Errorf("%w: a new fork check-in job must be queued and undispatched", storagecontract.ErrInvalidArgument)
	}
	if job.InputDigest == "" || len(job.InputDigest) != 64 {
		return forkcheckin.Job{}, fmt.Errorf("%w: input digest must be a SHA-256 hex digest", storagecontract.ErrInvalidArgument)
	}
	if _, err := hex.DecodeString(job.InputDigest); err != nil || strings.ToLower(job.InputDigest) != job.InputDigest {
		return forkcheckin.Job{}, fmt.Errorf("%w: input digest must be lowercase SHA-256 hex", storagecontract.ErrInvalidArgument)
	}
	if job.ID == "" {
		id, err := newForkCheckinToken("job_")
		if err != nil {
			return forkcheckin.Job{}, fmt.Errorf("create fork check-in job id: %w", err)
		}
		job.ID = forkcheckin.JobID(id)
	}
	if !validForkCheckinCode(string(job.ID), 96) {
		return forkcheckin.Job{}, fmt.Errorf("%w: invalid job id", storagecontract.ErrInvalidArgument)
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = store.now().UTC()
	} else {
		job.CreatedAt = job.CreatedAt.UTC()
	}
	if err := job.Validate(); err != nil {
		return forkcheckin.Job{}, fmt.Errorf("%w: invalid fork check-in job: %v", storagecontract.ErrInvalidArgument, err)
	}
	return job, nil
}

func validForkCheckinCode(value string, maxLength int) bool {
	if len(value) == 0 || len(value) > maxLength || value[0] < 'a' || value[0] > 'z' {
		return false
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' {
			return false
		}
	}
	return true
}

func insertForkCheckinJob(ctx context.Context, transaction *sql.Tx, job forkcheckin.Job) error {
	if _, err := transaction.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s
    (id, account_id, parent_id, action, trigger, expected_revision, status, dispatched, proof_source,
     request_id, request_fingerprint, site_date, reward_known, reward_quota, reward_unit,
     failure_code, created_at, finished_at, schedule_day)
VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?, 0, NULL, '', '', ?, '', ?)`, forkCheckinJobs),
		job.ID, job.AccountID, nullableForkCheckinID(job.ParentID), string(job.Action), string(job.Trigger),
		job.ExpectedRevision, string(job.Status), string(job.ProofSource), job.RequestID, job.InputDigest,
		job.SiteDay, formatForkCheckinTime(job.CreatedAt), job.ScheduleDay); err != nil {
		return mapForkCheckinJobWriteError(err, job.RequestID)
	}
	return nil
}

func insertForkCheckinReceipt(ctx context.Context, tx *sql.Tx, receipt ForkCheckinStoredReceipt, digest string, revision int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO fork_checkin_request_receipts
(request_id, route, request_fingerprint, expected_revision, response_status, response_body, created_at, job_snapshot)
VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, receipt.RequestID, receipt.Route, digest, revision,
		receipt.Status, string(receipt.Body), formatForkCheckinTime(receipt.CreatedAt), string(receipt.snapshot))
	if err != nil {
		return mapForkCheckinJobWriteError(err, receipt.RequestID)
	}
	return nil
}

// Take SQLite's writer lock before reading the idempotency ledger, including
// when an account has since been deleted. This mutates no account or service.
func lockForkCheckinWrites(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE fork_checkin_schema_migrations SET name = name WHERE version = 1`)
	return err
}

func lockForkCheckinAccount(ctx context.Context, transaction *sql.Tx, id forkcheckin.AccountID) (int64, error) {
	result, err := transaction.ExecContext(ctx, fmt.Sprintf(
		`UPDATE %s SET revision = revision WHERE id = ?`, forkCheckinAccounts), id)
	if err != nil {
		return 0, fmt.Errorf("lock fork check-in account: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("confirm fork check-in account lock: %w", err)
	}
	if changed != 1 {
		return 0, fmt.Errorf("%w: fork check-in account %q", storagecontract.ErrNotFound, id)
	}
	var revision int64
	if err := transaction.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT revision FROM %s WHERE id = ?`, forkCheckinAccounts), id).Scan(&revision); err != nil {
		return 0, fmt.Errorf("read fork check-in account revision: %w", err)
	}
	return revision, nil
}

func expireForkCheckinAttempts(ctx context.Context, transaction *sql.Tx, accountID forkcheckin.AccountID, now string) error {
	_, err := transaction.ExecContext(ctx, fmt.Sprintf(`UPDATE %s
SET outcome = 'lease_expired', ended_at = ?, lease_owner = '', lease_expires_at = ''
WHERE ended_at = '' AND lease_expires_at <> ? AND lease_expires_at <= ?
  AND job_id IN (SELECT id FROM %s WHERE account_id = ?)`, forkCheckinAttempts, forkCheckinJobs),
		now, "", now, accountID)
	if err != nil {
		return fmt.Errorf("expire fork check-in attempts: %w", err)
	}
	return nil
}

func validateForkCheckinClaim(accountID forkcheckin.AccountID, action forkcheckin.JobAction, siteDay string, revision int64, duration time.Duration) error {
	if err := accountID.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	if err := action.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	if _, err := time.Parse(time.DateOnly, siteDay); err != nil {
		return fmt.Errorf("%w: site day must be YYYY-MM-DD", storagecontract.ErrInvalidArgument)
	}
	if revision < 1 {
		return fmt.Errorf("%w: expected revision must be positive", storagecontract.ErrInvalidArgument)
	}
	if duration < 0 || duration > MaxForkCheckinLeaseDuration {
		return fmt.Errorf("%w: lease duration is outside the allowed range", storagecontract.ErrInvalidArgument)
	}
	return nil
}

func validateForkCheckinLease(lease ForkCheckinLease) error {
	if lease.JobID == "" || lease.Token == "" || lease.Attempt < 1 || lease.Revision < 1 {
		return fmt.Errorf("%w: incomplete fork check-in lease", storagecontract.ErrInvalidArgument)
	}
	if err := lease.AccountID.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	return nil
}

func mapForkCheckinJobWriteError(err error, value string) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "UNIQUE constraint failed"):
		return fmt.Errorf("%w: fork check-in request or job %q", storagecontract.ErrConflict, value)
	case strings.Contains(message, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("%w: fork check-in job reference %q", storagecontract.ErrNotFound, value)
	case strings.Contains(message, "CHECK constraint failed"):
		return fmt.Errorf("%w: fork check-in job %q", storagecontract.ErrInvalidRecord, value)
	default:
		return fmt.Errorf("write fork check-in job %q: %w", value, err)
	}
}

func scanForkCheckinJob(scanner forkCheckinScanner) (forkcheckin.Job, error) {
	var (
		job                                forkcheckin.Job
		id, accountID, action, trigger     string
		parentID, requestID, inputDigest   sql.NullString
		siteDay, status, proofSource       string
		dispatched, rewardKnown            int
		rewardQuota                        sql.NullInt64
		rewardUnit, failureCode, createdAt string
		finishedAt, retryNotBefore         string
	)
	if err := scanner.Scan(&id, &accountID, &parentID, &action, &trigger, &requestID, &inputDigest,
		&job.ExpectedRevision, &siteDay, &status, &dispatched, &proofSource, &rewardKnown,
		&rewardQuota, &rewardUnit, &failureCode, &createdAt, &finishedAt, &job.Attempts, &job.ScheduleDay, &retryNotBefore); err != nil {
		return job, wrapForkCheckinScanError(err)
	}
	job.ID = forkcheckin.JobID(id)
	job.AccountID = forkcheckin.AccountID(accountID)
	job.Action = forkcheckin.JobAction(action)
	job.Trigger = forkcheckin.JobTrigger(trigger)
	job.RequestID = requestID.String
	job.InputDigest = inputDigest.String
	job.SiteDay = siteDay
	job.Status = forkcheckin.JobStatus(status)
	job.Dispatched = intToBool(dispatched)
	job.ProofSource = forkcheckin.ProofSource(proofSource)
	job.Reward.Known = intToBool(rewardKnown)
	if rewardQuota.Valid {
		job.Reward.Quota = rewardQuota.Int64
	}
	job.Reward.Unit = rewardUnit
	job.FailureCode = failureCode
	if parentID.Valid {
		job.ParentID = forkcheckin.JobID(parentID.String)
	}
	created, err := parseForkCheckinTime(createdAt)
	if err != nil {
		return job, err
	}
	job.CreatedAt = created
	if finishedAt != "" {
		finished, parseErr := parseForkCheckinTime(finishedAt)
		if parseErr != nil {
			return job, parseErr
		}
		job.CompletedAt = &finished
	}
	if retryNotBefore != "" {
		deadline, err := parseForkCheckinTime(retryNotBefore)
		if err != nil {
			return job, err
		}
		job.RetryNotBefore = &deadline
	}
	if err := job.Validate(); err != nil {
		return job, fmt.Errorf("%w: invalid stored fork check-in job %q: %v", storagecontract.ErrInvalidRecord, job.ID, err)
	}
	return job, nil
}

func wrapForkCheckinScanError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return fmt.Errorf("scan fork check-in job: %w", err)
}

func newForkCheckinToken(prefix string) (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(raw[:]), nil
}

func formatForkCheckinTime(value time.Time) string {
	// Fixed fractional width makes SQLite TEXT comparisons chronological even
	// at an exact second boundary (RFC3339Nano removes trailing zeroes).
	return value.UTC().Format(strings.Replace(time.RFC3339Nano, ".999999999", ".000000000", 1))
}

func parseForkCheckinTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w: invalid fork check-in timestamp %q", storagecontract.ErrInvalidRecord, value)
	}
	return parsed.UTC(), nil
}

func nullableForkCheckinID(id forkcheckin.JobID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

func intToBool(value int) bool {
	return value != 0
}

func encodeForkCheckinJobCursor(id forkcheckin.JobID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

func decodeForkCheckinJobCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != cursor || !validForkCheckinCode(string(decoded), 96) {
		return "", fmt.Errorf("%w: fork check-in job cursor encoding", storagecontract.ErrInvalidCursor)
	}
	return string(decoded), nil
}
