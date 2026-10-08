package forkcheckin

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

type schedulerFixtureStore struct {
	mu       sync.Mutex
	accounts []ScheduledAccount
	times    []time.Time
}

func (s *schedulerFixtureStore) ListForkCheckinScheduledAccounts(_ context.Context, after AccountID, limit int) ([]ScheduledAccount, error) {
	var result []ScheduledAccount
	for _, account := range s.accounts {
		if account.ID > after && len(result) < limit {
			result = append(result, account)
		}
	}
	return result, nil
}
func (s *schedulerFixtureStore) PrepareForkCheckinScheduledJob(_ context.Context, id AccountID, now time.Time) (JobID, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.times = append(s.times, now)
	return JobID(id), nil
}

type schedulerExecutorFunc func(context.Context, JobID) (JobReceipt, error)

func (f schedulerExecutorFunc) Execute(ctx context.Context, id JobID) (JobReceipt, error) {
	return f(ctx, id)
}

type steppingSchedulerClock struct {
	times  []time.Time
	index  int
	cancel context.CancelFunc
}

func (c *steppingSchedulerClock) Now() time.Time { return c.times[c.index] }
func (c *steppingSchedulerClock) Wait(ctx context.Context, _ time.Duration) error {
	c.index++
	if c.index == len(c.times) {
		c.cancel()
		return ctx.Err()
	}
	return nil
}

func TestSchedulerRunRecomputesWallClockOnEveryWake(t *testing.T) {
	zone, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, time.March, 8, 1, 59, 0, 0, zone)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clock := &steppingSchedulerClock{times: []time.Time{start, start.Add(time.Minute), start.AddDate(0, 0, 4), start.Add(-time.Hour)}, cancel: cancel}
	store := &schedulerFixtureStore{accounts: []ScheduledAccount{{ID: "acct_one", DashboardBaseURL: "https://one.example"}}}
	scheduler, _ := NewScheduler(store, schedulerExecutorFunc(func(context.Context, JobID) (JobReceipt, error) { return JobReceipt{}, nil }))
	if len(store.times) != 0 {
		t.Fatal("constructor performed work")
	}
	if err := scheduler.Run(ctx, clock); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(store.times) != len(clock.times) {
		t.Fatalf("wakes: %v", store.times)
	}
	for i, got := range store.times {
		if !got.Equal(clock.times[i]) {
			t.Fatalf("wake %d reused an old clock", i)
		}
	}
}

func TestSchedulerBoundsWorkersSerializesSitesAndPages(t *testing.T) {
	store := &schedulerFixtureStore{}
	for i := 0; i < 105; i++ {
		store.accounts = append(store.accounts, ScheduledAccount{ID: AccountID(fmt.Sprintf("acct_%03d", i)), DashboardBaseURL: fmt.Sprintf("https://site%d.example/path%d", i%3, i)})
	}
	var mu sync.Mutex
	active, peak, calls := 0, 0, 0
	sites := map[int]bool{}
	executor := schedulerExecutorFunc(func(ctx context.Context, id JobID) (JobReceipt, error) {
		var n int
		_, _ = fmt.Sscanf(string(id), "acct_%03d", &n)
		mu.Lock()
		if sites[n%3] {
			t.Error("same site ran concurrently")
		}
		sites[n%3] = true
		active++
		calls++
		if active > peak {
			peak = active
		}
		mu.Unlock()
		select {
		case <-ctx.Done():
		case <-time.After(time.Millisecond):
		}
		mu.Lock()
		active--
		delete(sites, n%3)
		mu.Unlock()
		return JobReceipt{}, nil
	})
	scheduler, _ := NewScheduler(store, executor)
	if err := scheduler.Tick(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if calls != 105 || peak != 2 || active != 0 {
		t.Fatalf("calls=%d peak=%d active=%d", calls, peak, active)
	}
	if len(scheduler.gate.sites) != 0 {
		t.Fatal("gate retained inactive sites")
	}
}

func TestSchedulerStopJoinsWorkersAndRejectsConcurrentRun(t *testing.T) {
	store := &schedulerFixtureStore{accounts: []ScheduledAccount{{ID: "acct_one", DashboardBaseURL: "https://one.example"}, {ID: "acct_two", DashboardBaseURL: "https://two.example"}}}
	started, exited := make(chan struct{}, 2), make(chan struct{}, 2)
	release := make(chan struct{})
	scheduler, _ := NewScheduler(store, schedulerExecutorFunc(func(ctx context.Context, _ JobID) (JobReceipt, error) {
		started <- struct{}{}
		<-ctx.Done()
		<-release
		exited <- struct{}{}
		return JobReceipt{}, ctx.Err()
	}))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- scheduler.Run(ctx, nil) }()
	for range 2 {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("worker did not start")
		}
	}
	if err := scheduler.Tick(ctx, time.Now()); !errors.Is(err, ErrSchedulerRunning) {
		t.Fatalf("concurrent tick: %v", err)
	}
	cancel()
	select {
	case <-done:
		t.Fatal("returned before worker cleanup")
	default:
	}
	close(release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("stop hung")
	}
	if len(exited) != 2 {
		t.Fatal("worker cleanup incomplete")
	}
}

func TestRateLimitedParsesOnlySafeDeadline(t *testing.T) {
	now := time.Date(2026, time.January, 1, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		value string
		delay time.Duration
	}{
		{"120", 2 * time.Minute}, {now.Add(time.Hour).Format(http.TimeFormat), time.Hour}, {"-1", 0}, {"private-site-prose", 0}, {"18446744073709551615", 7 * 24 * time.Hour},
	} {
		err := RateLimited(tc.value, now)
		var limited *RateLimitError
		if !errors.Is(err, ErrRateLimited) || !errors.As(err, &limited) {
			t.Fatal(err)
		}
		if tc.delay == 0 {
			if !limited.NotBefore.IsZero() {
				t.Fatal("invalid retry deadline accepted")
			}
		} else if !limited.NotBefore.Equal(now.Add(tc.delay)) {
			t.Fatalf("deadline=%v", limited.NotBefore)
		}
		if err.Error() != ErrRateLimited.Error() {
			t.Fatal("site text leaked")
		}
	}
}
