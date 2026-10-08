package forkcheckin

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"
)

const (
	// AutomaticAttemptLimit includes interrupted pre-dispatch executions, not
	// just completed jobs. A restart cannot replenish the day's budget.
	AutomaticAttemptLimit = 3
	schedulerPollInterval = time.Minute
)

var ErrSchedulerRunning = errors.New("check-in scheduler is already running")
var ErrDayResolved = errors.New("check-in day already has proof or dispatch intent")
var ErrExecutionBusy = errors.New("check-in account has an active lease")

// ScheduledAccount contains only the fields required to bound execution.
// Bindings are deliberately absent: one account is scheduled once.
type ScheduledAccount struct {
	ID               AccountID
	DashboardBaseURL string
}

type SchedulerStore interface {
	// List uses an exclusive account ID cursor and returns at most limit rows.
	// Include automatic accounts and accounts with unfinished jobs, even when
	// automatic check-in was subsequently disabled.
	ListForkCheckinScheduledAccounts(context.Context, AccountID, int) ([]ScheduledAccount, error)
	// Prepare atomically resumes work or creates one persistent daily intent.
	// It never resets a terminal job. An empty ID means no work is due. Leased
	// work, same-day dispatch, automatic consent, retries and clock changes
	// must all be checked under the store's writer lock.
	PrepareForkCheckinScheduledJob(context.Context, AccountID, time.Time) (JobID, error)
}

type JobExecutor interface {
	Execute(context.Context, JobID) (JobReceipt, error)
}

// SchedulerClock is injectable without changing execution/lease timeouts.
// Wait must unblock on cancellation; it must not retain a timer afterwards.
type SchedulerClock interface {
	Now() time.Time
	Wait(context.Context, time.Duration) error
}

type wallSchedulerClock struct{}

func (wallSchedulerClock) Now() time.Time { return time.Now() }
func (wallSchedulerClock) Wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Scheduler is dormant until Run (or Tick) is called. Run returns only after
// both workers exit, so the owner can safely close the Vault afterwards. The
// minute wakeup scans local metadata only; completed/blocked accounts generate
// no HTTP polling. Each wake recomputes the calendar day rather than adding 24h.
type Scheduler struct {
	store    SchedulerStore
	executor JobExecutor
	mu       sync.Mutex
	gate     executionGate
}

func NewScheduler(store SchedulerStore, executor JobExecutor) (*Scheduler, error) {
	if store == nil || executor == nil {
		return nil, errors.New("check-in scheduler dependencies unavailable")
	}
	return &Scheduler{store: store, executor: executor}, nil
}

func (s *Scheduler) Run(ctx context.Context, clock SchedulerClock) error {
	if !s.mu.TryLock() {
		return ErrSchedulerRunning
	}
	defer s.mu.Unlock()
	if clock == nil {
		clock = wallSchedulerClock{}
	}
	for {
		// One account's local failure must not stop scheduling for all of
		// them. Only cancellation ends the loop; the next wake retries.
		if err := s.tick(ctx, clock.Now); err != nil && ctx.Err() != nil {
			return ctx.Err()
		}
		if err := clock.Wait(ctx, schedulerPollInterval); err != nil {
			return err
		}
	}
}

// Tick performs one bounded wakeup, also useful for a host-provided resume
// signal. It never catches up missed dates or waits through retry backoff.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) error {
	if !s.mu.TryLock() {
		return ErrSchedulerRunning
	}
	defer s.mu.Unlock()
	return s.tick(ctx, func() time.Time { return now })
}

func (s *Scheduler) tick(ctx context.Context, now func() time.Time) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan ScheduledAccount)
	var workers sync.WaitGroup
	var mu sync.Mutex
	var failures []error
	record := func(err error) {
		mu.Lock()
		// Bound error retention even when every account fails locally.
		if len(failures) < 2 {
			failures = append(failures, err)
		}
		mu.Unlock()
	}
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for account := range work {
				release, err := s.gate.acquire(ctx, account.DashboardBaseURL)
				if err != nil {
					record(err)
					continue
				}
				id, err := s.store.PrepareForkCheckinScheduledJob(ctx, account.ID, now())
				if err == nil && id != "" {
					_, err = s.executor.Execute(ctx, id)
				}
				release()
				if err != nil && !errors.Is(err, ErrExecutionBusy) && !errors.Is(err, ErrRevisionChanged) && !errors.Is(err, ErrDayResolved) {
					record(err)
				}
			}
		}()
	}
	var after AccountID
scan:
	for {
		if ctx.Err() != nil {
			record(ctx.Err())
			break
		}
		accounts, err := s.store.ListForkCheckinScheduledAccounts(ctx, after, MaxPageSize)
		if err != nil {
			record(err)
			break
		}
		if len(accounts) > MaxPageSize {
			record(errors.New("invalid scheduler account page"))
			break
		}
		for _, account := range accounts {
			if account.ID.Validate() != nil || account.ID <= after {
				record(errors.New("invalid scheduler account cursor"))
				break scan
			}
			after = account.ID
			select {
			case work <- account:
			case <-ctx.Done():
				record(ctx.Err())
				break scan
			}
		}
		if len(accounts) < MaxPageSize {
			break
		}
	}
	close(work)
	workers.Wait()
	return errors.Join(failures...)
}

// AutomaticRetryDelay is the minimum delay after a safe failure. Retry-After
// may extend it. Slots and actual claims are both persisted and bounded.
func AutomaticRetryDelay(attempts int) time.Duration {
	if attempts <= 1 {
		return time.Minute
	}
	return 5 * time.Minute
}

// executionGate is shared by all calls to one Runner, including manual ones.
// It retains only active origins, not an unbounded queue or account history.
// Host-level serialization also covers two dashboard subpaths on one site.
type executionGate struct {
	mu      sync.Mutex
	sites   map[string]bool
	changed chan struct{}
}

func (g *executionGate) acquire(ctx context.Context, base string) (func(), error) {
	normalized, err := NormalizeDashboardURL(base)
	if err != nil {
		return nil, ErrNetworkScope
	}
	parsed, _ := url.Parse(normalized)
	site := parsed.Hostname()
	for {
		g.mu.Lock()
		if err := ctx.Err(); err != nil {
			g.mu.Unlock()
			return nil, err
		}
		if g.sites == nil {
			g.sites = make(map[string]bool)
			g.changed = make(chan struct{})
		}
		if len(g.sites) < 2 && !g.sites[site] {
			g.sites[site] = true
			g.mu.Unlock()
			return func() {
				g.mu.Lock()
				delete(g.sites, site)
				close(g.changed)
				g.changed = make(chan struct{})
				g.mu.Unlock()
			}, nil
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}
