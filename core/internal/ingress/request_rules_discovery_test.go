package ingress

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
)

// discoveryRuleService mirrors ruleService but declares the listing capability,
// so the aggregate model endpoint reaches the same provider configuration a
// chat request would.
func discoveryRuleService(baseURL string, connection contract.HTTPConnection) contract.Service {
	service := ruleService(baseURL, connection)
	service.Capabilities = []contract.Capability{{
		Protocol: contract.ProtocolOpenAIModels, Mode: contract.CapabilityModeNative,
	}}
	return service
}

func serveDiscovery(t *testing.T, handler *Handler) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("User-Agent", "ExampleIDE/1.2.3")
	request.Header.Set("Originator", "example_ide")
	request.Header.Set("Authorization", "Bearer caller-secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func discoveryRuleHandler(service contract.Service, profiles IdentityProfileReader) *Handler {
	return NewWithDependencies(Dependencies{
		Resolver: candidateResolver{candidates: []endpoint.Resolved{{
			Service: service, BaseURL: service.HTTP.BaseURL,
			UpstreamProtocol: contract.ProtocolOpenAIModels,
		}}},
		Authorizer:       endpoint.NewServiceAuthorizer(codingPlanCredentials{}, nil),
		IdentityProfiles: profiles,
	})
}

// A provider that only answers a recognized client must still be able to list
// its models: without the configured identity the aggregate listing fails and
// the provider looks empty even though inference works.
func TestModelDiscoveryAppliesServiceRequestRules(t *testing.T) {
	upstream := newRuleUpstream(t)
	service := discoveryRuleService(upstream.url, contract.HTTPConnection{
		ExtraHeaders: map[string]string{"originator": ruleOriginator, "user-agent": ruleUserAgent},
		// A catch-all rule describes an inference request and must not be
		// attributed to a listing that names no model.
		ModelRules: []contract.ModelRule{{
			Match:   contract.ModelRuleMatchAll,
			Headers: map[string]string{"originator": "never_for_discovery"},
		}},
	})
	if response := serveDiscovery(t, discoveryRuleHandler(service, nil)); response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	header, _ := upstream.sent(t)
	if header.Get("Originator") != ruleOriginator || header.Get("User-Agent") != ruleUserAgent {
		t.Fatalf("service defaults did not reach discovery: %q / %q",
			header.Get("Originator"), header.Get("User-Agent"))
	}
	for _, name := range []string{"Originator", "User-Agent"} {
		if values := header.Values(name); len(values) != 1 {
			t.Fatalf("%s folded onto the caller value: %v", name, values)
		}
	}
	if header.Get("Authorization") == "Bearer caller-secret" {
		t.Fatal("caller credential reached the upstream listing")
	}
}

// The bound profile is rendered for the listing too, and an unusable one fails
// closed instead of listing models under the wrong client identity.
func TestModelDiscoveryIdentityProfileIsPinnedAndFailsClosed(t *testing.T) {
	const id contract.IdentityProfileID = "identity_discovery"
	upstream := newRuleUpstream(t)
	service := discoveryRuleService(upstream.url, contract.HTTPConnection{IdentityProfileID: id})
	profile := confirmedCodexProfile(service.ID, id)
	profiles := &fixedProfiles{profiles: map[contract.IdentityProfileID]contract.IdentityProfile{id: profile}}
	if response := serveDiscovery(t, discoveryRuleHandler(service, profiles)); response.Code != http.StatusOK {
		t.Fatalf("status = %d %s", response.Code, response.Body.String())
	}
	header, _ := upstream.sent(t)
	if header.Get("User-Agent") != profile.Fingerprint.UserAgent ||
		header.Get("Originator") != profile.Fingerprint.Headers["Originator"] {
		t.Fatalf("profile did not reach discovery: %q / %q",
			header.Get("User-Agent"), header.Get("Originator"))
	}

	// An unconfirmed candidate is not forwardable. The listing must report a
	// configuration problem rather than silently present the caller's identity.
	unconfirmed := confirmedCodexProfile(service.ID, id)
	unconfirmed.ConfirmedAt = nil
	broken := &fixedProfiles{profiles: map[contract.IdentityProfileID]contract.IdentityProfile{id: unconfirmed}}
	calls := upstream.calls
	response := serveDiscovery(t, discoveryRuleHandler(service, broken))
	if response.Code == http.StatusOK {
		t.Fatalf("unusable identity still listed models: %s", response.Body.String())
	}
	if upstream.calls != calls {
		t.Fatal("discovery reached the upstream without the pinned identity")
	}
}
