package controlapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
)

func newCheckinMountHandler(t *testing.T, extension http.Handler) *Handler {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "astrlink.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	handler, err := NewWithDependencies(contract.DefaultVersionResponse("0.1.0-test", "abc1234"), Dependencies{
		ServiceStore: store, ControlToken: testControlToken, ObserverToken: testObserverToken,
		CheckinExtension: extension,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler
}

func TestCheckinExtensionMountIsOperatorOnly(t *testing.T) {
	var reached atomic.Int32
	extension := http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		writer.WriteHeader(http.StatusOK)
	})
	handler := newCheckinMountHandler(t, extension)
	for _, path := range []string{
		CheckinExtensionPath, CheckinExtensionPath + "/status", CheckinExtensionPath + "/accounts",
		CheckinExtensionPath + "/jobs", CheckinExtensionPath + "/jobs/job_one/cancel",
		CheckinExtensionPath + "/authorizations", CheckinExtensionPath + "/authorizations/auth_one_session/complete",
	} {
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodPost, http.MethodPatch, http.MethodDelete} {
			unauthenticated := httptest.NewRecorder()
			handler.ServeHTTP(unauthenticated, httptest.NewRequest(method, path, nil))
			if unauthenticated.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s unauthenticated=%d", method, path, unauthenticated.Code)
			}
			for _, caller := range roleCallers {
				request := httptest.NewRequest(method, path, nil)
				caller.set(request)
				recorder := httptest.NewRecorder()
				before := reached.Load()
				handler.ServeHTTP(recorder, request)
				want := http.StatusForbidden
				if caller.role == RoleOperator {
					want = http.StatusOK
				}
				if recorder.Code != want || (want == http.StatusOK) != (reached.Load() == before+1) {
					t.Fatalf("%s %s as %s=%d", method, path, caller.name, recorder.Code)
				}
				if recorder.Header().Get("Cache-Control") != "no-store" {
					t.Fatalf("%s %s missing no-store", method, path)
				}
			}
		}
	}
}

// A Core built or configured without the extension answers its namespace
// with the ordinary 404, so the desktop reports "unavailable", and the
// bootstrap surface is identical with or without it.
func TestCheckinExtensionAbsentKeepsBootstrapUnchanged(t *testing.T) {
	without := newCheckinMountHandler(t, nil)
	with := newCheckinMountHandler(t, http.NotFoundHandler())
	request := httptest.NewRequest(http.MethodGet, CheckinExtensionPath+"/status", nil)
	request.Header.Set("Authorization", "Bearer "+testControlToken)
	recorder := httptest.NewRecorder()
	without.ServeHTTP(recorder, request)
	var envelope errorEnvelope
	if recorder.Code != http.StatusNotFound || json.Unmarshal(recorder.Body.Bytes(), &envelope) != nil || envelope.Error.Code != "not_found" {
		t.Fatalf("absent extension=%d %s", recorder.Code, recorder.Body)
	}
	for _, path := range []string{HealthPath, VersionPath, CapabilitiesPath} {
		a, b := httptest.NewRecorder(), httptest.NewRecorder()
		without.ServeHTTP(a, httptest.NewRequest(http.MethodGet, path, nil))
		with.ServeHTTP(b, httptest.NewRequest(http.MethodGet, path, nil))
		if a.Code != http.StatusOK || a.Code != b.Code || a.Body.String() != b.Body.String() {
			t.Fatalf("%s changed with the extension: %d %s vs %d %s", path, a.Code, a.Body, b.Code, b.Body)
		}
	}
	// The required-dependency contract is unchanged: the extension stays
	// optional and New still builds a bootstrap-only handler.
	if New(contract.DefaultVersionResponse("0.1.0-test", "abc1234")) == nil {
		t.Fatal("bootstrap handler unavailable")
	}
}
