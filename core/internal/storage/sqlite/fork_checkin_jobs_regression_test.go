package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestForkCheckinDispatchExpiryAndRecoveryFenceEveryWorker(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_fenced")
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	store.now = func() time.Time { return now }
	job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_fenced", "request_fenced_01", 'f')).Job
	lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecoverForkCheckinDispatch(ctx, job.ID, job.AccountID, 1, time.Second); !errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatalf("recovery stole a live submission lease: %v", err)
	}
	now = lease.ExpiresAt
	completion := ForkCheckinCompletion{Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceSubmitResponse}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, completion); !errors.Is(err, ErrForkCheckinLeaseExpired) {
		t.Fatalf("expired submission lease completed: %v", err)
	}
	recovery, err := store.RecoverForkCheckinDispatch(ctx, job.ID, job.AccountID, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, completion); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("replaced submission worker completed: %v", err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, recovery); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("recovery obtained submission permission: %v", err)
	}
	if _, err := store.RecoverForkCheckinDispatch(ctx, job.ID, job.AccountID, 1, time.Second); !errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatalf("two recovery workers acquired the account: %v", err)
	}
	now = recovery.ExpiresAt
	completion.ReadOnly, completion.ProofSource = true, forkcheckin.ProofSourceStatusRecheck
	if _, err := store.CompleteForkCheckinJob(ctx, recovery, completion); !errors.Is(err, ErrForkCheckinLeaseExpired) {
		t.Fatalf("expired recovery completed: %v", err)
	}
	last, err := store.RecoverForkCheckinDispatch(ctx, job.ID, job.AccountID, 1, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, recovery, completion); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("replaced recovery completed: %v", err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, last, completion); err != nil {
		t.Fatal(err)
	}
	if countRows(t, store, forkCheckinAttempts) != 3 {
		t.Fatal("recovery lost independent attempt history")
	}
}

func TestForkCheckinLeaseOrderingAcrossFractionalSeconds(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_nanos")
	now := time.Now().UTC().Truncate(time.Second).Add(time.Second)
	store.now = func() time.Time { return now }
	job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_nanos", "request_nanos_01", 'n')).Job
	lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	// RFC3339Nano would lexically sort the exact second after this expiry.
	if _, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Second); !errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatalf("nanosecond lease was treated as expired: %v", err)
	}
	now = lease.ExpiresAt
	if _, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Second); err != nil {
		t.Fatalf("lease at exact expiry was not released: %v", err)
	}
}

func TestForkCheckinReplaySurvivesMidnightRevisionAndAccountDeletion(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_deleted")
	input := forkCheckinJobDraft("acct_deleted", "request_deleted_01", 'd')
	first := createForkCheckinJob(t, store, input)
	account, err := store.GetForkCheckinAccount(ctx, input.AccountID)
	if err != nil {
		t.Fatal(err)
	}
	account.TimeZone = "UTC"
	if _, err := store.UpdateForkCheckinAccount(ctx, account, 1); err != nil {
		t.Fatal(err)
	}
	input.SiteDay = time.Now().UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	if got := createForkCheckinJob(t, store, input); got.Outcome != forkcheckin.IdempotencyReplayed || !bytes.Equal(got.Receipt.Body, first.Receipt.Body) {
		t.Fatal("revision or midnight changed the receipt")
	}
	if err := store.DeleteForkCheckinAccount(ctx, input.AccountID, 2); err != nil {
		t.Fatal(err)
	}
	if got := createForkCheckinJob(t, store, input); got.Outcome != forkcheckin.IdempotencyReplayed || !bytes.Equal(got.Receipt.Body, first.Receipt.Body) {
		t.Fatal("deleted account changed the receipt")
	}
	input.InputDigest = digestText("different_after_deletion")
	if _, err := store.CreateForkCheckinJobOnce(ctx, input); !errors.Is(err, storagecontract.ErrConflict) {
		t.Fatalf("deleted account allowed request ID reuse: %v", err)
	}
	if countRows(t, store, forkCheckinJobs) != 0 {
		t.Fatal("replay resurrected a deleted job")
	}
}

func TestForkCheckinCreateOnceSerializesConcurrentRequests(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "concurrent.db")
	one, two := openTestStore(t, path), openTestStore(t, path)
	defer one.Close()
	defer two.Close()
	if err := one.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	createForkCheckinJobAccount(t, one, "acct_create_race")
	job := forkCheckinJobDraft("acct_create_race", "request_create_race_01", 'c')
	start := make(chan struct{})
	type outcome struct {
		result ForkCheckinCreateResult
		err    error
	}
	results := make(chan outcome, 8)
	var wg sync.WaitGroup
	for index := 0; index < 8; index++ {
		store := []*Store{one, two}[index%2]
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			result, err := store.CreateForkCheckinJobOnce(ctx, job)
			results <- outcome{result, err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	created := 0
	var body []byte
	for got := range results {
		if got.err != nil {
			t.Fatal(got.err)
		}
		if got.result.Outcome == forkcheckin.IdempotencyCreated {
			created++
		}
		if body != nil && !bytes.Equal(body, got.result.Receipt.Body) {
			t.Fatal("concurrent callers received different acceptances")
		}
		body = got.result.Receipt.Body
	}
	if created != 1 || countRows(t, one, forkCheckinJobs) != 1 || countRows(t, one, forkCheckinReceipts) != 1 {
		t.Fatal("concurrent create was not atomic")
	}
}

func TestForkCheckinManualAutomaticAndRefreshShareAccountLease(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_shared_lease")
	job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_shared_lease", "request_shared_manual", 'm')).Job
	automatic := forkCheckinJobDraft(job.AccountID, "request_shared_auto", 'a')
	automatic.Trigger = forkcheckin.JobTriggerAutomatic
	createForkCheckinJob(t, store, automatic)
	refresh := forkCheckinJobDraft(job.AccountID, "request_shared_refresh", 'r')
	refresh.Action = forkcheckin.JobActionStatusRefresh
	createForkCheckinJob(t, store, refresh)
	if _, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	for _, action := range []forkcheckin.JobAction{job.Action, refresh.Action} {
		if _, err := store.ClaimForkCheckin(ctx, job.AccountID, action, job.SiteDay, 1, time.Minute); !errors.Is(err, ErrForkCheckinLeaseHeld) {
			t.Fatalf("action %s bypassed shared lease: %v", action, err)
		}
	}
}

// The child executes a real test binary, not a goroutine or a second handle in
// the same process. Its only IPC is a test-owned database path and output file.
func TestForkCheckinReceiptProcess(t *testing.T) {
	path := os.Getenv("ASTRLINK_CHECKIN_TEST_DB")
	if path == "" {
		t.Skip("subprocess helper")
	}
	store := openTestStore(t, path)
	defer store.Close()
	job := forkCheckinJobDraft("acct_process", "request_process_01", 'p')
	got := createForkCheckinJob(t, store, job)
	if got.Outcome != forkcheckin.IdempotencyReplayed {
		t.Fatal("subprocess created a second job")
	}
	if err := os.WriteFile(path+".receipt", got.Receipt.Body, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestForkCheckinReceiptReplaysInAnotherProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "process.db")
	store := openTestStore(t, path)
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	createForkCheckinJobAccount(t, store, "acct_process")
	got := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_process", "request_process_01", 'p'))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, executable, "-test.run=^TestForkCheckinReceiptProcess$")
	command.Env = append(os.Environ(), "ASTRLINK_CHECKIN_TEST_DB="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("subprocess replay: %v\n%s", err, output)
	}
	body, err := os.ReadFile(path + ".receipt")
	if err != nil || !bytes.Equal(body, got.Receipt.Body) {
		t.Fatalf("subprocess changed receipt: %v", err)
	}
}

func TestForkCheckinJobAndAttemptWritesRollbackTogether(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_rollback")
	input := forkCheckinJobDraft("acct_rollback", "request_rollback_01", 'r')
	mustExec(t, store, `CREATE TRIGGER reject_receipt BEFORE INSERT ON fork_checkin_request_receipts BEGIN SELECT RAISE(ABORT, 'injected receipt failure'); END`)
	if _, err := store.CreateForkCheckinJobOnce(ctx, input); err == nil {
		t.Fatal("injected receipt failure was ignored")
	}
	if countRows(t, store, forkCheckinJobs) != 0 || countRows(t, store, forkCheckinReceipts) != 0 {
		t.Fatal("failed receipt left a half-created job")
	}
	mustExec(t, store, `DROP TRIGGER reject_receipt`)
	job := createForkCheckinJob(t, store, input).Job
	lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, `CREATE TRIGGER reject_completion BEFORE UPDATE ON fork_checkin_job_attempts WHEN NEW.ended_at <> '' BEGIN SELECT RAISE(ABORT, 'injected attempt failure'); END`)
	completion := ForkCheckinCompletion{Status: forkcheckin.JobStatusRetryableFailure, ProofSource: forkcheckin.ProofSourceTransportError}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, completion); err == nil {
		t.Fatal("injected completion failure was ignored")
	}
	stored, err := store.GetForkCheckinJob(ctx, job.ID)
	if err != nil || stored.Status != forkcheckin.JobStatusRunning {
		t.Fatalf("job result escaped rollback: %+v %v", stored, err)
	}
	mustExec(t, store, `DROP TRIGGER reject_completion`)
	if _, err := store.CompleteForkCheckinJob(ctx, lease, completion); err != nil {
		t.Fatal(err)
	}
}

func TestForkCheckinLeaseAndCompletionRejectInvalidCapabilities(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_capability")
	job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_capability", "request_capability_01", 'c')).Job
	for _, duration := range []time.Duration{-time.Second, MaxForkCheckinLeaseDuration + time.Nanosecond} {
		if _, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, duration); !errors.Is(err, storagecontract.ErrInvalidArgument) {
			t.Fatalf("invalid duration %s: %v", duration, err)
		}
	}
	lease, err := store.ClaimForkCheckinJob(ctx, ForkCheckinLeaseRequest{AccountID: job.AccountID, Action: job.Action, SiteDay: job.SiteDay, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	wrong := lease
	wrong.Token = "another_worker"
	if err := store.MarkForkCheckinDispatched(ctx, wrong); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("wrong token dispatched: %v", err)
	}
	badResults := []ForkCheckinCompletion{
		{Status: forkcheckin.JobStatusRunning},
		{Status: forkcheckin.JobStatusRetryableFailure, ProofSource: forkcheckin.ProofSourceTransportError, FailureCode: "upstream raw response"},
		{Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceSubmitResponse, Reward: forkcheckin.Reward{Known: true, Quota: 1, Unit: "raw currency text"}},
	}
	for _, bad := range badResults {
		if _, err := store.CompleteForkCheckinJob(ctx, lease, bad); !errors.Is(err, storagecontract.ErrInvalidArgument) {
			t.Fatalf("invalid result accepted: %v", err)
		}
	}
	if err := store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{Status: forkcheckin.JobStatusRetryableFailure, ProofSource: forkcheckin.ProofSourceTransportError}); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("a dispatched job became safely retryable: %v", err)
	}
}

func TestForkCheckinSchemaUpgradePreservesV6Receipts(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "upgrade.db"))
	defer store.Close()
	if err := store.applyForkCheckinMigrations(ctx, forkCheckinMigrations()[:6]); err != nil {
		t.Fatal(err)
	}
	createForkCheckinJobAccount(t, store, "acct_upgrade")
	job, err := prepareForkCheckinJob(store, forkCheckinJobDraft("acct_upgrade", "request_upgrade_01", 'u'))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Write the actual V6 columns, not the current DAO's expanded schema.
	if _, err := tx.ExecContext(ctx, `INSERT INTO fork_checkin_jobs
(id, account_id, action, trigger, expected_revision, status, request_id, request_fingerprint, site_date, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, job.ID, job.AccountID, job.Action, job.Trigger, job.ExpectedRevision,
		job.Status, job.RequestID, job.InputDigest, job.SiteDay, formatForkCheckinTime(job.CreatedAt)); err != nil {
		t.Fatal(err)
	}
	legacyBody, err := json.Marshal(job.Receipt())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO fork_checkin_request_receipts VALUES (?, 'jobs', ?, 202, ?, ?)`, job.RequestID, job.InputDigest, string(legacyBody), formatForkCheckinTime(job.CreatedAt)); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().UTC().Truncate(time.Second).Add(time.Hour)
	if _, err := tx.ExecContext(ctx, `INSERT INTO fork_checkin_job_attempts(job_id, attempt, phase, outcome, lease_expires_at, started_at) VALUES (?, 1, 'claim', 'claimed', ?, ?)`, job.ID, expiry.Format(time.RFC3339Nano), formatForkCheckinTime(job.CreatedAt)); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	var storedExpiry string
	if err := store.db.QueryRowContext(ctx, `SELECT lease_expires_at FROM fork_checkin_job_attempts WHERE job_id = ?`, job.ID).Scan(&storedExpiry); err != nil || storedExpiry != formatForkCheckinTime(expiry) {
		t.Fatalf("legacy expiry was not normalized: %q %v", storedExpiry, err)
	}
	replayed := createForkCheckinJob(t, store, job)
	if replayed.Outcome != forkcheckin.IdempotencyReplayed || !bytes.Equal(replayed.Receipt.Body, legacyBody) {
		t.Fatal("migration rewrote a previously accepted response")
	}
}
