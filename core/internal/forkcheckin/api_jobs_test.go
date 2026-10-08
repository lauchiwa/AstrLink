package forkcheckin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

type apiJobs struct {
	err      error
	calls    int
	created  JobCreateRequest
	launch   []JobID
	interrup []JobID
}

func (j *apiJobs) CreateJobs(_ context.Context, request JobCreateRequest) (JobWriteResult, error) {
	j.calls++
	j.created = request
	if j.err != nil {
		return JobWriteResult{}, j.err
	}
	return JobWriteResult{AccountWriteResult: AccountWriteResult{Status: http.StatusAccepted, Body: []byte(`{"id":"job_one","status":"queued"}`)}, Launch: j.launch}, nil
}

func (j *apiJobs) CancelJob(context.Context, JobID, JobCancelRequest) (JobWriteResult, error) {
	j.calls++
	if j.err != nil {
		return JobWriteResult{}, j.err
	}
	return JobWriteResult{AccountWriteResult: AccountWriteResult{Status: http.StatusOK, Body: []byte(`{"id":"job_one","status":"cancelled"}`)}, Interrupt: j.interrup}, nil
}

func newJobsFixture(t *testing.T) (*lifecycleFixture, *apiJobs, http.Handler) {
	t.Helper()
	enabled := true
	f := newLifecycleFixture(t, &enabled)
	jobs := &apiJobs{}
	factory := f.facade.config.Factory
	f.facade.config.Factory = func(ctx context.Context) (ModuleParts, error) {
		parts, err := factory(ctx)
		parts.Reader, parts.Jobs = &apiReader{}, jobs
		return parts, err
	}
	f.facade.Start(context.Background())
	return f, jobs, NewAPIHandler(f.facade)
}

func TestAPIJobCreateAcceptsOnlyAccountsAndAction(t *testing.T) {
	_, jobs, handler := newJobsFixture(t)
	accepted := sendAPI(handler, http.MethodPost, APIPrefix+"/jobs", `{"request_id":"request_job_01","action":"check_in","accounts":["acct_one"],"expected_revision":3}`)
	if accepted.Code != http.StatusAccepted || jobs.created.ExpectedRevision != 3 || jobs.created.JobRequest().Trigger != JobTriggerManual {
		t.Fatalf("create=%d %+v", accepted.Code, jobs.created)
	}
	many := make([]string, MaxBatchAccounts+1)
	for index := range many {
		many[index] = fmt.Sprintf(`"acct_%02d"`, index)
	}
	cases := []struct{ name, body, code string }{
		{"url passthrough", `{"request_id":"request_job_02","action":"check_in","accounts":["acct_one"],"url":"https://evil.example"}`, "invalid_json"},
		{"method passthrough", `{"request_id":"request_job_03","action":"check_in","accounts":["acct_one"],"method":"DELETE"}`, "invalid_json"},
		{"unknown action", `{"request_id":"request_job_04","action":"redeem","accounts":["acct_one"]}`, "validation_failed"},
		{"no accounts", `{"request_id":"request_job_05","action":"check_in","accounts":[]}`, "validation_failed"},
		{"duplicate accounts", `{"request_id":"request_job_06","action":"check_in","accounts":["acct_one","acct_one"]}`, "validation_failed"},
		{"too many accounts", `{"request_id":"request_job_07","action":"check_in","accounts":[` + strings.Join(many, ",") + `]}`, "validation_failed"},
		{"batch revision", `{"request_id":"request_job_08","action":"check_in","accounts":["acct_one","acct_two"],"expected_revision":1}`, "validation_failed"},
		{"bad account id", `{"request_id":"request_job_09","action":"check_in","accounts":["../acct"]}`, "validation_failed"},
		{"no request id", `{"action":"check_in","accounts":["acct_one"]}`, "validation_failed"},
	}
	before := jobs.calls
	for _, tc := range cases {
		recorder := sendAPI(handler, http.MethodPost, APIPrefix+"/jobs", tc.body)
		if recorder.Code != http.StatusBadRequest || apiErrorCode(t, recorder) != tc.code {
			t.Fatalf("%s=%d %s", tc.name, recorder.Code, recorder.Body)
		}
	}
	if jobs.calls != before {
		t.Fatal("a rejected job body reached storage")
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPut, "/jobs"}, {http.MethodPost, "/jobs/job_one"}, {http.MethodGet, "/jobs/job_one/cancel"}, {http.MethodDelete, "/jobs/job_one"},
	} {
		if recorder := sendAPI(handler, tc.method, APIPrefix+tc.path, ""); recorder.Code != http.StatusMethodNotAllowed {
			t.Fatalf("%s %s=%d", tc.method, tc.path, recorder.Code)
		}
	}
	if recorder := sendAPI(handler, http.MethodPost, APIPrefix+"/jobs/job_one/cancel/extra", `{"request_id":"request_can_01"}`); recorder.Code != http.StatusNotFound {
		t.Fatalf("nested cancel=%d", recorder.Code)
	}
}

func TestAPIJobErrorsAreStable(t *testing.T) {
	_, jobs, handler := newJobsFixture(t)
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{ErrRequestIDReused, http.StatusConflict, "request_id_reused"},
		{fmt.Errorf("%w: account acct_one is at revision 4", storagecontract.ErrPrecondition), http.StatusPreconditionFailed, "revision_conflict"},
		{fmt.Errorf("%w: account %q", storagecontract.ErrNotFound, "acct_secret"), http.StatusNotFound, "not_found"},
		{ErrJobNotCancellable, http.StatusConflict, "job_not_cancellable"},
	} {
		jobs.err = tc.err
		path, body := "/jobs", `{"request_id":"request_err_01","action":"check_in","accounts":["acct_one"]}`
		if tc.err == ErrJobNotCancellable {
			path, body = "/jobs/job_one/cancel", `{"request_id":"request_err_02"}`
		}
		recorder := sendAPI(handler, http.MethodPost, APIPrefix+path, body)
		if recorder.Code != tc.status || apiErrorCode(t, recorder) != tc.code || strings.Contains(recorder.Body.String(), "acct_secret") || strings.Contains(recorder.Body.String(), "revision 4") {
			t.Fatalf("%v => %d %s", tc.err, recorder.Code, recorder.Body)
		}
	}
}

func TestAggregateBatchReportsWorstChild(t *testing.T) {
	early := time.Unix(100, 0).UTC()
	late := time.Unix(200, 0).UTC()
	child := func(status JobStatus, completed *time.Time) JobReceipt {
		return JobReceipt{ID: JobID("job_" + string(status)), AccountID: "acct_one", Action: JobActionCheckIn, Status: status, CreatedAt: early, CompletedAt: completed}
	}
	running := AggregateBatch("batch_one", []JobReceipt{child(JobStatusSuccess, &early), child(JobStatusRunning, nil)})
	if running.Status != JobStatusRunning || running.FinishedAt != nil || len(running.Children) != 2 || running.AccountID != "" {
		t.Fatalf("running=%+v", running)
	}
	mixed := AggregateBatch("batch_one", []JobReceipt{child(JobStatusSuccess, &early), child(JobStatusAuthRequired, &late)})
	if mixed.Status != JobStatusAuthRequired || mixed.FinishedAt == nil || !mixed.FinishedAt.Equal(late) {
		t.Fatalf("mixed=%+v", mixed)
	}
	if done := AggregateBatch("batch_one", []JobReceipt{child(JobStatusSuccess, &early), child(JobStatusAlreadyChecked, &late)}); done.Status != JobStatusAlreadyChecked {
		t.Fatalf("done=%+v", done)
	}
}

func TestAPIBatchParentIsProjectedFromChildren(t *testing.T) {
	_, _, handler := newJobsFixture(t)
	recorder := sendAPI(handler, http.MethodGet, APIPrefix+"/jobs/batch_one", "")
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"id":"batch_one"`) || !strings.Contains(recorder.Body.String(), `"id":"job_child"`) || strings.Contains(recorder.Body.String(), "request_private") {
		t.Fatalf("batch=%d %s", recorder.Code, recorder.Body)
	}
}
