package forkcheckin

import (
	"context"
	"errors"
	"time"
)

// ExecutionLease is an internal, non-public capability for one fenced attempt.
type ExecutionLease struct {
	JobID     JobID
	AccountID AccountID
	Revision  int64
	Attempt   int
	Token     string
	ExpiresAt time.Time
}

type ExecutionCompletion struct {
	Status         JobStatus
	ProofSource    ProofSource
	Reward         Reward
	FailureCode    string
	ReadOnly       bool
	RetryNotBefore *time.Time
}

// ExecutionStore performs each operation in a short local transaction. Network
// calls never run in those transactions. Mark must durably commit before return.
// An exact claim may lease an obsolete revision only to settle it locally; Mark
// must reject it. Complete accepts only cancelled/uncertain account_changed
// results after a revision change, never proof made with the obsolete session.
type ExecutionStore interface {
	GetForkCheckinJob(context.Context, JobID) (Job, error)
	GetForkCheckinAccount(context.Context, AccountID) (Account, error)
	ClaimForkCheckinJobID(context.Context, JobID, time.Duration) (ExecutionLease, error)
	RecoverForkCheckinDispatch(context.Context, JobID, AccountID, int64, time.Duration) (ExecutionLease, error)
	BindForkCheckinJobDay(context.Context, ExecutionLease, string) (Job, error)
	MarkForkCheckinDispatched(context.Context, ExecutionLease) error
	CompleteForkCheckinJob(context.Context, ExecutionLease, ExecutionCompletion) (JobReceipt, error)
}

// Runner has no background scheduler or unbounded queue. The store's account
// lease fences manual and automatic callers alike. The owner must wait for
// Execute to return before destroying the Vault or clearing its keys. The 90s
// execution, 15s recheck and 5s completion budgets fit inside the 2m lease.
type Runner struct {
	store   ExecutionStore
	vault   Vault
	adapter SiteAdapter
	now     func() time.Time
	gate    executionGate
}

func NewRunner(store ExecutionStore, vault Vault, adapter SiteAdapter) (*Runner, error) {
	if store == nil || vault == nil || adapter == nil {
		return nil, errors.New("check-in runner dependencies unavailable")
	}
	return &Runner{store: store, vault: vault, adapter: adapter, now: time.Now}, nil
}

func (r *Runner) Execute(ctx context.Context, id JobID) (JobReceipt, error) {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	job, err := r.store.GetForkCheckinJob(ctx, id)
	if err != nil {
		return JobReceipt{}, err
	}
	if job.ID != id || job.Validate() != nil {
		return JobReceipt{}, errors.New("invalid check-in execution job")
	}
	if job.Status.Terminal() {
		// The request-id ledger is the immutable acceptance response, not the
		// current result. Returning it here would turn a completed job queued.
		return job.Receipt(), nil
	}
	// The runtime shares this Runner between manual and automatic callers.
	// Admission precedes claiming, so a waiter never holds an expiring lease.
	gateAccount, err := r.store.GetForkCheckinAccount(ctx, job.AccountID)
	if err != nil {
		return JobReceipt{}, err
	}
	release, err := r.gate.acquire(ctx, gateAccount.DashboardBaseURL)
	if err != nil {
		return JobReceipt{}, err
	}
	defer release()
	// Another caller may have finished this exact job while we waited for
	// local admission. Return its current terminal result, not a claim error.
	job, err = r.store.GetForkCheckinJob(ctx, id)
	if err != nil {
		return JobReceipt{}, err
	}
	if job.ID != id || job.AccountID != gateAccount.ID || job.Validate() != nil {
		return JobReceipt{}, errors.New("invalid check-in execution job")
	}
	if job.Status.Terminal() {
		return job.Receipt(), nil
	}
	var lease ExecutionLease
	if job.Dispatched {
		lease, err = r.store.RecoverForkCheckinDispatch(ctx, id, job.AccountID, job.ExpectedRevision, 2*time.Minute)
	} else {
		lease, err = r.store.ClaimForkCheckinJobID(ctx, id, 2*time.Minute)
	}
	if err != nil {
		return JobReceipt{}, err
	}
	if lease.JobID != id || lease.AccountID != job.AccountID || lease.Revision != job.ExpectedRevision || lease.Attempt < 1 || lease.Token == "" {
		return JobReceipt{}, errors.New("check-in lease does not match the job")
	}
	complete := func(result ExecutionCompletion) (JobReceipt, error) {
		// Cancellation still needs a bounded durable result. Failure to commit
		// leaves recovery governed by the persisted dispatch flag, not memory.
		finish, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		receipt, err := r.store.CompleteForkCheckinJob(finish, lease, result)
		if errors.Is(err, ErrRevisionChanged) {
			return r.store.CompleteForkCheckinJob(finish, lease, executionFailure(ErrRevisionChanged, job.Dispatched))
		}
		return receipt, err
	}
	failure := func(err error) (JobReceipt, error) { return complete(executionFailure(err, job.Dispatched)) }
	if err := ctx.Err(); err != nil && !job.Dispatched {
		return failure(err)
	}
	account, err := r.store.GetForkCheckinAccount(ctx, job.AccountID)
	if err != nil {
		return failure(err)
	}
	if account.ID != job.AccountID || account.Revision != job.ExpectedRevision {
		return failure(ErrRevisionChanged)
	}
	if account.State != AccountStateConnected || account.RemoteUserID == "" {
		return failure(ErrAuthRequired)
	}
	if !job.Dispatched && job.Trigger == JobTriggerAutomatic && !account.Automatic {
		return failure(context.Canceled)
	}
	credential, err := r.vault.Get(ctx, job.AccountID)
	defer clear(credential)
	if err != nil {
		if ctx.Err() != nil {
			return failure(ctx.Err())
		}
		return failure(ErrCredentialUnavailable)
	}
	snapshot := AccountSnapshot{Account: account, Credential: credential}
	if snapshot.Validate() != nil {
		return failure(ErrCredentialUnavailable)
	}
	if job.Dispatched {
		return complete(r.recheck(ctx, snapshot, job))
	}
	identity, err := r.adapter.ValidateIdentity(ctx, snapshot)
	if err != nil {
		return failure(err)
	}
	if identity.RemoteUserID != account.RemoteUserID {
		return failure(ErrIdentityMismatch)
	}
	started := r.now()
	status, err := r.adapter.ReadStatus(ctx, snapshot)
	if err != nil {
		return failure(err)
	}
	if status.SiteDate != "" && status.SiteDate != job.SiteDay {
		// Rebinding is legal only under a live, pre-dispatch lease. The final
		// dispatch transaction checks the corrected day's proof/intent again.
		rebound, bindErr := r.store.BindForkCheckinJobDay(ctx, lease, status.SiteDate)
		if bindErr != nil {
			return failure(bindErr)
		}
		job = rebound
	}
	location, err := time.LoadLocation(account.TimeZone)
	if err != nil || !executionDayMatches(status.SiteDate, job.SiteDay, started, r.now(), location) {
		return failure(ErrRevisionChanged)
	}
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	if status.CheckedInToday {
		return complete(ExecutionCompletion{Status: JobStatusAlreadyChecked, ProofSource: ProofSourceSiteStatus, ReadOnly: true})
	}
	if job.Action == JobActionStatusRefresh {
		return complete(ExecutionCompletion{Status: JobStatusNotChecked, ProofSource: ProofSourceSiteStatus, ReadOnly: true})
	}
	if status.NextAvailableAt != nil && status.NextAvailableAt.After(r.now()) {
		return failure(&RateLimitError{NotBefore: *status.NextAvailableAt})
	}
	// Known unsupported/manual-only sites must not consume a durable dispatch
	// permission merely because Submit repeats its own defensive preflight.
	capability, err := r.adapter.Inspect(ctx, snapshot)
	if err != nil {
		return failure(err)
	}
	if capability.RequiresManual {
		return failure(ErrManualRequired)
	}
	if !capability.Supported {
		return failure(ErrUnsupported)
	}
	current, err := r.store.GetForkCheckinAccount(ctx, job.AccountID)
	if err != nil {
		return failure(err)
	}
	if current.Revision != account.Revision || current.ConfigFingerprint() != account.ConfigFingerprint() {
		return failure(ErrRevisionChanged)
	}
	if !executionDayMatches(status.SiteDate, job.SiteDay, started, r.now(), location) {
		return failure(ErrRevisionChanged)
	}
	if err := ctx.Err(); err != nil {
		return failure(err)
	}
	if err = r.store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		if errors.Is(err, ErrDayResolved) {
			return failure(err)
		}
		// A commit error can be ambiguous. Never assume intent was rolled back
		// and never Submit; a later Execute reads the persisted flag afresh.
		return JobReceipt{}, err
	}
	job.Dispatched = true
	if ctx.Err() == nil {
		out, submitErr := r.adapter.Submit(ctx, snapshot)
		if out.Validate() == nil && submitErr == nil && out.ResponseRead && out.Dispatched && out.SiteDate == job.SiteDay {
			if out.Succeeded {
				return complete(ExecutionCompletion{Status: JobStatusSuccess, ProofSource: ProofSourceSubmitResponse, Reward: out.Reward})
			}
			if out.AlreadyCheckedIn {
				return complete(ExecutionCompletion{Status: JobStatusAlreadyChecked, ProofSource: ProofSourceSubmitResponse})
			}
		}
	}
	// Even a reported pre-dispatch failure cannot roll back durable intent.
	// Recheck independently; no second POST, no reward inferred from a balance.
	return complete(r.recheck(ctx, snapshot, job))
}

func executionDayMatches(siteDate, jobDay string, started, finished time.Time, location *time.Location) bool {
	if siteDate != "" {
		day, err := time.Parse(time.DateOnly, siteDate)
		return err == nil && day.Year() > 0 && day.Format(time.DateOnly) == siteDate && siteDate == jobDay
	}
	return location != nil && started.In(location).Format(time.DateOnly) == jobDay && finished.In(location).Format(time.DateOnly) == jobDay
}

func (r *Runner) recheck(ctx context.Context, snapshot AccountSnapshot, job Job) ExecutionCompletion {
	read, stop := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer stop()
	current, err := r.store.GetForkCheckinAccount(read, job.AccountID)
	if err != nil {
		return executionFailure(err, true)
	}
	if current.Revision != snapshot.Account.Revision || current.ConfigFingerprint() != snapshot.Account.ConfigFingerprint() {
		return executionFailure(ErrRevisionChanged, true)
	}
	identity, err := r.adapter.ValidateIdentity(read, snapshot)
	if err != nil || identity.RemoteUserID != snapshot.Account.RemoteUserID {
		return executionFailure(ErrUncertain, true)
	}
	status, err := r.adapter.ReadStatus(read, snapshot)
	// A recovery may be on a later date. Only an explicit site date binds
	// today's status to the interrupted job. Historical awards are not proof
	// that this job earned them, and are never copied into its receipt.
	if err == nil && status.CheckedInToday && status.SiteDate != "" && executionDayMatches(status.SiteDate, job.SiteDay, time.Time{}, time.Time{}, nil) {
		return ExecutionCompletion{Status: JobStatusAlreadyChecked, ProofSource: ProofSourceStatusRecheck, ReadOnly: true}
	}
	return executionFailure(ErrUncertain, true)
}

func executionFailure(err error, dispatched bool) ExecutionCompletion {
	if dispatched {
		code := "unconfirmed"
		if errors.Is(err, ErrRevisionChanged) {
			code = "account_changed"
		}
		return ExecutionCompletion{Status: JobStatusUncertain, ProofSource: ProofSourceTransportError, FailureCode: code, ReadOnly: true}
	}
	result := ExecutionCompletion{Status: JobStatusRetryableFailure, ProofSource: ProofSourceTransportError, FailureCode: "network", ReadOnly: true}
	switch {
	case errors.Is(err, context.Canceled):
		result.Status = JobStatusCancelled
		result.ProofSource = ProofSourceNone
		result.FailureCode = "cancelled"
	case errors.Is(err, ErrAuthRequired), errors.Is(err, ErrCredentialUnavailable):
		result.Status = JobStatusAuthRequired
		result.FailureCode = "auth_required"
	case errors.Is(err, ErrManualRequired), errors.Is(err, ErrPermissionDenied):
		result.Status = JobStatusManualRequired
		result.FailureCode = "manual_required"
	case errors.Is(err, ErrUnsupported):
		result.Status = JobStatusUnsupported
		result.FailureCode = "unsupported"
	case errors.Is(err, ErrRateLimited):
		result.Status = JobStatusRateLimited
		result.FailureCode = "rate_limited"
	case errors.Is(err, ErrDayResolved):
		result.Status = JobStatusCancelled
		result.ProofSource = ProofSourceNone
		result.FailureCode = "day_resolved"
	case errors.Is(err, ErrRevisionChanged), errors.Is(err, ErrIdentityMismatch):
		result.Status = JobStatusCancelled
		result.ProofSource = ProofSourceNone
		result.FailureCode = "account_changed"
	}
	var limited *RateLimitError
	if errors.As(err, &limited) && !limited.NotBefore.IsZero() {
		deadline := limited.NotBefore.UTC()
		result.RetryNotBefore = &deadline
	}
	return result
}
