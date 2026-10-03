package requestrewrite

import (
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

func enabled(value bool) *bool { return &value }

// userRules reproduces the three rules this feature must migrate unchanged:
// headers-only, exact model ids, empty body objects.
func userRules() []contract.ModelRule {
	headers := map[string]string{
		"originator": "codex_exec",
		"user-agent": "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)",
	}
	rules := make([]contract.ModelRule, 0, 3)
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna", "gpt-6-astra"} {
		rules = append(rules, contract.ModelRule{Match: model, Headers: maps(headers)})
	}
	return rules
}

func maps(headers map[string]string) map[string]string {
	cloned := make(map[string]string, len(headers))
	for name, value := range headers {
		cloned[name] = value
	}
	return cloned
}

func bearer() contract.ServiceAuth {
	return contract.ServiceAuth{Scheme: contract.AuthSchemeBearer}
}

func TestMigratedRulesApplyExactlyTheConfiguredHeaders(t *testing.T) {
	connection := contract.HTTPConnection{
		BaseURL: "https://relay.example.test/v1", Auth: bearer(), ModelRules: userRules(),
	}
	if err := connection.Validate("service_relay"); err != nil {
		t.Fatalf("the three migrated rules must validate: %v", err)
	}
	plan, err := Compile(connection, bearer())
	if err != nil || plan == nil {
		t.Fatalf("Compile() = %v, %v", plan, err)
	}
	wantUA := "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"
	for _, model := range []string{"gpt-5.6-luna", "gpt-6-luna", "gpt-6-astra"} {
		decision := plan.Decide(model)
		if decision.Source != SourceModelRule || decision.RuleMatch != model || !decision.Configured() {
			t.Fatalf("%s: %+v", model, decision)
		}
		overlay := make(http.Header)
		if err := plan.Apply(overlay, decision, nil); err != nil {
			t.Fatal(err)
		}
		if overlay.Get("Originator") != "codex_exec" || overlay.Get("User-Agent") != wantUA {
			t.Fatalf("%s overlay = %v", model, overlay)
		}
		if len(overlay) != 2 {
			t.Fatalf("%s applied extra headers: %v", model, overlay)
		}
		if names := decision.HeaderNames(); !slices.Equal(names, []string{"Originator", "User-Agent"}) {
			t.Fatalf("diagnostic names = %v", names)
		}
	}
	// A model outside the three rules must keep the gateway's existing behavior.
	unmatched := plan.Decide("gpt-6-nova")
	if unmatched.Source != SourceNone || unmatched.Configured() {
		t.Fatalf("unconfigured model got an overlay: %+v", unmatched)
	}
	overlay := make(http.Header)
	if err := plan.Apply(overlay, unmatched, nil); err != nil || len(overlay) != 0 {
		t.Fatalf("unmatched apply = %v, %v", overlay, err)
	}
}

func TestUnconfiguredServiceCompilesToNoPlan(t *testing.T) {
	plan, err := Compile(contract.HTTPConnection{BaseURL: "https://relay.example.test/v1", Auth: bearer()}, bearer())
	if err != nil || plan != nil {
		t.Fatalf("Compile() = %v, %v", plan, err)
	}
	// A nil plan must be safe at the call site.
	decision := plan.Decide("gpt-6-astra")
	if decision.Source != SourceNone || decision.Configured() || decision.RuleIndex != -1 {
		t.Fatalf("nil plan decided %+v", decision)
	}
	overlay := make(http.Header)
	if err := plan.Apply(overlay, decision, nil); err != nil || len(overlay) != 0 {
		t.Fatalf("nil plan applied %v, %v", overlay, err)
	}
}

func TestRuleSelectionIsExactFirstThenFirstListed(t *testing.T) {
	connection := contract.HTTPConnection{
		BaseURL: "https://relay.example.test/v1", Auth: bearer(),
		ExtraHeaders: map[string]string{"X-Compat": "service-default"},
		ModelRules: []contract.ModelRule{
			{Match: "*", Headers: map[string]string{"X-Compat": "catch-all-first"}},
			{Match: "gpt-6-astra", Headers: map[string]string{"X-Compat": "exact-first"}},
			{Match: "gpt-6-astra", Headers: map[string]string{"X-Compat": "exact-second"}},
			{Match: "*", Headers: map[string]string{"X-Compat": "catch-all-second"}},
		},
	}
	// Duplicate matches must be refused when saving, so a stored document never
	// depends on list order to resolve a conflict.
	if err := connection.Validate("service_relay"); err == nil {
		t.Fatal("duplicate matches were accepted")
	}
	if _, err := Compile(connection, bearer()); err == nil {
		t.Fatal("Compile accepted a configuration the API refuses")
	}

	// Within a valid list, an exact match still wins over the catch-all.
	connection.ModelRules = []contract.ModelRule{
		{Match: "*", Headers: map[string]string{"X-Compat": "catch-all"}},
		{Match: "gpt-6-astra", Headers: map[string]string{"X-Compat": "exact"}},
	}
	if err := connection.Validate("service_relay"); err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(connection, bearer())
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]string{"gpt-6-astra": "exact", "gpt-6-luna": "catch-all"} {
		decision := plan.Decide(model)
		overlay := make(http.Header)
		if err := plan.Apply(overlay, decision, nil); err != nil {
			t.Fatal(err)
		}
		if overlay.Get("X-Compat") != want || decision.Source != SourceModelRule {
			t.Fatalf("%s = %q (%+v)", model, overlay.Get("X-Compat"), decision)
		}
		// A matched rule replaces the service default instead of layering.
		if len(overlay) != 1 {
			t.Fatalf("%s layered defaults onto a rule: %v", model, overlay)
		}
	}
}

func TestDisabledRuleFallsBackToTheServiceDefault(t *testing.T) {
	connection := contract.HTTPConnection{
		BaseURL: "https://relay.example.test/v1", Auth: bearer(),
		ExtraHeaders: map[string]string{"X-Compat": "service-default"},
		ModelRules: []contract.ModelRule{
			{Match: "gpt-6-astra", Headers: map[string]string{"X-Compat": "rule"}, Enabled: enabled(false)},
			{Match: "gpt-6-luna", Headers: map[string]string{"X-Compat": "rule"}},
		},
	}
	if err := connection.Validate("service_relay"); err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(connection, bearer())
	if err != nil {
		t.Fatal(err)
	}
	disabled := plan.Decide("gpt-6-astra")
	if disabled.Source != SourceServiceDefault || disabled.RuleIndex != -1 {
		t.Fatalf("disabled rule still matched: %+v", disabled)
	}
	overlay := make(http.Header)
	if err := plan.Apply(overlay, disabled, nil); err != nil || overlay.Get("X-Compat") != "service-default" {
		t.Fatalf("overlay = %v, %v", overlay, err)
	}
	if active := plan.Decide("gpt-6-luna"); active.Source != SourceModelRule {
		t.Fatalf("enabled rule did not match: %+v", active)
	}
	// An omitted enabled field must keep an older rule working.
	if !(contract.ModelRule{Match: "gpt-6-luna"}).Active() {
		t.Fatal("a rule without enabled defaulted to off")
	}
}

func TestApplyRefusesGatewayOwnedHeadersEvenFromStoredConfiguration(t *testing.T) {
	// Validation refuses these names, so reaching Apply means a hand-edited or
	// older document. Apply must still refuse rather than send them.
	for _, test := range []struct {
		name string
		auth contract.ServiceAuth
	}{
		{"Authorization", bearer()},
		{"X-Api-Key", contract.ServiceAuth{Scheme: contract.AuthSchemeAnthropicAPIKey}},
		{"X-Relay-Token", contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: "X-Relay-Token"}},
		{"Content-Length", bearer()},
		{"Accept-Encoding", bearer()},
		{"Session-Id", bearer()},
		{"X-AstrLink-Debug", bearer()},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := &Plan{auth: test.auth, catchAll: -1}
			decision := Decision{Source: SourceModelRule, RuleIndex: 0, headers: map[string]string{test.name: "injected"}}
			overlay := make(http.Header)
			if err := plan.Apply(overlay, decision, nil); err == nil {
				t.Fatalf("Apply accepted %s", test.name)
			}
			if len(overlay) != 0 {
				t.Fatalf("Apply wrote %v before refusing", overlay)
			}
			identity := http.Header{test.name: {"injected"}}
			if err := plan.Apply(overlay, Decision{}, identity); err == nil {
				t.Fatalf("Apply accepted %s from an identity profile", test.name)
			}
			if len(overlay) != 0 {
				t.Fatalf("Apply wrote %v before refusing", overlay)
			}
		})
	}
}

func TestEffectiveAuthDecidesWhichCredentialHeaderIsReserved(t *testing.T) {
	// Several providers send one saved key as Bearer on one protocol and as
	// x-api-key on another. Compiling against the effective auth is what keeps
	// a rule away from the header the outgoing request actually uses.
	connection := contract.HTTPConnection{
		BaseURL: "https://relay.example.test/v1", Auth: bearer(),
		ModelRules: []contract.ModelRule{{Match: "*", Headers: map[string]string{"X-Api-Key": "configured"}}},
	}
	if err := connection.Validate("service_relay"); err == nil {
		t.Fatal("a well-known credential header was configurable")
	}
	connection.ModelRules[0].Headers = map[string]string{"X-Relay-Token": "configured"}
	if err := connection.Validate("service_relay"); err != nil {
		t.Fatalf("a provider-specific compatibility header was refused: %v", err)
	}
	if _, err := Compile(connection, contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: "x-relay-token"}); err == nil {
		t.Fatal("Compile ignored the effective custom auth header")
	}
	if _, err := Compile(connection, bearer()); err != nil {
		t.Fatalf("Compile rejected a valid rule for the effective auth: %v", err)
	}
}

func TestConfiguredHeadersWinOverIdentityAndReplaceClientValues(t *testing.T) {
	connection := contract.HTTPConnection{
		BaseURL: "https://relay.example.test/v1", Auth: bearer(),
		ModelRules: []contract.ModelRule{{Match: "*", Headers: map[string]string{
			"User-Agent": "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)",
		}}},
	}
	plan, err := Compile(connection, bearer())
	if err != nil {
		t.Fatal(err)
	}
	identity := http.Header{
		"User-Agent": {"codex_cli_rs/0.160.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6.1"},
		"Originator": {"codex_cli_rs"},
	}
	overlay := http.Header{"User-Agent": {"stale-1"}}
	overlay.Add("User-Agent", "stale-2")
	if err := plan.Apply(overlay, plan.Decide("gpt-6-astra"), identity); err != nil {
		t.Fatal(err)
	}
	if got := overlay.Values("User-Agent"); len(got) != 1 || !strings.HasPrefix(got[0], "codex-tui/0.156.0") {
		t.Fatalf("User-Agent = %v, want exactly the configured value", got)
	}
	// An identity field the rule did not set still applies.
	if overlay.Get("Originator") != "codex_cli_rs" {
		t.Fatalf("identity header was dropped: %v", overlay)
	}
}

func TestDecisionsAreFrozenSnapshots(t *testing.T) {
	headers := map[string]string{"X-Compat": "configured"}
	connection := contract.HTTPConnection{
		BaseURL: "https://relay.example.test/v1", Auth: bearer(),
		ExtraHeaders: headers,
		ModelRules:   []contract.ModelRule{{Match: "gpt-6-astra", Headers: headers}},
	}
	plan, err := Compile(connection, bearer())
	if err != nil {
		t.Fatal(err)
	}
	decision := plan.Decide("gpt-6-astra")
	// Mutating the caller's maps must not change a compiled plan.
	headers["X-Compat"] = "mutated"
	headers["X-Extra"] = "mutated"
	connection.ModelRules[0].Match = "mutated"
	overlay := make(http.Header)
	if err := plan.Apply(overlay, decision, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(overlay, http.Header{"X-Compat": {"configured"}}) {
		t.Fatalf("overlay = %v", overlay)
	}
	if again := plan.Decide("gpt-6-astra"); again.RuleMatch != "gpt-6-astra" {
		t.Fatalf("plan followed a caller mutation: %+v", again)
	}
	// A Decision must not expose mutable plan data.
	if decision.headers["X-Compat"] = "tampered"; true {
		second := plan.Decide("gpt-6-astra")
		fresh := make(http.Header)
		if err := plan.Apply(fresh, second, nil); err != nil {
			t.Fatal(err)
		}
		if fresh.Get("X-Compat") != "configured" {
			t.Fatalf("mutating a decision changed the plan: %v", fresh)
		}
	}
}

func TestIdentityProfileSelectionInheritsTheServiceBinding(t *testing.T) {
	connection := contract.HTTPConnection{
		BaseURL: "https://relay.example.test/v1", Auth: bearer(),
		IdentityProfileID: "identity_service",
		ModelRules: []contract.ModelRule{
			{Match: "gpt-6-astra", IdentityProfile: "identity_rule"},
			{Match: "gpt-6-luna", Headers: map[string]string{"X-Compat": "configured"}},
		},
	}
	if err := connection.Validate("service_relay"); err != nil {
		t.Fatal(err)
	}
	plan, err := Compile(connection, bearer())
	if err != nil {
		t.Fatal(err)
	}
	for model, want := range map[string]contract.IdentityProfileID{
		"gpt-6-astra": "identity_rule",
		"gpt-6-luna":  "identity_service",
		"gpt-6-nova":  "identity_service",
	} {
		if got := plan.Decide(model).IdentityProfile; got != want {
			t.Fatalf("%s identity = %q, want %q", model, got, want)
		}
	}
}
