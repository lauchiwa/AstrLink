// This file defines the extension's own job model: what a check-in attempt
// is, how its result may be established, and how a write survives a retried
// control request.
//
// The desktop's control transport retries some writes on its own. Every
// write here therefore carries a caller-fixed request ID: replaying the same
// ID returns the first receipt instead of starting a second attempt, and the
// same ID with different content is a conflict rather than a silent
// overwrite. None of this makes an upstream check-in exactly-once; see
// [JobStatusUncertain].
package forkcheckin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

const (
	// ProtocolVersion is the extension contract version. A Core that does
	// not serve the extension answers 404, which the caller must report as
	// "unavailable" rather than as a failed check-in.
	ProtocolVersion = 1
	// MaxBatchAccounts bounds one batch request.
	MaxBatchAccounts = 25
	// MaxPageSize bounds a listing page.
	MaxPageSize = 100
	// DefaultPageSize is used when a caller omits the limit.
	DefaultPageSize = 20
	// MaxRequestBodyBytes matches the control API body limit.
	MaxRequestBodyBytes = 1 << 20
	maxRequestIDLen     = 128
	minRequestIDLen     = 8
)

// JobID identifies one check-in attempt record.
type JobID string

// JobAction is what a job was asked to do. Only [JobActionCheckIn] may
// submit; a refresh is read-only, so it can never consume the day's
// check-in.
type JobAction string

const (
	JobActionCheckIn       JobAction = "check_in"
	JobActionStatusRefresh JobAction = "status_refresh"
)

func (action JobAction) Validate() error {
	switch action {
	case JobActionCheckIn, JobActionStatusRefresh:
		return nil
	default:
		return fmt.Errorf("unknown job action %q", string(action))
	}
}

// JobTrigger records who asked. The scheduler only ever creates
// [JobTriggerAutomatic] work, and only for an account whose operator turned
// automatic check-in on.
type JobTrigger string

const (
	JobTriggerManual    JobTrigger = "manual"
	JobTriggerAutomatic JobTrigger = "automatic"
)

func (trigger JobTrigger) Validate() error {
	switch trigger {
	case JobTriggerManual, JobTriggerAutomatic:
		return nil
	default:
		return fmt.Errorf("unknown job trigger %q", string(trigger))
	}
}

// JobStatus is a job's single state. The non-terminal states are queued and
// running; every other value is a final outcome and never changes.
type JobStatus string

const (
	JobStatusQueued  JobStatus = "queued"
	JobStatusRunning JobStatus = "running"
	// JobStatusSuccess means the site affirmatively reported a check-in for
	// today. It requires proof; an HTTP 200 is not one.
	JobStatusSuccess JobStatus = "success"
	// JobStatusAlreadyChecked means the day was already done. It is a
	// success for the day and needs no submission.
	JobStatusAlreadyChecked JobStatus = "already_checked"
	// JobStatusNotChecked means a read-only refresh succeeded and the site
	// reported no check-in today. It is not an unsupported site or a failed
	// submission, and it does not consume the day's check-in.
	JobStatusNotChecked JobStatus = "not_checked"
	// JobStatusAuthRequired means the stored session was rejected.
	JobStatusAuthRequired JobStatus = "auth_required"
	// JobStatusManualRequired means a human must act, for example a
	// challenge page. Never retried automatically.
	JobStatusManualRequired JobStatus = "manual_required"
	// JobStatusUnsupported means the site offers no check-in this build
	// speaks.
	JobStatusUnsupported JobStatus = "unsupported"
	// JobStatusRateLimited means the site asked the caller to slow down.
	JobStatusRateLimited JobStatus = "rate_limited"
	// JobStatusRetryableFailure covers transport and server faults that may
	// succeed later, and only when nothing was dispatched.
	JobStatusRetryableFailure JobStatus = "retryable_failure"
	// JobStatusUncertain means a submission left the machine and its result
	// could not be established. The extension re-reads status; it never
	// submits again and never claims the day failed.
	JobStatusUncertain JobStatus = "uncertain"
	// JobStatusCancelled means the operator stopped the job locally. It
	// says nothing about the upstream request: a cancelled job that had
	// already dispatched keeps Dispatched set.
	JobStatusCancelled JobStatus = "cancelled"
)

var jobStatuses = map[JobStatus]struct{}{
	JobStatusQueued: {}, JobStatusRunning: {}, JobStatusSuccess: {},
	JobStatusAlreadyChecked: {}, JobStatusNotChecked: {}, JobStatusAuthRequired: {}, JobStatusManualRequired: {},
	JobStatusUnsupported: {}, JobStatusRateLimited: {}, JobStatusRetryableFailure: {},
	JobStatusUncertain: {}, JobStatusCancelled: {},
}

func (status JobStatus) Validate() error {
	if _, ok := jobStatuses[status]; !ok {
		return fmt.Errorf("unknown job status %q", string(status))
	}
	return nil
}

// Terminal reports whether the status is final.
func (status JobStatus) Terminal() bool {
	return status != JobStatusQueued && status != JobStatusRunning
}

// CountsAsDoneToday reports whether the day needs no further attempt. An
// uncertain result deliberately does not count: the day is resolved by
// re-reading status, not by assuming either outcome.
func (status JobStatus) CountsAsDoneToday() bool {
	return status == JobStatusSuccess || status == JobStatusAlreadyChecked
}

// ProofSource records where a result came from, so a success can always be
// traced to something the site said. A balance change is not a source and
// has no value here.
type ProofSource string

const (
	// ProofSourceNone is the only valid source for a job that has not
	// finished, and for a local cancellation.
	ProofSourceNone ProofSource = "none"
	// ProofSourceSiteStatus is a read-only status response, which can
	// establish "already checked in" without submitting.
	ProofSourceSiteStatus ProofSource = "site_status"
	// ProofSourceSubmitResponse is the parsed response of the submission.
	ProofSourceSubmitResponse ProofSource = "submit_response"
	// ProofSourceStatusRecheck is a read-only status response fetched after
	// a submission whose own response was lost.
	ProofSourceStatusRecheck ProofSource = "status_recheck"
	// ProofSourceTransportError is a transport or protocol failure, which
	// can only justify a failure, never a success.
	ProofSourceTransportError ProofSource = "transport_error"
)

var proofSources = map[ProofSource]struct{}{
	ProofSourceNone: {}, ProofSourceSiteStatus: {}, ProofSourceSubmitResponse: {},
	ProofSourceStatusRecheck: {}, ProofSourceTransportError: {},
}

func (source ProofSource) Validate() error {
	if _, ok := proofSources[source]; !ok {
		return fmt.Errorf("unknown proof source %q", string(source))
	}
	return nil
}

// provesCheckIn reports whether the source can establish that a check-in
// happened.
func (source ProofSource) provesCheckIn() bool {
	return source == ProofSourceSiteStatus ||
		source == ProofSourceSubmitResponse ||
		source == ProofSourceStatusRecheck
}

// Job is one attempt record. It is private; [JobReceipt] is published.
type Job struct {
	ID        JobID
	AccountID AccountID
	Action    JobAction
	Trigger   JobTrigger
	// RequestID is the caller-fixed idempotency key. The scheduler supplies
	// its own for automatic work.
	RequestID string
	// InputDigest covers the request content behind RequestID, so the same
	// ID with different content is detected instead of replayed.
	InputDigest string
	// ExpectedRevision is the account revision the work was prepared for.
	ExpectedRevision int64
	// SiteDay is the day this job accounts for, in the site's own date when
	// it reports one and otherwise the account's zone.
	SiteDay string
	Status  JobStatus
	// Dispatched records durable permission to submit, committed before I/O.
	// A crash after that commit is possibly sent even if no byte left the
	// machine. It stays true for every later state.
	Dispatched  bool
	ProofSource ProofSource
	Reward      Reward
	// FailureCode is a stable, adapter-owned classification. It is never
	// the site's own message.
	FailureCode string
	// ParentID links a batch child to its parent. A parent has none.
	ParentID    JobID
	CreatedAt   time.Time
	CompletedAt *time.Time
	// Attempts counts executions of this job. It does not license a second
	// submission; see [JobStatusUncertain].
	Attempts int
	// ScheduleDay is the immutable local calendar bucket for automatic work.
	// SiteDay may be corrected by an authoritative site response; that must
	// not reset the daily retry budget. Neither field below is published.
	ScheduleDay    string
	RetryNotBefore *time.Time
}

// Validate rejects job records that could not have happened, so a bug
// upstream of persistence cannot store a success nobody proved.
func (job Job) Validate() error {
	if job.ID == "" {
		return fmt.Errorf("job id is required")
	}
	if err := job.AccountID.Validate(); err != nil {
		return err
	}
	if err := job.Action.Validate(); err != nil {
		return err
	}
	if err := job.Trigger.Validate(); err != nil {
		return err
	}
	if err := job.Status.Validate(); err != nil {
		return err
	}
	if err := job.ProofSource.Validate(); err != nil {
		return err
	}
	if err := ValidateRequestID(job.RequestID); err != nil {
		return err
	}
	if job.InputDigest == "" {
		return fmt.Errorf("input digest is required")
	}
	if job.ExpectedRevision < 1 {
		return fmt.Errorf("expected revision must be positive")
	}
	if job.Attempts < 0 {
		return fmt.Errorf("attempts must not be negative")
	}
	if job.ID == job.ParentID {
		return fmt.Errorf("a job must not be its own parent")
	}
	if err := job.Reward.Validate(); err != nil {
		return err
	}
	if err := job.validateOutcome(); err != nil {
		return err
	}
	return job.validateTiming()
}

func (job Job) validateOutcome() error {
	// A read-only refresh must never report a dispatched write.
	if job.Action == JobActionStatusRefresh && job.Dispatched {
		return fmt.Errorf("a status refresh must not dispatch a submission")
	}
	if !job.Status.Terminal() {
		if job.ProofSource != ProofSourceNone {
			return fmt.Errorf("an unfinished job has no proof")
		}
		if job.Reward.Known || job.FailureCode != "" {
			return fmt.Errorf("an unfinished job has no result")
		}
		return nil
	}
	switch job.Status {
	case JobStatusSuccess:
		// Only the site's own answer establishes a check-in, and a
		// successful check-in must have been submitted by this job.
		if !job.ProofSource.provesCheckIn() {
			return fmt.Errorf("success requires proof from the site, got %q", job.ProofSource)
		}
		if job.Action != JobActionCheckIn {
			return fmt.Errorf("only a check-in job can succeed at checking in")
		}
		if !job.Dispatched {
			return fmt.Errorf("success requires a dispatched submission")
		}
		if job.ProofSource == ProofSourceSiteStatus {
			return fmt.Errorf("a status read alone cannot prove this job checked in")
		}
	case JobStatusAlreadyChecked:
		if !job.ProofSource.provesCheckIn() {
			return fmt.Errorf("an already-checked result requires proof from the site")
		}
	case JobStatusNotChecked:
		if job.Action != JobActionStatusRefresh || job.Dispatched || job.ProofSource != ProofSourceSiteStatus || job.FailureCode != "" {
			return fmt.Errorf("not-checked requires a successful read-only status refresh")
		}
	case JobStatusUncertain:
		if !job.Dispatched {
			return fmt.Errorf("an uncertain result requires a dispatched submission")
		}
	case JobStatusRetryableFailure:
		// Retrying a dispatched submission would risk a second check-in;
		// such a job is uncertain instead.
		if job.Dispatched {
			return fmt.Errorf("a dispatched submission must not be marked retryable")
		}
	case JobStatusCancelled:
		if job.ProofSource != ProofSourceNone {
			return fmt.Errorf("a local cancellation carries no site proof")
		}
	}
	if job.Status != JobStatusSuccess && job.Reward.Known {
		return fmt.Errorf("only a successful check-in carries a reward")
	}
	return nil
}

func (job Job) validateTiming() error {
	if job.ScheduleDay != "" {
		day, err := time.Parse(time.DateOnly, job.ScheduleDay)
		if job.Trigger != JobTriggerAutomatic || err != nil || day.Year() < 1 || day.Format(time.DateOnly) != job.ScheduleDay {
			return fmt.Errorf("invalid automatic schedule day")
		}
	}
	if job.RetryNotBefore != nil && (job.Dispatched || !job.Status.Terminal() || job.Status != JobStatusRateLimited && job.Status != JobStatusRetryableFailure) {
		return fmt.Errorf("only a safe terminal failure carries retry timing")
	}
	if job.CreatedAt.IsZero() {
		return fmt.Errorf("created time is required")
	}
	if job.Status.Terminal() != (job.CompletedAt != nil) {
		return fmt.Errorf("a completion time exists exactly for a finished job")
	}
	if job.CompletedAt != nil && job.CompletedAt.Before(job.CreatedAt) {
		return fmt.Errorf("completion must not precede creation")
	}
	return nil
}

// ValidateRequestID bounds the caller-fixed idempotency key. It must be long
// enough that two unrelated requests cannot collide by accident.
func ValidateRequestID(value string) error {
	if len(value) < minRequestIDLen || len(value) > maxRequestIDLen {
		return fmt.Errorf("request id must be %d-%d characters", minRequestIDLen, maxRequestIDLen)
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character >= '0' && character <= '9',
			character == '-', character == '_':
		default:
			return fmt.Errorf("request id must be url-safe")
		}
	}
	return nil
}

// JobRequest is one accepted write. The digest of its content is what makes
// a replay distinguishable from a different request reusing an ID.
type JobRequest struct {
	RequestID        string
	AccountIDs       []AccountID
	Action           JobAction
	Trigger          JobTrigger
	ExpectedRevision int64
}

func (request JobRequest) Validate() error {
	if err := ValidateRequestID(request.RequestID); err != nil {
		return err
	}
	if err := request.Action.Validate(); err != nil {
		return err
	}
	if err := request.Trigger.Validate(); err != nil {
		return err
	}
	if len(request.AccountIDs) == 0 {
		return fmt.Errorf("at least one account is required")
	}
	if len(request.AccountIDs) > MaxBatchAccounts {
		return fmt.Errorf("a batch may contain at most %d accounts", MaxBatchAccounts)
	}
	seen := make(map[AccountID]struct{}, len(request.AccountIDs))
	for _, id := range request.AccountIDs {
		if err := id.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("account %q appears twice in one batch", id)
		}
		seen[id] = struct{}{}
	}
	// A single-account write pins the revision it was prepared for; a batch
	// pins each account's revision when it runs.
	if len(request.AccountIDs) == 1 && request.ExpectedRevision < 1 {
		return fmt.Errorf("expected revision must be positive")
	}
	if len(request.AccountIDs) > 1 && request.ExpectedRevision != 0 {
		return fmt.Errorf("a batch must not pin one revision")
	}
	return nil
}

// Digest is the content fingerprint stored with the request ID.
func (request JobRequest) Digest() string {
	accounts := make([]string, 0, len(request.AccountIDs))
	for _, id := range request.AccountIDs {
		accounts = append(accounts, string(id))
	}
	sort.Strings(accounts)
	digest := sha256.New()
	for _, field := range []string{
		string(request.Action),
		string(request.Trigger),
		fmt.Sprintf("%d", request.ExpectedRevision),
		strings.Join(accounts, ","),
	} {
		fmt.Fprintf(digest, "%d:%s\n", len(field), field)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// IdempotencyOutcome is what a repeated request ID means.
type IdempotencyOutcome string

const (
	// IdempotencyCreated is a request ID seen for the first time.
	IdempotencyCreated IdempotencyOutcome = "created"
	// IdempotencyReplayed is the same ID with the same content: the stored
	// receipt is returned and no new work starts.
	IdempotencyReplayed IdempotencyOutcome = "replayed"
	// IdempotencyConflict is the same ID with different content, answered
	// 409 rather than overwriting the first request.
	IdempotencyConflict IdempotencyOutcome = "conflict"
)

// ClassifyRequest decides how an incoming request relates to what is
// already stored under its ID. storedDigest is empty when the ID is new.
func ClassifyRequest(storedDigest string, request JobRequest) IdempotencyOutcome {
	switch {
	case storedDigest == "":
		return IdempotencyCreated
	case storedDigest == request.Digest():
		return IdempotencyReplayed
	default:
		return IdempotencyConflict
	}
}

// JobReceipt is the published job shape. It carries no session, no site
// message and no request content beyond the caller's own ID.
type JobReceipt struct {
	ID          JobID       `json:"id"`
	AccountID   AccountID   `json:"account_id"`
	Action      JobAction   `json:"action"`
	Trigger     JobTrigger  `json:"trigger"`
	RequestID   string      `json:"request_id"`
	Status      JobStatus   `json:"status"`
	Dispatched  bool        `json:"dispatched"`
	ProofSource ProofSource `json:"proof_source"`
	SiteDay     string      `json:"site_day,omitempty"`
	FailureCode string      `json:"failure_code,omitempty"`
	ParentID    JobID       `json:"parent_id,omitempty"`
	Reward      *RewardView `json:"reward,omitempty"`
	CreatedAt   time.Time   `json:"created_at"`
	CompletedAt *time.Time  `json:"completed_at,omitempty"`
	Attempts    int         `json:"attempts"`
}

// RewardView publishes an award only when the site stated one in a unit the
// adapter recognized. The UI shows "unknown" rather than an estimate.
type RewardView struct {
	Quota int64  `json:"quota"`
	Unit  string `json:"unit"`
}

// Receipt projects the public job shape.
func (job Job) Receipt() JobReceipt {
	receipt := JobReceipt{
		ID:          job.ID,
		AccountID:   job.AccountID,
		Action:      job.Action,
		Trigger:     job.Trigger,
		RequestID:   job.RequestID,
		Status:      job.Status,
		Dispatched:  job.Dispatched,
		ProofSource: job.ProofSource,
		SiteDay:     job.SiteDay,
		FailureCode: job.FailureCode,
		ParentID:    job.ParentID,
		CreatedAt:   job.CreatedAt,
		CompletedAt: job.CompletedAt,
		Attempts:    job.Attempts,
	}
	if job.Reward.Known {
		receipt.Reward = &RewardView{Quota: job.Reward.Quota, Unit: job.Reward.Unit}
	}
	return receipt
}

// BatchReceipt is the 202 body of a batch write: one parent plus the child
// per account, so a lost response can be resumed by the same request ID.
type BatchReceipt struct {
	ParentID  JobID        `json:"parent_id"`
	RequestID string       `json:"request_id"`
	Children  []JobReceipt `json:"children"`
}

// ExtensionStatus is the only shape available while the extension is off.
// It is what the workspace needs to show an entry point and let the
// operator turn the feature on; it touches no table and makes no site
// request.
type ExtensionStatus struct {
	ProtocolVersion int `json:"protocol_version"`
	// Enabled is the persisted switch, default false.
	Enabled bool `json:"enabled"`
	// Initialized is true once the extension's own storage and scheduler
	// are running. It stays false while disabled.
	Initialized bool `json:"initialized"`
	// LastInitError is a stable code when enabling failed, so the operator
	// can retry instead of seeing an empty page.
	LastInitError string `json:"last_init_error,omitempty"`
	// AccountCount is zero while disabled rather than read from storage.
	AccountCount int `json:"account_count"`
	// NativeCaptureSupported reflects the platform admission recorded by
	// the host. False means manual check-in, not a silent fallback.
	NativeCaptureSupported bool `json:"native_capture_supported"`
}

// ListOptions is the shared paging input. A zero limit means
// [DefaultPageSize]; anything above [MaxPageSize] is rejected rather than
// silently clamped, so a caller cannot believe it received everything.
type ListOptions struct {
	Limit  int
	Cursor string
}

func (options ListOptions) Validate() error {
	if options.Limit < 0 || options.Limit > MaxPageSize {
		return fmt.Errorf("limit must be between 1 and %d", MaxPageSize)
	}
	if len(options.Cursor) > 256 {
		return fmt.Errorf("cursor is too long")
	}
	return nil
}

// EffectiveLimit resolves the page size actually used.
func (options ListOptions) EffectiveLimit() int {
	if options.Limit == 0 {
		return DefaultPageSize
	}
	return options.Limit
}

// JobPage is a page of receipts. NextCursor is empty on the last page.
type JobPage struct {
	Items      []JobReceipt `json:"items"`
	NextCursor string       `json:"next_cursor,omitempty"`
}
