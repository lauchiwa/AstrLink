package ingress

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

// Vertex inference with the fork's request rules and a bound identity profile:
// the final request carries the configured headers, the connection's project,
// and no gateway header from the client, the authorizer, or the rules.
func TestForkVertexForwardingCarriesRulesWithoutGatewayHeaders(t *testing.T) {
	service := vertexServiceForTest()
	profile := confirmedCodexProfile(service.ID, "identity_vertex")
	service.HTTP.IdentityProfileID = profile.ID
	service.HTTP.ExtraHeaders = map[string]string{"X-Relay-Client": "fake-default-value", "X-Goog-User-Project": "fake-rule-project"}
	if err := service.Validate(); err != nil {
		t.Fatal(err)
	}
	resolver, err := endpoint.NewStoreResolver(redirectServices([]contract.Service{service}))
	if err != nil {
		t.Fatal(err)
	}
	engine := relaykitbridge.NewEngine()
	resolver.WithRuntimeProfile(contract.RuntimeProfile{RelayKitAvailable: true, Edges: engine.Edges()})
	calls := 0
	handler := NewWithDependencies(Dependencies{
		Resolver: resolver, ConversionEngine: engine,
		Authorizer: authorizerFunc(func(context.Context, contract.Endpoint) (http.Header, error) {
			return http.Header{"Authorization": {"Bearer google-token"}, providerapi.VertexProjectHeader: {"key-project"}}, nil
		}),
		IdentityProfiles: &fixedProfiles{profiles: map[contract.IdentityProfileID]contract.IdentityProfile{profile.ID: profile}},
		Forwarder: transport.New(roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			for name := range r.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Astrlink-") {
					t.Errorf("gateway header forwarded: %s", name)
				}
			}
			for name, want := range map[string]string{
				"Authorization":       "Bearer google-token",
				"X-Goog-User-Project": "vertex-test",
				"X-Relay-Client":      "fake-default-value",
				"User-Agent":          profile.Fingerprint.UserAgent,
				"Originator":          "codex_cli_rs",
			} {
				if got := r.Header.Values(name); len(got) != 1 || got[0] != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			return geminiReply(false), nil
		})),
	})
	request := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-test:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	request.Header.Set("Content-Type", "application/json")
	for _, name := range []string{"X-AstrLink-Test", "X-AstrLink-Debug", providerapi.VertexProjectHeader} {
		request.Header.Set(name, "local-only")
	}
	// Pins the service; it must stay local too.
	request.Header.Set("X-AstrLink-Service", string(service.ID))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if calls != 1 || response.Code != http.StatusOK {
		t.Fatalf("calls = %d, response = %d %s", calls, response.Code, response.Body)
	}
}
