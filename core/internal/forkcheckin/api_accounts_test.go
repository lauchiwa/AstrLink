package forkcheckin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

type apiWriter struct {
	err   error
	calls int
}

func (w *apiWriter) result(status int) (AccountWriteResult, error) {
	w.calls++
	if w.err != nil {
		return AccountWriteResult{}, w.err
	}
	return AccountWriteResult{Status: status, Body: []byte(`{"id":"acct_one"}`)}, nil
}

func (w *apiWriter) CreateAccount(context.Context, AccountDraftRequest) (AccountWriteResult, error) {
	return w.result(http.StatusCreated)
}

func (w *apiWriter) UpdateAccount(context.Context, AccountID, AccountUpdateRequest) (AccountWriteResult, error) {
	return w.result(http.StatusOK)
}

func (w *apiWriter) DeleteAccount(context.Context, AccountID, AccountDeleteRequest) (AccountWriteResult, error) {
	return w.result(http.StatusNoContent)
}

func newWriteFixture(t *testing.T, enabled *bool) (*lifecycleFixture, *apiWriter, http.Handler) {
	t.Helper()
	f := newLifecycleFixture(t, enabled)
	writer := &apiWriter{}
	factory := f.facade.config.Factory
	f.facade.config.Factory = func(ctx context.Context) (ModuleParts, error) {
		parts, err := factory(ctx)
		parts.Reader, parts.Writer = &apiReader{}, writer
		return parts, err
	}
	f.facade.Start(context.Background())
	return f, writer, NewAPIHandler(f.facade)
}

func sendAPI(handler http.Handler, method, path, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestAPISettingsPutEnablesOnceAndReplays(t *testing.T) {
	f, _, handler := newWriteFixture(t, nil)
	if recorder := sendAPI(handler, http.MethodPut, APIPrefix+"/settings", `{"request_id":"settings_off_1","enabled":false}`); recorder.Code != http.StatusOK || f.calls.Load() != 0 {
		t.Fatalf("disable while off=%d calls=%d", recorder.Code, f.calls.Load())
	}
	on := `{"request_id":"settings_on_01","enabled":true}`
	if recorder := sendAPI(handler, http.MethodPut, APIPrefix+"/settings", on); recorder.Code != http.StatusOK || strings.TrimSpace(recorder.Body.String()) != `{"enabled":true}` {
		t.Fatalf("enable=%d %s", recorder.Code, recorder.Body)
	}
	if recorder := sendAPI(handler, http.MethodPut, APIPrefix+"/settings", on); recorder.Code != http.StatusOK {
		t.Fatalf("replay=%d", recorder.Code)
	}
	if recorder := sendAPI(handler, http.MethodPut, APIPrefix+"/settings", `{"request_id":"settings_on_02","enabled":true}`); recorder.Code != http.StatusOK {
		t.Fatalf("second enable=%d", recorder.Code)
	}
	if f.calls.Load() != 1 {
		t.Fatalf("repeated enables rebuilt the module: %d factory calls", f.calls.Load())
	}
	reused := sendAPI(handler, http.MethodPut, APIPrefix+"/settings", `{"request_id":"settings_on_01","enabled":false}`)
	if reused.Code != http.StatusConflict || apiErrorCode(t, reused) != "request_id_reused" || !f.facade.Status(context.Background()).Enabled {
		t.Fatalf("reused id=%d %s", reused.Code, reused.Body)
	}
	if recorder := sendAPI(handler, http.MethodPut, APIPrefix+"/settings", `{"request_id":"settings_off_2","enabled":false}`); recorder.Code != http.StatusOK || f.facade.Status(context.Background()).Initialized {
		t.Fatalf("disable=%d %+v", recorder.Code, f.facade.Status(context.Background()))
	}
	// Replaying the earlier enable after a disable must not turn it back on.
	if recorder := sendAPI(handler, http.MethodPut, APIPrefix+"/settings", on); recorder.Code != http.StatusOK || f.facade.Status(context.Background()).Enabled || f.calls.Load() != 1 {
		t.Fatalf("stale replay re-enabled: %d calls=%d", recorder.Code, f.calls.Load())
	}
}

func TestAPIWriteBodiesAreBoundedAndStrict(t *testing.T) {
	enabled := true
	_, writer, handler := newWriteFixture(t, &enabled)
	cases := []struct {
		name, method, path, contentType, body string
		status                                int
		code                                  string
	}{
		{"oversized", http.MethodPost, "/accounts", "application/json", `{"request_id":"request_big_01","dashboard_base_url":"` + strings.Repeat("a", maxWriteBodyBytes) + `"}`, http.StatusRequestEntityTooLarge, "payload_too_large"},
		{"media", http.MethodPost, "/accounts", "text/plain", `{}`, http.StatusUnsupportedMediaType, "unsupported_media_type"},
		{"unknown field", http.MethodPost, "/accounts", "application/json", `{"request_id":"request_unk_01","dashboard_base_url":"https://a.example","time_zone":"UTC","state":"connected"}`, http.StatusBadRequest, "invalid_json"},
		{"trailing", http.MethodPost, "/accounts", "application/json", `{"request_id":"request_trl_01","dashboard_base_url":"https://a.example","time_zone":"UTC"}{}`, http.StatusBadRequest, "invalid_json"},
		{"missing request id", http.MethodPost, "/accounts", "application/json", `{"dashboard_base_url":"https://a.example","time_zone":"UTC"}`, http.StatusBadRequest, "validation_failed"},
		{"missing revision", http.MethodPatch, "/accounts/acct_one", "application/json", `{"request_id":"request_rev_01","time_zone":"UTC"}`, http.StatusBadRequest, "validation_failed"},
		{"empty patch", http.MethodPatch, "/accounts/acct_one", "application/json", `{"request_id":"request_emp_01","expected_revision":1}`, http.StatusBadRequest, "validation_failed"},
		{"server-owned field", http.MethodPatch, "/accounts/acct_one", "application/json", `{"request_id":"request_own_01","expected_revision":1,"remote_user_id":"7"}`, http.StatusBadRequest, "invalid_json"},
		{"settings without enabled", http.MethodPut, "/settings", "application/json", `{"request_id":"settings_no_01"}`, http.StatusBadRequest, "validation_failed"},
		{"delete without revision", http.MethodDelete, "/accounts/acct_one?request_id=request_del_01", "", "", http.StatusBadRequest, "validation_failed"},
		{"delete padded revision", http.MethodDelete, "/accounts/acct_one?request_id=request_del_01&expected_revision=01", "", "", http.StatusBadRequest, "validation_failed"},
		{"delete extra query", http.MethodDelete, "/accounts/acct_one?request_id=request_del_01&expected_revision=1&force=1", "", "", http.StatusBadRequest, "validation_failed"},
	}
	for _, tc := range cases {
		request := httptest.NewRequest(tc.method, APIPrefix+tc.path, strings.NewReader(tc.body))
		if tc.contentType != "" {
			request.Header.Set("Content-Type", tc.contentType)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != tc.status || apiErrorCode(t, recorder) != tc.code {
			t.Fatalf("%s=%d %s", tc.name, recorder.Code, recorder.Body)
		}
	}
	if writer.calls != 0 {
		t.Fatalf("rejected bodies reached storage %d times", writer.calls)
	}
	deleted := sendAPI(handler, http.MethodDelete, APIPrefix+"/accounts/acct_one?request_id=request_del_01&expected_revision=1", "")
	if deleted.Code != http.StatusNoContent || deleted.Body.Len() != 0 {
		t.Fatalf("delete=%d %q", deleted.Code, deleted.Body)
	}
}

func TestAPIWriteErrorsAreStableAndSanitized(t *testing.T) {
	enabled := true
	_, writer, handler := newWriteFixture(t, &enabled)
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrRequestIDReused, http.StatusConflict, "request_id_reused"},
		{fmt.Errorf("%w: acct_secret at /private", ErrAccountConflict), http.StatusConflict, "account_conflict"},
		{ErrAccountBusy, http.StatusConflict, "account_busy"},
		{ErrUnknownService, http.StatusBadRequest, "validation_failed"},
		{fmt.Errorf("%w: revision 3, not 2", storagecontract.ErrPrecondition), http.StatusPreconditionFailed, "revision_conflict"},
		{fmt.Errorf("%w: proxy url carries user:pass", storagecontract.ErrInvalidRecord), http.StatusBadRequest, "validation_failed"},
		{fmt.Errorf("%w: acct_x", storagecontract.ErrNotFound), http.StatusNotFound, "not_found"},
		{fmt.Errorf("disk I/O error at /Users/private/astrlink.db"), http.StatusInternalServerError, "checkin_storage_failed"},
	}
	for _, tc := range cases {
		writer.err = tc.err
		recorder := sendAPI(handler, http.MethodPatch, APIPrefix+"/accounts/acct_one", `{"request_id":"request_err_01","expected_revision":1,"time_zone":"UTC"}`)
		if recorder.Code != tc.status || apiErrorCode(t, recorder) != tc.code {
			t.Fatalf("%v => %d %s", tc.err, recorder.Code, recorder.Body)
		}
		for _, leaked := range []string{"/private", "/Users", "user:pass", "acct_secret", "revision 3"} {
			if strings.Contains(recorder.Body.String(), leaked) {
				t.Fatalf("%v leaked %q: %s", tc.err, leaked, recorder.Body)
			}
		}
	}
}

func TestAPIWritesWhileDisabledAreRefused(t *testing.T) {
	f, writer, handler := newWriteFixture(t, nil)
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/accounts", `{"request_id":"request_dis_01","dashboard_base_url":"https://a.example","time_zone":"UTC"}`},
		{http.MethodPatch, "/accounts/acct_one", `{"request_id":"request_dis_02","expected_revision":1,"automatic":true}`},
		{http.MethodDelete, "/accounts/acct_one?request_id=request_dis_03&expected_revision=1", ""},
	} {
		recorder := sendAPI(handler, tc.method, APIPrefix+tc.path, tc.body)
		if recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != "checkin_disabled" {
			t.Fatalf("%s %s=%d %s", tc.method, tc.path, recorder.Code, recorder.Body)
		}
	}
	if writer.calls != 0 || f.calls.Load() != 0 {
		t.Fatal("a disabled write created the module")
	}
	var envelope apiErrorEnvelope
	_ = json.Unmarshal(sendAPI(handler, http.MethodGet, APIPrefix+"/accounts", "").Body.Bytes(), &envelope)
	if envelope.Error.Code != "checkin_disabled" {
		t.Fatalf("read while disabled=%+v", envelope)
	}
}
