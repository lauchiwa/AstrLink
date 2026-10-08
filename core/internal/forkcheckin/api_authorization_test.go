package forkcheckin

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

const authSecretMarker = "AUTHORIZATION-SECRET-MARKER"

type authStore struct {
	mu        sync.Mutex
	account   Account
	busy      bool
	receipts  map[string]AuthorizationReceipt
	commits   int
	commitErr error
}

func (s *authStore) ReadForkCheckinAuthorizationAccount(_ context.Context, id AccountID) (Account, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.account.ID {
		return Account{}, false, storagecontract.ErrNotFound
	}
	return s.account, s.busy, nil
}

func (s *authStore) LookupForkCheckinAuthorizationReceipt(_ context.Context, requestID string) (AuthorizationReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.receipts[requestID]
	if !ok {
		return AuthorizationReceipt{}, storagecontract.ErrNotFound
	}
	return receipt, nil
}

func (s *authStore) CompleteForkCheckinAuthorization(_ context.Context, commit AuthorizationCommit) (AccountWriteResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.commitErr != nil {
		return AccountWriteResult{}, s.commitErr
	}
	if s.account.Revision != commit.Revision || s.account.ConfigFingerprint() != commit.ConfigFingerprint {
		return AccountWriteResult{}, ErrRevisionChanged
	}
	s.commits++
	s.account.Revision++
	s.account.State, s.account.RemoteUserID = AccountStateConnected, commit.RemoteUserID
	body, _ := json.Marshal(s.account.View())
	s.receipts[commit.RequestID] = AuthorizationReceipt{Route: WriteRouteAuthorizationComplete, Fingerprint: commit.Fingerprint, Status: http.StatusOK, Body: body}
	return AccountWriteResult{Status: http.StatusOK, Body: body}, nil
}

type authAdapter struct {
	SiteAdapter
	mu        sync.Mutex
	calls     int
	expected  []string
	identity  SiteIdentity
	err       error
	invalided int
}

func (a *authAdapter) ValidateIdentity(_ context.Context, snapshot AccountSnapshot) (SiteIdentity, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	a.expected = append(a.expected, snapshot.Account.RemoteUserID)
	return a.identity, a.err
}

func (a *authAdapter) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func newAuthorizationFixture(t *testing.T) (*authStore, *authAdapter, http.Handler) {
	t.Helper()
	enabled := true
	f := newLifecycleFixture(t, &enabled)
	store := &authStore{account: authAccount("acct_auth"), receipts: make(map[string]AuthorizationReceipt)}
	adapter := &authAdapter{identity: SiteIdentity{RemoteUserID: "7"}}
	factory := f.facade.config.Factory
	f.facade.config.Factory = func(ctx context.Context) (ModuleParts, error) {
		parts, err := factory(ctx)
		parts.Authorizations, parts.VerifyAuthorization = store, adapter.ValidateIdentity
		parts.Invalidate = func(AccountID) {
			adapter.mu.Lock()
			adapter.invalided++
			adapter.mu.Unlock()
		}
		return parts, err
	}
	f.facade.Start(context.Background())
	return store, adapter, NewAPIHandler(f.facade)
}

func authCredential(t *testing.T, bearer string) string {
	t.Helper()
	encoded, err := EncodeNetworkCredential(NetworkCredential{Version: 1, Bearer: bearer})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

func beginAuthorization(t *testing.T, handler http.Handler, requestID string, revision int64) string {
	t.Helper()
	recorder := sendAPI(handler, http.MethodPost, APIPrefix+"/authorizations",
		fmt.Sprintf(`{"request_id":%q,"account_id":"acct_auth","expected_revision":%d}`, requestID, revision))
	var session Authorization
	if recorder.Code != http.StatusCreated || json.Unmarshal(recorder.Body.Bytes(), &session) != nil || session.SessionID == "" {
		t.Fatalf("begin=%d %s", recorder.Code, recorder.Body)
	}
	return session.SessionID
}

func completeAuthorizationBody(requestID, credential, extra string) string {
	return fmt.Sprintf(`{"request_id":%q,"credential":%q%s}`, requestID, credential, extra)
}

func TestAPIAuthorizationRejectsMalformedInputWithoutContactingTheSite(t *testing.T) {
	store, adapter, handler := newAuthorizationFixture(t)
	session := beginAuthorization(t, handler, "request_begin_01", 1)
	complete := APIPrefix + "/authorizations/" + session + "/complete"
	credential := authCredential(t, authSecretMarker)
	foreign, err := EncodeNetworkCredential(NetworkCredential{Version: 1, Cookies: []SessionCookie{
		{Name: "session", Value: authSecretMarker, Domain: "other.example", Path: "/"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"begin method", http.MethodGet, APIPrefix + "/authorizations", "", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"begin unknown field", http.MethodPost, APIPrefix + "/authorizations", `{"request_id":"request_begin_02","account_id":"acct_auth","expected_revision":1,"dashboard_base_url":"https://evil.example"}`, http.StatusBadRequest, "invalid_json"},
		{"begin no revision", http.MethodPost, APIPrefix + "/authorizations", `{"request_id":"request_begin_03","account_id":"acct_auth"}`, http.StatusBadRequest, "validation_failed"},
		{"begin bad account", http.MethodPost, APIPrefix + "/authorizations", `{"request_id":"request_begin_04","account_id":"Acct","expected_revision":1}`, http.StatusBadRequest, "validation_failed"},
		{"begin missing account", http.MethodPost, APIPrefix + "/authorizations", `{"request_id":"request_begin_05","account_id":"acct_missing","expected_revision":1}`, http.StatusNotFound, "not_found"},
		{"begin stale revision", http.MethodPost, APIPrefix + "/authorizations", `{"request_id":"request_begin_06","account_id":"acct_auth","expected_revision":2}`, http.StatusPreconditionFailed, "revision_conflict"},
		{"begin reused id", http.MethodPost, APIPrefix + "/authorizations", `{"request_id":"request_begin_01","account_id":"acct_auth","expected_revision":2}`, http.StatusConflict, "request_id_reused"},
		{"session is not readable", http.MethodGet, APIPrefix + "/authorizations/" + session, "", http.StatusNotFound, "not_found"},
		{"complete method", http.MethodGet, complete, "", http.StatusMethodNotAllowed, "method_not_allowed"},
		{"complete bad session path", http.MethodPost, APIPrefix + "/authorizations/bad!/complete", completeAuthorizationBody("request_complete_01", credential, ""), http.StatusNotFound, "authorization_not_found"},
		{"complete unknown session", http.MethodPost, APIPrefix + "/authorizations/auth_unknown_session/complete", completeAuthorizationBody("request_complete_02", credential, ""), http.StatusNotFound, "authorization_not_found"},
		{"complete unknown field", http.MethodPost, complete, completeAuthorizationBody("request_complete_03", credential, `,"remote_user_id":"7"`), http.StatusBadRequest, "invalid_json"},
		{"complete query", http.MethodPost, complete + "?debug=1", completeAuthorizationBody("request_complete_04", credential, ""), http.StatusBadRequest, "validation_failed"},
		{"complete bad base64", http.MethodPost, complete, completeAuthorizationBody("request_complete_05", "not base64!", ""), http.StatusBadRequest, "invalid_json"},
		{"complete empty credential", http.MethodPost, complete, completeAuthorizationBody("request_complete_06", "", ""), http.StatusBadRequest, "validation_failed"},
		{"complete empty claim", http.MethodPost, complete, completeAuthorizationBody("request_complete_07", credential, `,"claimed_user_id":""`), http.StatusBadRequest, "validation_failed"},
		{"complete zero revision", http.MethodPost, complete, completeAuthorizationBody("request_complete_08", credential, `,"expected_revision":0`), http.StatusBadRequest, "validation_failed"},
		{"complete not an envelope", http.MethodPost, complete, completeAuthorizationBody("request_complete_09", base64.StdEncoding.EncodeToString([]byte(authSecretMarker)), ""), http.StatusBadRequest, "validation_failed"},
		{"complete foreign cookie", http.MethodPost, complete, completeAuthorizationBody("request_complete_10", base64.StdEncoding.EncodeToString(foreign), ""), http.StatusBadRequest, "validation_failed"},
		{"complete oversized credential", http.MethodPost, complete, completeAuthorizationBody("request_complete_11", base64.StdEncoding.EncodeToString(make([]byte, MaxCredentialBytes+1)), ""), http.StatusBadRequest, "validation_failed"},
		{"complete oversized body", http.MethodPost, complete, completeAuthorizationBody("request_complete_12", strings.Repeat("A", maxAuthorizationBodyBytes), ""), http.StatusRequestEntityTooLarge, "payload_too_large"},
	} {
		recorder := sendAPI(handler, test.method, test.path, test.body)
		if recorder.Code != test.status || apiErrorCode(t, recorder) != test.code {
			t.Fatalf("%s=%d %s", test.name, recorder.Code, recorder.Body)
		}
		if recorder.Header().Get("Cache-Control") != "no-store" || strings.Contains(recorder.Body.String(), authSecretMarker) {
			t.Fatalf("%s leaked or was cacheable: %s", test.name, recorder.Body)
		}
	}
	if adapter.callCount() != 0 || store.commits != 0 {
		t.Fatalf("malformed input reached the site=%d or storage=%d", adapter.callCount(), store.commits)
	}
}

func TestAPIAuthorizationClassifiesSiteAnswersAndReplays(t *testing.T) {
	store, adapter, handler := newAuthorizationFixture(t)
	session := beginAuthorization(t, handler, "request_begin_10", 1)
	if again := beginAuthorization(t, handler, "request_begin_10", 1); again != session {
		t.Fatalf("begin replay returned another session %q", again)
	}
	complete := APIPrefix + "/authorizations/" + session + "/complete"
	credential := authCredential(t, authSecretMarker)
	for _, test := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("%w: site said %s", ErrAuthRequired, authSecretMarker), http.StatusConflict, "auth_required"},
		{ErrPermissionDenied, http.StatusConflict, "auth_required"},
		{ErrManualRequired, http.StatusConflict, "manual_required"},
		{ErrCheckInDisabled, http.StatusConflict, "unsupported"},
		{RateLimited("30", time.Now()), http.StatusServiceUnavailable, "rate_limited"},
		{fmt.Errorf("dial https://relay.example?token=%s: %w", authSecretMarker, ErrNetwork), http.StatusServiceUnavailable, "site_unavailable"},
		{ErrIdentityMismatch, http.StatusConflict, "identity_mismatch"},
	} {
		adapter.mu.Lock()
		adapter.err = test.err
		adapter.mu.Unlock()
		recorder := sendAPI(handler, http.MethodPost, complete, completeAuthorizationBody("request_complete_20", credential, ""))
		if recorder.Code != test.status || apiErrorCode(t, recorder) != test.code || strings.Contains(recorder.Body.String(), authSecretMarker) {
			t.Fatalf("%v=%d %s", test.err, recorder.Code, recorder.Body)
		}
	}
	adapter.mu.Lock()
	adapter.err, adapter.identity = nil, SiteIdentity{RemoteUserID: "8"}
	adapter.mu.Unlock()
	if recorder := sendAPI(handler, http.MethodPost, complete, completeAuthorizationBody("request_complete_20", credential, `,"claimed_user_id":"7"`)); recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != "identity_mismatch" {
		t.Fatalf("site answered another user=%d %s", recorder.Code, recorder.Body)
	}
	store.mu.Lock()
	store.busy = true
	store.mu.Unlock()
	if recorder := sendAPI(handler, http.MethodPost, complete, completeAuthorizationBody("request_complete_20", credential, "")); recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != "account_busy" {
		t.Fatalf("busy account=%d %s", recorder.Code, recorder.Body)
	}
	store.mu.Lock()
	store.busy = false
	store.mu.Unlock()
	if store.commits != 0 {
		t.Fatalf("a refused completion stored %d sessions", store.commits)
	}
	adapter.mu.Lock()
	adapter.identity = SiteIdentity{RemoteUserID: "7"}
	adapter.mu.Unlock()
	body := completeAuthorizationBody("request_complete_20", credential, `,"expected_revision":1,"claimed_user_id":"7"`)
	first := sendAPI(handler, http.MethodPost, complete, body)
	var view AccountView
	if first.Code != http.StatusOK || json.Unmarshal(first.Body.Bytes(), &view) != nil || view.State != AccountStateConnected || view.RemoteUserID != "7" || view.Revision != 2 {
		t.Fatalf("complete=%d %s", first.Code, first.Body)
	}
	if strings.Contains(first.Body.String(), authSecretMarker) || strings.Contains(first.Body.String(), "credential") {
		t.Fatalf("receipt echoed the session: %s", first.Body)
	}
	calls := adapter.callCount()
	if replay := sendAPI(handler, http.MethodPost, complete, body); replay.Code != http.StatusOK || replay.Body.String() != first.Body.String() {
		t.Fatalf("replay=%d %s", replay.Code, replay.Body)
	}
	for _, test := range []struct{ name, path, body, code string }{
		{"new request on a completed session", complete, completeAuthorizationBody("request_complete_21", credential, ""), "authorization_completed"},
		{"same request, other content", complete, completeAuthorizationBody("request_complete_20", credential, ""), "request_id_reused"},
		{"begin id as a completion", complete, completeAuthorizationBody("request_begin_10", credential, ""), "request_id_reused"},
	} {
		if recorder := sendAPI(handler, http.MethodPost, test.path, test.body); recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != test.code {
			t.Fatalf("%s=%d %s", test.name, recorder.Code, recorder.Body)
		}
	}
	if recorder := sendAPI(handler, http.MethodPost, APIPrefix+"/authorizations", `{"request_id":"request_complete_20","account_id":"acct_auth","expected_revision":2}`); recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != "request_id_reused" {
		t.Fatalf("completion id as a begin=%d %s", recorder.Code, recorder.Body)
	}
	if adapter.callCount() != calls || store.commits != 1 {
		t.Fatalf("replays reached the site=%d or stored again=%d", adapter.callCount()-calls, store.commits)
	}

	// An identified account keeps its user: another claim is refused before
	// any request, and a re-login without a claim asks for the stored user.
	next := beginAuthorization(t, handler, "request_begin_11", 2)
	again := APIPrefix + "/authorizations/" + next + "/complete"
	if recorder := sendAPI(handler, http.MethodPost, again, completeAuthorizationBody("request_complete_22", credential, `,"claimed_user_id":"9"`)); recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != "identity_mismatch" || adapter.callCount() != calls {
		t.Fatalf("other claim=%d %s", recorder.Code, recorder.Body)
	}
	if recorder := sendAPI(handler, http.MethodPost, again, completeAuthorizationBody("request_complete_23", credential, "")); recorder.Code != http.StatusOK {
		t.Fatalf("re-login=%d %s", recorder.Code, recorder.Body)
	}
	adapter.mu.Lock()
	expected, invalidated := append([]string(nil), adapter.expected...), adapter.invalided
	adapter.mu.Unlock()
	if expected[len(expected)-1] != "7" || invalidated != 0 {
		t.Fatalf("re-login asked for %q; invalidated %d job clients", expected[len(expected)-1], invalidated)
	}
}

func TestAPIAuthorizationWithoutStoreIsUnavailable(t *testing.T) {
	_, _, handler := newAPIFixture(t, true)
	recorder := sendAPI(handler, http.MethodPost, APIPrefix+"/authorizations", `{"request_id":"request_begin_30","account_id":"acct_auth","expected_revision":1}`)
	if recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != "checkin_disabled" {
		t.Fatalf("begin without store=%d %s", recorder.Code, recorder.Body)
	}
	recorder = sendAPI(handler, http.MethodPost, APIPrefix+"/authorizations/auth_unknown_session/complete",
		completeAuthorizationBody("request_complete_30", authCredential(t, authSecretMarker), ""))
	if recorder.Code != http.StatusConflict || apiErrorCode(t, recorder) != "checkin_disabled" {
		t.Fatalf("complete without store=%d %s", recorder.Code, recorder.Body)
	}
}

func TestClassifyAuthorizationFailureDropsSiteText(t *testing.T) {
	for _, err := range []error{ErrNetworkTLS, ErrNetworkRedirect, ErrResponseTooLarge, ErrClientInvalidated, ErrClientCapacity, errors.New(authSecretMarker)} {
		if classified := classifyAuthorizationFailure(fmt.Errorf("wrapped %s: %w", authSecretMarker, err)); !errors.Is(classified, ErrSiteUnavailable) || strings.Contains(classified.Error(), authSecretMarker) {
			t.Fatalf("%v classified as %v", err, classified)
		}
	}
	if classified := classifyAuthorizationFailure(ErrCredentialUnavailable); !errors.Is(classified, ErrCredentialInvalid) {
		t.Fatalf("unusable envelope classified as %v", classified)
	}
	recorder := httptest.NewRecorder()
	writeAuthorizationError(recorder, ErrSiteUnavailable)
	var envelope apiErrorEnvelope
	if json.Unmarshal(recorder.Body.Bytes(), &envelope) != nil || !envelope.Error.Retryable || recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("site unavailable=%d %s", recorder.Code, recorder.Body)
	}
}
