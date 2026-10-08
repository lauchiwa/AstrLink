package forkcheckin

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type runnerStore struct {
	job         Job
	account     Account
	marked      bool
	completed   ExecutionCompletion
	claims      int
	recoveries  int
	completions int
	onMark      func() error
	onComplete  func() error
}

func (s *runnerStore) GetForkCheckinJob(context.Context, JobID) (Job, error) { return s.job, nil }
func (s *runnerStore) GetForkCheckinAccount(context.Context, AccountID) (Account, error) {
	return s.account, nil
}
func (s *runnerStore) lease() ExecutionLease {
	s.job.Attempts++
	s.job.Status = JobStatusRunning
	return ExecutionLease{JobID: s.job.ID, AccountID: s.job.AccountID, Revision: s.job.ExpectedRevision, Attempt: s.job.Attempts, Token: "synthetic_lease", ExpiresAt: time.Now().Add(2 * time.Minute)}
}
func (s *runnerStore) ClaimForkCheckinJobID(context.Context, JobID, time.Duration) (ExecutionLease, error) {
	s.claims++
	return s.lease(), nil
}
func (s *runnerStore) RecoverForkCheckinDispatch(context.Context, JobID, AccountID, int64, time.Duration) (ExecutionLease, error) {
	s.recoveries++
	return s.lease(), nil
}
func (s *runnerStore) BindForkCheckinJobDay(_ context.Context, _ ExecutionLease, day string) (Job, error) {
	s.job.SiteDay = day
	return s.job, nil
}
func (s *runnerStore) MarkForkCheckinDispatched(context.Context, ExecutionLease) error {
	if s.onMark != nil {
		if err := s.onMark(); err != nil {
			return err
		}
	}
	s.marked, s.job.Dispatched = true, true
	return nil
}
func (s *runnerStore) CompleteForkCheckinJob(ctx context.Context, _ ExecutionLease, c ExecutionCompletion) (JobReceipt, error) {
	if ctx.Err() != nil {
		return JobReceipt{}, ctx.Err()
	}
	s.completions++
	if s.onComplete != nil {
		if err := s.onComplete(); err != nil {
			return JobReceipt{}, err
		}
	}
	s.completed = c
	s.job.RetryNotBefore = c.RetryNotBefore
	s.job.Status, s.job.ProofSource, s.job.Reward, s.job.FailureCode = c.Status, c.ProofSource, c.Reward, c.FailureCode
	now := time.Now()
	s.job.CompletedAt = &now
	if err := s.job.Validate(); err != nil {
		return JobReceipt{}, err
	}
	return s.job.Receipt(), nil
}

type runnerVault struct {
	Vault
	bytes []byte
	err   error
	gets  int
}

func (v *runnerVault) Get(context.Context, AccountID) ([]byte, error) {
	v.gets++
	return v.bytes, v.err
}

type runnerAdapter struct {
	SiteAdapter
	store        *runnerStore
	posts, reads int
	identities   int
	day          string
	identity     func(context.Context) (SiteIdentity, error)
	read         func(context.Context) (CheckInStatus, error)
	inspect      func(context.Context) (SiteCapability, error)
	submit       func(context.Context) (SubmitOutcome, error)
}

func (a *runnerAdapter) ValidateIdentity(ctx context.Context, _ AccountSnapshot) (SiteIdentity, error) {
	a.identities++
	if a.identity != nil {
		return a.identity(ctx)
	}
	return SiteIdentity{RemoteUserID: a.store.account.RemoteUserID}, nil
}
func (a *runnerAdapter) Inspect(ctx context.Context, _ AccountSnapshot) (SiteCapability, error) {
	if a.inspect != nil {
		return a.inspect(ctx)
	}
	return SiteCapability{Supported: true}, nil
}
func (a *runnerAdapter) ReadStatus(ctx context.Context, _ AccountSnapshot) (CheckInStatus, error) {
	a.reads++
	if a.read != nil {
		return a.read(ctx)
	}
	return CheckInStatus{CheckedInToday: a.posts > 0, SiteDate: a.day}, nil
}
func (a *runnerAdapter) Submit(ctx context.Context, _ AccountSnapshot) (SubmitOutcome, error) {
	if !a.store.marked {
		panic("POST before durable intent")
	}
	a.posts++
	if a.submit != nil {
		return a.submit(ctx)
	}
	return SubmitOutcome{Dispatched: true, ResponseRead: true, Succeeded: true, SiteDate: a.day, Reward: Reward{Known: true, Quota: 5, Unit: "quota"}}, nil
}

func runnerFixture(t *testing.T) (*Runner, *runnerStore, *runnerVault, *runnerAdapter) {
	t.Helper()
	account := connectedAccount()
	account.TimeZone = "UTC"
	now := time.Now().UTC()
	store := &runnerStore{account: account, job: Job{
		ID: "job_runner", AccountID: account.ID, ExpectedRevision: account.Revision,
		Action: JobActionCheckIn, Trigger: JobTriggerManual, SiteDay: now.Format(time.DateOnly),
		Status: JobStatusQueued, ProofSource: ProofSourceNone, RequestID: "request_runner", InputDigest: "synthetic_digest", CreatedAt: now,
	}}
	vault := &runnerVault{bytes: []byte("synthetic-session")}
	adapter := &runnerAdapter{store: store, day: store.job.SiteDay}
	runner, err := NewRunner(store, vault, adapter)
	if err != nil {
		t.Fatal(err)
	}
	runner.now = func() time.Time { return now }
	return runner, store, vault, adapter
}

func assertRunnerCredentialCleared(t *testing.T, vault *runnerVault) {
	t.Helper()
	for _, b := range vault.bytes {
		if b != 0 {
			t.Fatal("credential not cleared")
		}
	}
}

func TestRunnerSubmissionAndRecovery(t *testing.T) {
	for _, mode := range []string{"success", "lost", "restart", "unknown_day", "other_day", "invalid_outcome", "already_response"} {
		t.Run(mode, func(t *testing.T) {
			runner, store, vault, adapter := runnerFixture(t)
			want, posts := JobStatusAlreadyChecked, 1
			switch mode {
			case "success":
				want = JobStatusSuccess
			case "restart":
				store.job.Dispatched, store.job.Status = true, JobStatusRunning
				adapter.read = func(context.Context) (CheckInStatus, error) {
					return CheckInStatus{CheckedInToday: true, SiteDate: adapter.day, Reward: &Reward{Known: true, Quota: 999, Unit: "quota"}}, nil
				}
				posts = 0
			case "lost", "unknown_day", "other_day":
				adapter.submit = func(context.Context) (SubmitOutcome, error) { return SubmitOutcome{Dispatched: true}, ErrUncertain }
				if mode != "lost" {
					want = JobStatusUncertain
					adapter.read = func(context.Context) (CheckInStatus, error) {
						day := adapter.day
						if adapter.posts > 0 {
							day = ""
							if mode == "other_day" {
								day = runner.now().AddDate(0, 0, 1).Format(time.DateOnly)
							}
						}
						return CheckInStatus{CheckedInToday: adapter.posts > 0, SiteDate: day}, nil
					}
				}
			case "invalid_outcome":
				adapter.submit = func(context.Context) (SubmitOutcome, error) {
					return SubmitOutcome{Succeeded: true, SiteDate: adapter.day, Reward: Reward{Known: true, Quota: 900, Unit: "quota"}}, nil
				}
			case "already_response":
				adapter.submit = func(context.Context) (SubmitOutcome, error) {
					return SubmitOutcome{Dispatched: true, ResponseRead: true, AlreadyCheckedIn: true, SiteDate: adapter.day}, nil
				}
			}
			receipt, err := runner.Execute(context.Background(), store.job.ID)
			if err != nil || receipt.Status != want || adapter.posts != posts {
				t.Fatalf("receipt=%+v posts=%d err=%v, want %s/%d", receipt, adapter.posts, err, want, posts)
			}
			if mode != "success" && receipt.Reward != nil {
				t.Fatal("readback invented reward")
			}
			if mode == "success" && (receipt.Reward == nil || receipt.Reward.Quota != 5) {
				t.Fatal("authoritative reward missing")
			}
			if mode == "restart" && (store.claims != 0 || store.recoveries != 1 || adapter.identities != 1) {
				t.Fatal("restart did not validate identity under a recovery lease")
			}
			assertRunnerCredentialCleared(t, vault)
			// A terminal execution returns the current result without claiming,
			// opening the Vault, or even performing another GET.
			reads := adapter.reads
			again, err := runner.Execute(context.Background(), store.job.ID)
			if err != nil || !reflect.DeepEqual(again, receipt) || adapter.posts != posts || adapter.reads != reads || vault.gets != 1 {
				t.Fatalf("terminal execution changed result or performed I/O: %+v %v", again, err)
			}
		})
	}
}

func TestRunnerReadOnlyResultsNeverConsumeDispatch(t *testing.T) {
	for _, checked := range []bool{false, true} {
		runner, store, _, adapter := runnerFixture(t)
		store.job.Action = JobActionStatusRefresh
		adapter.read = func(context.Context) (CheckInStatus, error) {
			return CheckInStatus{CheckedInToday: checked, SiteDate: adapter.day}, nil
		}
		receipt, err := runner.Execute(context.Background(), store.job.ID)
		want := JobStatusNotChecked
		if checked {
			want = JobStatusAlreadyChecked
		}
		if err != nil || receipt.Status != want || receipt.Dispatched || store.marked || adapter.posts != 0 || receipt.Status.CountsAsDoneToday() != checked {
			t.Fatalf("refresh result=%+v posts=%d err=%v", receipt, adapter.posts, err)
		}
	}
}

func TestRunnerPreflightFailuresNeverSubmit(t *testing.T) {
	for _, mode := range []string{"revision", "revision_after_read", "identity", "automatic_revoked", "vault", "manual", "unsupported", "rate_limited", "cancel", "deadline", "already_checked"} {
		t.Run(mode, func(t *testing.T) {
			runner, store, vault, adapter := runnerFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			want := JobStatusCancelled
			switch mode {
			case "revision":
				store.account.Revision++
			case "revision_after_read":
				adapter.read = func(context.Context) (CheckInStatus, error) {
					store.account.Revision++
					return CheckInStatus{SiteDate: adapter.day}, nil
				}
			case "identity":
				adapter.identity = func(context.Context) (SiteIdentity, error) { return SiteIdentity{RemoteUserID: "wrong_user"}, nil }
			case "automatic_revoked":
				store.job.Trigger = JobTriggerAutomatic
			case "vault":
				vault.err, want = errors.New("synthetic-session"), JobStatusAuthRequired
			case "manual", "unsupported":
				want = JobStatusUnsupported
				if mode == "manual" {
					want = JobStatusManualRequired
				}
				adapter.inspect = func(context.Context) (SiteCapability, error) {
					return SiteCapability{RequiresManual: mode == "manual"}, nil
				}
			case "rate_limited", "deadline":
				failure := ErrRateLimited
				want = JobStatusRateLimited
				if mode == "deadline" {
					failure, want = context.DeadlineExceeded, JobStatusRetryableFailure
				}
				adapter.read = func(context.Context) (CheckInStatus, error) { return CheckInStatus{}, failure }
			case "cancel":
				adapter.read = func(context.Context) (CheckInStatus, error) {
					cancel()
					return CheckInStatus{SiteDate: adapter.day}, nil
				}
			case "already_checked":
				want = JobStatusAlreadyChecked
				adapter.read = func(context.Context) (CheckInStatus, error) {
					return CheckInStatus{CheckedInToday: true, SiteDate: adapter.day}, nil
				}
			}
			receipt, err := runner.Execute(ctx, store.job.ID)
			if err != nil || receipt.Status != want || receipt.Dispatched || store.marked || adapter.posts != 0 {
				t.Fatalf("receipt=%+v marked=%v posts=%d err=%v", receipt, store.marked, adapter.posts, err)
			}
			if vault.gets > 0 {
				assertRunnerCredentialCleared(t, vault)
			}
			body, _ := json.Marshal(receipt.Public())
			if strings.Contains(string(body), "synthetic-session") {
				t.Fatal("credential or private error leaked into public result")
			}
		})
	}
}

func TestRunnerCancelAfterIntentUsesBoundedReadOnlyRecheck(t *testing.T) {
	for _, afterPost := range []bool{false, true} {
		runner, store, vault, adapter := runnerFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store.onMark = func() error {
			if !afterPost {
				cancel()
			}
			return nil
		}
		adapter.submit = func(context.Context) (SubmitOutcome, error) {
			cancel()
			return SubmitOutcome{Dispatched: true}, context.Canceled
		}
		adapter.read = func(ctx context.Context) (CheckInStatus, error) {
			if store.marked {
				deadline, ok := ctx.Deadline()
				if ctx.Err() != nil || !ok || time.Until(deadline) > 15*time.Second {
					t.Fatal("recheck inherited cancellation or has no bounded deadline")
				}
			}
			return CheckInStatus{SiteDate: adapter.day}, nil
		}
		receipt, err := runner.Execute(ctx, store.job.ID)
		posts := 0
		if afterPost {
			posts = 1
		}
		if err != nil || receipt.Status != JobStatusUncertain || !receipt.Dispatched || adapter.posts != posts || adapter.reads != 2 {
			t.Fatalf("cancelled execution=%+v posts=%d reads=%d err=%v", receipt, adapter.posts, adapter.reads, err)
		}
		assertRunnerCredentialCleared(t, vault)
	}
}

func TestRunnerAmbiguousDispatchCommitNeverSubmits(t *testing.T) {
	runner, store, vault, adapter := runnerFixture(t)
	commitError := errors.New("synthetic commit acknowledgement lost")
	store.onMark = func() error {
		store.job.Dispatched = true
		return commitError
	}
	if _, err := runner.Execute(context.Background(), store.job.ID); !errors.Is(err, commitError) || adapter.posts != 0 || store.completions != 0 {
		t.Fatalf("ambiguous commit submitted or overwrote intent: posts=%d err=%v", adapter.posts, err)
	}
	assertRunnerCredentialCleared(t, vault)
	vault.bytes = []byte("synthetic-session")
	if receipt, err := runner.Execute(context.Background(), store.job.ID); err != nil || receipt.Status != JobStatusUncertain || adapter.posts != 0 || store.recoveries != 1 {
		t.Fatalf("recovery resent after ambiguous commit: %+v %v", receipt, err)
	}
}

func TestRunnerCompletionRevisionChangeDiscardsSiteProof(t *testing.T) {
	runner, store, _, adapter := runnerFixture(t)
	store.onComplete = func() error {
		if store.completions == 1 {
			return ErrRevisionChanged
		}
		return nil
	}
	receipt, err := runner.Execute(context.Background(), store.job.ID)
	if err != nil || receipt.Status != JobStatusUncertain || receipt.FailureCode != "account_changed" || receipt.Reward != nil || adapter.posts != 1 {
		t.Fatalf("obsolete revision kept proof: %+v %v", receipt, err)
	}
}

func TestRunnerDoesNotTrustLocalDayAcrossMidnight(t *testing.T) {
	runner, store, _, adapter := runnerFixture(t)
	day, _ := time.Parse(time.DateOnly, store.job.SiteDay)
	now := day.Add(24*time.Hour - time.Second)
	runner.now = func() time.Time { return now }
	adapter.read = func(context.Context) (CheckInStatus, error) {
		now = now.Add(2 * time.Second)
		return CheckInStatus{CheckedInToday: true}, nil
	}
	receipt, err := runner.Execute(context.Background(), store.job.ID)
	if err != nil || receipt.Status != JobStatusCancelled || adapter.posts != 0 {
		t.Fatalf("midnight read was attributed to the wrong day: %+v %v", receipt, err)
	}
}

func TestRunnerHonorsNextOpeningBeforeConsumingIntent(t *testing.T) {
	runner, store, _, adapter := runnerFixture(t)
	opening := runner.now().Add(time.Hour)
	adapter.read = func(context.Context) (CheckInStatus, error) {
		return CheckInStatus{SiteDate: adapter.day, NextAvailableAt: &opening}, nil
	}
	receipt, err := runner.Execute(context.Background(), store.job.ID)
	if err != nil || receipt.Status != JobStatusRateLimited || store.marked || adapter.posts != 0 || store.job.RetryNotBefore == nil || !store.job.RetryNotBefore.Equal(opening) {
		t.Fatalf("opening ignored: %+v %v", receipt, err)
	}
}

func TestRunnerFailureClassification(t *testing.T) {
	for _, err := range []error{context.Canceled, ErrAuthRequired, ErrRevisionChanged, errors.New("private site prose")} {
		result := executionFailure(err, true)
		if result.Status != JobStatusUncertain || result.Reward.Known || (result.FailureCode != "unconfirmed" && result.FailureCode != "account_changed") {
			t.Fatalf("dispatched failure downgraded: %+v", result)
		}
	}
}
