package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin/adapters"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

func newAPIModuleBuilder(factory *forkcheckin.TransportFactory) (forkcheckin.SiteAdapter, error) {
	read, err := adapters.NewNewAPIRead(factory, adapters.NewAPILegacy)
	if err != nil {
		return nil, err
	}
	return adapters.NewNewAPISubmit(read)
}

// parkedClock lets the scheduler tick once at start and then waits until the
// module stops. idle closes when that first tick has finished, so a test that
// waits for it sees no automatic job and no site traffic.
type parkedClock struct {
	once sync.Once
	idle chan struct{}
}

func newParkedClock() *parkedClock { return &parkedClock{idle: make(chan struct{})} }

func (*parkedClock) Now() time.Time { return time.Now() }

func (clock *parkedClock) Wait(ctx context.Context, _ time.Duration) error {
	clock.once.Do(func() { close(clock.idle) })
	<-ctx.Done()
	return ctx.Err()
}

func countForkCheckinObjects(t *testing.T, store *Store) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name LIKE 'fork_checkin%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestForkCheckinModuleDisabledLeavesNoExtensionState(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "disabled.db"))
	defer store.Close()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer server.Close()
	facade, err := forkcheckin.NewFacade(forkcheckin.FacadeConfig{
		SettingsPath: filepath.Join(t.TempDir(), "checkin.json"),
		Factory:      store.ForkCheckinModuleFactory(newAPIModuleBuilder),
		Clock:        newParkedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	facade.Start(context.Background())
	status := facade.Status(context.Background())
	if status.Enabled || status.Initialized || countForkCheckinObjects(t, store) != 0 || requests.Load() != 0 {
		t.Fatalf("disabled extension created state: %+v objects=%d", status, countForkCheckinObjects(t, store))
	}
	if err := facade.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The shared Store is untouched and still usable for the main schema.
	if _, err := store.ListServices(context.Background(), storagecontract.ServiceListOptions{}); err != nil {
		t.Fatalf("main store unusable after extension stop: %v", err)
	}
}

func TestForkCheckinModuleEnableCountsAndDisableKeepsStoreOpen(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "enabled.db"))
	defer store.Close()
	facade, err := forkcheckin.NewFacade(forkcheckin.FacadeConfig{
		SettingsPath: filepath.Join(t.TempDir(), "checkin.json"),
		Factory:      store.ForkCheckinModuleFactory(newAPIModuleBuilder),
		Clock:        newParkedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err := facade.Enable(context.Background())
	if err != nil || !status.Initialized || status.AccountCount != 0 || countForkCheckinObjects(t, store) == 0 {
		t.Fatalf("enable=%+v err=%v", status, err)
	}
	if _, err := store.CreateForkCheckinAccount(context.Background(), forkCheckinDraft("acct_module", "https://relay.example")); err != nil {
		t.Fatal(err)
	}
	if facade.Status(context.Background()).AccountCount != 1 {
		t.Fatal("account count not reported while initialized")
	}
	if status, err = facade.Disable(context.Background()); err != nil || status.Initialized || status.AccountCount != 0 {
		t.Fatalf("disable=%+v %v", status, err)
	}
	// Disabling keeps data and the shared store; it does not drop tables.
	if _, err := store.GetForkCheckinAccount(context.Background(), "acct_module"); err != nil {
		t.Fatalf("disable lost extension data or closed the store: %v", err)
	}
}

// The 10s stop target is measured with the real adapter and transport against
// a site that never answers, both before and after durable dispatch intent.
func TestForkCheckinModuleStopsWithinTargetDuringHungIO(t *testing.T) {
	for _, phase := range []string{"read", "submit"} {
		t.Run(phase, func(t *testing.T) {
			hung, released := make(chan struct{}), make(chan struct{})
			var once atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				hang := phase == "read" && r.URL.Path == "/api/user/self" || phase == "submit" && r.Method == http.MethodPost
				if hang {
					if once.CompareAndSwap(false, true) {
						close(hung)
					}
					// Drain the body so the server watches for client disconnect.
					_, _ = io.Copy(io.Discard, r.Body)
					select {
					case <-r.Context().Done():
					case <-released:
					}
					return
				}
				switch {
				case r.URL.Path == "/api/status":
					_, _ = io.WriteString(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":false}}`)
				case r.URL.Path == "/api/user/self":
					_, _ = io.WriteString(w, `{"success":true,"data":{"id":7}}`)
				default:
					_, _ = io.WriteString(w, `{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":false,"records":[]}}}`)
				}
			}))
			defer server.Close()
			defer close(released)
			f := newCheckinRunnerFixture(t, server.URL)
			f.create(t)
			facade, err := forkcheckin.NewFacade(forkcheckin.FacadeConfig{
				SettingsPath: filepath.Join(t.TempDir(), "checkin.json"),
				Factory:      f.store.ForkCheckinModuleFactory(newAPIModuleBuilder),
			})
			if err != nil {
				t.Fatal(err)
			}
			if status, err := facade.Enable(context.Background()); err != nil || !status.Initialized {
				t.Fatalf("enable=%+v %v", status, err)
			}
			executed := make(chan error, 1)
			go func() { _, err := facade.Execute(context.Background(), f.job.ID); executed <- err }()
			select {
			case <-hung:
			case <-time.After(10 * time.Second):
				t.Fatal("request never reached the hung endpoint")
			}
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := facade.Stop(ctx); err != nil {
				t.Fatalf("stop missed the 10s target: %v", err)
			}
			elapsed := time.Since(started)
			t.Logf("stop with hung %s I/O took %v", phase, elapsed)
			if elapsed > 10*time.Second {
				t.Fatalf("stop took %v", elapsed)
			}
			select {
			case <-executed:
			case <-time.After(time.Second):
				t.Fatal("Execute outlived Stop")
			}
			f.vault.assertCleared(t)
			job, err := f.store.GetForkCheckinJob(context.Background(), f.job.ID)
			if err != nil {
				t.Fatal(err)
			}
			// Before dispatch, shutdown is a safe local failure. After durable
			// intent it cannot become retryable: it is uncertain, never resent.
			safe := job.Status == forkcheckin.JobStatusRetryableFailure || job.Status == forkcheckin.JobStatusCancelled
			if phase == "submit" {
				safe = job.Status == forkcheckin.JobStatusUncertain
			}
			if !safe || job.Dispatched != (phase == "submit") {
				t.Fatalf("job after stop=%+v", job)
			}
			if _, err := facade.Execute(context.Background(), f.job.ID); !errors.Is(err, forkcheckin.ErrExtensionStopping) {
				t.Fatalf("execute after stop=%v", err)
			}
		})
	}
}

func getCheckinAPI(t *testing.T, handler http.Handler, path string) (int, map[string]any) {
	t.Helper()
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, forkcheckin.APIPrefix+path, nil))
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("%s: %v %s", path, err, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "request_api_") || strings.Contains(recorder.Body.String(), "input_digest") {
		t.Fatalf("%s leaked private job fields: %s", path, recorder.Body)
	}
	return recorder.Code, body
}

// The read API over real storage: disabled creates nothing, enabled pages
// public views of what is stored, and no read opens the vault or the network.
func TestForkCheckinReadAPIOverStore(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "api.db"))
	defer store.Close()
	facade, err := forkcheckin.NewFacade(forkcheckin.FacadeConfig{
		SettingsPath: filepath.Join(t.TempDir(), "checkin.json"),
		Factory:      store.ForkCheckinModuleFactory(newAPIModuleBuilder),
		Clock:        newParkedClock(),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer facade.Stop(context.Background())
	handler := forkcheckin.NewAPIHandler(facade)
	if code, body := getCheckinAPI(t, handler, "/status"); code != http.StatusOK || body["enabled"] != false || body["storage_ready"] != false {
		t.Fatalf("disabled status=%d %v", code, body)
	}
	if code, body := getCheckinAPI(t, handler, "/accounts"); code != http.StatusConflict || body["error"].(map[string]any)["code"] != "checkin_disabled" {
		t.Fatalf("disabled accounts=%d %v", code, body)
	}
	if countForkCheckinObjects(t, store) != 0 {
		t.Fatal("disabled reads created extension tables")
	}
	if _, err := facade.Enable(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, id := range []forkcheckin.AccountID{"acct_api_a", "acct_api_b"} {
		if _, err := store.CreateForkCheckinAccount(context.Background(), forkCheckinDraft(id, "https://"+strings.ReplaceAll(string(id), "_", "-")+".example")); err != nil {
			t.Fatal(err)
		}
	}
	createForkCheckinJob(t, store, forkCheckinJobDraft("acct_api_a", "request_api_a1", 'a'))
	createForkCheckinJob(t, store, forkCheckinJobDraft("acct_api_b", "request_api_b1", 'b'))
	code, page := getCheckinAPI(t, handler, "/accounts?limit=1")
	cursor, _ := page["next_cursor"].(string)
	if code != http.StatusOK || len(page["items"].([]any)) != 1 || cursor == "" {
		t.Fatalf("first account page=%d %v", code, page)
	}
	if _, next := getCheckinAPI(t, handler, "/accounts?limit=1&cursor="+cursor); len(next["items"].([]any)) != 1 || next["next_cursor"] != nil {
		t.Fatalf("second account page=%v", next)
	}
	if code, account := getCheckinAPI(t, handler, "/accounts/acct_api_a"); code != http.StatusOK || account["state"] != "draft" || account["config_fingerprint"] == "" {
		t.Fatalf("account=%d %v", code, account)
	}
	code, jobs := getCheckinAPI(t, handler, "/jobs?account_id=acct_api_b")
	items := jobs["items"].([]any)
	if code != http.StatusOK || len(items) != 1 || items[0].(map[string]any)["account_id"] != "acct_api_b" {
		t.Fatalf("filtered jobs=%d %v", code, jobs)
	}
	id := items[0].(map[string]any)["id"].(string)
	if code, job := getCheckinAPI(t, handler, "/jobs/"+id); code != http.StatusOK || job["status"] != "queued" || job["dispatched"] != false {
		t.Fatalf("job=%d %v", code, job)
	}
	if code, _ := getCheckinAPI(t, handler, "/jobs?cursor=%21%21"); code != http.StatusBadRequest {
		t.Fatalf("forged cursor=%d", code)
	}
	if code, _ := getCheckinAPI(t, handler, "/jobs/job_missing"); code != http.StatusNotFound {
		t.Fatalf("missing job=%d", code)
	}
}

// snapshotMainTables hashes every row outside the extension namespace, so a
// test can prove an extension write left services, their ETags and every
// sealed credential exactly as they were.
func snapshotMainTables(t *testing.T, store *Store) string {
	t.Helper()
	rows, err := store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'fork_checkin%' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		tables = append(tables, name)
	}
	rows.Close()
	digest := sha256.New()
	for _, table := range tables {
		data, err := store.db.Query(`SELECT * FROM "` + table + `" ORDER BY rowid`)
		if err != nil {
			// WITHOUT ROWID tables order by their primary key instead.
			data, err = store.db.Query(`SELECT * FROM "` + table + `"`)
			if err != nil {
				t.Fatal(err)
			}
		}
		columns, _ := data.Columns()
		for data.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := data.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(digest, "%s:%v\n", table, values)
		}
		data.Close()
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func sendCheckinAPI(t *testing.T, handler http.Handler, method, path, body string) (int, map[string]any) {
	t.Helper()
	request := httptest.NewRequest(method, forkcheckin.APIPrefix+path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	var decoded map[string]any
	if recorder.Body.Len() > 0 {
		if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
			t.Fatalf("%s %s: %v %s", method, path, err, recorder.Body)
		}
	}
	return recorder.Code, decoded
}

// Account writes over real storage: enable through settings, idempotent
// create, CAS update, session invalidation on redirect, busy and clean
// delete, and no change to any row outside the extension.
func TestForkCheckinAccountWriteAPIOverStore(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "writes.db"))
	defer store.Close()
	createForkCheckinService(t, store, "service_alpha")
	clock := newParkedClock()
	facade, err := forkcheckin.NewFacade(forkcheckin.FacadeConfig{
		SettingsPath: filepath.Join(t.TempDir(), "checkin.json"),
		Factory:      store.ForkCheckinModuleFactory(newAPIModuleBuilder),
		Clock:        clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer facade.Stop(ctx)
	handler := forkcheckin.NewAPIHandler(facade)
	before := snapshotMainTables(t, store)

	if code, _ := sendCheckinAPI(t, handler, http.MethodPut, "/settings", `{"request_id":"settings_off_01","enabled":false}`); code != http.StatusOK || countForkCheckinObjects(t, store) != 0 {
		t.Fatalf("disable while off=%d objects=%d", code, countForkCheckinObjects(t, store))
	}
	if code, _ := sendCheckinAPI(t, handler, http.MethodPut, "/settings", `{"request_id":"settings_on_001","enabled":true}`); code != http.StatusOK {
		t.Fatalf("enable=%d", code)
	}
	<-clock.idle
	draft := `{"request_id":"request_create_1","dashboard_base_url":"https://Relay.Example/","time_zone":"Asia/Shanghai"}`
	code, created := sendCheckinAPI(t, handler, http.MethodPost, "/accounts", draft)
	if code != http.StatusCreated || created["state"] != "draft" || created["automatic"] != false || created["revision"] != float64(1) {
		t.Fatalf("create=%d %v", code, created)
	}
	id := forkcheckin.AccountID(created["id"].(string))
	if code, replay := sendCheckinAPI(t, handler, http.MethodPost, "/accounts", draft); code != http.StatusCreated || replay["id"] != string(id) {
		t.Fatalf("replayed create=%d %v", code, replay)
	}
	if code, body := sendCheckinAPI(t, handler, http.MethodPost, "/accounts", strings.Replace(draft, "Asia/Shanghai", "UTC", 1)); code != http.StatusConflict || body["error"].(map[string]any)["code"] != "request_id_reused" {
		t.Fatalf("reused create=%d %v", code, body)
	}
	if page, err := store.ListForkCheckinAccounts(ctx, forkcheckin.ListOptions{}); err != nil || len(page.Items) != 1 {
		t.Fatalf("replay created another account: %v %v", page.Items, err)
	}

	// Connect out of band, as an authorization would, and seal a session.
	connected, err := store.ConnectForkCheckinAccount(ctx, id, "42", 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ForkCheckinSessions().Put(ctx, id, []byte("session-cookie")); err != nil {
		t.Fatal(err)
	}
	bind := fmt.Sprintf(`{"request_id":"request_update_1","expected_revision":%d,"automatic":true,"bound_services":["service_alpha"]}`, connected.Revision)
	code, updated := sendCheckinAPI(t, handler, http.MethodPatch, "/accounts/"+string(id), bind)
	if code != http.StatusOK || updated["automatic"] != true || updated["state"] != "connected" || len(updated["bound_services"].([]any)) != 1 {
		t.Fatalf("bind=%d %v", code, updated)
	}
	if code, replay := sendCheckinAPI(t, handler, http.MethodPatch, "/accounts/"+string(id), bind); code != http.StatusOK || replay["revision"] != updated["revision"] {
		t.Fatalf("replayed update advanced the revision: %d %v", code, replay)
	}
	stale := `{"request_id":"request_update_2","expected_revision":1,"time_zone":"UTC"}`
	if code, body := sendCheckinAPI(t, handler, http.MethodPatch, "/accounts/"+string(id), stale); code != http.StatusPreconditionFailed || body["error"].(map[string]any)["code"] != "revision_conflict" {
		t.Fatalf("stale CAS=%d %v", code, body)
	}
	unknown := fmt.Sprintf(`{"request_id":"request_update_3","expected_revision":%v,"bound_services":["service_ghost"]}`, updated["revision"])
	if code, _ := sendCheckinAPI(t, handler, http.MethodPatch, "/accounts/"+string(id), unknown); code != http.StatusBadRequest {
		t.Fatalf("unknown service=%d", code)
	}
	redirect := fmt.Sprintf(`{"request_id":"request_update_4","expected_revision":%v,"dashboard_base_url":"https://other.example"}`, updated["revision"])
	code, moved := sendCheckinAPI(t, handler, http.MethodPatch, "/accounts/"+string(id), redirect)
	if code != http.StatusOK || moved["state"] != "draft" || moved["automatic"] != false || moved["remote_user_id"] != nil {
		t.Fatalf("redirect=%d %v", code, moved)
	}
	if present, err := store.HasForkCheckinSession(ctx, id); err != nil || present {
		t.Fatalf("redirect kept the session: %v %v", present, err)
	}

	// A live lease blocks the delete; an expired or finished one does not.
	revision := int64(moved["revision"].(float64))
	busyJob := forkCheckinJobDraft(id, "request_job_busy", 'z')
	busyJob.ExpectedRevision = revision
	createForkCheckinJob(t, store, busyJob)
	jobs, err := store.ListForkCheckinJobs(ctx, id, forkcheckin.ListOptions{})
	if err != nil || len(jobs.Items) != 1 {
		t.Fatalf("jobs=%v %v", jobs, err)
	}
	if _, err := store.ClaimForkCheckinJobID(ctx, jobs.Items[0].ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	remove := fmt.Sprintf("/accounts/%s?request_id=request_delete_1&expected_revision=%d", id, revision)
	if code, body := sendCheckinAPI(t, handler, http.MethodDelete, remove, ""); code != http.StatusConflict || body["error"].(map[string]any)["code"] != "account_busy" {
		t.Fatalf("busy delete=%d %v", code, body)
	}
	if _, err := store.db.Exec(`UPDATE fork_checkin_job_attempts SET lease_expires_at = ?`, formatForkCheckinTime(time.Now().Add(-time.Minute))); err != nil {
		t.Fatal(err)
	}
	if code, _ := sendCheckinAPI(t, handler, http.MethodDelete, remove, ""); code != http.StatusNoContent {
		t.Fatalf("delete=%d", code)
	}
	if code, _ := sendCheckinAPI(t, handler, http.MethodDelete, remove, ""); code != http.StatusNoContent {
		t.Fatalf("replayed delete=%d", code)
	}
	for _, table := range []string{forkCheckinAccounts, forkCheckinBindings, forkCheckinCredentials, forkCheckinJobs} {
		if countRows(t, store, table) != 0 {
			t.Fatalf("delete left rows in %s", table)
		}
	}
	if after := snapshotMainTables(t, store); after != before {
		t.Fatal("extension writes changed a service, ETag or credential row")
	}
	// Disabling keeps the data and stops the module; repeating is harmless.
	for _, request := range []string{"settings_off_02", "settings_off_03"} {
		if code, _ := sendCheckinAPI(t, handler, http.MethodPut, "/settings", `{"request_id":"`+request+`","enabled":false}`); code != http.StatusOK {
			t.Fatalf("disable=%d", code)
		}
	}
	if code, _ := sendCheckinAPI(t, handler, http.MethodPost, "/accounts", draft); code != http.StatusConflict {
		t.Fatalf("write after disable=%d", code)
	}
}

func TestForkCheckinAccountWriterConcurrentRetriesCreateOnce(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	writer := forkCheckinAccountWriter{store: store}
	request := forkcheckin.AccountDraftRequest{RequestID: "request_race_01", DashboardBaseURL: "https://relay.example", TimeZone: "UTC"}
	var wg sync.WaitGroup
	results := make([]forkcheckin.AccountWriteResult, 8)
	errs := make([]error, len(results))
	for index := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[index], errs[index] = writer.CreateAccount(ctx, request)
		}()
	}
	wg.Wait()
	replays := 0
	for index, result := range results {
		if errs[index] != nil || string(result.Body) != string(results[0].Body) {
			t.Fatalf("retry %d: %v %s", index, errs[index], result.Body)
		}
		if result.Replayed {
			replays++
		}
	}
	if replays != len(results)-1 || countRows(t, store, forkCheckinAccounts) != 1 {
		t.Fatalf("replays=%d accounts=%d", replays, countRows(t, store, forkCheckinAccounts))
	}
}

func TestForkCheckinAccountUpdateKeepsBindingAge(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinService(t, store, "service_alpha")
	account := forkCheckinDraft("acct_age", "https://relay.example/")
	account.BoundServices = []contract.ServiceID{"service_alpha"}
	if _, err := store.CreateForkCheckinAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	first, err := store.ResolveForkCheckinBindings(ctx, "acct_age")
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	time.Sleep(5 * time.Millisecond)
	writer := forkCheckinAccountWriter{store: store}
	zone := "UTC"
	if _, err := writer.UpdateAccount(ctx, "acct_age", forkcheckin.AccountUpdateRequest{RequestID: "request_age_01", ExpectedRevision: 1, TimeZone: &zone}); err != nil {
		t.Fatal(err)
	}
	second, err := store.ResolveForkCheckinBindings(ctx, "acct_age")
	if err != nil || len(second) != 1 || !second[0].BoundAt.Equal(first[0].BoundAt) {
		t.Fatalf("binding age reset: %v -> %v (%v)", first, second, err)
	}
}
