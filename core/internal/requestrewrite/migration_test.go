package requestrewrite

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

// migratedRules is the operator's existing configuration, in the exact JSON
// shape the previous tool stored it in, including "body": {} and lowercase
// header names. Decoding it here is the real migration path: a hand-written Go
// struct would not prove that the saved document still parses.
const migratedRules = `{
  "base_url": "https://relay.example.test/v1",
  "auth": {"scheme": "bearer"},
  "model_rules": [
    {
      "match": "gpt-5.6-luna",
      "headers": {
        "originator": "codex_exec",
        "user-agent": "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"
      },
      "body": {}
    },
    {
      "match": "gpt-6-luna",
      "headers": {
        "originator": "codex_exec",
        "user-agent": "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"
      },
      "body": {}
    },
    {
      "match": "gpt-6-astra",
      "headers": {
        "originator": "codex_exec",
        "user-agent": "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"
      },
      "body": {}
    }
  ]
}`

func TestMigratedRulesApplyVerbatim(t *testing.T) {
	const (
		originator = "codex_exec"
		userAgent  = "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"
	)
	var connection contract.HTTPConnection
	decoder := json.NewDecoder(strings.NewReader(migratedRules))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&connection); err != nil {
		t.Fatalf("saved configuration no longer decodes: %v", err)
	}
	if err := connection.Validate("service_relay"); err != nil {
		t.Fatalf("migrated configuration was rejected: %v", err)
	}
	plan, err := Compile(connection, connection.Auth)
	if err != nil {
		t.Fatal(err)
	}
	for index, model := range []string{"gpt-5.6-luna", "gpt-6-luna", "gpt-6-astra"} {
		decision := plan.Decide(model)
		if decision.Source != SourceModelRule || decision.RuleIndex != index || decision.RuleMatch != model {
			t.Fatalf("%s selected %+v", model, decision)
		}
		overlay := make(http.Header)
		if err := plan.Apply(overlay, decision, nil); err != nil {
			t.Fatal(err)
		}
		// Both values must arrive byte for byte, each exactly once, under the
		// canonical name the transport will send.
		if len(overlay) != 2 || overlay.Get("Originator") != originator || overlay.Get("User-Agent") != userAgent {
			t.Fatalf("%s overlay = %v", model, overlay)
		}
		for _, name := range []string{"Originator", "User-Agent"} {
			if values := overlay.Values(name); len(values) != 1 {
				t.Fatalf("%s folded %s: %v", model, name, values)
			}
		}
	}
	// An unlisted model keeps the gateway's existing behavior.
	if decision := plan.Decide("gpt-6-nova"); decision.Configured() || decision.Source != SourceNone {
		t.Fatalf("unlisted model = %+v", decision)
	}
	// An empty "body" migrates, but any field must be refused rather than look
	// like it applied.
	withBody := connection
	withBody.ModelRules = append([]contract.ModelRule(nil), connection.ModelRules...)
	withBody.ModelRules[0].Body = map[string]json.RawMessage{"stream": json.RawMessage("false")}
	if err := withBody.Validate("service_relay"); err == nil {
		t.Fatal("a body rewrite was accepted")
	}
}
