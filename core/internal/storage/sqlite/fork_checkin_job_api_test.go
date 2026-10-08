package sqlite

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

// jobAPIAdapter stands in for a site. It counts submissions and can hold the
// read or the submission open, so a test can cancel at either side of the
// durable dispatch mark.
type jobAPIAdapter struct {
	forkcheckin.SiteAdapter
	posts      atomic.Int32
	checked    sync.Map // AccountID -> struct{}: the site's own record
	readGate   chan struct{}
	submitGate chan struct{}
	reading    chan struct{}
	submitting chan struct{}
}

func newJobAPIAdapter() *jobAPIAdapter {
	return &jobAPIAdapter{reading: make(chan struct{}, 8), submitting: make(chan struct{}, 8)}
}

func jobAPIToday() string {
	location, _ := time.LoadLocation("Asia/Shanghai")
	return time.Now().In(location).Format(time.DateOnly)
}

func (a *jobAPIAdapter) Dialect() string { return "test" }

func (a *jobAPIAdapter) ValidateIdentity(context.Context, forkcheckin.AccountSnapshot) (forkcheckin.SiteIdentity, error) {
	return forkcheckin.SiteIdentity{RemoteUserID: "7"}, nil
}

func (a *jobAPIAdapter) Inspect(context.Context, forkcheckin.AccountSnapshot) (forkcheckin.SiteCapability, error) {
	return forkcheckin.SiteCapability{Supported: true}, nil
}

func (a *jobAPIAdapter) ReadStatus(ctx context.Context, snapshot forkcheckin.AccountSnapshot) (forkcheckin.CheckInStatus, error) {
	a.reading <- struct{}{}
	if a.readGate != nil {
		select {
		case <-a.readGate:
		case <-ctx.Done():
			return forkcheckin.CheckInStatus{}, ctx.Err()
		}
	}
	_, checked := a.checked.Load(snapshot.Account.ID)
	return forkcheckin.CheckInStatus{CheckedInToday: checked}, nil
}

func (a *jobAPIAdapter) Submit(ctx context.Context, snapshot forkcheckin.AccountSnapshot) (forkcheckin.SubmitOutcome, error) {
	a.posts.Add(1)
	a.checked.Store(snapshot.Account.ID, struct{}{})
	a.submitting <- struct{}{}
	if a.submitGate != nil {
		select {
		case <-a.submitGate:
		case <-ctx.Done():
			return forkcheckin.SubmitOutcome{Dispatched: true}, ctx.Err()
		}
	}
	return forkcheckin.SubmitOutcome{Dispatched: true, ResponseRead: true, Succeeded: true, SiteDate: jobAPIToday()}, nil
}

type jobAPIFixture struct {
	store   *Store
	facade  *forkcheckin.Facade
	handler http.Handler
	adapter *jobAPIAdapter
}

// newJobAPIFixture enables the module over real storage with connected
// accounts acct_a and acct_c and an unconnected draft acct_b.
func newJobAPIFixture(t *testing.T, adapter *jobAPIAdapter) *jobAPIFixture {
	t.Helper()
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "jobs.db"))
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	for _, id := range []forkcheckin.AccountID{"acct_a", "acct_b", "acct_c"} {
		if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft(id, "https://"+string(id)+".example/")); err != nil {
			t.Fatal(err)
		}
		if id == "acct_b" {
			continue
		}
		if _, err := store.ConnectForkCheckinAccount(ctx, id, "7", 1); err != nil {
			t.Fatal(err)
		}
		if err := store.ForkCheckinSessions().Put(ctx, id, []byte("session-"+string(id))); err != nil {
			t.Fatal(err)
		}
	}
	settings := filepath.Join(t.TempDir(), "checkin.json")
	if err := forkcheckin.SaveSettings(settings, forkcheckin.Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	clock := newParkedClock()
	facade, err := forkcheckin.NewFacade(forkcheckin.FacadeConfig{
		SettingsPath: settings,
		Factory: store.ForkCheckinModuleFactory(func(*forkcheckin.TransportFactory) (forkcheckin.SiteAdapter, error) {
			return adapter, nil
		}),
		Clock: clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	facade.Start(ctx)
	<-clock.idle
	t.Cleanup(func() {
		stop, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := facade.Stop(stop); err != nil {
			t.Errorf("stop: %v", err)
		}
		_ = store.Close()
	})
	return &jobAPIFixture{store: store, facade: facade, handler: forkcheckin.NewAPIHandler(facade), adapter: adapter}
}

func (f *jobAPIFixture) post(t *testing.T, path, body string) (int, map[string]any, string) {
	t.Helper()
	code, decoded := sendCheckinAPI(t, f.handler, http.MethodPost, path, body)
	raw, _ := json.Marshal(decoded)
	return code, decoded, string(raw)
}

// waitJob polls local state until the job reaches want.
func (f *jobAPIFixture) waitJob(t *testing.T, id string, want forkcheckin.JobStatus) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		code, job := sendCheckinAPI(t, f.handler, http.MethodGet, "/jobs/"+id, "")
		if code == http.StatusOK && job["status"] == string(want) {
			return job
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s=%d %v, want %s", id, code, job, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func errorCode(body map[string]any) any {
	if envelope, ok := body["error"].(map[string]any); ok {
		return envelope["code"]
	}
	return nil
}

func TestForkCheckinJobAPIRetriedCreateQueuesOnceAndPostsOnce(t *testing.T) {
	f := newJobAPIFixture(t, newJobAPIAdapter())
	body := `{"request_id":"request_single_1","action":"check_in","accounts":["acct_a"]}`
	code, first, firstRaw := f.post(t, "/jobs", body)
	if code != http.StatusAccepted || first["status"] != "queued" || first["dispatched"] != false {
		t.Fatalf("create=%d %v", code, first)
	}
	// The desktop retries a write whose response it lost with the same id.
	for range 3 {
		if code, _, raw := f.post(t, "/jobs", body); code != http.StatusAccepted || raw != firstRaw {
			t.Fatalf("retry=%d %s, want the original %s", code, raw, firstRaw)
		}
	}
	id := first["id"].(string)
	done := f.waitJob(t, id, forkcheckin.JobStatusSuccess)
	if done["dispatched"] != true || done["proof_source"] != "submission_response" {
		t.Fatalf("done=%v", done)
	}
	if code, _, raw := f.post(t, "/jobs", body); code != http.StatusAccepted || raw != firstRaw {
		t.Fatalf("retry after finishing=%d %s", code, raw)
	}
	time.Sleep(50 * time.Millisecond)
	if posts := f.adapter.posts.Load(); posts != 1 {
		t.Fatalf("one logical request posted %d times", posts)
	}
	if count := countRows(t, f.store, forkCheckinJobs); count != 1 {
		t.Fatalf("one logical request created %d jobs", count)
	}
	// Same id, different content: refused, and nothing new is queued.
	for _, other := range []string{
		`{"request_id":"request_single_1","action":"status_refresh","accounts":["acct_a"]}`,
		`{"request_id":"request_single_1","action":"check_in","accounts":["acct_c"]}`,
		`{"request_id":"request_single_1","action":"check_in","accounts":["acct_a","acct_c"]}`,
	} {
		if code, body, _ := f.post(t, "/jobs", other); code != http.StatusConflict || errorCode(body) != "request_id_reused" {
			t.Fatalf("reused id=%d %v", code, body)
		}
	}
	if count := countRows(t, f.store, forkCheckinJobs); count != 1 {
		t.Fatalf("a refused request created jobs: %d", count)
	}
	// A pinned revision that is stale is refused before anything is queued.
	if code, body, _ := f.post(t, "/jobs", `{"request_id":"request_single_2","action":"check_in","accounts":["acct_c"],"expected_revision":1}`); code != http.StatusPreconditionFailed || errorCode(body) != "revision_conflict" {
		t.Fatalf("stale revision=%d %v", code, body)
	}
	if code, body, _ := f.post(t, "/jobs", `{"request_id":"request_single_3","action":"check_in","accounts":["acct_missing"]}`); code != http.StatusNotFound {
		t.Fatalf("missing account=%d %v", code, body)
	}
}

func TestForkCheckinJobAPIBatchChildrenAreIndependent(t *testing.T) {
	f := newJobAPIFixture(t, newJobAPIAdapter())
	if code, body, _ := f.post(t, "/jobs", `{"request_id":"request_batch_0","action":"check_in","accounts":["acct_a","acct_missing"]}`); code != http.StatusNotFound {
		t.Fatalf("batch with a missing account=%d %v", code, body)
	}
	if count := countRows(t, f.store, forkCheckinJobs); count != 0 {
		t.Fatalf("a refused batch left %d jobs", count)
	}
	body := `{"request_id":"request_batch_1","action":"check_in","accounts":["acct_c","acct_b","acct_a"]}`
	code, parent, raw := f.post(t, "/jobs", body)
	if code != http.StatusAccepted || !strings.HasPrefix(parent["id"].(string), forkcheckin.BatchIDPrefix) || len(parent["children"].([]any)) != 3 || parent["account_id"] != nil {
		t.Fatalf("batch=%d %v", code, parent)
	}
	if code, _, again := f.post(t, "/jobs", body); code != http.StatusAccepted || again != raw {
		t.Fatalf("batch retry=%d %s", code, again)
	}
	id := parent["id"].(string)
	// The draft account fails on its own; the others still check in.
	settled := f.waitJob(t, id, forkcheckin.JobStatusAuthRequired)
	statuses := map[string]any{}
	for _, child := range settled["children"].([]any) {
		job := child.(map[string]any)
		statuses[job["account_id"].(string)] = job["status"]
	}
	if statuses["acct_a"] != "success" || statuses["acct_c"] != "success" || statuses["acct_b"] != "auth_required" || settled["finished_at"] == nil {
		t.Fatalf("children=%v", statuses)
	}
	if posts := f.adapter.posts.Load(); posts != 2 {
		t.Fatalf("batch posted %d times, want 2", posts)
	}
	if code, body, _ := f.post(t, "/jobs/"+id+"/cancel", `{"request_id":"request_cancel_b"}`); code != http.StatusConflict || errorCode(body) != "job_not_cancellable" {
		t.Fatalf("cancel finished batch=%d %v", code, body)
	}
}

func TestForkCheckinJobAPICancelBeforeDispatchNeverPosts(t *testing.T) {
	adapter := newJobAPIAdapter()
	adapter.readGate = make(chan struct{})
	f := newJobAPIFixture(t, adapter)
	_, job, _ := f.post(t, "/jobs", `{"request_id":"request_cancel_1","action":"check_in","accounts":["acct_a"]}`)
	id := job["id"].(string)
	<-adapter.reading
	f.waitJob(t, id, forkcheckin.JobStatusRunning)
	cancel := `{"request_id":"request_cancel_2"}`
	code, cancelled, raw := f.post(t, "/jobs/"+id+"/cancel", cancel)
	if code != http.StatusOK || cancelled["status"] != "cancelled" || cancelled["dispatched"] != false || cancelled["finished_at"] == nil {
		t.Fatalf("cancel=%d %v", code, cancelled)
	}
	if code, _, again := f.post(t, "/jobs/"+id+"/cancel", cancel); code != http.StatusOK || again != raw {
		t.Fatalf("cancel retry=%d %s", code, again)
	}
	if code, body, _ := f.post(t, "/jobs/job_other_one/cancel", cancel); code != http.StatusConflict || errorCode(body) != "request_id_reused" {
		t.Fatalf("cancel id reused for another job=%d %v", code, body)
	}
	// Even a worker that ignored the interrupt is fenced by storage.
	close(adapter.readGate)
	time.Sleep(100 * time.Millisecond)
	final := f.waitJob(t, id, forkcheckin.JobStatusCancelled)
	if final["dispatched"] != false || adapter.posts.Load() != 0 {
		t.Fatalf("cancelled job posted: %v posts=%d", final, adapter.posts.Load())
	}
	if code, body, _ := f.post(t, "/jobs/"+id+"/cancel", `{"request_id":"request_cancel_3"}`); code != http.StatusConflict || errorCode(body) != "job_not_cancellable" {
		t.Fatalf("cancel twice=%d %v", code, body)
	}
}

func TestForkCheckinJobAPICancelAfterDispatchKeepsTheSubmission(t *testing.T) {
	adapter := newJobAPIAdapter()
	adapter.submitGate = make(chan struct{})
	f := newJobAPIFixture(t, adapter)
	_, job, _ := f.post(t, "/jobs", `{"request_id":"request_sent_01","action":"check_in","accounts":["acct_a"]}`)
	id := job["id"].(string)
	<-adapter.submitting
	code, kept, _ := f.post(t, "/jobs/"+id+"/cancel", `{"request_id":"request_sent_02"}`)
	if code != http.StatusOK || kept["status"] != "running" || kept["dispatched"] != true {
		t.Fatalf("cancel after dispatch=%d %v", code, kept)
	}
	// The submission was not interrupted: its answer still settles the job.
	close(adapter.submitGate)
	done := f.waitJob(t, id, forkcheckin.JobStatusSuccess)
	if done["dispatched"] != true || adapter.posts.Load() != 1 {
		t.Fatalf("done=%v posts=%d", done, adapter.posts.Load())
	}
}

func TestForkCheckinJobAPICancelsQueuedWorkTheSchedulerWouldResume(t *testing.T) {
	f := newJobAPIFixture(t, newJobAPIAdapter())
	ctx := context.Background()
	// A job created outside the API is queued and not started in process.
	job := forkCheckinJobDraft("acct_c", "request_queued_1", 'q')
	job.ExpectedRevision = 2
	job.SiteDay = jobAPIToday()
	created := createForkCheckinJob(t, f.store, job)
	code, cancelled, _ := f.post(t, "/jobs/"+string(created.Job.ID)+"/cancel", `{"request_id":"request_queued_2"}`)
	if code != http.StatusOK || cancelled["status"] != "cancelled" {
		t.Fatalf("cancel queued=%d %v", code, cancelled)
	}
	if _, err := f.store.ClaimForkCheckinJobID(ctx, created.Job.ID, time.Minute); err == nil {
		t.Fatal("a cancelled job could still be claimed")
	}
	next, err := f.store.PrepareForkCheckinScheduledJob(ctx, "acct_c", time.Now())
	if err != nil || next == created.Job.ID {
		t.Fatalf("scheduler resumed a cancelled job: %q %v", next, err)
	}
	if f.adapter.posts.Load() != 0 {
		t.Fatal("cancelled work reached the site")
	}
	if code, body, _ := f.post(t, "/jobs/job_absent_one/cancel", `{"request_id":"request_queued_3"}`); code != http.StatusNotFound {
		t.Fatalf("cancel missing=%d %v", code, body)
	}
	if code, _ := sendCheckinAPI(t, f.handler, http.MethodGet, "/jobs/batch_absent", ""); code != http.StatusNotFound {
		t.Fatalf("missing batch=%d", code)
	}
}
