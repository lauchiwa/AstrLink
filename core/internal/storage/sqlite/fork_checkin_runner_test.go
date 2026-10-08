package sqlite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin/adapters"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

type checkinRunnerVault struct {
	forkcheckin.Vault
	buffers [][]byte
}

func (v *checkinRunnerVault) Get(ctx context.Context, id forkcheckin.AccountID) ([]byte, error) {
	buffer, err := v.Vault.Get(ctx, id)
	v.buffers = append(v.buffers, buffer)
	return buffer, err
}

func (v *checkinRunnerVault) assertCleared(t *testing.T) {
	t.Helper()
	for _, buffer := range v.buffers {
		if !bytes.Equal(buffer, make([]byte, len(buffer))) {
			t.Fatal("runner retained a plaintext Vault buffer")
		}
	}
}

type checkinRunnerFixture struct {
	store   *Store
	path    string
	account forkcheckin.Account
	job     forkcheckin.Job
	vault   *checkinRunnerVault
}

func newCheckinRunnerFixture(t *testing.T, base string) *checkinRunnerFixture {
	t.Helper()
	f := &checkinRunnerFixture{path: filepath.Join(t.TempDir(), "runner.db")}
	f.store = openWithKey(t, f.path, testLocalKey(t, 0x97), nil)
	t.Cleanup(func() { _ = f.store.Close() })
	ctx := context.Background()
	if err := f.store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	account := forkCheckinDraft("acct_runner", base)
	account.State, account.RemoteUserID, account.Automatic, account.TimeZone = forkcheckin.AccountStateConnected, "7", true, "UTC"
	var err error
	f.account, err = f.store.CreateForkCheckinAccount(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := forkcheckin.EncodeNetworkCredential(forkcheckin.NetworkCredential{Version: 1, Bearer: forkCheckinSessionMarker})
	if err != nil {
		t.Fatal(err)
	}
	defer clear(credential)
	if err := f.store.ForkCheckinSessions().Put(ctx, account.ID, credential); err != nil {
		t.Fatal(err)
	}
	f.vault = &checkinRunnerVault{Vault: f.store.ForkCheckinSessions()}
	f.job = forkCheckinJobDraft(account.ID, "request_runner_integration", 'r')
	return f
}

func (f *checkinRunnerFixture) create(t *testing.T) {
	t.Helper()
	f.job = createForkCheckinJob(t, f.store, f.job).Job
}

func (f *checkinRunnerFixture) reopen(t *testing.T) {
	t.Helper()
	f.vault.assertCleared(t)
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store = openWithKey(t, f.path, testLocalKey(t, 0x97), nil)
	// Move the persisted lease clock past expiry without sleeping. Only the
	// Store clock changes; no production timeout or network policy is weakened.
	f.store.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	f.vault = &checkinRunnerVault{Vault: f.store.ForkCheckinSessions()}
}

func checkinExecute(t *testing.T, store forkcheckin.ExecutionStore, vault forkcheckin.Vault, adapter forkcheckin.SiteAdapter, id forkcheckin.JobID) (forkcheckin.JobReceipt, error) {
	t.Helper()
	runner, err := forkcheckin.NewRunner(store, vault, adapter)
	if err != nil {
		t.Fatal(err)
	}
	return runner.Execute(context.Background(), id)
}

func TestForkCheckinRunnerRealHTTPAndSealedVault(t *testing.T) {
	for _, mode := range []string{"success", "dropped_response", "refresh", "already_checked", "manual"} {
		t.Run(mode, func(t *testing.T) {
			var posts, requests atomic.Int32
			var f *checkinRunnerFixture
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Header.Get("Authorization") != "Bearer "+forkCheckinSessionMarker || r.Header.Get("New-Api-User") != "7" {
					t.Error("expected account-scoped identity and sealed session")
				}
				for name, values := range r.Header {
					if strings.HasPrefix(strings.ToLower(name), "x-astrlink-") || strings.Contains(strings.ToLower(strings.Join(values, " ")), "astrlink") {
						t.Error("gateway identity escaped to the site")
					}
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/api/status":
					fmt.Fprintf(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":%t}}`, mode == "manual")
				case r.URL.Path == "/api/user/self":
					_, _ = io.WriteString(w, `{"success":true,"data":{"id":7}}`)
				case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodGet:
					fmt.Fprintf(w, `{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":%t,"records":[]}}}`, posts.Load() > 0 || mode == "already_checked")
				case r.URL.Path == "/api/user/checkin" && r.Method == http.MethodPost:
					posts.Add(1)
					body, _ := io.ReadAll(r.Body)
					if string(body) != "{}" {
						t.Error("unexpected submission body")
					}
					// Reading the real DB here proves dispatch committed before I/O
					// and the network call does not hold that SQL transaction.
					job, err := f.store.GetForkCheckinJob(r.Context(), f.job.ID)
					if err != nil || !job.Dispatched || job.Status != forkcheckin.JobStatusRunning {
						t.Errorf("POST without durable intent: %+v %v", job, err)
					}
					if mode == "dropped_response" {
						connection, _, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						_ = connection.Close()
						return
					}
					fmt.Fprintf(w, `{"success":true,"data":{"checkin_date":%q,"quota_awarded":17}}`, f.job.SiteDay)
				default:
					t.Error("unexpected upstream operation")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			f = newCheckinRunnerFixture(t, server.URL)
			if mode == "refresh" {
				f.job.Action = forkcheckin.JobActionStatusRefresh
			}
			f.create(t)
			accepted, err := f.store.LookupForkCheckinReceipt(context.Background(), f.job.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			factory := forkcheckin.NewTransportFactory()
			defer func() { _ = factory.Close(context.Background()) }()
			read, err := adapters.NewNewAPIRead(factory, adapters.NewAPILegacy)
			if err != nil {
				t.Fatal(err)
			}
			adapter, err := adapters.NewNewAPISubmit(read)
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := checkinExecute(t, f.store, f.vault, adapter, f.job.ID)
			want, wantPosts := forkcheckin.JobStatusSuccess, int32(1)
			switch mode {
			case "dropped_response":
				// Standard New API has no authoritative current date in a GET;
				// checked_in_today alone cannot confirm the interrupted job day.
				want = forkcheckin.JobStatusUncertain
			case "refresh":
				want, wantPosts = forkcheckin.JobStatusNotChecked, 0
			case "already_checked":
				want, wantPosts = forkcheckin.JobStatusAlreadyChecked, 0
			case "manual":
				want, wantPosts = forkcheckin.JobStatusManualRequired, 0
			}
			if err != nil || receipt.Status != want || posts.Load() != wantPosts {
				t.Fatalf("result=%+v posts=%d err=%v", receipt, posts.Load(), err)
			}
			if mode == "success" && (receipt.Reward == nil || receipt.Reward.Quota != 17 || receipt.ProofSource != forkcheckin.ProofSourceSubmitResponse) {
				t.Fatal("authoritative award missing")
			}
			if mode != "success" && receipt.Reward != nil {
				t.Fatal("unproven award published")
			}
			f.vault.assertCleared(t)
			before := requests.Load()
			again, err := checkinExecute(t, f.store, f.vault, adapter, f.job.ID)
			if err != nil || !reflect.DeepEqual(again, receipt) || requests.Load() != before {
				t.Fatalf("terminal replay performed work or returned stale acceptance: %+v %v", again, err)
			}
			ledger, err := f.store.LookupForkCheckinReceipt(context.Background(), f.job.RequestID)
			if err != nil || !bytes.Equal(ledger.Body, accepted.Body) {
				t.Fatal("execution rewrote the immutable acceptance receipt")
			}
			body, _ := json.Marshal(receipt.Public())
			if bytes.Contains(body, []byte(forkCheckinSessionMarker)) || bytes.Contains(readDatabaseBytes(t, f.path), []byte(forkCheckinSessionMarker)) {
				t.Fatal("plaintext session in a receipt or the database")
			}
			if mode == "refresh" {
				next := forkCheckinJobDraft(f.account.ID, "request_after_refresh", 'n')
				created := createForkCheckinJob(t, f.store, next)
				if _, err := f.store.ClaimForkCheckinJobID(context.Background(), created.Job.ID, time.Minute); err != nil {
					t.Fatalf("not-checked refresh consumed the day: %v", err)
				}
			}
		})
	}
}

type checkinRunnerAdapter struct {
	forkcheckin.SiteAdapter
	fixture  *checkinRunnerFixture
	posts    atomic.Int32
	reads    atomic.Int32
	read     func() (forkcheckin.CheckInStatus, error)
	onSubmit func(context.Context) error
}

func (a *checkinRunnerAdapter) ValidateIdentity(context.Context, forkcheckin.AccountSnapshot) (forkcheckin.SiteIdentity, error) {
	return forkcheckin.SiteIdentity{RemoteUserID: "7"}, nil
}
func (a *checkinRunnerAdapter) Inspect(context.Context, forkcheckin.AccountSnapshot) (forkcheckin.SiteCapability, error) {
	return forkcheckin.SiteCapability{Supported: true}, nil
}
func (a *checkinRunnerAdapter) ReadStatus(context.Context, forkcheckin.AccountSnapshot) (forkcheckin.CheckInStatus, error) {
	a.reads.Add(1)
	if a.read != nil {
		return a.read()
	}
	return forkcheckin.CheckInStatus{CheckedInToday: a.posts.Load() > 0, SiteDate: a.fixture.job.SiteDay}, nil
}
func (a *checkinRunnerAdapter) Submit(ctx context.Context, _ forkcheckin.AccountSnapshot) (forkcheckin.SubmitOutcome, error) {
	a.posts.Add(1)
	if a.onSubmit != nil {
		if err := a.onSubmit(ctx); err != nil {
			return forkcheckin.SubmitOutcome{Dispatched: true}, err
		}
	}
	return forkcheckin.SubmitOutcome{Dispatched: true, ResponseRead: true, Succeeded: true, SiteDate: a.fixture.job.SiteDay, Reward: forkcheckin.Reward{Known: true, Quota: 31, Unit: "quota"}}, nil
}

type checkinRunnerFaultStore struct {
	forkcheckin.ExecutionStore
	mark     func(context.Context, forkcheckin.ExecutionLease) error
	complete func(context.Context, forkcheckin.ExecutionLease, forkcheckin.ExecutionCompletion) (forkcheckin.JobReceipt, error)
}

func (s checkinRunnerFaultStore) MarkForkCheckinDispatched(ctx context.Context, lease forkcheckin.ExecutionLease) error {
	if s.mark != nil {
		return s.mark(ctx, lease)
	}
	return s.ExecutionStore.MarkForkCheckinDispatched(ctx, lease)
}
func (s checkinRunnerFaultStore) CompleteForkCheckinJob(ctx context.Context, lease forkcheckin.ExecutionLease, completion forkcheckin.ExecutionCompletion) (forkcheckin.JobReceipt, error) {
	if s.complete != nil {
		return s.complete(ctx, lease, completion)
	}
	return s.ExecutionStore.CompleteForkCheckinJob(ctx, lease, completion)
}

func TestForkCheckinRunnerReopensInterruptedExecutionWithoutResubmitting(t *testing.T) {
	for _, phase := range []string{"before_intent", "after_intent", "after_post", "after_completion"} {
		t.Run(phase, func(t *testing.T) {
			f := newCheckinRunnerFixture(t, "https://relay.example")
			f.create(t)
			adapter := &checkinRunnerAdapter{fixture: f}
			interrupted := errors.New("simulated process loss")
			fault := checkinRunnerFaultStore{ExecutionStore: f.store}
			if phase == "before_intent" || phase == "after_intent" {
				fault.mark = func(ctx context.Context, lease forkcheckin.ExecutionLease) error {
					if phase == "after_intent" {
						if err := f.store.MarkForkCheckinDispatched(ctx, lease); err != nil {
							return err
						}
					}
					return interrupted
				}
			} else {
				fault.complete = func(ctx context.Context, lease forkcheckin.ExecutionLease, completion forkcheckin.ExecutionCompletion) (forkcheckin.JobReceipt, error) {
					if phase == "after_completion" {
						if _, err := f.store.CompleteForkCheckinJob(ctx, lease, completion); err != nil {
							return forkcheckin.JobReceipt{}, err
						}
					}
					return forkcheckin.JobReceipt{}, interrupted
				}
			}
			if _, err := checkinExecute(t, fault, f.vault, adapter, f.job.ID); !errors.Is(err, interrupted) {
				t.Fatalf("did not reach fault boundary: %v", err)
			}
			f.reopen(t)
			receipt, err := checkinExecute(t, f.store, f.vault, adapter, f.job.ID)
			want, posts := forkcheckin.JobStatusAlreadyChecked, int32(1)
			switch phase {
			case "before_intent", "after_completion":
				want = forkcheckin.JobStatusSuccess
			case "after_intent":
				want, posts = forkcheckin.JobStatusUncertain, 0
			}
			if err != nil || receipt.Status != want || adapter.posts.Load() != posts || !receipt.Dispatched {
				t.Fatalf("reopen result=%+v posts=%d err=%v", receipt, adapter.posts.Load(), err)
			}
			if want != forkcheckin.JobStatusSuccess && receipt.Reward != nil {
				t.Fatal("recovery copied an unproven reward")
			}
			f.vault.assertCleared(t)
			// A different request ID must not re-license the same day's write.
			next := createForkCheckinJob(t, f.store, forkCheckinJobDraft(f.account.ID, "request_runner_second", 's')).Job
			if _, err := checkinExecute(t, f.store, f.vault, adapter, next.ID); err == nil {
				t.Fatal("second job bypassed daily dispatch deduplication")
			}
			if adapter.posts.Load() != posts {
				t.Fatal("interrupted submission was repeated")
			}
		})
	}
}

func TestForkCheckinRunnerManualAndAutomaticShareAccountLease(t *testing.T) {
	f := newCheckinRunnerFixture(t, "https://relay.example")
	f.create(t)
	second := openWithKey(t, f.path, testLocalKey(t, 0x97), nil)
	defer second.Close()
	automatic := forkCheckinJobDraft(f.account.ID, "request_runner_automatic", 'a')
	automatic.Trigger = forkcheckin.JobTriggerAutomatic
	automatic = createForkCheckinJob(t, second, automatic).Job
	started, release := make(chan struct{}), make(chan struct{})
	adapter := &checkinRunnerAdapter{fixture: f, onSubmit: func(ctx context.Context) error {
		close(started)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	runner, err := forkcheckin.NewRunner(f.store, f.vault, adapter)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := runner.Execute(context.Background(), f.job.ID); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		<-done
		t.Fatal("manual execution did not reach Submit")
	}
	_, claimErr := checkinExecute(t, second, second.ForkCheckinSessions(), adapter, automatic.ID)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !errors.Is(claimErr, ErrForkCheckinLeaseHeld) || adapter.posts.Load() != 1 {
		t.Fatalf("parallel automatic execution bypassed manual lease: posts=%d err=%v", adapter.posts.Load(), claimErr)
	}
	f.vault.assertCleared(t)
}

func TestForkCheckinRunnerRevisionChangeSettlesWithoutObsoleteProof(t *testing.T) {
	for _, phase := range []string{"queued", "reading", "submitting", "recovery"} {
		t.Run(phase, func(t *testing.T) {
			f := newCheckinRunnerFixture(t, "https://relay.example")
			f.create(t)
			adapter := &checkinRunnerAdapter{fixture: f}
			update := func() {
				next := f.account
				next.Automatic = false
				var err error
				f.account, err = f.store.UpdateForkCheckinAccount(context.Background(), next, next.Revision)
				if err != nil {
					t.Fatal(err)
				}
			}
			switch phase {
			case "queued":
				update()
			case "reading":
				adapter.read = func() (forkcheckin.CheckInStatus, error) {
					update()
					return forkcheckin.CheckInStatus{SiteDate: f.job.SiteDay}, nil
				}
			case "submitting":
				adapter.onSubmit = func(context.Context) error { update(); return nil }
			case "recovery":
				lease, err := f.store.ClaimForkCheckinJobID(context.Background(), f.job.ID, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.store.MarkForkCheckinDispatched(context.Background(), lease); err != nil {
					t.Fatal(err)
				}
				update()
				f.reopen(t)
			}
			receipt, err := checkinExecute(t, f.store, f.vault, adapter, f.job.ID)
			want := forkcheckin.JobStatusCancelled
			if phase == "submitting" || phase == "recovery" {
				want = forkcheckin.JobStatusUncertain
			}
			if err != nil || receipt.Status != want || receipt.FailureCode != "account_changed" || receipt.Reward != nil {
				t.Fatalf("obsolete work did not settle: %+v %v", receipt, err)
			}
			posts := int32(0)
			if phase == "submitting" {
				posts = 1
			}
			if adapter.posts.Load() != posts {
				t.Fatal("obsolete work submitted again")
			}
			if (phase == "queued" || phase == "recovery") && (len(f.vault.buffers) != 0 || adapter.reads.Load() != 0) {
				t.Fatal("obsolete work opened the Vault or contacted the site")
			}
			var live int
			if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM fork_checkin_job_attempts WHERE job_id = ? AND ended_at = ''`, f.job.ID).Scan(&live); err != nil || live != 0 {
				t.Fatalf("obsolete work retained a live lease: %d %v", live, err)
			}
			f.vault.assertCleared(t)
		})
	}
}

func TestForkCheckinRunnerShutdownCancelsInFlightPOST(t *testing.T) {
	started := make(chan struct{})
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/status":
			_, _ = io.WriteString(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":false}}`)
		case r.URL.Path == "/api/user/self":
			_, _ = io.WriteString(w, `{"success":true,"data":{"id":7}}`)
		case r.Method == http.MethodPost:
			posts.Add(1)
			_, _ = io.Copy(io.Discard, r.Body)
			close(started)
			<-r.Context().Done()
		default:
			_, _ = io.WriteString(w, `{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":false,"records":[]}}}`)
		}
	}))
	defer server.Close()
	f := newCheckinRunnerFixture(t, server.URL)
	f.create(t)
	factory := forkcheckin.NewTransportFactory()
	defer func() { _ = factory.Close(context.Background()) }()
	read, err := adapters.NewNewAPIRead(factory, adapters.NewAPILegacy)
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := adapters.NewNewAPISubmit(read)
	if err != nil {
		t.Fatal(err)
	}
	runner, err := forkcheckin.NewRunner(f.store, f.vault, adapter)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		receipt forkcheckin.JobReceipt
		err     error
	}
	done := make(chan result, 1)
	go func() { receipt, err := runner.Execute(ctx, f.job.ID); done <- result{receipt, err} }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		cancel()
		<-done
		t.Fatal("runner did not reach POST")
	}
	// Match owner shutdown ordering: cancel work, stop clients, wait for the
	// runner to persist its final state, only then destroy the Store/key.
	cancel()
	stop, release := context.WithTimeout(context.Background(), 3*time.Second)
	defer release()
	if err := factory.Close(stop); err != nil {
		t.Fatal(err)
	}
	select {
	case out := <-done:
		if out.err != nil || out.receipt.Status != forkcheckin.JobStatusUncertain || !out.receipt.Dispatched || out.receipt.Reward != nil || posts.Load() != 1 {
			t.Fatalf("shutdown lost dispatch or reported cancellation: %+v %v", out.receipt, out.err)
		}
	case <-stop.Done():
		t.Fatal("shutdown did not finish within its bound")
	}
	f.vault.assertCleared(t)
}

func TestForkCheckinRunnerExpiredWorkerCannotSendAfterReplacement(t *testing.T) {
	f := newCheckinRunnerFixture(t, "https://relay.example")
	f.create(t)
	started, release := make(chan struct{}), make(chan struct{})
	old := &checkinRunnerAdapter{fixture: f, read: func() (forkcheckin.CheckInStatus, error) {
		close(started)
		<-release
		return forkcheckin.CheckInStatus{SiteDate: f.job.SiteDay}, nil
	}}
	runner, err := forkcheckin.NewRunner(f.store, f.vault, old)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := runner.Execute(context.Background(), f.job.ID); done <- err }()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		close(release)
		<-done
		t.Fatal("old worker did not reach status read")
	}
	second := openWithKey(t, f.path, testLocalKey(t, 0x97), nil)
	defer second.Close()
	second.now = func() time.Time { return time.Now().Add(3 * time.Minute) }
	replacement := &checkinRunnerAdapter{fixture: f}
	receipt, replaceErr := checkinExecute(t, second, second.ForkCheckinSessions(), replacement, f.job.ID)
	close(release)
	oldErr := <-done
	if replaceErr != nil || receipt.Status != forkcheckin.JobStatusSuccess || receipt.Attempts != 2 || replacement.posts.Load() != 1 {
		t.Fatalf("replacement result=%+v err=%v", receipt, replaceErr)
	}
	if oldErr == nil || old.posts.Load() != 0 {
		t.Fatalf("expired worker submitted after replacement: %v", oldErr)
	}
	f.vault.assertCleared(t)
}

func TestForkCheckinObsoleteLeaseCannotPublishSuccess(t *testing.T) {
	f := newCheckinRunnerFixture(t, "https://relay.example")
	f.create(t)
	ctx := context.Background()
	lease, err := f.store.ClaimForkCheckinJobID(ctx, f.job.ID, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	f.account.Automatic = false
	if _, err := f.store.UpdateForkCheckinAccount(ctx, f.account, f.account.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceSubmitResponse}); !errors.Is(err, forkcheckin.ErrRevisionChanged) {
		t.Fatalf("obsolete proof bypassed revision fence: %v", err)
	}
	lease.Token = "wrong_lease"
	if _, err := f.store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{Status: forkcheckin.JobStatusUncertain, ProofSource: forkcheckin.ProofSourceTransportError, ReadOnly: true, FailureCode: "account_changed"}); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("obsolete cleanup bypassed lease fence: %v", err)
	}
}
