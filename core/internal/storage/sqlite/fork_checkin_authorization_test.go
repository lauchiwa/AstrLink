package sqlite

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

// authSecretMarker is in every captured session these tests send. It must
// appear in no response, no extension row outside the sealed column, and no
// database file.
const authSecretMarker = "AUTHSECRET-MARKER"

// authSite is a legacy New API dashboard. Bearer sessions map to users; with
// strict set, /api/user/self refuses a New-Api-User header that differs from
// the session's user, as the legacy middleware does.
type authSite struct {
	server *httptest.Server
	users  map[string]string
	strict atomic.Bool
	self   atomic.Int32
}

func newAuthSite(t *testing.T) *authSite {
	t.Helper()
	site := &authSite{users: map[string]string{authSecretMarker + "-7": "7", authSecretMarker + "-8": "8"}}
	site.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/api/user/self" {
			http.NotFound(w, r)
			return
		}
		site.self.Add(1)
		user, ok := site.users[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")]
		if !ok || site.strict.Load() && r.Header.Get("New-Api-User") != user {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprintf(w, `{"success":false,"message":"rejected %s"}`, authSecretMarker)
			return
		}
		_, _ = fmt.Fprintf(w, `{"success":true,"data":{"id":%s}}`, user)
	}))
	t.Cleanup(site.server.Close)
	return site
}

// authClock parks the scheduler like parkedClock and lets a test move the
// authorization TTL forward.
type authClock struct {
	*parkedClock
	mu     sync.Mutex
	offset time.Duration
}

func (clock *authClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return time.Now().Add(clock.offset)
}

func (clock *authClock) advance(delay time.Duration) {
	clock.mu.Lock()
	clock.offset += delay
	clock.mu.Unlock()
}

type authFixture struct {
	store    *Store
	path     string
	site     *authSite
	settings string
	clock    *authClock
	handler  http.Handler
}

func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	ctx := context.Background()
	f := &authFixture{site: newAuthSite(t), settings: filepath.Join(t.TempDir(), "checkin.json"), path: filepath.Join(t.TempDir(), "authorization.db")}
	f.store = openTestStore(t, f.path)
	t.Cleanup(func() { _ = f.store.Close() })
	if err := f.store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	account := forkCheckinDraft("acct_auth", f.site.server.URL)
	account.TimeZone = "UTC"
	if _, err := f.store.CreateForkCheckinAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := forkcheckin.SaveSettings(f.settings, forkcheckin.Settings{Enabled: true}); err != nil {
		t.Fatal(err)
	}
	f.start(t)
	return f
}

// start runs one Core lifetime over the same store and switch file.
func (f *authFixture) start(t *testing.T) {
	t.Helper()
	f.clock = &authClock{parkedClock: newParkedClock()}
	facade, err := forkcheckin.NewFacade(forkcheckin.FacadeConfig{
		SettingsPath: f.settings, Factory: f.store.ForkCheckinModuleFactory(newAPIModuleBuilder), Clock: f.clock,
	})
	if err != nil {
		t.Fatal(err)
	}
	facade.Start(context.Background())
	<-f.clock.idle
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := facade.Stop(ctx); err != nil {
			t.Errorf("stop: %v", err)
		}
	})
	f.handler = forkcheckin.NewAPIHandler(facade)
}

func (f *authFixture) post(t *testing.T, path, body string) (int, map[string]any, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, forkcheckin.APIPrefix+path, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	f.handler.ServeHTTP(recorder, request)
	raw := recorder.Body.String()
	if strings.Contains(raw, authSecretMarker) || strings.Contains(raw, "credential\":") {
		t.Fatalf("POST %s leaked a captured session: %s", path, raw)
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("POST %s: %v %s", path, err, raw)
	}
	return recorder.Code, decoded, raw
}

func authErrorCode(body map[string]any) any {
	failure, _ := body["error"].(map[string]any)
	return failure["code"]
}

func (f *authFixture) begin(t *testing.T, requestID string, revision int) string {
	t.Helper()
	code, body, raw := f.post(t, "/authorizations", fmt.Sprintf(`{"request_id":%q,"account_id":"acct_auth","expected_revision":%d}`, requestID, revision))
	if code != http.StatusCreated || body["login_url"] != f.site.server.URL || body["account_id"] != "acct_auth" {
		t.Fatalf("begin=%d %s", code, raw)
	}
	return body["session_id"].(string)
}

func (f *authFixture) complete(t *testing.T, session, requestID, bearer, extra string) (int, map[string]any, string) {
	t.Helper()
	return f.post(t, "/authorizations/"+session+"/complete",
		fmt.Sprintf(`{"request_id":%q,"credential":%q%s}`, requestID, authEnvelope(t, bearer), extra))
}

func authEnvelope(t *testing.T, bearer string) string {
	t.Helper()
	encoded, err := forkcheckin.EncodeNetworkCredential(forkcheckin.NetworkCredential{Version: 1, Bearer: bearer})
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(encoded)
}

type authState struct {
	revision int64
	state    forkcheckin.AccountState
	remote   string
	sealed   string
	receipts int
}

func (f *authFixture) state(t *testing.T) authState {
	t.Helper()
	account, err := f.store.GetForkCheckinAccount(context.Background(), "acct_auth")
	if err != nil {
		t.Fatal(err)
	}
	state := authState{revision: account.Revision, state: account.State, remote: account.RemoteUserID}
	var sealed []byte
	if err := f.store.db.QueryRow(`SELECT sealed_value FROM fork_checkin_credentials WHERE account_id = 'acct_auth'`).Scan(&sealed); err == nil {
		state.sealed = hex.EncodeToString(sealed)
	}
	if err := f.store.db.QueryRow(`SELECT COUNT(*) FROM fork_checkin_request_receipts WHERE route = ?`, forkcheckin.WriteRouteAuthorizationComplete).Scan(&state.receipts); err != nil {
		t.Fatal(err)
	}
	return state
}

// assertNoPlaintextSession scans every extension row outside the sealed
// column, and the database files themselves, for the captured session.
func (f *authFixture) assertNoPlaintextSession(t *testing.T) {
	t.Helper()
	rows, err := f.store.db.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name LIKE 'fork_checkin%'`)
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
	for _, table := range tables {
		data, err := f.store.db.Query(`SELECT * FROM "` + table + `"`)
		if err != nil {
			t.Fatal(err)
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
			for index, value := range values {
				text := fmt.Sprintf("%s", value)
				if strings.Contains(text, authSecretMarker) || strings.Contains(text, base64.StdEncoding.EncodeToString([]byte(authSecretMarker))) {
					t.Fatalf("%s.%s holds a plaintext session", table, columns[index])
				}
			}
		}
		data.Close()
	}
	matches, _ := filepath.Glob(f.path + "*")
	if len(matches) == 0 {
		t.Fatal("no database files to scan")
	}
	for _, path := range matches {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(content), authSecretMarker) {
			t.Fatalf("%s contains a plaintext session", filepath.Base(path))
		}
	}
}

// A captured session is stored only once the account's own site confirms
// whose it is. The receipt replays a lost response, also after a restart,
// and an unfinished session does not survive one.
func TestForkCheckinAuthorizationStoresVerifiedSessionAndReplays(t *testing.T) {
	f := newAuthFixture(t)
	session := f.begin(t, "request_auth_begin_1", 1)
	if again := f.begin(t, "request_auth_begin_1", 1); again != session {
		t.Fatalf("begin replay made another session %q", again)
	}
	// The legacy dialect cannot ask a draft's site without a claimed user.
	if code, body, _ := f.complete(t, session, "request_auth_done_0", authSecretMarker+"-7", ""); code != http.StatusConflict || authErrorCode(body) != "manual_required" || f.site.self.Load() != 0 {
		t.Fatalf("unclaimed legacy draft=%d %v hits=%d", code, body, f.site.self.Load())
	}
	before := f.state(t)
	if code, body, _ := f.complete(t, session, "request_auth_done_0", authSecretMarker+"-unknown", `,"claimed_user_id":"7"`); code != http.StatusConflict || authErrorCode(body) != "auth_required" {
		t.Fatalf("rejected session=%d %v", code, body)
	}
	if after := f.state(t); after != before || after.sealed != "" || after.receipts != 0 {
		t.Fatalf("a rejected session wrote state: %+v", after)
	}
	f.site.strict.Store(true)
	done := fmt.Sprintf(`{"request_id":"request_auth_done_1","credential":%q,"expected_revision":1,"claimed_user_id":"7"}`, authEnvelope(t, authSecretMarker+"-7"))
	code, body, first := f.post(t, "/authorizations/"+session+"/complete", done)
	if code != http.StatusOK || body["state"] != "connected" || body["remote_user_id"] != "7" || body["revision"] != float64(2) {
		t.Fatalf("complete=%d %s", code, first)
	}
	stored := f.state(t)
	if stored.sealed == "" || stored.receipts != 1 {
		t.Fatalf("complete did not store the session and receipt: %+v", stored)
	}
	plaintext, err := f.store.ForkCheckinSessions().Get(context.Background(), "acct_auth")
	if err != nil {
		t.Fatal(err)
	}
	if want, _ := base64.StdEncoding.DecodeString(authEnvelope(t, authSecretMarker+"-7")); string(plaintext) != string(want) {
		t.Fatal("the stored session is not the verified one")
	}
	hits := f.site.self.Load()
	if code, _, replay := f.post(t, "/authorizations/"+session+"/complete", done); code != http.StatusOK || replay != first {
		t.Fatalf("lost-response retry=%d %s", code, replay)
	}
	if code, body, _ := f.complete(t, session, "request_auth_done_2", authSecretMarker+"-7", `,"claimed_user_id":"7"`); code != http.StatusConflict || authErrorCode(body) != "authorization_completed" {
		t.Fatalf("second completion=%d %v", code, body)
	}
	if code, body, _ := f.complete(t, session, "request_auth_done_1", authSecretMarker+"-7", `,"claimed_user_id":"8"`); code != http.StatusConflict || authErrorCode(body) != "request_id_reused" {
		t.Fatalf("reused request id=%d %v", code, body)
	}
	if code, body, _ := f.complete(t, session, "request_auth_begin_1", authSecretMarker+"-7", `,"claimed_user_id":"7"`); code != http.StatusConflict || authErrorCode(body) != "request_id_reused" {
		t.Fatalf("begin id as a completion=%d %v", code, body)
	}
	if f.site.self.Load() != hits || f.state(t) != stored {
		t.Fatalf("replays reached the site (%d) or rewrote state", f.site.self.Load()-hits)
	}

	// A restart loses unfinished sessions; stored receipts still answer.
	pending := f.begin(t, "request_auth_begin_2", 2)
	f.start(t)
	if code, _, replay := f.post(t, "/authorizations/"+session+"/complete", done); code != http.StatusOK || replay != first {
		t.Fatalf("retry after restart=%d %s", code, replay)
	}
	if code, body, _ := f.complete(t, pending, "request_auth_done_3", authSecretMarker+"-7", ""); code != http.StatusNotFound || authErrorCode(body) != "authorization_not_found" {
		t.Fatalf("session after restart=%d %v", code, body)
	}
	if code, body, _ := f.post(t, "/authorizations", `{"request_id":"request_auth_done_1","account_id":"acct_auth","expected_revision":2}`); code != http.StatusConflict || authErrorCode(body) != "request_id_reused" {
		t.Fatalf("completion id as a begin after restart=%d %v", code, body)
	}
	if f.site.self.Load() != hits || f.state(t) != stored {
		t.Fatal("a restart replay reached the site or rewrote state")
	}
	f.assertNoPlaintextSession(t)
}

// Nothing is stored for a changed account, another user, a busy account or
// an expired session; an identified account can only log in as its user.
func TestForkCheckinAuthorizationRefusesChangedAccountsAndOtherUsers(t *testing.T) {
	ctx := context.Background()
	f := newAuthFixture(t)
	if code, body, _ := f.post(t, "/authorizations", `{"request_id":"request_change_0","account_id":"acct_auth","expected_revision":2}`); code != http.StatusPreconditionFailed || authErrorCode(body) != "revision_conflict" {
		t.Fatalf("stale begin=%d %v", code, body)
	}
	stale := f.begin(t, "request_change_1", 1)
	zone := "Asia/Shanghai"
	if _, err := (forkCheckinAccountWriter{store: f.store}).UpdateAccount(ctx, "acct_auth", forkcheckin.AccountUpdateRequest{
		RequestID: "request_change_edit", ExpectedRevision: 1, TimeZone: &zone,
	}); err != nil {
		t.Fatal(err)
	}
	if code, body, _ := f.complete(t, stale, "request_change_2", authSecretMarker+"-7", `,"claimed_user_id":"7"`); code != http.StatusPreconditionFailed || authErrorCode(body) != "revision_conflict" {
		t.Fatalf("edited account=%d %v", code, body)
	}
	session := f.begin(t, "request_change_3", 2)
	if code, body, _ := f.complete(t, session, "request_change_4", authSecretMarker+"-7", `,"expected_revision":1,"claimed_user_id":"7"`); code != http.StatusPreconditionFailed || authErrorCode(body) != "revision_conflict" {
		t.Fatalf("wrong expected_revision=%d %v", code, body)
	}
	if f.site.self.Load() != 0 {
		t.Fatalf("a changed account reached the site %d times", f.site.self.Load())
	}
	// The site answers for the session, not for the window's claim.
	if code, body, _ := f.complete(t, session, "request_change_5", authSecretMarker+"-8", `,"claimed_user_id":"7"`); code != http.StatusConflict || authErrorCode(body) != "identity_mismatch" {
		t.Fatalf("other user=%d %v", code, body)
	}
	if state := f.state(t); state.revision != 2 || state.sealed != "" || state.receipts != 0 {
		t.Fatalf("a refused completion wrote state: %+v", state)
	}
	if code, _, raw := f.complete(t, session, "request_change_6", authSecretMarker+"-7", `,"claimed_user_id":"7"`); code != http.StatusOK {
		t.Fatalf("connect=%d %s", code, raw)
	}
	connected := f.state(t)
	relogin := f.begin(t, "request_change_7", 3)
	hits := f.site.self.Load()
	if code, body, _ := f.complete(t, relogin, "request_change_8", authSecretMarker+"-8", `,"claimed_user_id":"8"`); code != http.StatusConflict || authErrorCode(body) != "identity_mismatch" || f.site.self.Load() != hits {
		t.Fatalf("identified account claimed another user=%d %v", code, body)
	}
	if code, body, _ := f.complete(t, relogin, "request_change_9", authSecretMarker+"-8", ""); code != http.StatusConflict || authErrorCode(body) != "identity_mismatch" {
		t.Fatalf("identified account logged in as another user=%d %v", code, body)
	}
	if f.state(t) != connected {
		t.Fatal("another user's session replaced the stored one")
	}
	f.site.strict.Store(true)
	if code, body, raw := f.complete(t, relogin, "request_change_10", authSecretMarker+"-7", ""); code != http.StatusOK || body["remote_user_id"] != "7" || body["revision"] != float64(4) {
		t.Fatalf("re-login as the stored user=%d %s", code, raw)
	}
	if renewed := f.state(t); renewed.sealed == connected.sealed || renewed.receipts != 2 {
		t.Fatalf("re-login did not replace the session: %+v", renewed)
	}

	// A running check-in keeps its network client.
	job := forkCheckinJobDraft("acct_auth", "request_change_job", 'z')
	job.ExpectedRevision = 4
	created := createForkCheckinJob(t, f.store, job)
	if _, err := f.store.ClaimForkCheckinJobID(ctx, created.Job.ID, time.Minute); err != nil {
		t.Fatal(err)
	}
	busy := f.begin(t, "request_change_11", 4)
	hits = f.site.self.Load()
	if code, body, _ := f.complete(t, busy, "request_change_12", authSecretMarker+"-7", ""); code != http.StatusConflict || authErrorCode(body) != "account_busy" || f.site.self.Load() != hits {
		t.Fatalf("busy account=%d %v", code, body)
	}

	// An unfinished session expires on its own.
	f.clock.advance(forkcheckin.AuthorizationTTL + time.Second)
	if code, body, _ := f.complete(t, busy, "request_change_13", authSecretMarker+"-7", ""); code != http.StatusNotFound || authErrorCode(body) != "authorization_not_found" {
		t.Fatalf("expired session=%d %v", code, body)
	}
	if f.site.self.Load() != hits {
		t.Fatal("an expired session reached the site")
	}
	f.assertNoPlaintextSession(t)
}

// A failure after the site confirmed the session rolls the whole commit back:
// no account change, no sealed session and no receipt. The same request then
// completes normally.
func TestForkCheckinAuthorizationRollsBackPartialCommit(t *testing.T) {
	f := newAuthFixture(t)
	f.site.strict.Store(true)
	session := f.begin(t, "request_rollback_0", 1)
	if _, err := f.store.db.Exec(`CREATE TRIGGER fork_checkin_test_receipt_failure BEFORE INSERT ON fork_checkin_request_receipts
WHEN NEW.route = 'authorizations.complete' BEGIN SELECT RAISE(ABORT, 'injected receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if code, body, raw := f.complete(t, session, "request_rollback_1", authSecretMarker+"-7", `,"claimed_user_id":"7"`); code != http.StatusInternalServerError || authErrorCode(body) != "checkin_storage_failed" || strings.Contains(raw, "injected") {
		t.Fatalf("failed commit=%d %s", code, raw)
	}
	if state := f.state(t); state.revision != 1 || state.state != forkcheckin.AccountStateDraft || state.remote != "" || state.sealed != "" || state.receipts != 0 {
		t.Fatalf("a failed commit left state behind: %+v", state)
	}
	if _, err := f.store.db.Exec(`DROP TRIGGER fork_checkin_test_receipt_failure`); err != nil {
		t.Fatal(err)
	}
	if code, body, raw := f.complete(t, session, "request_rollback_1", authSecretMarker+"-7", `,"claimed_user_id":"7"`); code != http.StatusOK || body["state"] != "connected" {
		t.Fatalf("retry after rollback=%d %s", code, raw)
	}
	if state := f.state(t); state.revision != 2 || state.sealed == "" || state.receipts != 1 {
		t.Fatalf("retry did not commit: %+v", state)
	}
	f.assertNoPlaintextSession(t)
}
