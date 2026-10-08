package forkcheckin

import (
	"context"
	"errors"
	"time"
)

// ErrJobNotCancellable: the job already finished. Cancelling it would claim
// to stop work that is over.
var ErrJobNotCancellable = errors.New("check-in job already finished")

// Write routes for jobs. A request_id belongs to exactly one route; the
// creation routes are the ones the job DAO already records.
const (
	WriteRouteJobCreate = "jobs"
	WriteRouteJobBatch  = "jobs/batch"
	WriteRouteJobCancel = "jobs.cancel"
)

// BatchIDPrefix marks a batch parent. A parent is not an executable job.
const BatchIDPrefix = "batch_"

// JobCreateRequest is POST /jobs. It names accounts and an action only: no
// URL, method, header or body can be passed through to a site.
type JobCreateRequest struct {
	RequestID        string      `json:"request_id"`
	Action           JobAction   `json:"action"`
	Accounts         []AccountID `json:"accounts"`
	ExpectedRevision int64       `json:"expected_revision,omitempty"`
}

// JobRequest is the stored, manual form of the request.
func (request JobCreateRequest) JobRequest() JobRequest {
	return JobRequest{
		RequestID: request.RequestID, AccountIDs: append([]AccountID(nil), request.Accounts...),
		Action: request.Action, Trigger: JobTriggerManual, ExpectedRevision: request.ExpectedRevision,
	}
}

// JobCancelRequest is POST /jobs/{id}/cancel.
type JobCancelRequest struct {
	RequestID string `json:"request_id"`
}

// Fingerprint binds a cancel request id to the job it names.
func (request JobCancelRequest) Fingerprint(id JobID) string {
	return writeFingerprint(WriteRouteJobCancel, AccountID(id), struct{}{})
}

// JobWriteResult is the stored public receipt plus the in-process follow-up
// the write requires. Launch names queued jobs to start in the background;
// Interrupt names running jobs whose execution should stop now. Neither is
// part of the response.
type JobWriteResult struct {
	AccountWriteResult
	Launch    []JobID
	Interrupt []JobID
}

// JobWriter accepts and cancels jobs. It never contacts a site: execution is
// started afterwards, outside the request, by the Facade.
type JobWriter interface {
	CreateJobs(context.Context, JobCreateRequest) (JobWriteResult, error)
	CancelJob(context.Context, JobID, JobCancelRequest) (JobWriteResult, error)
}

// batchStatusOrder ranks child outcomes for a parent's aggregate status. Live
// work comes first, then outcomes that need the operator, then settled ones,
// so a parent never looks better than its worst child.
var batchStatusOrder = []JobStatus{
	JobStatusRunning, JobStatusQueued,
	JobStatusUncertain, JobStatusAuthRequired, JobStatusManualRequired,
	JobStatusRateLimited, JobStatusRetryableFailure, JobStatusUnsupported,
	JobStatusCancelled, JobStatusNotChecked, JobStatusAlreadyChecked, JobStatusSuccess,
}

// AggregateBatch projects a batch parent from its children's current state.
// The parent has no proof or reward of its own; it is finished only when
// every child is.
func AggregateBatch(id JobID, children []JobReceipt) PublicJob {
	parent := PublicJob{ID: id, Status: JobStatusQueued, ProofSource: PublicProofNone, Children: make([]PublicJob, 0, len(children))}
	rank := len(batchStatusOrder)
	var finished *time.Time
	for index, child := range children {
		public := child.Public()
		parent.Children = append(parent.Children, public)
		if index == 0 || child.CreatedAt.Before(parent.CreatedAt) {
			parent.Action, parent.CreatedAt = child.Action, child.CreatedAt
		}
		parent.Dispatched = parent.Dispatched || child.Dispatched
		for position, status := range batchStatusOrder {
			if status == child.Status && position < rank {
				rank = position
			}
		}
		if child.CompletedAt != nil && (finished == nil || child.CompletedAt.After(*finished)) {
			value := *child.CompletedAt
			finished = &value
		}
	}
	if rank < len(batchStatusOrder) {
		parent.Status = batchStatusOrder[rank]
	}
	if parent.Status.Terminal() {
		parent.FinishedAt = finished
	}
	return parent
}
