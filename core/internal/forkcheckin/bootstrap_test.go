package forkcheckin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSettingsLoadNeverBlocksCore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checkin.json")
	cases := []struct {
		name, content, reason string
		enabled               bool
	}{
		{"valid_on", `{"schema_version":1,"enabled":true}`, "", true},
		{"valid_off", `{"schema_version":1,"enabled":false}`, "", false},
		{"corrupt", `{"schema_version":1,`, SettingsReasonInvalid, false},
		{"unknown_version", `{"schema_version":2,"enabled":true,"future":1}`, SettingsReasonUnsupported, false},
		{"unknown_field", `{"schema_version":1,"enabled":true,"accounts":[]}`, SettingsReasonInvalid, false},
		{"missing_enabled", `{"schema_version":1}`, SettingsReasonInvalid, false},
		{"trailing", `{"schema_version":1,"enabled":true}{}`, SettingsReasonInvalid, false},
		{"oversized", `{"schema_version":1,"enabled":true,"x":"` + string(make([]byte, 5000)) + `"}`, SettingsReasonInvalid, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			settings, reason := LoadSettings(path)
			if reason != tc.reason || settings.Enabled != tc.enabled {
				t.Fatalf("settings=%+v reason=%q", settings, reason)
			}
		})
	}
	if settings, reason := LoadSettings(filepath.Join(dir, "absent.json")); settings.Enabled || reason != "" {
		t.Fatal("missing settings must be the silent default")
	}
	if settings, reason := LoadSettings(dir); settings.Enabled || reason == "" {
		t.Fatal("unreadable settings must disable with a reason")
	}
}

func TestSaveSettingsIsPrivateAndAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "checkin.json")
	for _, enabled := range []bool{true, false, true} {
		if err := SaveSettings(path, Settings{Enabled: enabled}); err != nil {
			t.Fatal(err)
		}
		if settings, reason := LoadSettings(path); reason != "" || settings.Enabled != enabled {
			t.Fatalf("round trip=%+v %q", settings, reason)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("settings mode=%v", info.Mode().Perm())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
}

// lifecycleStore runs every job to an immediate local cancellation; it only
// needs to prove which components exist and when they are touched.
type lifecycleStore struct {
	runnerStore
	mu       sync.Mutex
	listed   atomic.Int32
	accounts []ScheduledAccount
	block    chan struct{}
}

func (s *lifecycleStore) ListForkCheckinScheduledAccounts(ctx context.Context, after AccountID, _ int) ([]ScheduledAccount, error) {
	s.listed.Add(1)
	if after != "" {
		return nil, nil
	}
	return s.accounts, nil
}

func (s *lifecycleStore) PrepareForkCheckinScheduledJob(ctx context.Context, _ AccountID, _ time.Time) (JobID, error) {
	if s.block != nil {
		select {
		case <-s.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "", nil
}

type lifecycleVault struct {
	gets    atomic.Int32
	stopped atomic.Bool
	t       *testing.T
}

func (v *lifecycleVault) Get(context.Context, AccountID) ([]byte, error) {
	v.gets.Add(1)
	if v.stopped.Load() {
		v.t.Error("vault accessed after Stop returned")
	}
	return nil, ErrCredentialUnavailable
}
func (v *lifecycleVault) Put(context.Context, AccountID, []byte) error { return nil }
func (v *lifecycleVault) Delete(context.Context, AccountID) error      { return nil }

type lifecycleFixture struct {
	path    string
	calls   atomic.Int32
	fail    atomic.Bool
	closes  atomic.Int32
	store   *lifecycleStore
	vault   *lifecycleVault
	facade  *Facade
	closeFn func(context.Context) error
}

func newLifecycleFixture(t *testing.T, enabled *bool) *lifecycleFixture {
	t.Helper()
	f := &lifecycleFixture{path: filepath.Join(t.TempDir(), "checkin.json"), store: &lifecycleStore{}, vault: &lifecycleVault{t: t}}
	if enabled != nil {
		if err := SaveSettings(f.path, Settings{Enabled: *enabled}); err != nil {
			t.Fatal(err)
		}
	}
	facade, err := NewFacade(FacadeConfig{SettingsPath: f.path, Factory: func(context.Context) (ModuleParts, error) {
		f.calls.Add(1)
		if f.fail.Load() {
			return ModuleParts{}, errors.New("private init detail")
		}
		return ModuleParts{Store: f.store, Vault: f.vault, Adapter: &runnerAdapter{store: &f.store.runnerStore},
			Close: func(ctx context.Context) error {
				f.closes.Add(1)
				if f.closeFn != nil {
					return f.closeFn(ctx)
				}
				return nil
			},
			CountAccounts: func(context.Context) (int, error) { return 3, nil }}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	f.facade = facade
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = facade.Stop(ctx)
	})
	return f
}

func TestFacadeDisabledCreatesNothing(t *testing.T) {
	for _, name := range []string{"missing", "off", "corrupt"} {
		t.Run(name, func(t *testing.T) {
			var enabled *bool
			if name == "off" {
				off := false
				enabled = &off
			}
			f := newLifecycleFixture(t, enabled)
			if name == "corrupt" {
				if err := os.WriteFile(f.path, []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
				facade, _ := NewFacade(FacadeConfig{SettingsPath: f.path, Factory: f.facade.config.Factory})
				f.facade = facade
			}
			before := runtime.NumGoroutine()
			f.facade.Start(context.Background())
			status := f.facade.Status(context.Background())
			if status.Enabled || status.Initialized || status.AccountCount != 0 || f.calls.Load() != 0 || f.store.listed.Load() != 0 {
				t.Fatalf("disabled extension did work: %+v calls=%d", status, f.calls.Load())
			}
			if name == "corrupt" && status.LastInitError != SettingsReasonInvalid {
				t.Fatalf("corrupt settings reason=%q", status.LastInitError)
			}
			if _, err := f.facade.Execute(context.Background(), "job_any"); !errors.Is(err, ErrExtensionDisabled) {
				t.Fatalf("disabled execute=%v", err)
			}
			if runtime.NumGoroutine() > before {
				t.Fatal("disabled extension started a goroutine")
			}
		})
	}
}

func TestFacadeEnableFailureIsVisibleAndRetryable(t *testing.T) {
	f := newLifecycleFixture(t, nil)
	f.fail.Store(true)
	status, err := f.facade.Enable(context.Background())
	if err != nil || !status.Enabled || status.Initialized || status.LastInitError != InitReasonFailed {
		t.Fatalf("failed enable status=%+v err=%v", status, err)
	}
	if settings, _ := LoadSettings(f.path); !settings.Enabled {
		t.Fatal("operator choice was not persisted")
	}
	f.fail.Store(false)
	status, err = f.facade.Enable(context.Background())
	if err != nil || !status.Initialized || status.LastInitError != "" || status.AccountCount != 3 || f.calls.Load() != 2 {
		t.Fatalf("retry status=%+v err=%v", status, err)
	}
	// A repeated enable is idempotent; it must not start a second module.
	if _, err := f.facade.Enable(context.Background()); err != nil || f.calls.Load() != 2 {
		t.Fatal("repeated enable reinitialized the module")
	}
	// A restarted Core resumes the persisted switch.
	facade, _ := NewFacade(FacadeConfig{SettingsPath: f.path, Factory: f.facade.config.Factory})
	facade.Start(context.Background())
	defer facade.Stop(context.Background())
	if !facade.Status(context.Background()).Initialized {
		t.Fatal("enabled switch was not resumed after restart")
	}
}

func TestFacadeDisableStopsWorkersWithoutClosingStore(t *testing.T) {
	on := true
	f := newLifecycleFixture(t, &on)
	f.store.accounts = []ScheduledAccount{{ID: "acct_one", DashboardBaseURL: "https://one.example"}}
	f.store.block = make(chan struct{})
	f.facade.Start(context.Background())
	deadline := time.Now().Add(3 * time.Second)
	for f.store.listed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	status, err := f.facade.Disable(context.Background())
	if err != nil || status.Enabled || status.Initialized || f.closes.Load() != 1 {
		t.Fatalf("disable status=%+v err=%v closes=%d", status, err, f.closes.Load())
	}
	f.vault.stopped.Store(true)
	if _, err := f.facade.Execute(context.Background(), "job_any"); !errors.Is(err, ErrExtensionDisabled) {
		t.Fatalf("execute after disable=%v", err)
	}
	if settings, _ := LoadSettings(f.path); settings.Enabled {
		t.Fatal("disable was not persisted")
	}
	// Re-enabling creates a fresh module; the old one is fully retired.
	f.vault.stopped.Store(false)
	f.store.block = nil
	if status, err := f.facade.Enable(context.Background()); err != nil || !status.Initialized || f.calls.Load() != 2 {
		t.Fatalf("re-enable=%+v %v", status, err)
	}
}

func TestFacadeStopTimeoutNeverAbandonsWorkers(t *testing.T) {
	on := true
	f := newLifecycleFixture(t, &on)
	release := make(chan struct{})
	f.closeFn = func(context.Context) error { <-release; return nil }
	f.facade.Start(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := f.facade.Stop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop returned before network drained: %v", err)
	}
	if status := f.facade.Status(context.Background()); status.LastInitError != InitReasonStopIncomplete {
		t.Fatalf("incomplete stop not visible: %+v", status)
	}
	if _, err := f.facade.Execute(context.Background(), "job_any"); !errors.Is(err, ErrExtensionStopping) {
		t.Fatalf("work admitted during stop: %v", err)
	}
	if _, err := f.facade.Enable(context.Background()); !errors.Is(err, ErrExtensionStopping) {
		t.Fatalf("enable during shutdown: %v", err)
	}
	close(release)
	if err := f.facade.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.closes.Load() != 1 || f.facade.Status(context.Background()).LastInitError != "" {
		t.Fatal("retried stop did not finish the same module")
	}
}

func TestFacadeConcurrentExecuteAndStopRace(t *testing.T) {
	on := true
	f := newLifecycleFixture(t, &on)
	f.store.runnerStore = *newLifecycleRunnerStore()
	f.facade.Start(context.Background())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				_, err := f.facade.Execute(context.Background(), "job_runner")
				if err != nil && !errors.Is(err, ErrExtensionStopping) && !errors.Is(err, ErrExtensionDisabled) && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}
		}()
	}
	if err := f.facade.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.vault.stopped.Store(true)
	wg.Wait()
}

func newLifecycleRunnerStore() *runnerStore {
	account := connectedAccount()
	account.TimeZone = "UTC"
	now := time.Now().UTC()
	return &runnerStore{account: account, job: Job{ID: "job_runner", AccountID: account.ID, ExpectedRevision: account.Revision,
		Action: JobActionStatusRefresh, Trigger: JobTriggerManual, SiteDay: now.Format(time.DateOnly), Status: JobStatusAlreadyChecked,
		Dispatched: false, ProofSource: ProofSourceSiteStatus, RequestID: "request_runner", InputDigest: "synthetic_digest",
		CreatedAt: now, CompletedAt: &now}}
}
