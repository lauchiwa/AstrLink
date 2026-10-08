package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

func forkCheckinJobDraft(accountID forkcheckin.AccountID, requestID string, marker byte) forkcheckin.Job {
	digest := sha256.Sum256([]byte{marker})
	return forkcheckin.Job{
		AccountID: accountID, Action: forkcheckin.JobActionCheckIn, Trigger: forkcheckin.JobTriggerManual,
		RequestID: requestID, InputDigest: hex.EncodeToString(digest[:]), ExpectedRevision: 1,
		SiteDay: time.Now().UTC().Format(time.DateOnly), Status: forkcheckin.JobStatusQueued,
		ProofSource: forkcheckin.ProofSourceNone,
	}
}

func createForkCheckinJob(t *testing.T, store *Store, job forkcheckin.Job) ForkCheckinCreateResult {
	t.Helper()
	result, err := store.CreateForkCheckinJobOnce(context.Background(), job)
	if err != nil {
		t.Fatalf("CreateForkCheckinJobOnce: %v", err)
	}
	return result
}

func createForkCheckinJobAccount(t *testing.T, store *Store, id forkcheckin.AccountID) {
	t.Helper()
	if _, err := store.CreateForkCheckinAccount(context.Background(), forkCheckinDraft(id, "https://relay.example/")); err != nil {
		t.Fatalf("create job account: %v", err)
	}
}

func TestForkCheckinJobCreateOncePersistsAndReplaysReceiptAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "astrlink.db")
	store := openTestStore(t, path)
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	createForkCheckinJobAccount(t, store, "acct_receipt")
	job := forkCheckinJobDraft("acct_receipt", "request_receipt_01", 'a')
	job.ID = "job_receipt_01"
	job.InputDigest = digestText("request-body-fingerprint")
	created := createForkCheckinJob(t, store, job)
	if created.Outcome != forkcheckin.IdempotencyCreated {
		t.Fatalf("first outcome = %q, want created", created.Outcome)
	}
	initial, err := store.LookupForkCheckinReceipt(ctx, job.RequestID)
	if err != nil || initial.Status != 202 || !json.Valid(initial.Body) {
		t.Fatalf("initial receipt = %+v, %v", initial, err)
	}
	if bytes.Contains(initial.Body, []byte(job.InputDigest)) || bytes.Contains(initial.Body, []byte("request-body-fingerprint")) {
		t.Fatal("receipt contains request-private material")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	t.Cleanup(func() { _ = reopened.Close() })
	replayed := createForkCheckinJob(t, reopened, job)
	if replayed.Outcome != forkcheckin.IdempotencyReplayed || replayed.Job.ID != created.Job.ID {
		t.Fatalf("replay = %+v, want original job", replayed)
	}
	receipt, err := reopened.LookupForkCheckinReceipt(ctx, job.RequestID)
	if err != nil || receipt.Status != initial.Status || !bytes.Equal(receipt.Body, initial.Body) || !receipt.CreatedAt.Equal(initial.CreatedAt) {
		t.Fatalf("receipt changed across restart: before=%+v after=%+v, err=%v", initial, receipt, err)
	}
	if countRows(t, reopened, forkCheckinJobs) != 1 || countRows(t, reopened, forkCheckinReceipts) != 1 {
		t.Fatal("replay created duplicate jobs or receipts")
	}
}

func TestForkCheckinJobCreateOnceRejectsRequestIDReuseWithDifferentContent(t *testing.T) {
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_conflict")
	createForkCheckinJobAccount(t, store, "acct_other_conflict")
	job := forkCheckinJobDraft("acct_conflict", "request_reuse_01", 'a')
	createForkCheckinJob(t, store, job)
	different := job
	different.InputDigest = digestText("different-input")
	if _, err := store.CreateForkCheckinJobOnce(context.Background(), different); !errors.Is(err, storagecontract.ErrConflict) {
		t.Fatalf("reused request id error = %v, want ErrConflict", err)
	}
	crossAccount := job
	crossAccount.AccountID = "acct_other_conflict"
	if _, err := store.CreateForkCheckinJobOnce(context.Background(), crossAccount); !errors.Is(err, storagecontract.ErrConflict) {
		t.Fatalf("same digest reused across accounts = %v, want ErrConflict", err)
	}
	if countRows(t, store, forkCheckinJobs) != 1 || countRows(t, store, forkCheckinReceipts) != 1 {
		t.Fatal("conflicting request changed the original")
	}
}

func TestForkCheckinClaimHasOneWinnerAcrossStoreConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "astrlink.db")
	first := openTestStore(t, path)
	if err := first.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	createForkCheckinJobAccount(t, first, "acct_race")
	job := forkCheckinJobDraft("acct_race", "request_race_01", 'a')
	createForkCheckinJob(t, first, job)
	second := openTestStore(t, path)
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })
	start := make(chan struct{})
	type outcome struct {
		lease ForkCheckinLease
		err   error
	}
	results := make(chan outcome, 2)
	var workers sync.WaitGroup
	for _, store := range []*Store{first, second} {
		workers.Add(1)
		go func(store *Store) {
			defer workers.Done()
			<-start
			lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute)
			results <- outcome{lease: lease, err: err}
		}(store)
	}
	close(start)
	workers.Wait()
	close(results)
	wins := 0
	for result := range results {
		if result.err == nil {
			wins++
		} else if !errors.Is(result.err, ErrForkCheckinLeaseHeld) {
			t.Fatalf("losing claim = %v, want lease held", result.err)
		}
	}
	if wins != 1 || countRows(t, first, forkCheckinAttempts) != 1 {
		t.Fatalf("claim winners=%d attempts=%d, want one each", wins, countRows(t, first, forkCheckinAttempts))
	}
}

func TestForkCheckinExpiredLeaseFencesOldWorkerAndKeepsAttempts(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_lease")
	job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_lease", "request_lease_01", 'a')).Job
	now := time.Now().UTC().Truncate(time.Second)
	store.now = func() time.Time { return now }
	oldLease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	newLease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute)
	if err != nil || newLease.Attempt != oldLease.Attempt+1 || newLease.Token == oldLease.Token {
		t.Fatalf("new lease=%+v old=%+v err=%v", newLease, oldLease, err)
	}
	completion := ForkCheckinCompletion{Status: forkcheckin.JobStatusRetryableFailure,
		ProofSource: forkcheckin.ProofSourceTransportError, FailureCode: "transport_before_dispatch"}
	if _, err := store.CompleteForkCheckinJob(ctx, oldLease, completion); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("old worker completion = %v, want ErrPrecondition", err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, newLease, completion); err != nil {
		t.Fatalf("current worker completion: %v", err)
	}
	stored, err := store.GetForkCheckinJob(ctx, job.ID)
	if err != nil || stored.Attempts != 2 {
		t.Fatalf("attempt count=%d err=%v, want 2", stored.Attempts, err)
	}
	var outcomes []string
	rows, err := store.db.QueryContext(ctx, `SELECT outcome FROM fork_checkin_job_attempts WHERE job_id = ? ORDER BY attempt`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		outcomes = append(outcomes, value)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 2 || outcomes[0] != "lease_expired" || outcomes[1] != string(forkcheckin.JobStatusRetryableFailure) {
		t.Fatalf("attempt outcomes = %v", outcomes)
	}
}

func TestForkCheckinDispatchedJobOnlyAllowsReadOnlyRecovery(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "astrlink.db")
	store := openTestStore(t, path)
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	createForkCheckinJobAccount(t, store, "acct_dispatched")
	job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_dispatched", "request_dispatch_01", 'a')).Job
	lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, lease); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("repeated dispatch must not grant another send: %v", err)
	}
	if _, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute); !errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatalf("second submit claim = %v, want lease held", err)
	}
	createForkCheckinJob(t, store, forkCheckinJobDraft(job.AccountID, "request_dispatch_02", 'b'))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openTestStore(t, path)
	t.Cleanup(func() { _ = reopened.Close() })
	reopened.now = func() time.Time { return lease.ExpiresAt.Add(time.Second) }
	if _, err := reopened.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute); !errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatalf("claim after restart = %v, want lease held", err)
	}
	recovery, err := reopened.RecoverForkCheckinDispatch(ctx, job.ID, job.AccountID, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopened.MarkForkCheckinDispatched(ctx, recovery); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("recovery lease dispatched = %v, want ErrPrecondition", err)
	}
	if _, err := reopened.CompleteForkCheckinJob(ctx, recovery, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceSubmitResponse,
	}); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("recovery accepted submit response: %v", err)
	}
	completed, err := reopened.CompleteForkCheckinJob(ctx, recovery, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceStatusRecheck,
		ReadOnly: true, Reward: forkcheckin.Reward{Known: true, Quota: 12, Unit: "quota"},
	})
	if err != nil || completed.Status != forkcheckin.JobStatusSuccess || !completed.Dispatched {
		t.Fatalf("read-only recovery result=%+v err=%v", completed, err)
	}
	var phases []string
	rows, err := reopened.db.QueryContext(ctx, `SELECT phase FROM fork_checkin_job_attempts WHERE job_id = ? ORDER BY attempt`, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var phase string
		if err := rows.Scan(&phase); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		phases = append(phases, phase)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(phases) != 2 || phases[0] != "submission" || phases[1] != "status_recheck" {
		t.Fatalf("attempt phases = %v", phases)
	}
	receipt, err := reopened.LookupForkCheckinReceipt(ctx, job.RequestID)
	initial, marshalErr := json.Marshal(job.Receipt().Public())
	if err != nil || marshalErr != nil || !bytes.Equal(receipt.Body, initial) {
		t.Fatalf("receipt changed: %s err=%v marshal=%v", receipt.Body, err, marshalErr)
	}
}

func TestForkCheckinDispatchedSubmitResponseCompletesUnderOriginalLease(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_submit_response")
	job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_submit_response", "request_submit_response_01", 's')).Job
	lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	completed, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceSubmitResponse,
		Reward: forkcheckin.Reward{Known: true, Quota: 7, Unit: "quota"},
	})
	if err != nil || completed.Status != forkcheckin.JobStatusSuccess || completed.ProofSource != forkcheckin.ProofSourceSubmitResponse {
		t.Fatalf("submit response completion=%+v err=%v", completed, err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusUncertain, ProofSource: forkcheckin.ProofSourceTransportError,
	}); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("old lease completed twice: %v", err)
	}
}

func TestForkCheckinSameDaySuccessDeduplicatesButSafeFailureDoesNot(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_daily_success")
	first := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_daily_success", "request_daily_01", '1')).Job
	lease, err := store.ClaimForkCheckin(ctx, first.AccountID, first.Action, first.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusRetryableFailure, ProofSource: forkcheckin.ProofSourceTransportError,
		FailureCode: "connect_failed_before_dispatch",
	}); err != nil {
		t.Fatal(err)
	}
	second := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_daily_success", "request_daily_02", '2')).Job
	secondLease, err := store.ClaimForkCheckin(ctx, second.AccountID, second.Action, second.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatalf("safe retry was blocked: %v", err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, secondLease); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, secondLease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceSubmitResponse,
	}); err != nil {
		t.Fatal(err)
	}
	third := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_daily_success", "request_daily_03", '3')).Job
	if _, err := store.ClaimForkCheckin(ctx, third.AccountID, third.Action, third.SiteDay, 1, time.Minute); !errors.Is(err, storagecontract.ErrConflict) {
		t.Fatalf("claim after same-day success = %v, want ErrConflict", err)
	}
}

func TestForkCheckinUncertainDispatchCannotBeResubmitted(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_daily_uncertain")
	first := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_daily_uncertain", "request_uncertain_01", 'u')).Job
	lease, err := store.ClaimForkCheckin(ctx, first.AccountID, first.Action, first.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusUncertain, ProofSource: forkcheckin.ProofSourceTransportError,
	}); err != nil {
		t.Fatal(err)
	}
	second := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_daily_uncertain", "request_uncertain_02", 'v')).Job
	if _, err := store.ClaimForkCheckin(ctx, second.AccountID, second.Action, second.SiteDay, 1, time.Minute); !errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatalf("claim after uncertain dispatch = %v, want lease held", err)
	}
}

func TestForkCheckinSafeFailureAllowsAnExplicitNewDailyRetry(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_retry")
	first := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_retry", "request_retry_01", 'a')).Job
	lease, err := store.ClaimForkCheckin(ctx, first.AccountID, first.Action, first.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusRetryableFailure, ProofSource: forkcheckin.ProofSourceTransportError,
		FailureCode: "connect_failed_before_dispatch",
	}); err != nil {
		t.Fatal(err)
	}
	second := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_retry", "request_retry_02", 'b')).Job
	if lease, err := store.ClaimForkCheckin(ctx, second.AccountID, second.Action, second.SiteDay, 1, time.Minute); err != nil || lease.JobID != second.ID {
		t.Fatalf("explicit same-day retry lease=%+v err=%v", lease, err)
	}
}

func TestForkCheckinChildReceiptsUseStablePaginationAcrossAccounts(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	ids := []forkcheckin.AccountID{"acct_child_c", "acct_child_a", "acct_child_b"}
	days := make(map[forkcheckin.AccountID]string)
	for _, id := range ids {
		createForkCheckinJobAccount(t, store, id)
		days[id] = time.Now().UTC().Format(time.DateOnly)
	}
	request := forkcheckin.JobRequest{RequestID: "request_batch_01", AccountIDs: ids,
		Action: forkcheckin.JobActionCheckIn, Trigger: forkcheckin.JobTriggerManual}
	created, err := store.CreateForkCheckinBatchOnce(ctx, request, days)
	if err != nil {
		t.Fatal(err)
	}
	page, err := store.ListForkCheckinChildren(ctx, created.Batch.ParentID, forkcheckin.ListOptions{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" {
		t.Fatalf("first child page=%+v err=%v", page, err)
	}
	next, err := store.ListForkCheckinChildren(ctx, created.Batch.ParentID, forkcheckin.ListOptions{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(next.Items) != 1 || next.NextCursor != "" || next.Items[0].ID <= page.Items[1].ID {
		t.Fatalf("second child page=%+v err=%v", next, err)
	}
	seen := make(map[forkcheckin.AccountID]bool)
	for _, child := range append(page.Items, next.Items...) {
		if child.ParentID != created.Batch.ParentID || seen[child.AccountID] {
			t.Fatal("batch child membership is inconsistent")
		}
		seen[child.AccountID] = true
		if _, err := store.ClaimForkCheckin(ctx, child.AccountID, child.Action, child.SiteDay, 1, time.Minute); err != nil {
			t.Fatalf("claim child: %v", err)
		}
	}
	if countRows(t, store, forkCheckinJobs) != 3 || countRows(t, store, forkCheckinAttempts) != 3 {
		t.Fatal("batch parent must never be an executable job")
	}
	if _, err := store.ListForkCheckinChildren(ctx, created.Batch.ParentID, forkcheckin.ListOptions{Limit: forkcheckin.MaxPageSize + 1}); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("oversized page: %v", err)
	}
}

func TestForkCheckinJobCreateOnceValidatesDigestAndRequestID(t *testing.T) {
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_validate")
	badID := forkCheckinJobDraft("acct_validate", "short", 'a')
	badDigest := forkCheckinJobDraft("acct_validate", "request_digest_01", 'a')
	badDigest.InputDigest = "not-a-digest"
	badDate := forkCheckinJobDraft("acct_validate", "request_bad_date_01", 'd')
	badDate.SiteDay = "not-a-date"
	for _, job := range []forkcheckin.Job{badID, badDigest, badDate} {
		if _, err := store.CreateForkCheckinJobOnce(context.Background(), job); !errors.Is(err, storagecontract.ErrInvalidArgument) {
			t.Fatalf("invalid job input = %v, want ErrInvalidArgument", err)
		}
	}
	if countRows(t, store, forkCheckinJobs) != 0 || countRows(t, store, forkCheckinReceipts) != 0 {
		t.Fatal("invalid request wrote a job or receipt")
	}
}

func TestForkCheckinCompletionRejectsOldAccountRevision(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	account, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_revision", "https://relay.example/"))
	if err != nil {
		t.Fatal(err)
	}
	job := createForkCheckinJob(t, store, forkCheckinJobDraft(account.ID, "request_revision_01", 'a')).Job
	lease, err := store.ClaimForkCheckin(ctx, account.ID, job.Action, job.SiteDay, account.Revision, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	updated := account
	updated.TimeZone = "UTC"
	if _, err := store.UpdateForkCheckinAccount(ctx, updated, account.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusRetryableFailure, ProofSource: forkcheckin.ProofSourceTransportError,
		FailureCode: "not_dispatched",
	}); !errors.Is(err, forkcheckin.ErrRevisionChanged) {
		t.Fatalf("completion on old revision = %v, want ErrRevisionChanged", err)
	}
}

func TestForkCheckinJobLookupMissingReturnsNotFound(t *testing.T) {
	store := openForkCheckinStore(t)
	if _, err := store.LookupForkCheckinReceipt(context.Background(), "request_missing_01"); !errors.Is(err, storagecontract.ErrNotFound) {
		t.Fatalf("missing receipt = %v, want ErrNotFound", err)
	}
	if _, err := store.GetForkCheckinJob(context.Background(), "job_missing"); !errors.Is(err, storagecontract.ErrNotFound) {
		t.Fatalf("missing job = %v, want ErrNotFound", err)
	}
}

func TestForkCheckinReadOnlyRefreshCannotMarkSubmissionDispatched(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_refresh")
	job := forkCheckinJobDraft("acct_refresh", "request_refresh_01", 'r')
	job.Action = forkcheckin.JobActionStatusRefresh
	created := createForkCheckinJob(t, store, job)
	lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, job.ExpectedRevision, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, lease); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("status refresh dispatch = %v, want ErrPrecondition", err)
	}
	completed, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusAlreadyChecked, ProofSource: forkcheckin.ProofSourceSiteStatus,
	})
	if err != nil || completed.Dispatched || completed.Status != forkcheckin.JobStatusAlreadyChecked {
		t.Fatalf("status refresh completion=%+v err=%v created=%s", completed, err, created.Job.ID)
	}
}

func digestText(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
