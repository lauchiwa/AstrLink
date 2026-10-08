package forkcheckin

import (
	"context"
	"errors"
	"sync"
	"time"
)

var (
	// ErrExtensionDisabled is returned for work requested while the switch is
	// off or the module failed to initialize. Callers report "unavailable".
	ErrExtensionDisabled = errors.New("check-in extension is disabled")
	// ErrExtensionStopping refuses work after shutdown began.
	ErrExtensionStopping = errors.New("check-in extension is stopping")
)

// Stable initialization and persistence reasons for ExtensionStatus. They are
// codes, never wrapped error text, so no path, key or site detail escapes.
const (
	InitReasonFailed         = "init_failed"
	InitReasonSettingsWrite  = "settings_write_failed"
	InitReasonStopIncomplete = "stop_incomplete"
)

// ModuleStore is what the lifecycle needs from the extension's storage. The
// storage itself is shared and owned by the Core; the module never closes it.
type ModuleStore interface {
	ExecutionStore
	SchedulerStore
}

// ModuleParts are created only when the extension is enabled. Close must
// cancel and wait for every network operation the adapter admitted (for
// example TransportFactory.Close) and must not clear shared keys.
type ModuleParts struct {
	Store   ModuleStore
	Vault   Vault
	Adapter SiteAdapter
	Close   func(context.Context) error
	// CountAccounts is optional; Status reports zero while disabled.
	CountAccounts func(context.Context) (int, error)
	// Reader serves the cache-only control reads. Optional; without it the
	// read routes report the extension as unavailable.
	Reader Reader
	// Writer serves operator account writes. Optional, like Reader.
	Writer AccountWriter
	// Jobs accepts and cancels jobs. Optional, like Reader.
	Jobs JobWriter
	// Authorizations stores verified sessions. Optional, like Reader.
	Authorizations AuthorizationStore
	// VerifyAuthorization must use request-scoped clients, never the job
	// adapter's account cache. Required when Authorizations is provided.
	VerifyAuthorization func(context.Context, AccountSnapshot) (SiteIdentity, error)
	// Invalidate drops the adapter's cached network client for one account,
	// for example TransportFactory.Invalidate. Optional.
	Invalidate func(AccountID)
}

// AccountViewPage is one page of public account views.
type AccountViewPage struct {
	Items      []AccountView `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

// Reader is the local-state read surface behind the control API. It never
// contacts a site, opens the vault or changes a job.
type Reader interface {
	GetAccount(context.Context, AccountID) (AccountView, error)
	ListAccounts(context.Context, ListOptions) (AccountViewPage, error)
	GetJob(context.Context, JobID) (JobReceipt, error)
	ListJobs(context.Context, AccountID, ListOptions) (JobPage, error)
	// GetBatch returns a batch parent's children in their current state.
	GetBatch(context.Context, JobID) ([]JobReceipt, error)
}

// ModuleFactory creates the extension tables, store adapter and network
// clients. It is never called while the switch is off.
type ModuleFactory func(context.Context) (ModuleParts, error)

// FacadeConfig wires the lifecycle. SettingsPath is the private switch file.
type FacadeConfig struct {
	SettingsPath           string
	Factory                ModuleFactory
	NativeCaptureSupported bool
	// Clock is for tests only; production uses wall time.
	Clock SchedulerClock
}

// Facade owns the extension's on/off state and its bounded worker lifetime.
// While disabled it holds no table, timer, goroutine or network client, and
// Status only reflects the persisted switch. Shutdown order is: refuse new
// work, then cancel workers and close network clients together, then wait
// for every worker and admitted request. Only after Stop returns nil may the
// owner close the shared Store and its keys; a timeout never forces that.
type Facade struct {
	config FacadeConfig

	lifecycle sync.Mutex // Serializes Start, Enable, Disable and Stop.

	mu         sync.Mutex
	enabled    bool
	reason     string
	stopped    bool
	module     *module
	retiring   *module
	accounting func(context.Context) (int, error)
}

type module struct {
	parts     ModuleParts
	runner    *Runner
	cancel    context.CancelFunc
	ctx       context.Context
	scheduler chan struct{}
	active    sync.WaitGroup
	closing   bool
	// inflight maps a job started by launch to the cancel of its execution.
	inflight  map[JobID]context.CancelFunc
	launching chan struct{}
	finished  chan struct{}
	closeErr  error
	// authorizations live and die with this module generation.
	authorizations *authorizationRegistry
}

// NewFacade reads the switch without touching storage or the network.
func NewFacade(config FacadeConfig) (*Facade, error) {
	if config.SettingsPath == "" || config.Factory == nil {
		return nil, errors.New("check-in extension dependencies unavailable")
	}
	settings, reason := LoadSettings(config.SettingsPath)
	return &Facade{config: config, enabled: settings.Enabled, reason: reason}, nil
}

// Start initializes the module when the stored switch is on. A failure is
// visible in Status and retried by Enable; it never fails the Core.
func (f *Facade) Start(ctx context.Context) {
	f.lifecycle.Lock()
	defer f.lifecycle.Unlock()
	f.mu.Lock()
	enabled, stopped := f.enabled, f.stopped
	f.mu.Unlock()
	if enabled && !stopped {
		f.startLocked(ctx)
	}
}

// Status is cheap and safe in every state; it is the always-available route.
func (f *Facade) Status(ctx context.Context) ExtensionStatus {
	f.mu.Lock()
	status := ExtensionStatus{
		ProtocolVersion:        ProtocolVersion,
		Enabled:                f.enabled,
		Initialized:            f.module != nil,
		LastInitError:          f.reason,
		NativeCaptureSupported: f.config.NativeCaptureSupported,
	}
	count := f.accounting
	f.mu.Unlock()
	if status.Initialized && count != nil {
		if accounts, err := count(ctx); err == nil && accounts >= 0 {
			status.AccountCount = accounts
		}
	}
	return status
}

// Enable persists the switch first, so a failed initialization remains
// enabled and retryable rather than reverting the operator's choice.
func (f *Facade) Enable(ctx context.Context) (ExtensionStatus, error) {
	f.lifecycle.Lock()
	defer f.lifecycle.Unlock()
	f.mu.Lock()
	stopped, running := f.stopped, f.module != nil
	f.mu.Unlock()
	if stopped {
		return f.Status(ctx), ErrExtensionStopping
	}
	if err := SaveSettings(f.config.SettingsPath, Settings{Enabled: true}); err != nil {
		f.setReason(InitReasonSettingsWrite)
		return f.Status(ctx), err
	}
	f.mu.Lock()
	f.enabled = true
	f.reason = ""
	f.mu.Unlock()
	if !running {
		f.startLocked(ctx)
	}
	return f.Status(ctx), nil
}

// Disable persists the switch, then stops the module. The shared Store stays
// open. If the stop does not finish before ctx, Status reports it and a later
// Disable or Stop resumes waiting; the module is never abandoned mid-I/O.
func (f *Facade) Disable(ctx context.Context) (ExtensionStatus, error) {
	f.lifecycle.Lock()
	defer f.lifecycle.Unlock()
	if err := SaveSettings(f.config.SettingsPath, Settings{Enabled: false}); err != nil {
		f.setReason(InitReasonSettingsWrite)
		return f.Status(ctx), err
	}
	f.mu.Lock()
	f.enabled = false
	f.reason = ""
	f.mu.Unlock()
	err := f.stopLocked(ctx)
	return f.Status(ctx), err
}

// Stop is the Core shutdown path. It does not change the stored switch. It
// returns nil only once every worker and network operation has exited.
func (f *Facade) Stop(ctx context.Context) error {
	f.lifecycle.Lock()
	defer f.lifecycle.Unlock()
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	return f.stopLocked(ctx)
}

// Execute runs one job on the shared Runner. Manual callers and the
// scheduler share its site gate and account leases.
func (f *Facade) Execute(ctx context.Context, id JobID) (JobReceipt, error) {
	f.mu.Lock()
	current := f.module
	if current == nil {
		f.mu.Unlock()
		if f.isStopped() {
			return JobReceipt{}, ErrExtensionStopping
		}
		return JobReceipt{}, ErrExtensionDisabled
	}
	if current.closing {
		f.mu.Unlock()
		return JobReceipt{}, ErrExtensionStopping
	}
	current.active.Add(1)
	f.mu.Unlock()
	defer current.active.Done()
	execution, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(current.ctx, cancel)
	defer stop()
	return current.runner.Execute(execution, id)
}

func (f *Facade) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// maxLaunchedJobs bounds background executions started by control requests.
// Work that does not fit stays queued for the scheduler's next wakeup, so
// the bound never drops a job.
const maxLaunchedJobs = 4

// jobs runs one job write against the live module, then starts or interrupts
// the executions it names. The control request never waits for either.
func (f *Facade) jobs(ctx context.Context, run func(context.Context, JobWriter) (JobWriteResult, error)) (JobWriteResult, error) {
	var result JobWriteResult
	err := f.withModule(ctx, func(ctx context.Context, parts ModuleParts) (err error) {
		if parts.Jobs == nil {
			return ErrExtensionDisabled
		}
		result, err = run(ctx, parts.Jobs)
		return err
	})
	if err != nil {
		return result, err
	}
	f.mu.Lock()
	current := f.module
	if current != nil && !current.closing {
		for _, id := range result.Interrupt {
			if cancel := current.inflight[id]; cancel != nil {
				cancel()
			}
		}
		for _, id := range result.Launch {
			f.launchLocked(current, id)
		}
	}
	f.mu.Unlock()
	return result, nil
}

// launchLocked starts one execution in the background when a slot is free.
// It requires f.mu. The runner's account lease fences it against the
// scheduler, so starting a job the scheduler also picks up is harmless.
func (f *Facade) launchLocked(current *module, id JobID) {
	if _, running := current.inflight[id]; running {
		return
	}
	select {
	case current.launching <- struct{}{}:
	default:
		return
	}
	execution, cancel := context.WithCancel(current.ctx)
	current.inflight[id] = cancel
	current.active.Add(1)
	go func() {
		defer current.active.Done()
		defer func() { <-current.launching }()
		defer func() {
			f.mu.Lock()
			delete(current.inflight, id)
			f.mu.Unlock()
			cancel()
		}()
		// The outcome is durable in storage; nothing waits on it here.
		_, _ = current.runner.Execute(execution, id)
	}()
}

// read runs one local-state read against the live module.
func (f *Facade) read(ctx context.Context, run func(context.Context, Reader) error) error {
	return f.withModule(ctx, func(ctx context.Context, parts ModuleParts) error {
		if parts.Reader == nil {
			return ErrExtensionDisabled
		}
		return run(ctx, parts.Reader)
	})
}

// write runs one operator write against the live module.
func (f *Facade) write(ctx context.Context, run func(context.Context, AccountWriter) error) error {
	return f.withModule(ctx, func(ctx context.Context, parts ModuleParts) error {
		if parts.Writer == nil {
			return ErrExtensionDisabled
		}
		return run(ctx, parts.Writer)
	})
}

// authorize runs one authorization step against the live module. Sessions
// belong to the module generation, so disabling or restarting expires them.
func (f *Facade) authorize(ctx context.Context, run func(context.Context, *authorizer) error) error {
	return f.withLive(ctx, func(ctx context.Context, current *module) error {
		parts := current.parts
		if parts.Authorizations == nil || parts.VerifyAuthorization == nil || current.authorizations == nil {
			return ErrExtensionDisabled
		}
		return run(ctx, &authorizer{registry: current.authorizations, store: parts.Authorizations,
			verifyIdentity: parts.VerifyAuthorization})
	})
}

// withModule registers control work as active, so Stop waits for it before
// the owner closes the shared Store, and cancels it when the module retires.
func (f *Facade) withModule(ctx context.Context, run func(context.Context, ModuleParts) error) error {
	return f.withLive(ctx, func(ctx context.Context, current *module) error {
		return run(ctx, current.parts)
	})
}

func (f *Facade) withLive(ctx context.Context, run func(context.Context, *module) error) error {
	f.mu.Lock()
	current := f.module
	if current == nil {
		f.mu.Unlock()
		if f.isStopped() {
			return ErrExtensionStopping
		}
		return ErrExtensionDisabled
	}
	if current.closing {
		f.mu.Unlock()
		return ErrExtensionStopping
	}
	current.active.Add(1)
	f.mu.Unlock()
	defer current.active.Done()
	working, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(current.ctx, cancel)
	defer stop()
	return run(working, current)
}

func (f *Facade) setReason(reason string) {
	f.mu.Lock()
	f.reason = reason
	f.mu.Unlock()
}

// startLocked requires f.lifecycle. Any retired module must finish first, so
// two generations never run the same jobs concurrently.
func (f *Facade) startLocked(ctx context.Context) {
	f.mu.Lock()
	retiring := f.retiring
	f.mu.Unlock()
	if retiring != nil {
		if f.waitRetired(ctx, retiring) != nil {
			f.setReason(InitReasonStopIncomplete)
			return
		}
	}
	parts, err := f.config.Factory(ctx)
	if err != nil {
		f.setReason(InitReasonFailed)
		return
	}
	runner, err := NewRunner(parts.Store, parts.Vault, parts.Adapter)
	var scheduler *Scheduler
	if err == nil {
		scheduler, err = NewScheduler(parts.Store, runner)
	}
	if err != nil {
		if parts.Close != nil {
			_ = parts.Close(context.WithoutCancel(ctx))
		}
		f.setReason(InitReasonFailed)
		return
	}
	moduleCtx, cancel := context.WithCancel(context.Background())
	clock := f.config.Clock
	now := time.Now
	if clock != nil {
		now = clock.Now
	}
	current := &module{
		parts: parts, runner: runner, ctx: moduleCtx, cancel: cancel, scheduler: make(chan struct{}),
		inflight: make(map[JobID]context.CancelFunc), launching: make(chan struct{}, maxLaunchedJobs),
		authorizations: newAuthorizationRegistry(now),
	}
	go func() {
		defer close(current.scheduler)
		_ = scheduler.Run(moduleCtx, clock)
	}()
	f.mu.Lock()
	f.module = current
	f.reason = ""
	f.accounting = parts.CountAccounts
	f.mu.Unlock()
}

// stopLocked requires f.lifecycle.
func (f *Facade) stopLocked(ctx context.Context) error {
	f.mu.Lock()
	current := f.module
	if current != nil {
		// Refuse new Execute calls before cancelling, so none can slip in
		// between the cancellation and the wait.
		current.closing = true
		f.module = nil
		f.accounting = nil
		f.retiring = current
	} else {
		current = f.retiring
	}
	f.mu.Unlock()
	if current == nil {
		return nil
	}
	return f.waitRetired(ctx, current)
}

func (f *Facade) waitRetired(ctx context.Context, current *module) error {
	f.mu.Lock()
	if current.finished == nil {
		current.finished = make(chan struct{})
		current.cancel()
		// Close network clients concurrently with cancellation, not after the
		// workers: a post-dispatch recheck deliberately ignores cancellation
		// for its own 15s budget, and must instead fail fast as invalidated.
		// The job then settles as uncertain with its intent already durable.
		closed := make(chan struct{})
		go func() {
			defer close(closed)
			if current.parts.Close != nil {
				current.closeErr = current.parts.Close(context.Background())
			}
		}()
		go func() {
			defer close(current.finished)
			<-current.scheduler
			current.active.Wait()
			<-closed
		}()
	}
	finished := current.finished
	f.mu.Unlock()
	select {
	case <-finished:
	case <-ctx.Done():
		f.setReason(InitReasonStopIncomplete)
		return ctx.Err()
	}
	f.mu.Lock()
	if f.retiring == current {
		f.retiring = nil
	}
	if f.reason == InitReasonStopIncomplete {
		f.reason = ""
	}
	f.mu.Unlock()
	return current.closeErr
}
