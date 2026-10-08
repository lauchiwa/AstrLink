package sqlite

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

type observingCheckinExecutionStore struct {
	forkcheckin.ExecutionStore
	calls    atomic.Int32
	observed chan struct{}
}

func (s *observingCheckinExecutionStore) GetForkCheckinJob(ctx context.Context, id forkcheckin.JobID) (forkcheckin.Job, error) {
	job, err := s.ExecutionStore.GetForkCheckinJob(ctx, id)
	if s.calls.Add(1) == 3 {
		close(s.observed)
	}
	return job, err
}

func TestForkCheckinSharedRunnerConcurrentReplayReturnsTerminalResult(t *testing.T) {
	f := newCheckinRunnerFixture(t, "https://relay.example")
	f.create(t)
	started, release := make(chan struct{}), make(chan struct{})
	adapter := &checkinRunnerAdapter{fixture: f, onSubmit: func(context.Context) error {
		close(started)
		<-release
		return nil
	}}
	store := &observingCheckinExecutionStore{ExecutionStore: f.store, observed: make(chan struct{})}
	runner, err := forkcheckin.NewRunner(store, f.store.ForkCheckinSessions(), adapter)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		receipt forkcheckin.JobReceipt
		err     error
	}
	results := make(chan result, 2)
	run := func() {
		receipt, err := runner.Execute(context.Background(), f.job.ID)
		results <- result{receipt, err}
	}
	go run()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("runner did not submit")
	}
	go run()
	select {
	case <-store.observed:
	case <-time.After(3 * time.Second):
		t.Fatal("second caller did not observe running job")
	}
	close(release)
	for range 2 {
		select {
		case got := <-results:
			if got.err != nil || got.receipt.Status != forkcheckin.JobStatusSuccess {
				t.Fatalf("replay=%+v err=%v", got.receipt, got.err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("replay hung")
		}
	}
	if adapter.posts.Load() != 1 {
		t.Fatal("concurrent replay submitted twice")
	}
}

func TestForkCheckinSchedulerRevokedConsentCancelsQueueWithoutOpeningVault(t *testing.T) {
	s := newScheduleFixture(t, time.Now())
	ctx := context.Background()
	id, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	s.account(t, func(a *forkcheckin.Account) { a.Automatic = false })
	s.tick(t)
	job, err := s.f.store.GetForkCheckinJob(ctx, id)
	if err != nil || job.Status != forkcheckin.JobStatusCancelled || job.Dispatched || s.adapter.identities.Load() != 0 {
		t.Fatalf("revoked job=%+v err=%v", job, err)
	}
	s.clock.add(48 * time.Hour)
	s.reopen(t)
	s.tick(t)
	if len(s.jobs(t)) != 1 || s.adapter.posts.Load() != 0 {
		t.Fatal("disabled account scheduled")
	}
}

func TestForkCheckinSchedulerRevokedConsentStillSettlesPriorDispatch(t *testing.T) {
	s := newScheduleFixture(t, time.Now())
	ctx := context.Background()
	id, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.f.store.ClaimForkCheckinJobID(ctx, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.f.store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	s.account(t, func(a *forkcheckin.Account) { a.Automatic = false })
	s.clock.add(2 * time.Minute)
	s.reopen(t)
	s.tick(t)
	job := s.jobs(t)[0]
	if job.Status != forkcheckin.JobStatusUncertain || job.FailureCode != "account_changed" || !job.Dispatched || s.adapter.identities.Load() != 0 {
		t.Fatalf("revoked recovery=%+v", job)
	}
	s.tick(t)
	if len(s.jobs(t)) != 1 {
		t.Fatal("disabled account got a supplemental job")
	}
}

func TestForkCheckinSchedulerRequiresOperatorAfterNonRetryableFailure(t *testing.T) {
	for _, status := range []forkcheckin.JobStatus{forkcheckin.JobStatusAuthRequired, forkcheckin.JobStatusManualRequired, forkcheckin.JobStatusUnsupported} {
		t.Run(string(status), func(t *testing.T) {
			s := newScheduleFixture(t, time.Now())
			ctx := context.Background()
			id, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := s.f.store.ClaimForkCheckinJobID(ctx, id, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			_, err = s.f.store.CompleteForkCheckinJob(ctx, lease, forkcheckin.ExecutionCompletion{Status: status, ProofSource: forkcheckin.ProofSourceTransportError, ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			s.clock.add(48 * time.Hour)
			s.reopen(t)
			s.tick(t)
			if len(s.jobs(t)) != 1 || s.adapter.posts.Load() != 0 {
				t.Fatal("human-only outcome retried on a later day")
			}
			s.account(t, func(a *forkcheckin.Account) { a.Automatic = true })
			s.tick(t)
			if s.adapter.posts.Load() != 1 {
				t.Fatal("explicit reauthorization did not resume current day")
			}
		})
	}
}

func TestForkCheckinJobDayBindingRequiresLivePredispatchLease(t *testing.T) {
	for _, boundary := range []string{"expired", "dispatched", "revision", "completed", "invalid_date", "wrong_token"} {
		t.Run(boundary, func(t *testing.T) {
			s := newScheduleFixture(t, time.Now())
			ctx := context.Background()
			id, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := s.f.store.ClaimForkCheckinJobID(ctx, id, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			day := s.clock.now().Add(-24 * time.Hour).Format(time.DateOnly)
			switch boundary {
			case "expired":
				s.clock.add(2 * time.Minute)
			case "dispatched":
				if err := s.f.store.MarkForkCheckinDispatched(ctx, lease); err != nil {
					t.Fatal(err)
				}
			case "revision":
				s.account(t, func(a *forkcheckin.Account) { a.Automatic = false })
			case "completed":
				_, err = s.f.store.CompleteForkCheckinJob(ctx, lease, forkcheckin.ExecutionCompletion{Status: forkcheckin.JobStatusCancelled, ProofSource: forkcheckin.ProofSourceNone, ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
			case "invalid_date":
				day = "not-a-date"
			case "wrong_token":
				lease.Token = "other_lease"
			}
			before, err := s.f.store.GetForkCheckinJob(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.f.store.BindForkCheckinJobDay(ctx, lease, day); err == nil {
				t.Fatal("unfenced date binding accepted")
			}
			after, err := s.f.store.GetForkCheckinJob(ctx, id)
			if err != nil || after.SiteDay != before.SiteDay || after.ScheduleDay != before.ScheduleDay {
				t.Fatal("rejected binding mutated dates", err)
			}
		})
	}
}

func TestForkCheckinSchedulerRecoveryConfirmsSameDayWithoutAward(t *testing.T) {
	s := newScheduleFixture(t, time.Now())
	ctx := context.Background()
	id, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.f.store.ClaimForkCheckinJobID(ctx, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.f.store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	job, err := s.f.store.GetForkCheckinJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	s.adapter.checked[string(s.f.account.ID)+job.SiteDay] = true
	s.clock.add(2 * time.Minute)
	s.reopen(t)
	s.tick(t)
	job = s.jobs(t)[0]
	if job.Status != forkcheckin.JobStatusAlreadyChecked || job.ProofSource != forkcheckin.ProofSourceStatusRecheck || job.Reward.Known || s.adapter.posts.Load() != 0 {
		t.Fatalf("recovery=%+v", job)
	}
}

func TestForkCheckinCompletionSurvivesBackwardsWallClock(t *testing.T) {
	s := newScheduleFixture(t, time.Now())
	ctx := context.Background()
	id, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
	if err != nil {
		t.Fatal(err)
	}
	lease, err := s.f.store.ClaimForkCheckinJobID(ctx, id, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	s.clock.add(-time.Hour)
	receipt, err := s.f.store.CompleteForkCheckinJob(ctx, lease, forkcheckin.ExecutionCompletion{Status: forkcheckin.JobStatusRetryableFailure, ProofSource: forkcheckin.ProofSourceTransportError, ReadOnly: true})
	if err != nil || receipt.CompletedAt.Before(receipt.CreatedAt) {
		t.Fatalf("backwards clock stranded completion: %+v %v", receipt, err)
	}
	if _, err := s.f.store.ClaimForkCheckinJobID(ctx, id, time.Minute); err == nil || errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatal("completion retained active lease", err)
	}
}
