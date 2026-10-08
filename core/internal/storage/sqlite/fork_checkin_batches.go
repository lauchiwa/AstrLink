package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// ForkCheckinBatchResult contains the original acceptance receipt. Current
// child states are read through ListForkCheckinChildren, not by rewriting this
// receipt when a child finishes or its account is deleted.
type ForkCheckinBatchResult struct {
	Batch   forkcheckin.BatchReceipt
	Receipt ForkCheckinStoredReceipt
	Outcome forkcheckin.IdempotencyOutcome
}

// CreateForkCheckinBatchOnce atomically accepts a multi-account request. The
// caller resolves each site's day before entering storage; no network work
// happens in the transaction. Day and revision snapshots are server-owned and
// are ignored on replay. The request digest uses K01's sorted account set.
func (store *Store) CreateForkCheckinBatchOnce(ctx context.Context, request forkcheckin.JobRequest, siteDays map[forkcheckin.AccountID]string) (result ForkCheckinBatchResult, err error) {
	if err := request.Validate(); err != nil {
		return result, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	if len(request.AccountIDs) < 2 {
		return result, fmt.Errorf("%w: a batch requires multiple accounts", storagecontract.ErrInvalidArgument)
	}
	tx, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return result, err
	}
	defer rollbackOnError(tx, &err)
	if err = lockForkCheckinWrites(ctx, tx); err != nil {
		return result, err
	}
	stored, digest, _, lookupErr := readForkCheckinReceipt(ctx, tx, request.RequestID)
	if lookupErr == nil {
		if stored.Route != "jobs/batch" || digest != request.Digest() {
			return result, storagecontract.ErrConflict
		}
		if err = json.Unmarshal(stored.snapshot, &result.Batch); err != nil {
			return result, storagecontract.ErrInvalidRecord
		}
		if result.Batch.ParentID == "" || len(result.Batch.Children) != len(request.AccountIDs) {
			return result, storagecontract.ErrInvalidRecord
		}
		if err = tx.Commit(); err != nil {
			return result, err
		}
		result.Receipt, result.Outcome = stored, forkcheckin.IdempotencyReplayed
		return result, nil
	}
	if !errors.Is(lookupErr, storagecontract.ErrNotFound) {
		return result, lookupErr
	}
	if len(siteDays) != len(request.AccountIDs) {
		return result, fmt.Errorf("%w: every batch account requires a site day", storagecontract.ErrInvalidArgument)
	}
	parentID, err := newForkCheckinToken("batch_")
	if err != nil {
		return result, err
	}
	ids := append([]forkcheckin.AccountID(nil), request.AccountIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	jobs := make([]forkcheckin.Job, 0, len(ids))
	batch := forkcheckin.BatchReceipt{ParentID: forkcheckin.JobID(parentID), RequestID: request.RequestID}
	now := store.now().UTC()
	for _, id := range ids {
		revision, readErr := lockForkCheckinAccount(ctx, tx, id)
		if readErr != nil {
			return result, readErr
		}
		childKey := sha256.Sum256([]byte(request.RequestID + ":" + string(id)))
		childRequest := forkcheckin.JobRequest{
			RequestID: "child_" + hex.EncodeToString(childKey[:]), AccountIDs: []forkcheckin.AccountID{id},
			Action: request.Action, Trigger: request.Trigger, ExpectedRevision: revision,
		}
		job, prepareErr := prepareForkCheckinJob(store, forkcheckin.Job{
			AccountID: id, Action: request.Action, Trigger: request.Trigger,
			RequestID: childRequest.RequestID, InputDigest: childRequest.Digest(), ExpectedRevision: revision,
			SiteDay: siteDays[id], ProofSource: forkcheckin.ProofSourceNone, CreatedAt: now,
		})
		if prepareErr != nil {
			return result, prepareErr
		}
		jobs = append(jobs, job)
		child := job.Receipt()
		child.ParentID = batch.ParentID
		batch.Children = append(batch.Children, child)
	}
	body, err := json.Marshal(batch.Public())
	if err != nil {
		return result, err
	}
	snapshot, err := json.Marshal(batch)
	if err != nil {
		return result, err
	}
	stored = ForkCheckinStoredReceipt{RequestID: request.RequestID, Route: "jobs/batch", Status: 202, Body: body, CreatedAt: now, snapshot: snapshot}
	if err = insertForkCheckinReceipt(ctx, tx, stored, request.Digest(), 0); err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO fork_checkin_batches(id, request_id) VALUES (?, ?)`, parentID, request.RequestID); err != nil {
		return result, err
	}
	for index, job := range jobs {
		if err = insertForkCheckinJob(ctx, tx, job); err != nil {
			return result, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO fork_checkin_batch_jobs(batch_id, job_id) VALUES (?, ?)`, parentID, job.ID); err != nil {
			return result, err
		}
		childBody, marshalErr := json.Marshal(batch.Children[index].Public())
		if marshalErr != nil {
			return result, marshalErr
		}
		childSnapshot, marshalErr := json.Marshal(batch.Children[index])
		if marshalErr != nil {
			return result, marshalErr
		}
		childReceipt := ForkCheckinStoredReceipt{RequestID: job.RequestID, Route: "jobs/batch_child", Status: 202, Body: childBody, CreatedAt: now, snapshot: childSnapshot}
		if err = insertForkCheckinReceipt(ctx, tx, childReceipt, job.InputDigest, job.ExpectedRevision); err != nil {
			return result, err
		}
	}
	if err = tx.Commit(); err != nil {
		return result, err
	}
	return ForkCheckinBatchResult{Batch: batch, Receipt: stored, Outcome: forkcheckin.IdempotencyCreated}, nil
}
