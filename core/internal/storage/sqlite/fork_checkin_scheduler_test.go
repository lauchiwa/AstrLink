package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

type checkinScheduleClock struct{ value atomic.Int64 }

func (c *checkinScheduleClock) now() time.Time      { return time.Unix(0, c.value.Load()).UTC() }
func (c *checkinScheduleClock) set(t time.Time)     { c.value.Store(t.UnixNano()) }
func (c *checkinScheduleClock) add(d time.Duration) { c.value.Add(int64(d)) }

type scheduleAdapter struct {
	forkcheckin.SiteAdapter
	clock      *checkinScheduleClock
	mode       string
	day        string
	retryAt    time.Time
	posts      atomic.Int32
	identities atomic.Int32
	reads      atomic.Int32
	mu         sync.Mutex
	checked    map[string]bool
}

func (a *scheduleAdapter) ValidateIdentity(_ context.Context, s forkcheckin.AccountSnapshot) (forkcheckin.SiteIdentity, error) {
	a.identities.Add(1)
	switch a.mode {
	case "network":
		return forkcheckin.SiteIdentity{}, forkcheckin.ErrNetwork
	case "rate":
		return forkcheckin.SiteIdentity{}, &forkcheckin.RateLimitError{NotBefore: a.retryAt}
	}
	return forkcheckin.SiteIdentity{RemoteUserID: s.Account.RemoteUserID}, nil
}
func (a *scheduleAdapter) Inspect(context.Context, forkcheckin.AccountSnapshot) (forkcheckin.SiteCapability, error) {
	return forkcheckin.SiteCapability{Supported: true}, nil
}
func (a *scheduleAdapter) siteDay(account forkcheckin.Account) string {
	if a.day != "" {
		return a.day
	}
	zone, _ := time.LoadLocation(account.TimeZone)
	return a.clock.now().In(zone).Format(time.DateOnly)
}
func (a *scheduleAdapter) ReadStatus(_ context.Context, s forkcheckin.AccountSnapshot) (forkcheckin.CheckInStatus, error) {
	a.reads.Add(1)
	day := a.siteDay(s.Account)
	a.mu.Lock()
	checked := a.checked[string(s.Account.ID)+day]
	a.mu.Unlock()
	return forkcheckin.CheckInStatus{SiteDate: day, CheckedInToday: checked, Reward: &forkcheckin.Reward{Known: true, Quota: 999, Unit: "quota"}}, nil
}
func (a *scheduleAdapter) Submit(_ context.Context, s forkcheckin.AccountSnapshot) (forkcheckin.SubmitOutcome, error) {
	a.posts.Add(1)
	day := a.siteDay(s.Account)
	if a.mode == "uncertain" {
		return forkcheckin.SubmitOutcome{Dispatched: true}, forkcheckin.ErrUncertain
	}
	a.mu.Lock()
	a.checked[string(s.Account.ID)+day] = true
	a.mu.Unlock()
	return forkcheckin.SubmitOutcome{Dispatched: true, ResponseRead: true, Succeeded: true, SiteDate: day, Reward: forkcheckin.Reward{Known: true, Quota: 5, Unit: "quota"}}, nil
}

type scheduleFixture struct {
	f       *checkinRunnerFixture
	clock   *checkinScheduleClock
	adapter *scheduleAdapter
}

func newScheduleFixture(t *testing.T, now time.Time) *scheduleFixture {
	t.Helper()
	f := newCheckinRunnerFixture(t, "https://relay.example")
	clock := &checkinScheduleClock{}
	clock.set(now)
	f.store.now = clock.now
	return &scheduleFixture{f: f, clock: clock, adapter: &scheduleAdapter{clock: clock, checked: map[string]bool{}}}
}
func (s *scheduleFixture) tick(t *testing.T) {
	t.Helper()
	runner, err := forkcheckin.NewRunner(s.f.store, s.f.store.ForkCheckinSessions(), s.adapter)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := forkcheckin.NewScheduler(s.f.store, runner)
	if err != nil {
		t.Fatal(err)
	}
	if err := scheduler.Tick(context.Background(), s.clock.now()); err != nil {
		t.Fatal(err)
	}
}
func (s *scheduleFixture) jobs(t *testing.T) []forkcheckin.Job {
	t.Helper()
	rows, err := s.f.store.db.Query(forkCheckinJobSelect + ` ORDER BY j.rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var jobs []forkcheckin.Job
	for rows.Next() {
		job, err := scanForkCheckinJob(rows)
		if err != nil {
			t.Fatal(err)
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return jobs
}
func (s *scheduleFixture) reopen(t *testing.T) {
	t.Helper()
	s.f.reopen(t)
	s.f.store.now = s.clock.now
}
func (s *scheduleFixture) account(t *testing.T, change func(*forkcheckin.Account)) {
	t.Helper()
	a, err := s.f.store.GetForkCheckinAccount(context.Background(), s.f.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	change(&a)
	a, err = s.f.store.UpdateForkCheckinAccount(context.Background(), a, a.Revision)
	if err != nil {
		t.Fatal(err)
	}
	s.f.account = a
}

func TestForkCheckinSchedulerCalendarDSTSleepRestartAndClockRollback(t *testing.T) {
	zone, _ := time.LoadLocation("America/New_York")
	for _, month := range []time.Month{time.March, time.November} {
		t.Run(month.String(), func(t *testing.T) {
			day := 8
			if month == time.November {
				day = 1
			}
			start := time.Date(2026, month, day, 0, 30, 0, 0, zone)
			s := newScheduleFixture(t, start)
			s.account(t, func(a *forkcheckin.Account) { a.TimeZone = zone.String() })
			s.tick(t)
			s.clock.add(3 * time.Hour)
			s.tick(t) // Both repeated and skipped hours stay in one day.
			if s.adapter.posts.Load() != 1 {
				t.Fatal("DST repeated a daily submission")
			}
			s.reopen(t)
			s.tick(t)
			if s.adapter.posts.Load() != 1 {
				t.Fatal("restart repeated a completed day")
			}
			next := start.AddDate(0, 0, 1)
			s.clock.set(next)
			s.tick(t)
			s.clock.set(start)
			s.tick(t) // Returning to a known bucket cannot reset its intent.
			s.clock.set(start.AddDate(0, 0, 5))
			s.tick(t) // Sleep skips historical days.
			jobs := s.jobs(t)
			if len(jobs) != 3 || s.adapter.posts.Load() != 3 {
				t.Fatalf("jobs=%d posts=%d", len(jobs), s.adapter.posts.Load())
			}
			for i, expected := range []time.Time{start, next, start.AddDate(0, 0, 5)} {
				if jobs[i].ScheduleDay != expected.Format(time.DateOnly) || jobs[i].Status != forkcheckin.JobStatusSuccess {
					t.Fatalf("job=%+v", jobs[i])
				}
			}
		})
	}
}

func TestForkCheckinSchedulerConsentBindingsAndNoStatusPolling(t *testing.T) {
	s := newScheduleFixture(t, time.Now())
	// Multiple existing services fund one account, but are not schedule inputs.
	for i := 0; i < 2; i++ {
		createForkCheckinService(t, s.f.store, contract.ServiceID(fmt.Sprintf("svc_schedule_%d", i)))
	}
	s.account(t, func(a *forkcheckin.Account) {
		a.BoundServices = []contract.ServiceID{"svc_schedule_0", "svc_schedule_1"}
	})
	s.tick(t)
	reads := s.adapter.reads.Load()
	for range 5 {
		s.clock.add(time.Minute)
		s.tick(t)
	}
	if s.adapter.posts.Load() != 1 || s.adapter.reads.Load() != reads {
		t.Fatal("bindings duplicated work or completed day was polled")
	}
	s.account(t, func(a *forkcheckin.Account) { a.Automatic = false })
	s.clock.add(24 * time.Hour)
	s.tick(t)
	if len(s.jobs(t)) != 1 {
		t.Fatal("automatic consent was ignored")
	}
}

func TestForkCheckinSchedulerRetryAfterSurvivesRestartAndMidnight(t *testing.T) {
	start := time.Date(2026, time.June, 1, 23, 58, 0, 0, time.UTC)
	s := newScheduleFixture(t, start)
	s.adapter.mode = "rate"
	s.adapter.retryAt = start.Add(10 * time.Minute)
	s.tick(t)
	first := s.jobs(t)[0]
	if first.Status != forkcheckin.JobStatusRateLimited || first.RetryNotBefore == nil || !first.RetryNotBefore.Equal(s.adapter.retryAt) {
		t.Fatalf("rate limit not persisted: %+v", first)
	}
	s.reopen(t)
	s.clock.add(5 * time.Minute)
	s.tick(t)
	if len(s.jobs(t)) != 1 {
		t.Fatal("midnight reset Retry-After")
	}
	s.adapter.mode = ""
	s.clock.add(5 * time.Minute)
	s.tick(t)
	jobs := s.jobs(t)
	if len(jobs) != 2 || jobs[1].Status != forkcheckin.JobStatusSuccess || jobs[1].ScheduleDay == jobs[0].ScheduleDay {
		t.Fatalf("current day did not resume: %+v", jobs)
	}
}

func TestForkCheckinSchedulerSafeRetryBudgetIsDurable(t *testing.T) {
	s := newScheduleFixture(t, time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC))
	s.adapter.mode = "network"
	s.tick(t)
	s.reopen(t)
	s.tick(t)
	if len(s.jobs(t)) != 1 {
		t.Fatal("restart bypassed backoff")
	}
	s.clock.add(time.Minute)
	s.tick(t)
	s.reopen(t)
	s.clock.add(4 * time.Minute)
	s.tick(t)
	if len(s.jobs(t)) != 2 {
		t.Fatal("second backoff ignored")
	}
	s.clock.add(time.Minute)
	s.tick(t)
	s.reopen(t)
	s.clock.add(time.Hour)
	s.tick(t)
	jobs := s.jobs(t)
	if len(jobs) != 3 || s.adapter.identities.Load() != 3 || s.adapter.posts.Load() != 0 {
		t.Fatalf("budget reset: jobs=%d reads=%d", len(jobs), s.adapter.identities.Load())
	}
	for _, job := range jobs {
		if job.Status != forkcheckin.JobStatusRetryableFailure || job.Attempts != 1 {
			t.Fatalf("terminal job was reset: %+v", job)
		}
	}
}

func TestForkCheckinSchedulerInterruptedClaimsConsumeBudget(t *testing.T) {
	s := newScheduleFixture(t, time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	id, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
	if err != nil || id == "" {
		t.Fatal(id, err)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		lease, err := s.f.store.ClaimForkCheckinJobID(ctx, id, 2*time.Minute)
		if err != nil || lease.Attempt != attempt {
			t.Fatal(lease, err)
		}
		s.clock.add(3 * time.Minute)
		s.reopen(t)
		next, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
		if err != nil || next != "" {
			t.Fatal("crash skipped persisted backoff", next, err)
		}
		s.clock.add(forkcheckin.AutomaticRetryDelay(attempt))
		next, err = s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
		if err != nil {
			t.Fatal(err)
		}
		if attempt < 3 && next != id {
			t.Fatal("claim was not resumed")
		}
		if attempt == 3 && next != "" {
			t.Fatal("crash replenished budget")
		}
	}
	job := s.jobs(t)[0]
	if job.Attempts != 3 || job.Status != forkcheckin.JobStatusCancelled || job.FailureCode != "retry_exhausted" {
		t.Fatalf("job=%+v", job)
	}
}

func TestForkCheckinSchedulerUncertainSupplementNeverSubmitsOrAddsReward(t *testing.T) {
	s := newScheduleFixture(t, time.Now())
	s.adapter.mode = "uncertain"
	s.tick(t)
	first := s.jobs(t)[0]
	if first.Status != forkcheckin.JobStatusUncertain || !first.Dispatched {
		t.Fatalf("job=%+v", first)
	}
	s.reopen(t)
	s.adapter.checked[string(s.f.account.ID)+first.SiteDay] = true
	s.tick(t)
	s.tick(t)
	jobs := s.jobs(t)
	if len(jobs) != 2 || jobs[0].Status != forkcheckin.JobStatusUncertain || jobs[1].Action != forkcheckin.JobActionStatusRefresh || jobs[1].Status != forkcheckin.JobStatusAlreadyChecked || jobs[1].Reward.Known || s.adapter.posts.Load() != 1 {
		t.Fatalf("unsafe supplement: %+v posts=%d", jobs, s.adapter.posts.Load())
	}
}

func TestForkCheckinSchedulerRecoversDispatchAndDropsStaleQueue(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(fmt.Sprint(dispatched), func(t *testing.T) {
			s := newScheduleFixture(t, time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC))
			ctx := context.Background()
			id, err := s.f.store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
			if err != nil {
				t.Fatal(err)
			}
			lease, err := s.f.store.ClaimForkCheckinJobID(ctx, id, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if dispatched {
				if err := s.f.store.MarkForkCheckinDispatched(ctx, lease); err != nil {
					t.Fatal(err)
				}
			}
			s.clock.add(48 * time.Hour)
			s.reopen(t)
			s.tick(t)
			jobs := s.jobs(t)
			if dispatched {
				if len(jobs) != 1 || jobs[0].Status != forkcheckin.JobStatusUncertain || s.adapter.posts.Load() != 0 {
					t.Fatalf("recovery replayed: %+v", jobs)
				}
				s.tick(t)
			} else if len(jobs) != 2 || jobs[0].Status != forkcheckin.JobStatusCancelled || jobs[0].FailureCode != "day_expired" {
				t.Fatalf("old queue executed: %+v", jobs)
			}
			if s.adapter.posts.Load() != 1 {
				t.Fatal("current day not scheduled")
			}
		})
	}
}

func TestForkCheckinSchedulerConcurrentPrepareSharesOneIntent(t *testing.T) {
	s := newScheduleFixture(t, time.Now())
	ctx := context.Background()
	second := openWithKey(t, s.f.path, testLocalKey(t, 0x97), nil)
	defer second.Close()
	second.now = s.clock.now
	var wg sync.WaitGroup
	ids := make(chan forkcheckin.JobID, 2)
	errs := make(chan error, 2)
	for _, store := range []*Store{s.f.store, second} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := store.PrepareForkCheckinScheduledJob(ctx, s.f.account.ID, s.clock.now())
			ids <- id
			errs <- err
		}()
	}
	wg.Wait()
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	one, two := <-ids, <-ids
	if one == "" || one != two || len(s.jobs(t)) != 1 {
		t.Fatalf("duplicate intents: %q %q", one, two)
	}
	lease, err := s.f.store.ClaimForkCheckinJobID(ctx, one, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.ClaimForkCheckinJobID(ctx, two, time.Minute); !errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatal("shared intent not fenced", err)
	}
	if err := s.f.store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
}

func TestForkCheckinSchedulerAuthoritativeDateRebindingKeepsBudgetAndDedup(t *testing.T) {
	s := newScheduleFixture(t, time.Date(2026, time.June, 2, 1, 0, 0, 0, time.UTC))
	s.adapter.day = s.clock.now().Add(-24 * time.Hour).Format(time.DateOnly)
	s.tick(t)
	s.tick(t)
	jobs := s.jobs(t)
	if len(jobs) != 1 || jobs[0].SiteDay != s.adapter.day || jobs[0].ScheduleDay == jobs[0].SiteDay || jobs[0].Status != forkcheckin.JobStatusSuccess {
		t.Fatalf("site date not preferred: %+v", jobs)
	}
	// A later local bucket still cannot submit again for the prior site day.
	s.clock.add(24 * time.Hour)
	s.adapter.checked = map[string]bool{} // Site inconsistency must not override durable dispatch.
	s.tick(t)
	jobs = s.jobs(t)
	if len(jobs) != 2 || jobs[1].Status != forkcheckin.JobStatusCancelled || jobs[1].FailureCode != "day_resolved" || s.adapter.posts.Load() != 1 {
		t.Fatalf("rebind bypassed dedup: %+v", jobs)
	}
}
