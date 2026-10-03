package contract

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Every header the forwarder strips as an inbound credential, and every header
// an authorizer supplies, must be unreachable from configuration.
func TestClassifyRequestHeaderProtectsGatewayOwnedFields(t *testing.T) {
	bearer := ServiceAuth{Scheme: AuthSchemeBearer}
	for _, test := range []struct {
		name string
		auth ServiceAuth
		want HeaderClass
	}{
		// Credentials and account bindings, in any casing.
		{"Authorization", bearer, HeaderClassAuthReserved},
		{"authorization", bearer, HeaderClassAuthReserved},
		{"AUTHORIZATION", ServiceAuth{Scheme: AuthSchemeNone}, HeaderClassAuthReserved},
		{"Proxy-Authorization", bearer, HeaderClassAuthReserved},
		{"Cookie", bearer, HeaderClassAuthReserved},
		{"X-Api-Key", bearer, HeaderClassAuthReserved},
		{"x-goog-api-key", bearer, HeaderClassAuthReserved},
		{"X-XAI-Token-Auth", bearer, HeaderClassAuthReserved},
		{"ChatGPT-Account-ID", bearer, HeaderClassAuthReserved},
		{"OAI-Product-Sku", bearer, HeaderClassAuthReserved},
		// Transport and framing.
		{"Host", bearer, HeaderClassTransportReserved},
		{"Content-Length", bearer, HeaderClassTransportReserved},
		{"Content-Type", bearer, HeaderClassTransportReserved},
		{"Content-Encoding", bearer, HeaderClassTransportReserved},
		{"Accept-Encoding", bearer, HeaderClassTransportReserved},
		{"Transfer-Encoding", bearer, HeaderClassTransportReserved},
		{"Connection", bearer, HeaderClassTransportReserved},
		{"Upgrade", bearer, HeaderClassTransportReserved},
		{"Sec-WebSocket-Key", bearer, HeaderClassTransportReserved},
		{"Sec-WebSocket-Protocol", bearer, HeaderClassTransportReserved},
		// Session and per-request binding.
		{"Session-Id", bearer, HeaderClassSessionReserved},
		{"X-Claude-Code-Session-Id", bearer, HeaderClassSessionReserved},
		{"X-Request-Id", bearer, HeaderClassSessionReserved},
		{"Idempotency-Key", bearer, HeaderClassSessionReserved},
		{"X-Stainless-Retry-Count", bearer, HeaderClassSessionReserved},
		{"X-Stainless-Timeout", bearer, HeaderClassSessionReserved},
		// The local namespace is stripped before forwarding.
		{"X-AstrLink-Reachable", bearer, HeaderClassGatewayReserved},
		{"x-astrlink-anything", bearer, HeaderClassGatewayReserved},
		// Identity and compatibility fields a rule legitimately sets.
		{"User-Agent", bearer, HeaderClassOverridable},
		{"Originator", bearer, HeaderClassOverridable},
		{"Version", bearer, HeaderClassOverridable},
		{"X-App", bearer, HeaderClassOverridable},
		{"X-Stainless-Lang", bearer, HeaderClassOverridable},
		{"Anthropic-Beta", bearer, HeaderClassOverridable},
		{"X-Grok-Client-Version", bearer, HeaderClassOverridable},
	} {
		if got := ClassifyRequestHeader(test.name, test.auth); got != test.want {
			t.Errorf("ClassifyRequestHeader(%q, %s) = %s, want %s", test.name, test.auth.Scheme, got, test.want)
		}
	}
}

// The classification must follow the auth actually used for the outgoing
// request, so a service's own custom auth header is never configurable.
func TestClassifyRequestHeaderFollowsEffectiveAuth(t *testing.T) {
	custom := ServiceAuth{Scheme: AuthSchemeCustomHeader, HeaderName: "X-Relay-Token"}
	if got := ClassifyRequestHeader("X-Relay-Token", custom); got != HeaderClassAuthReserved {
		t.Fatalf("custom auth header = %s", got)
	}
	if got := ClassifyRequestHeader("x-relay-token", custom); got != HeaderClassAuthReserved {
		t.Fatalf("custom auth header is casing sensitive: %s", got)
	}
	// With a different scheme the same name carries no credential.
	if got := ClassifyRequestHeader("X-Relay-Token", ServiceAuth{Scheme: AuthSchemeBearer}); got != HeaderClassOverridable {
		t.Fatalf("unrelated header = %s", got)
	}
	// A custom header scheme does not make Authorization configurable.
	if got := ClassifyRequestHeader("Authorization", custom); got != HeaderClassAuthReserved {
		t.Fatalf("Authorization under custom auth = %s", got)
	}
}

func TestValidateRequestRulesRejectsUnsafeConfiguration(t *testing.T) {
	auth := ServiceAuth{Scheme: AuthSchemeBearer}
	for _, test := range []struct {
		name    string
		headers map[string]string
		rules   []ModelRule
		want    string
	}{
		{"credential overlay", map[string]string{"Authorization": "Bearer x"}, nil, "auth_reserved"},
		{"credential rule", nil, []ModelRule{{Match: "m", Headers: map[string]string{"X-Api-Key": "x"}}}, "auth_reserved"},
		{"transport rule", nil, []ModelRule{{Match: "m", Headers: map[string]string{"Content-Length": "1"}}}, "transport_reserved"},
		{"session rule", nil, []ModelRule{{Match: "m", Headers: map[string]string{"Session-Id": "s"}}}, "session_reserved"},
		{"gateway rule", nil, []ModelRule{{Match: "m", Headers: map[string]string{"X-AstrLink-Debug": "1"}}}, "gateway_reserved"},
		{"empty match", nil, []ModelRule{{Match: ""}}, "must not be empty"},
		{"glob suffix", nil, []ModelRule{{Match: "gpt-6-*"}}, "unsupported pattern syntax"},
		{"glob question", nil, []ModelRule{{Match: "gpt-?"}}, "unsupported pattern syntax"},
		{"glob class", nil, []ModelRule{{Match: "gpt-[56]"}}, "unsupported pattern syntax"},
		{"padded match", nil, []ModelRule{{Match: " gpt-6 "}}, "whitespace"},
		{"duplicate exact", nil, []ModelRule{{Match: "m"}, {Match: "m"}}, "duplicates match"},
		{"duplicate catch-all", nil, []ModelRule{{Match: "*"}, {Match: "*"}}, "duplicates the \"*\" rule"},
		{"non-empty body", nil, []ModelRule{{Match: "m", Body: map[string]json.RawMessage{"stream": json.RawMessage("true")}}}, "body rewriting is not supported yet"},
		{"casing conflict", map[string]string{"User-Agent": "a", "user-agent": "b"}, nil, "twice with different casing"},
		{"invalid header name", map[string]string{"Bad Header": "a"}, nil, "not a valid HTTP field name"},
		{"non-ascii value", map[string]string{"User-Agent": "ué"}, nil, "printable ASCII"},
		{"padded value", map[string]string{"User-Agent": " a"}, nil, "whitespace"},
		{"oversized value", map[string]string{"User-Agent": strings.Repeat("a", MaxConfiguredHeaderValueBytes+1)}, nil, "exceeds"},
		{"bad profile", nil, []ModelRule{{Match: "m", IdentityProfile: "not a valid id"}}, "identity_profile"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateRequestRules(test.headers, test.rules, auth)
			if err == nil {
				t.Fatalf("configuration was accepted")
			}
			if !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want mention of %q", err, test.want)
			}
		})
	}
}

func TestValidateRequestRulesAcceptsTheMigratedHeadersOnlyRules(t *testing.T) {
	// The three rules this feature must migrate unchanged: headers only, with
	// an explicitly empty body object.
	rules := []ModelRule{
		{Match: "gpt-5.6-luna", Headers: map[string]string{
			"originator": "codex_exec",
			"user-agent": "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)",
		}, Body: map[string]json.RawMessage{}},
		{Match: "gpt-6-luna", Headers: map[string]string{
			"originator": "codex_exec",
			"user-agent": "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)",
		}, Body: map[string]json.RawMessage{}},
		{Match: "gpt-6-astra", Headers: map[string]string{
			"originator": "codex_exec",
			"user-agent": "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)",
		}, Body: map[string]json.RawMessage{}},
	}
	for _, auth := range []ServiceAuth{
		{Scheme: AuthSchemeBearer},
		{Scheme: AuthSchemeNone},
		{Scheme: AuthSchemeCustomHeader, HeaderName: "X-Relay-Token"},
	} {
		if err := ValidateRequestRules(nil, rules, auth); err != nil {
			t.Fatalf("auth=%s: %v", auth.Scheme, err)
		}
	}
}

// A rule is enabled unless explicitly disabled, so configuration written
// before the switch existed keeps working.
func TestModelRuleEnabledDefaultsToTrue(t *testing.T) {
	disabled := false
	enabled := true
	if !(ModelRule{Match: "m"}).Active() {
		t.Fatal("omitted enabled disabled the rule")
	}
	if !(ModelRule{Match: "m", Enabled: &enabled}).Active() {
		t.Fatal("explicit true disabled the rule")
	}
	if (ModelRule{Match: "m", Enabled: &disabled}).Active() {
		t.Fatal("explicit false did not disable the rule")
	}
	var decoded ModelRule
	if err := json.Unmarshal([]byte(`{"match":"m"}`), &decoded); err != nil || !decoded.Active() {
		t.Fatalf("decoded legacy rule is inactive: %v", err)
	}
}

func TestModelRuleCloneDoesNotShareState(t *testing.T) {
	enabled := true
	rule := ModelRule{
		Match: "m", Headers: map[string]string{"User-Agent": "a"},
		Body: map[string]json.RawMessage{}, Enabled: &enabled,
	}
	cloned := rule.Clone()
	cloned.Headers["User-Agent"] = "changed"
	*cloned.Enabled = false
	if rule.Headers["User-Agent"] != "a" || !*rule.Enabled {
		t.Fatal("clone shares mutable state with the original rule")
	}
}

// The service document is the persistence boundary: an unsafe rule must fail
// validation there too, not only in the control API decoder.
func TestHTTPConnectionValidateRejectsUnsafeRules(t *testing.T) {
	connection := HTTPConnection{
		BaseURL: "https://upstream.example.test/v1",
		Auth:    ServiceAuth{Scheme: AuthSchemeBearer},
		ModelRules: []ModelRule{{Match: "gpt-6-astra", Headers: map[string]string{
			"Authorization": "Bearer <SECRET_PLACEHOLDER>",
		}}},
	}
	if err := connection.Validate("service_fixture"); err == nil {
		t.Fatal("a credential rule was accepted by the service document")
	}
	connection.ModelRules[0].Headers = map[string]string{"Originator": "codex_exec"}
	if err := connection.Validate("service_fixture"); err != nil {
		t.Fatalf("headers-only rule rejected: %v", err)
	}
}

// Classification must not depend on the caller canonicalizing names first.
func TestClassifyRequestHeaderCanonicalizesInput(t *testing.T) {
	for _, name := range []string{"authorization", " Authorization ", "AUTHORIZATION"} {
		if got := ClassifyRequestHeader(name, ServiceAuth{Scheme: AuthSchemeBearer}); got != HeaderClassAuthReserved {
			t.Fatalf("ClassifyRequestHeader(%q) = %s", name, got)
		}
	}
	if http.CanonicalHeaderKey("x-astrlink-x") != "X-Astrlink-X" {
		t.Skip("canonicalization changed; the gateway prefix check needs review")
	}
}
