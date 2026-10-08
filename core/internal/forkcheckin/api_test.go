package forkcheckin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

type apiReader struct {
	err      error
	lastJobs ListOptions
	account  AccountID
}

func (r *apiReader) GetAccount(_ context.Context, id AccountID) (AccountView, error) {
	if r.err != nil {
		return AccountView{}, r.err
	}
	if id != "acct_one" {
		return AccountView{}, fmt.Errorf("%w: account %q in /private/path", storagecontract.ErrNotFound, id)
	}
	return AccountView{ID: id}, nil
}

func (r *apiReader) ListAccounts(context.Context, ListOptions) (AccountViewPage, error) {
	return AccountViewPage{}, r.err
}

func (r *apiReader) GetJob(_ context.Context, id JobID) (JobReceipt, error) {
	if r.err != nil {
		return JobReceipt{}, r.err
	}
	now := time.Unix(1700000000, 0).UTC()
	return JobReceipt{ID: id, AccountID: "acct_one", Action: JobActionCheckIn, Status: JobStatusSuccess,
		Dispatched: true, ProofSource: ProofSourceSubmitResponse, RequestID: "request_private",
		CreatedAt: now, CompletedAt: &now}, nil
}

func (r *apiReader) GetBatch(_ context.Context, id JobID) ([]JobReceipt, error) {
	if r.err != nil {
		return nil, r.err
	}
	child, _ := r.GetJob(context.Background(), "job_child")
	return []JobReceipt{child}, nil
}

func (r *apiReader) ListJobs(_ context.Context, account AccountID, options ListOptions) (JobPage, error) {
	r.lastJobs, r.account = options, account
	return JobPage{NextCursor: "next"}, r.err
}

func newAPIFixture(t *testing.T, enabled bool) (*lifecycleFixture, *apiReader, http.Handler) {
	t.Helper()
	f := newLifecycleFixture(t, &enabled)
	reader := &apiReader{}
	factory := f.facade.config.Factory
	f.facade.config.Factory = func(ctx context.Context) (ModuleParts, error) {
		parts, err := factory(ctx)
		parts.Reader = reader
		return parts, err
	}
	f.facade.Start(context.Background())
	return f, reader, NewAPIHandler(f.facade)
}

func serveAPI(handler http.Handler, method, path string) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder
}

func apiErrorCode(t *testing.T, recorder *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope apiErrorEnvelope
	if err := json.Unmarshal(recorder.Body.Bytes(), &envelope); err != nil || envelope.RequestID == "" || envelope.Error.Details == nil {
		t.Fatalf("bad error envelope: %s", recorder.Body)
	}
	return envelope.Error.Code
}

func TestAPIDisabledAnswersOnlyStatusAndSettings(t *testing.T) {
	f, _, handler := newAPIFixture(t, false)
	status := serveAPI(handler, http.MethodGet, APIPrefix+"/status")
	var body map[string]any
	if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &body) != nil {
		t.Fatalf("status=%d %s", status.Code, status.Body)
	}
	want := map[string]any{"protocol_version": float64(1), "present": true, "enabled": false, "storage_ready": false, "scheduler_running": false}
	if fmt.Sprint(body) != fmt.Sprint(want) || status.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("disabled status=%v", body)
	}
	if settings := serveAPI(handler, http.MethodGet, APIPrefix+"/settings"); settings.Code != http.StatusOK || strings.TrimSpace(settings.Body.String()) != `{"enabled":false}` {
		t.Fatalf("settings=%d %s", settings.Code, settings.Body)
	}
	for _, path := range []string{"/accounts", "/accounts/acct_one", "/jobs", "/jobs/job_one"} {
		recorder := serveAPI(handler, http.MethodGet, APIPrefix+path)
		if recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != "checkin_disabled" {
			t.Fatalf("%s while disabled=%d %s", path, recorder.Code, recorder.Body)
		}
	}
	if f.calls.Load() != 0 || f.store.listed.Load() != 0 {
		t.Fatal("disabled reads created the module")
	}
}

func TestAPIReadsArePagedStrictAndSanitized(t *testing.T) {
	_, reader, handler := newAPIFixture(t, true)
	if recorder := serveAPI(handler, http.MethodGet, APIPrefix+"/jobs?limit=100&cursor=abc&account_id=acct_one"); recorder.Code != http.StatusOK {
		t.Fatalf("jobs page=%d %s", recorder.Code, recorder.Body)
	} else if reader.lastJobs.Limit != 100 || reader.lastJobs.Cursor != "abc" || reader.account != "acct_one" ||
		strings.TrimSpace(recorder.Body.String()) != `{"items":[],"next_cursor":"next"}` {
		t.Fatalf("jobs forwarded=%+v %q body=%s", reader.lastJobs, reader.account, recorder.Body)
	}
	if recorder := serveAPI(handler, http.MethodGet, APIPrefix+"/accounts"); strings.TrimSpace(recorder.Body.String()) != `{"items":[]}` {
		t.Fatalf("empty accounts=%s", recorder.Body)
	}
	for _, query := range []string{"limit=0", "limit=101", "limit=01", "limit=x", "limit=1&limit=2", "cursor=", "unknown=1", "account_id=Bad"} {
		recorder := serveAPI(handler, http.MethodGet, APIPrefix+"/jobs?"+query)
		if recorder.Code != http.StatusBadRequest || apiErrorCode(t, recorder) != "validation_failed" {
			t.Fatalf("query %q=%d %s", query, recorder.Code, recorder.Body)
		}
	}
	job := serveAPI(handler, http.MethodGet, APIPrefix+"/jobs/job_one")
	var public map[string]any
	if job.Code != http.StatusOK || json.Unmarshal(job.Body.Bytes(), &public) != nil || public["proof_source"] != "submission_response" {
		t.Fatalf("job=%d %s", job.Code, job.Body)
	}
	if strings.Contains(job.Body.String(), "request_private") || public["request_id"] != nil || public["children"] == nil {
		t.Fatalf("job leaked private receipt fields: %s", job.Body)
	}
	missing := serveAPI(handler, http.MethodGet, APIPrefix+"/accounts/acct_missing")
	if missing.Code != http.StatusNotFound || strings.Contains(missing.Body.String(), "/private/path") {
		t.Fatalf("missing account=%d %s", missing.Code, missing.Body)
	}
	for _, path := range []string{"/accounts/BAD", "/jobs/job_one/other", "/authorizations/auth_one_session", "/unknown"} {
		if recorder := serveAPI(handler, http.MethodGet, APIPrefix+path); recorder.Code != http.StatusNotFound {
			t.Fatalf("%s=%d", path, recorder.Code)
		}
	}
	// An authorization session is never readable; it can only be begun.
	if recorder := serveAPI(handler, http.MethodGet, APIPrefix+"/authorizations"); recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != http.MethodPost {
		t.Fatalf("GET /authorizations=%d", recorder.Code)
	}
	if recorder := serveAPI(handler, http.MethodGet, APIPrefix+"/jobs/job_one?x=1"); recorder.Code != http.StatusBadRequest {
		t.Fatalf("query on item=%d", recorder.Code)
	}
	for _, method := range []string{http.MethodPost, http.MethodPatch, http.MethodDelete} {
		recorder := serveAPI(handler, method, APIPrefix+"/settings")
		if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != "GET, PUT" {
			t.Fatalf("%s settings=%d", method, recorder.Code)
		}
	}
	for _, path := range []string{"/jobs/job_one", "/status"} {
		if recorder := serveAPI(handler, http.MethodPost, APIPrefix+path); recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("POST %s=%d", path, recorder.Code)
		}
	}
	reader.err = errors.New("SELECT secret FROM fork_checkin_vault at /Users/private")
	failed := serveAPI(handler, http.MethodGet, APIPrefix+"/accounts")
	if failed.Code != http.StatusInternalServerError || apiErrorCode(t, failed) != "checkin_storage_failed" ||
		strings.Contains(failed.Body.String(), "secret") || strings.Contains(failed.Body.String(), "/Users") {
		t.Fatalf("storage failure leaked: %d %s", failed.Code, failed.Body)
	}
}

func TestAPIStatusReportsEnabledAndStopping(t *testing.T) {
	f, _, handler := newAPIFixture(t, true)
	var status APIStatus
	recorder := serveAPI(handler, http.MethodGet, APIPrefix+"/status")
	if json.Unmarshal(recorder.Body.Bytes(), &status) != nil || !status.Enabled || !status.StorageReady || !status.SchedulerRunning {
		t.Fatalf("enabled status=%s", recorder.Body)
	}
	if err := f.facade.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder := serveAPI(handler, http.MethodGet, APIPrefix+"/accounts"); recorder.Code != http.StatusServiceUnavailable || apiErrorCode(t, recorder) != "checkin_stopping" {
		t.Fatalf("read after stop=%d %s", recorder.Code, recorder.Body)
	}
	if recorder := serveAPI(handler, http.MethodGet, APIPrefix+"/status"); recorder.Code != http.StatusOK {
		t.Fatalf("status after stop=%d", recorder.Code)
	}
}
