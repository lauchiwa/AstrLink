package ingress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/identitycapture"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// memoryProfiles records what capture publishes without a database, so the test
// asserts on the candidate the inference path produced, not on storage details.
type memoryProfiles struct {
	mu       sync.Mutex
	created  []contract.IdentityProfile
	failWith error
}

func (profiles *memoryProfiles) CreateIdentityProfile(_ context.Context, candidate contract.IdentityProfile) (storage.IdentityProfileRecord, error) {
	profiles.mu.Lock()
	defer profiles.mu.Unlock()
	if profiles.failWith != nil {
		return storage.IdentityProfileRecord{}, profiles.failWith
	}
	candidate.CreatedAt = candidate.ObservedAt.Add(1)
	if err := candidate.Validate(); err != nil {
		return storage.IdentityProfileRecord{}, err
	}
	profiles.created = append(profiles.created, candidate.Clone())
	return storage.IdentityProfileRecord{Profile: candidate, ETag: "etag"}, nil
}

func (profiles *memoryProfiles) snapshot() []contract.IdentityProfile {
	profiles.mu.Lock()
	defer profiles.mu.Unlock()
	return append([]contract.IdentityProfile(nil), profiles.created...)
}

func captureService(baseURL string) contract.Service {
	return contract.Service{
		ID: "service_relay", Name: "Relay", Kind: contract.ServiceKindOpenAICompatible, Enabled: true,
		Models:       []string{"gpt-6-luna"},
		Capabilities: []contract.Capability{{Protocol: contract.ProtocolOpenAIResponses, Mode: contract.CapabilityModeNative, Streaming: true}},
		HTTP: &contract.HTTPConnection{
			BaseURL: baseURL + "/v1", Auth: contract.ServiceAuth{Scheme: contract.AuthSchemeBearer},
			CredentialRef: "local://service/service_relay",
		},
	}
}

func newCaptureHarness(t *testing.T, profiles identitycapture.ProfileCreator) (*identitycapture.Registry, *capturedUpstream, http.Handler) {
	t.Helper()
	upstream := newCapturedUpstream(t, http.StatusOK, "application/json",
		`{"id":"resp_1","object":"response","model":"gpt-6-luna","status":"completed","output":[]}`)
	capture, err := identitycapture.New(profiles)
	if err != nil {
		t.Fatal(err)
	}
	service := captureService(upstream.url)
	if err := service.Validate(); err != nil {
		t.Fatal(err)
	}
	handler := NewWithDependencies(Dependencies{
		Resolver: candidateResolver{candidates: []endpoint.Resolved{
			{Service: service, BaseURL: service.HTTP.BaseURL, UpstreamProtocol: contract.ProtocolOpenAIResponses},
		}},
		Authorizer:      endpoint.NewServiceAuthorizer(codingPlanCredentials{}, nil),
		IdentityCapture: capture,
	})
	return capture, upstream, handler
}

const captureRequestBodyJSON = `{"model":"gpt-6-luna","input":"private prompt content","store":true}`

func serveCapture(t *testing.T, handler http.Handler, header http.Header) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(captureRequestBodyJSON))
	for name, values := range header {
		request.Header[name] = values
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer local-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func codexCaptureHeaders() http.Header {
	return http.Header{
		"User-Agent": {"codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal"},
		"Originator": {"codex-tui"},
		"Session-Id": {"0f8c1d3e-3a4b-4c5d-8e9f-0a1b2c3d4e5f"},
	}
}

// An unarmed service must never publish a candidate, and arming must not change
// a single forwarded byte: capture is an observation, not a rewrite.
func TestIdentityCaptureRequiresArmingAndNeverAltersForwarding(t *testing.T) {
	profiles := &memoryProfiles{}
	capture, upstream, handler := newCaptureHarness(t, profiles)

	if response := serveCapture(t, handler, codexCaptureHeaders()); response.Code != http.StatusOK {
		t.Fatalf("unarmed request = %d %s", response.Code, response.Body.String())
	}
	unarmedHeader, unarmedBody := upstream.request(t)
	if len(profiles.snapshot()) != 0 {
		t.Fatal("an unarmed service published a candidate")
	}

	if _, err := capture.Arm("service_relay", contract.IdentityClientCodexCLI, 0); err != nil {
		t.Fatal(err)
	}
	if response := serveCapture(t, handler, codexCaptureHeaders()); response.Code != http.StatusOK {
		t.Fatalf("armed request = %d %s", response.Code, response.Body.String())
	}
	armedHeader, armedBody := upstream.request(t)
	if string(unarmedBody) != captureRequestBodyJSON || string(armedBody) != captureRequestBodyJSON {
		t.Fatal("capture changed the forwarded body")
	}
	// Compare the full outbound envelope: capture must add, remove and reorder
	// nothing, including the credentials the gateway owns.
	if len(armedHeader) != len(unarmedHeader) {
		t.Fatalf("armed header set differs: %v vs %v", armedHeader, unarmedHeader)
	}
	for name, values := range unarmedHeader {
		if strings.Join(armedHeader[name], ",") != strings.Join(values, ",") {
			t.Fatalf("armed request changed %s: %v vs %v", name, armedHeader[name], values)
		}
	}

	published := profiles.snapshot()
	if len(published) != 1 {
		t.Fatalf("published %d candidates", len(published))
	}
	candidate := published[0]
	if candidate.ServiceID != "service_relay" || candidate.Client != contract.IdentityClientCodexCLI ||
		candidate.Source != contract.IdentityProfileRequestCapture || candidate.ObservedAt == nil {
		t.Fatalf("candidate metadata = %+v", candidate)
	}
	if candidate.ConfirmedAt != nil {
		t.Fatal("capture published a confirmed, immediately usable profile")
	}
	if candidate.Fingerprint.UserAgent != codexCaptureHeaders().Get("User-Agent") ||
		candidate.Fingerprint.Headers["Originator"] != "codex-tui" {
		t.Fatalf("candidate did not record the observed client: %+v", candidate.Fingerprint)
	}
	// The window closes itself after publishing one candidate.
	if _, armed := capture.Armed("service_relay"); armed {
		t.Fatal("window stayed open after publishing")
	}
	if response := serveCapture(t, handler, codexCaptureHeaders()); response.Code != http.StatusOK {
		t.Fatal("request after capture failed")
	}
	if len(profiles.snapshot()) != 1 {
		t.Fatal("a closed window kept sampling requests")
	}
}

// A candidate must contain only reusable identity fields: no session or request
// identifiers, no prompt content, and no gateway credentials.
func TestCapturedCandidateExcludesSessionCredentialAndBodyContent(t *testing.T) {
	profiles := &memoryProfiles{}
	capture, _, handler := newCaptureHarness(t, profiles)
	if _, err := capture.Arm("service_relay", contract.IdentityClientCodexCLI, 0); err != nil {
		t.Fatal(err)
	}
	header := codexCaptureHeaders()
	header.Set("Cookie", "secret-cookie-value")
	header.Set("X-Request-Id", "request-trace-value")
	if response := serveCapture(t, handler, header); response.Code != http.StatusOK {
		t.Fatalf("request = %d %s", response.Code, response.Body.String())
	}
	published := profiles.snapshot()
	if len(published) != 1 {
		t.Fatalf("published %d candidates", len(published))
	}
	fingerprint := published[0].Fingerprint
	if len(fingerprint.Headers) != 1 {
		t.Fatalf("candidate kept extra headers: %v", fingerprint.Headers)
	}
	for _, excluded := range []string{
		"secret-cookie-value", "request-trace-value", "local-secret",
		"0f8c1d3e-3a4b-4c5d-8e9f-0a1b2c3d4e5f", "private prompt content",
	} {
		if strings.Contains(fingerprint.UserAgent, excluded) {
			t.Fatalf("user agent retained %s", excluded)
		}
		for name, value := range fingerprint.Headers {
			if strings.Contains(value, excluded) || strings.Contains(name, excluded) {
				t.Fatalf("candidate retained %s", excluded)
			}
		}
	}
}

// An unrecognized client must not publish a guessed snapshot, and a storage
// failure must not fail the user's request.
func TestIdentityCaptureRejectsUnrecognizedClientsAndSurvivesStorageFailure(t *testing.T) {
	profiles := &memoryProfiles{}
	capture, _, handler := newCaptureHarness(t, profiles)
	if _, err := capture.Arm("service_relay", contract.IdentityClientClaudeCode, 0); err != nil {
		t.Fatal(err)
	}
	// Codex headers while Claude Code is selected: a mismatch, not a candidate.
	if response := serveCapture(t, handler, codexCaptureHeaders()); response.Code != http.StatusOK {
		t.Fatalf("mismatched client request = %d %s", response.Code, response.Body.String())
	}
	if len(profiles.snapshot()) != 0 {
		t.Fatal("a mismatched client published a candidate")
	}
	if status := capture.Status("service_relay"); !status.Armed || status.Rejected != 1 {
		t.Fatalf("rejection was not reported: %+v", status)
	}

	profiles.failWith = io.ErrUnexpectedEOF
	if _, err := capture.Arm("service_relay", contract.IdentityClientCodexCLI, 0); err != nil {
		t.Fatal(err)
	}
	// A failed write must leave the request successful and the window open.
	if response := serveCapture(t, handler, codexCaptureHeaders()); response.Code != http.StatusOK {
		t.Fatalf("storage failure changed the response: %d %s", response.Code, response.Body.String())
	}
	if status := capture.Status("service_relay"); !status.Armed || status.CapturedProfile != "" {
		t.Fatalf("failed write published a candidate: %+v", status)
	}
}
