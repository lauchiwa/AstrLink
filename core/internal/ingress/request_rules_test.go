package ingress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// The operator's migrated configuration: two identity headers for three models,
// with no body rewriting.
const (
	ruleOriginator = "codex_exec"
	ruleUserAgent  = "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"
)

// fixedProfiles serves one confirmed snapshot, so forwarding is tested without
// a database. A missing id behaves like the store's not-found.
type fixedProfiles struct {
	mu       sync.Mutex
	profiles map[contract.IdentityProfileID]contract.IdentityProfile
	reads    int
}

func (reader *fixedProfiles) GetIdentityProfile(
	_ context.Context, serviceID contract.ServiceID, id contract.IdentityProfileID,
) (storage.IdentityProfileRecord, error) {
	reader.mu.Lock()
	defer reader.mu.Unlock()
	reader.reads++
	profile, ok := reader.profiles[id]
	if !ok || profile.ServiceID != serviceID {
		return storage.IdentityProfileRecord{}, storage.ErrNotFound
	}
	return storage.IdentityProfileRecord{Profile: profile.Clone(), ETag: "etag"}, nil
}

func confirmedCodexProfile(serviceID contract.ServiceID, id contract.IdentityProfileID) contract.IdentityProfile {
	created := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	confirmed := created.Add(time.Minute)
	return contract.IdentityProfile{
		ID: id, ServiceID: serviceID, Client: contract.IdentityClientCodexCLI,
		Source: contract.IdentityProfileBuiltin,
		Fingerprint: contract.IdentityFingerprint{
			UserAgent: "codex_cli_rs/0.160.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6.1",
			Version:   "0.160.0",
			Headers:   map[string]string{"Originator": "codex_cli_rs"},
		},
		CreatedAt: created, ConfirmedAt: &confirmed,
	}
}

// ruleService builds an API provider whose HTTP connection carries the given
// configuration, exactly as a stored service would.
func ruleService(baseURL string, connection contract.HTTPConnection) contract.Service {
	connection.BaseURL = baseURL
	connection.Auth = contract.ServiceAuth{Scheme: contract.AuthSchemeBearer}
	connection.CredentialRef = "local://service/service_rules"
	return contract.Service{
		ID: "service_rules", Name: "Relay", Kind: contract.ServiceKindOpenAICompatible, Enabled: true,
		Models:       []string{"gpt-5.6-luna", "gpt-6-luna", "gpt-6-astra", "gpt-6-nova"},
		Capabilities: []contract.Capability{{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeNative, Streaming: true}},
		HTTP:         &connection,
	}
}

func ruleHandler(t *testing.T, service contract.Service, profiles IdentityProfileReader, upstreamModel string) *Handler {
	t.Helper()
	if err := service.Validate(); err != nil {
		t.Fatalf("configuration was rejected before forwarding: %v", err)
	}
	candidate := endpoint.Resolved{
		Service: service, BaseURL: service.HTTP.BaseURL,
		UpstreamProtocol: contract.ProtocolOpenAIChat, UpstreamModel: upstreamModel,
	}
	return NewWithDependencies(Dependencies{
		Resolver:         candidateResolver{candidates: []endpoint.Resolved{candidate}},
		Authorizer:       endpoint.NewServiceAuthorizer(codingPlanCredentials{}, nil),
		IdentityProfiles: profiles,
	})
}

// ruleUpstream records the final outgoing request, which is the only place a
// header overlay can be verified: an intermediate overlay says nothing about
// what the transport actually sends.
type ruleUpstream struct {
	mu     sync.Mutex
	header http.Header
	body   []byte
	url    string
	calls  int
}

func newRuleUpstream(t *testing.T) *ruleUpstream {
	t.Helper()
	upstream := &ruleUpstream{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		upstream.mu.Lock()
		upstream.header, upstream.body = request.Header.Clone(), body
		upstream.calls++
		upstream.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"id":"chat_1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`)
	}))
	t.Cleanup(server.Close)
	upstream.url = server.URL
	return upstream
}

func (upstream *ruleUpstream) sent(t *testing.T) (http.Header, []byte) {
	t.Helper()
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.header == nil {
		t.Fatal("upstream was not called")
	}
	return upstream.header.Clone(), append([]byte(nil), upstream.body...)
}

func serveRuleRequest(t *testing.T, handler *Handler, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	// A caller-supplied identity must lose to the configured one, and a caller
	// credential must never reach the upstream.
	request.Header.Set("User-Agent", "ExampleIDE/1.2.3")
	request.Header.Set("Originator", "example_ide")
	request.Header.Set("Authorization", "Bearer caller-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func migratedModelRules() []contract.ModelRule {
	rules := make([]contract.ModelRule, 0, 3)
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna", "gpt-6-astra"} {
		rules = append(rules, contract.ModelRule{
			Match:   model,
			Headers: map[string]string{"originator": ruleOriginator, "user-agent": ruleUserAgent},
		})
	}
	return rules
}

func TestModelRulesReachUpstreamVerbatimAndNeverLeakCredentials(t *testing.T) {
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna", "gpt-6-astra"} {
		t.Run(model, func(t *testing.T) {
			upstream := newRuleUpstream(t)
			service := ruleService(upstream.url, contract.HTTPConnection{ModelRules: migratedModelRules()})
			handler := ruleHandler(t, service, nil, model)
			if response := serveRuleRequest(t, handler, model); response.Code != http.StatusOK {
				t.Fatalf("status = %d %s", response.Code, response.Body.String())
			}
			header, body := upstream.sent(t)
			// Both configured values arrive byte for byte, exactly once each.
			if header.Get("Originator") != ruleOriginator || header.Get("User-Agent") != ruleUserAgent {
				t.Fatalf("identity = %q / %q", header.Get("Originator"), header.Get("User-Agent"))
			}
			for _, name := range []string{"Originator", "User-Agent"} {
				if values := header.Values(name); len(values) != 1 {
					t.Fatalf("%s folded onto the caller value: %v", name, values)
				}
			}
			// The provider credential replaced the caller's, and the body is the
			// client's bytes: a headers-only rule must not touch it.
			if header.Get("Authorization") != "Bearer plan-key" {
				t.Fatalf("Authorization = %q", header.Get("Authorization"))
			}
			if want := `{"model":"` + model + `","messages":[{"role":"user","content":"hello"}]}`; string(body) != want {
				t.Fatalf("body changed: %s", body)
			}
		})
	}
}

func TestUnmatchedModelKeepsExistingBehavior(t *testing.T) {
	upstream := newRuleUpstream(t)
	service := ruleService(upstream.url, contract.HTTPConnection{ModelRules: migratedModelRules()})
	handler := ruleHandler(t, service, nil, "gpt-6-nova")
	if response := serveRuleRequest(t, handler, "gpt-6-nova"); response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	header, _ := upstream.sent(t)
	// Without a matching rule and without a default overlay, the caller's own
	// identity is forwarded unchanged, as before this feature existed.
	if header.Get("User-Agent") != "ExampleIDE/1.2.3" || header.Get("Originator") != "example_ide" {
		t.Fatalf("unmatched model was rewritten: %q / %q", header.Get("User-Agent"), header.Get("Originator"))
	}
}

func TestDefaultHeadersAndModelRulesAreMutuallyExclusive(t *testing.T) {
	connection := contract.HTTPConnection{
		ExtraHeaders: map[string]string{"User-Agent": "DefaultAgent/1.0", "X-Relay-Tier": "default"},
		ModelRules: []contract.ModelRule{{
			Match:   "gpt-6-astra",
			Headers: map[string]string{"User-Agent": ruleUserAgent},
		}},
	}
	for _, test := range []struct {
		model, agent, tier string
	}{
		// A matched rule replaces the service default instead of layering on it,
		// so the default's unrelated header is also dropped.
		{"gpt-6-astra", ruleUserAgent, ""},
		{"gpt-6-nova", "DefaultAgent/1.0", "default"},
	} {
		t.Run(test.model, func(t *testing.T) {
			upstream := newRuleUpstream(t)
			service := ruleService(upstream.url, connection)
			handler := ruleHandler(t, service, nil, test.model)
			if response := serveRuleRequest(t, handler, test.model); response.Code != http.StatusOK {
				t.Fatalf("status = %d %s", response.Code, response.Body.String())
			}
			header, _ := upstream.sent(t)
			if header.Get("User-Agent") != test.agent || header.Get("X-Relay-Tier") != test.tier {
				t.Fatalf("overlay = %q / %q", header.Get("User-Agent"), header.Get("X-Relay-Tier"))
			}
		})
	}
}

func TestRulesSelectOnTheUpstreamModelNotTheRequestedOne(t *testing.T) {
	upstream := newRuleUpstream(t)
	// Routing rewrites the client's model, so the rule must follow the id the
	// provider really receives.
	service := ruleService(upstream.url, contract.HTTPConnection{
		ModelRules: []contract.ModelRule{
			{Match: "gpt-6-astra", Headers: map[string]string{"X-Relay-Tier": "upstream-model"}},
			{Match: "gpt-6-nova", Headers: map[string]string{"X-Relay-Tier": "requested-model"}},
		},
	})
	handler := ruleHandler(t, service, nil, "gpt-6-astra")
	if response := serveRuleRequest(t, handler, "gpt-6-nova"); response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	header, body := upstream.sent(t)
	if header.Get("X-Relay-Tier") != "upstream-model" {
		t.Fatalf("rule selected on the requested model: %q", header.Get("X-Relay-Tier"))
	}
	if !strings.Contains(string(body), `"gpt-6-astra"`) {
		t.Fatalf("upstream body model = %s", body)
	}
}

func TestIdentityProfileBindingAppliesAndRulesWin(t *testing.T) {
	const profileID = contract.IdentityProfileID("identity_fixture01")
	profile := confirmedCodexProfile("service_rules", profileID)
	for _, test := range []struct {
		name, model, agent, originator, version string
		connection                              contract.HTTPConnection
	}{
		{
			name: "service binding", model: "gpt-6-nova",
			agent: profile.Fingerprint.UserAgent, originator: "codex_cli_rs", version: "0.160.0",
			connection: contract.HTTPConnection{IdentityProfileID: profileID},
		},
		{
			// An explicit rule header must not be silently replaced by the
			// profile's own value for the same field.
			name: "rule overrides profile", model: "gpt-6-astra",
			agent: ruleUserAgent, originator: ruleOriginator, version: "0.160.0",
			connection: contract.HTTPConnection{
				IdentityProfileID: profileID,
				ModelRules: []contract.ModelRule{{
					Match:   "gpt-6-astra",
					Headers: map[string]string{"User-Agent": ruleUserAgent, "Originator": ruleOriginator},
				}},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newRuleUpstream(t)
			profiles := &fixedProfiles{profiles: map[contract.IdentityProfileID]contract.IdentityProfile{profileID: profile}}
			service := ruleService(upstream.url, test.connection)
			handler := ruleHandler(t, service, profiles, test.model)
			if response := serveRuleRequest(t, handler, test.model); response.Code != http.StatusOK {
				t.Fatalf("status = %d %s", response.Code, response.Body.String())
			}
			header, _ := upstream.sent(t)
			if header.Get("User-Agent") != test.agent || header.Get("Originator") != test.originator {
				t.Fatalf("identity = %q / %q", header.Get("User-Agent"), header.Get("Originator"))
			}
			// The profile's generated companion header still applies.
			if header.Get("Version") != test.version {
				t.Fatalf("Version = %q", header.Get("Version"))
			}
			if header.Get("Authorization") != "Bearer plan-key" {
				t.Fatalf("Authorization = %q", header.Get("Authorization"))
			}
		})
	}
}

func TestUnusableIdentityProfileFailsInsteadOfForwardingWithoutIt(t *testing.T) {
	const profileID = contract.IdentityProfileID("identity_fixture01")
	candidate := confirmedCodexProfile("service_rules", profileID)
	candidate.ConfirmedAt = nil
	for _, test := range []struct {
		name     string
		profiles IdentityProfileReader
	}{
		// A pinned identity that cannot be produced must not degrade into a
		// request that silently presents the wrong client.
		{"missing profile", &fixedProfiles{profiles: map[contract.IdentityProfileID]contract.IdentityProfile{}}},
		{"unconfirmed candidate", &fixedProfiles{profiles: map[contract.IdentityProfileID]contract.IdentityProfile{profileID: candidate}}},
		{"storage unavailable", nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newRuleUpstream(t)
			service := ruleService(upstream.url, contract.HTTPConnection{IdentityProfileID: profileID})
			handler := ruleHandler(t, service, test.profiles, "gpt-6-nova")
			response := serveRuleRequest(t, handler, "gpt-6-nova")
			if response.Code == http.StatusOK {
				t.Fatalf("request succeeded without the pinned identity: %s", response.Body.String())
			}
			upstream.mu.Lock()
			calls := upstream.calls
			upstream.mu.Unlock()
			if calls != 0 {
				t.Fatalf("upstream was called %d times", calls)
			}
			if strings.Contains(response.Body.String(), "plan-key") {
				t.Fatal("error body leaked the provider credential")
			}
		})
	}
}

func TestStoredConfigurationCannotReachGatewayOwnedHeaders(t *testing.T) {
	// These documents bypass the control API's validation, standing in for a
	// hand-edited or older database row. Compile must refuse them at forward
	// time rather than apply them.
	for _, test := range []struct {
		name       string
		connection contract.HTTPConnection
	}{
		{"credential", contract.HTTPConnection{ExtraHeaders: map[string]string{"Authorization": "Bearer stolen"}}},
		{"api key", contract.HTTPConnection{ExtraHeaders: map[string]string{"X-Api-Key": "stolen"}}},
		{"framing", contract.HTTPConnection{ExtraHeaders: map[string]string{"Content-Length": "0"}}},
		{"session", contract.HTTPConnection{ModelRules: []contract.ModelRule{{Match: "*", Headers: map[string]string{"Session-Id": "pinned"}}}}},
		{"gateway namespace", contract.HTTPConnection{ModelRules: []contract.ModelRule{{Match: "*", Headers: map[string]string{"X-AstrLink-Debug": "on"}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			upstream := newRuleUpstream(t)
			connection := test.connection
			connection.BaseURL = upstream.url
			connection.Auth = contract.ServiceAuth{Scheme: contract.AuthSchemeBearer}
			connection.CredentialRef = "local://service/service_rules"
			service := contract.Service{
				ID: "service_rules", Name: "Relay", Kind: contract.ServiceKindOpenAICompatible, Enabled: true,
				Models:       []string{"gpt-6-nova"},
				Capabilities: []contract.Capability{{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeNative, Streaming: true}},
				HTTP:         &connection,
			}
			// Validation rejects the document, which is why forwarding must not
			// be the only line of defense.
			if err := service.Validate(); err == nil {
				t.Fatal("validation accepted a gateway-owned header")
			}
			candidate := endpoint.Resolved{
				Service: service, BaseURL: connection.BaseURL,
				UpstreamProtocol: contract.ProtocolOpenAIChat, UpstreamModel: "gpt-6-nova",
			}
			handler := NewWithDependencies(Dependencies{
				Resolver:   candidateResolver{candidates: []endpoint.Resolved{candidate}},
				Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, nil),
			})
			if response := serveRuleRequest(t, handler, "gpt-6-nova"); response.Code == http.StatusOK {
				t.Fatalf("stored configuration was applied: %s", response.Body.String())
			}
			upstream.mu.Lock()
			calls := upstream.calls
			upstream.mu.Unlock()
			if calls != 0 {
				t.Fatalf("upstream was called %d times", calls)
			}
		})
	}
}

func TestCustomAuthHeaderStaysUnreachableAndCredentialIsSent(t *testing.T) {
	upstream := newRuleUpstream(t)
	connection := contract.HTTPConnection{
		BaseURL:       upstream.url,
		Auth:          contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: "X-Relay-Token"},
		CredentialRef: "local://service/service_rules",
		ModelRules: []contract.ModelRule{{
			Match:   "*",
			Headers: map[string]string{"X-Relay-Token": "forged"},
		}},
	}
	service := contract.Service{
		ID: "service_rules", Name: "Relay", Kind: contract.ServiceKindOpenAICompatible, Enabled: true,
		Models:       []string{"gpt-6-nova"},
		Capabilities: []contract.Capability{{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeNative, Streaming: true}},
		HTTP:         &connection,
	}
	// The service's own custom auth header belongs to authentication, so a rule
	// may not set it even though the name is otherwise unremarkable.
	if err := service.Validate(); err == nil {
		t.Fatal("validation accepted the service's custom auth header")
	}

	// With the rule removed, the credential itself still reaches the upstream.
	connection.ModelRules = []contract.ModelRule{{Match: "*", Headers: map[string]string{"User-Agent": ruleUserAgent}}}
	service.HTTP = &connection
	handler := ruleHandler(t, service, nil, "gpt-6-nova")
	if response := serveRuleRequest(t, handler, "gpt-6-nova"); response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	header, _ := upstream.sent(t)
	if header.Get("X-Relay-Token") != "plan-key" || header.Get("User-Agent") != ruleUserAgent {
		t.Fatalf("auth = %q, identity = %q", header.Get("X-Relay-Token"), header.Get("User-Agent"))
	}
}

func TestDisabledRuleFallsBackToTheServiceDefault(t *testing.T) {
	disabled := false
	upstream := newRuleUpstream(t)
	service := ruleService(upstream.url, contract.HTTPConnection{
		ExtraHeaders: map[string]string{"X-Relay-Tier": "default"},
		ModelRules: []contract.ModelRule{{
			Match:   "gpt-6-astra",
			Headers: map[string]string{"X-Relay-Tier": "rule"},
			Enabled: &disabled,
		}},
	})
	handler := ruleHandler(t, service, nil, "gpt-6-astra")
	if response := serveRuleRequest(t, handler, "gpt-6-astra"); response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	header, _ := upstream.sent(t)
	if header.Get("X-Relay-Tier") != "default" {
		t.Fatalf("disabled rule still applied: %q", header.Get("X-Relay-Tier"))
	}
}
