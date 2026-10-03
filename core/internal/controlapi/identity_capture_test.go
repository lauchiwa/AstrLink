package controlapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/identitycapture"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
	"github.com/QuantumNous/astrlink/core/internal/subscription"
)

// newCaptureHandler mirrors newServiceHandler but wires the capture registry
// the inference plane shares, so the control surface is exercised against the
// same in-memory consent state rather than a stub.
func newCaptureHandler(t *testing.T, ids ...contract.ServiceID) (*identitycapture.Registry, *Handler) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), t.TempDir()+"/astrlink.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	preferred, fallback := controlAPITestPortPair(t)
	manager, err := subscription.NewManager(
		subscription.StorageAccountStore{Store: store},
		accountauth.NewMemoryCredentialStore(),
		accountauth.OAuthConfig{
			ClientID: "astrlink_registered_test_client", Issuer: "https://auth.example.test",
			PreferredPort: preferred, FallbackPort: fallback,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	capture, err := identitycapture.New(store)
	if err != nil {
		t.Fatal(err)
	}
	nextID := 0
	handler, err := NewWithDependencies(
		contract.DefaultVersionResponse("0.1.0-test", "abc1234"),
		Dependencies{
			ServiceStore: store, Subscriptions: manager, IdentityCapture: capture,
			ControlToken: testControlToken, ObserverToken: testObserverToken,
			NewServiceID: func() (contract.ServiceID, error) {
				id := ids[nextID]
				nextID++
				return id, nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return capture, handler
}

func TestIdentityCaptureArmingIsExplicitBoundedAndScoped(t *testing.T) {
	capture, handler := newCaptureHandler(t, "service_a", "service_b", "service_sub")
	service := createServiceForTest(t, handler, profileServiceBody)
	other := createServiceForTest(t, handler, profileServiceBody)
	subscriptionService := createServiceForTest(t, handler, `{"name":"subscription","kind":"codex_subscription"}`)
	path := ServicesPath + "/" + string(service.ID) + "/identity-capture"

	idle := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
	if idle.Code != http.StatusOK {
		t.Fatalf("status: %d %s", idle.Code, idle.Body.String())
	}
	// Each response is decoded into a fresh value: omitted fields such as
	// expires_at must read as cleared, not inherit an earlier window's value.
	status := decodeCaptureStatus(t, idle)
	if status.Armed || status.ServiceID != service.ID || status.Client != "" || status.ExpiresAt != nil {
		t.Fatalf("a service starts armed: %+v", status)
	}
	if _, armed := capture.Armed(service.ID); armed {
		t.Fatal("reading status armed the window")
	}

	armed := serviceRequestForTest(t, handler, http.MethodPut, path, "application/json", `{"client":"codex_cli","ttl_seconds":60}`, "")
	if armed.Code != http.StatusOK {
		t.Fatalf("arm: %d %s", armed.Code, armed.Body.String())
	}
	status = decodeCaptureStatus(t, armed)
	if !status.Armed || status.Client != contract.IdentityClientCodexCLI || status.ArmedAt == nil || status.ExpiresAt == nil {
		t.Fatalf("arm did not open a bounded window: %+v", status)
	}
	if window := status.ExpiresAt.Sub(*status.ArmedAt); window != time.Minute {
		t.Fatalf("window = %s", window)
	}
	if client, open := capture.Armed(service.ID); !open || client != contract.IdentityClientCodexCLI {
		t.Fatal("control arming did not reach the inference registry")
	}
	// Arming one service must not arm any other.
	if _, open := capture.Armed(other.ID); open {
		t.Fatal("arming leaked to another service")
	}

	// An oversized or omitted TTL is clamped to the default, never unbounded.
	for _, body := range []string{`{"client":"claude_code"}`, `{"client":"claude_code","ttl_seconds":86400}`} {
		response := serviceRequestForTest(t, handler, http.MethodPut, path, "application/json", body, "")
		if response.Code != http.StatusOK {
			t.Fatalf("arm %s: %d %s", body, response.Code, response.Body.String())
		}
		status = decodeCaptureStatus(t, response)
		if status.ExpiresAt == nil || status.ArmedAt == nil || status.ExpiresAt.Sub(*status.ArmedAt) != identitycapture.DefaultWindow {
			t.Fatalf("window was not clamped: %+v", status)
		}
	}

	disarmed := serviceRequestForTest(t, handler, http.MethodDelete, path, "", "", "")
	if disarmed.Code != http.StatusOK {
		t.Fatalf("disarm: %d %s", disarmed.Code, disarmed.Body.String())
	}
	status = decodeCaptureStatus(t, disarmed)
	if status.Armed || status.ExpiresAt != nil {
		t.Fatalf("disarm left the window open: %+v", status)
	}
	if _, open := capture.Armed(service.ID); open {
		t.Fatal("disarm did not reach the inference registry")
	}

	for _, test := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"unsupported client", http.MethodPut, path, `{"client":"gemini_cli"}`, http.StatusUnprocessableEntity},
		{"missing client", http.MethodPut, path, `{}`, http.StatusUnprocessableEntity},
		{"negative ttl", http.MethodPut, path, `{"client":"codex_cli","ttl_seconds":-1}`, http.StatusUnprocessableEntity},
		{"raw fingerprint", http.MethodPut, path, `{"client":"codex_cli","fingerprint":{"user_agent":"codex-tui/1.0.0"}}`, http.StatusBadRequest},
		{"header override", http.MethodPut, path, `{"client":"codex_cli","headers":{"Originator":"codex_exec"}}`, http.StatusBadRequest},
		{"two documents", http.MethodPut, path, `{"client":"codex_cli"} {}`, http.StatusBadRequest},
		{"patch", http.MethodPatch, path, `{"client":"codex_cli"}`, http.StatusMethodNotAllowed},
		{"post", http.MethodPost, path, `{"client":"codex_cli"}`, http.StatusMethodNotAllowed},
		{"query", http.MethodGet, path + "?client=codex_cli", "", http.StatusBadRequest},
		{"subscription service", http.MethodPut, ServicesPath + "/" + string(subscriptionService.ID) + "/identity-capture", `{"client":"codex_cli"}`, http.StatusUnprocessableEntity},
		{"unknown service", http.MethodPut, ServicesPath + "/service_missing/identity-capture", `{"client":"codex_cli"}`, http.StatusNotFound},
		{"nested path", http.MethodGet, path + "/extra", "", http.StatusNotFound},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := serviceRequestForTest(t, handler, test.method, test.path, "application/json", test.body, "")
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
			if _, open := capture.Armed(service.ID); open {
				t.Fatal("a rejected request armed capture")
			}
		})
	}

	// Arming requires the operator role and a local control token.
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		request := httptest.NewRequest(method, path, strings.NewReader(`{"client":"codex_cli"}`))
		request.Header.Set("Authorization", "Bearer "+testObserverToken)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden {
			t.Fatalf("observer %s: %d", method, response.Code)
		}
		request.Header.Del("Authorization")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s: %d", method, response.Code)
		}
		if _, open := capture.Armed(service.ID); open {
			t.Fatal("an unauthorized request armed capture")
		}
	}
}

// A build without capture wiring must report unavailability instead of
// pretending a window was armed.
func TestIdentityCaptureReportsUnavailableWithoutRegistry(t *testing.T) {
	_, handler := newServiceHandler(t, "service_a")
	service := createServiceForTest(t, handler, profileServiceBody)
	path := ServicesPath + "/" + string(service.ID) + "/identity-capture"
	for _, test := range []struct{ method, body string }{
		{http.MethodGet, ""}, {http.MethodPut, `{"client":"codex_cli"}`}, {http.MethodDelete, ""},
	} {
		response := serviceRequestForTest(t, handler, test.method, path, "application/json", test.body, "")
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s = %d %s", test.method, response.Code, response.Body.String())
		}
	}
}

// decodeCaptureStatus decodes into a fresh value so an omitted field reads as
// cleared instead of inheriting an earlier response's value.
func decodeCaptureStatus(t *testing.T, response *httptest.ResponseRecorder) contract.IdentityCaptureStatus {
	t.Helper()
	var status contract.IdentityCaptureStatus
	decode(t, response, &status)
	return status
}
