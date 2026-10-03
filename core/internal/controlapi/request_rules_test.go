package controlapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

// migratedServiceBody is the operator's existing configuration as it arrives
// over the control API: lowercase header names and an empty "body" per rule.
const migratedServiceBody = `{
	"name":"relay","kind":"openai_compatible",
	"http":{
		"base_url":"https://relay.example.test/v1",
		"auth":{"scheme":"bearer"},
		"credential":{"secret":"relay-secret-value"},
		"model_rules":[
			{"match":"gpt-5.6-luna","headers":{"originator":"codex_exec","user-agent":"codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"},"body":{}},
			{"match":"gpt-6-luna","headers":{"originator":"codex_exec","user-agent":"codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"},"body":{}},
			{"match":"gpt-6-astra","headers":{"originator":"codex_exec","user-agent":"codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"},"body":{}}
		]
	},
	"capabilities":[{"protocol":"openai.responses","mode":"native","streaming":true}]
}`

func TestServiceAcceptsMigratedModelRulesAndReadsThemBack(t *testing.T) {
	const userAgent = "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"
	_, handler := newServiceHandler(t, "service_relay")
	service := createServiceForTest(t, handler, migratedServiceBody)
	if service.HTTP == nil || len(service.HTTP.ModelRules) != 3 {
		t.Fatalf("rules were dropped on create: %+v", service.HTTP)
	}
	for index, model := range []string{"gpt-5.6-luna", "gpt-6-luna", "gpt-6-astra"} {
		rule := service.HTTP.ModelRules[index]
		// Order is preserved and values survive the round trip byte for byte.
		if rule.Match != model || rule.Headers["originator"] != "codex_exec" || rule.Headers["user-agent"] != userAgent {
			t.Fatalf("rule %d = %+v", index, rule)
		}
		if !rule.Active() {
			t.Fatalf("rule %d was saved disabled", index)
		}
	}

	read := serviceRequestForTest(t, handler, http.MethodGet, ServicesPath+"/"+string(service.ID), "", "", "")
	if read.Code != http.StatusOK {
		t.Fatalf("read: %d %s", read.Code, read.Body.String())
	}
	var stored contract.Service
	decode(t, read, &stored)
	if stored.HTTP == nil || len(stored.HTTP.ModelRules) != 3 {
		t.Fatalf("rules were dropped on read: %+v", stored.HTTP)
	}
	// The saved credential must never come back with the configuration.
	if strings.Contains(read.Body.String(), "relay-secret-value") {
		t.Fatal("service read returned the stored credential")
	}
}

func TestServiceRejectsConfigurationThatReachesGatewayOwnedHeaders(t *testing.T) {
	_, handler := newServiceHandler(t, "service_a", "service_b", "service_c", "service_d", "service_e", "service_f", "service_g")
	for _, test := range []struct{ name, httpBody string }{
		{"credential header", `"model_rules":[{"match":"*","headers":{"Authorization":"Bearer leaked"}}]`},
		{"bearer scheme header", `"extra_headers":{"authorization":"Bearer leaked"}`},
		{"framing header", `"extra_headers":{"Content-Length":"12"}`},
		{"session header", `"model_rules":[{"match":"*","headers":{"Session-Id":"pinned"}}]`},
		{"gateway namespace", `"extra_headers":{"X-AstrLink-Debug":"on"}`},
		{"body rewrite", `"model_rules":[{"match":"*","body":{"stream":false}}]`},
		{"unsupported glob", `"model_rules":[{"match":"gpt-6-*","headers":{"Originator":"codex_exec"}}]`},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{
				"name":"relay","kind":"openai_compatible",
				"http":{"base_url":"https://relay.example.test/v1","auth":{"scheme":"bearer"},
					"credential":{"secret":"relay-secret-value"},` + test.httpBody + `},
				"capabilities":[{"protocol":"openai.responses","mode":"native","streaming":true}]
			}`
			response := serviceRequestForTest(t, handler, http.MethodPost, ServicesPath, "application/json", body, "")
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("accepted %s: %d %s", test.name, response.Code, response.Body.String())
			}
		})
	}
}

// A custom auth header is named by the service itself, so the reserved set
// cannot be a fixed list: the validator must consult this service's own auth.
func TestCustomAuthHeaderCannotBeConfiguredEvenWhenNamedByTheService(t *testing.T) {
	_, handler := newServiceHandler(t, "service_relay", "service_other")
	customAuth := `{"name":"relay","kind":"openai_compatible","http":{"base_url":"https://relay.example.test/v1",` +
		`"auth":{"scheme":"custom_header","header_name":"X-Relay-Token"},"credential":{"secret":"relay-secret-value"},` +
		`"extra_headers":{"X-Relay-Token":"attacker-supplied"}},` +
		`"capabilities":[{"protocol":"openai.chat","mode":"native","streaming":true}]}`
	response := serviceRequestForTest(t, handler, http.MethodPost, ServicesPath, "application/json", customAuth, "")
	if response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("a service's own auth header was configurable: %d %s", response.Code, response.Body.String())
	}

	// The same name is ordinary configuration for a service that does not use it
	// for authentication, so ownership is per-service rather than global.
	bearerAuth := `{"name":"other","kind":"openai_compatible","http":{"base_url":"https://other.example.test/v1",` +
		`"auth":{"scheme":"bearer"},"credential":{"secret":"other-secret-value"},` +
		`"extra_headers":{"X-Relay-Token":"compat-value"}},` +
		`"capabilities":[{"protocol":"openai.chat","mode":"native","streaming":true}]}`
	accepted := serviceRequestForTest(t, handler, http.MethodPost, ServicesPath, "application/json", bearerAuth, "")
	if accepted.Code != http.StatusCreated {
		t.Fatalf("an overridable header was refused: %d %s", accepted.Code, accepted.Body.String())
	}

	// Switching that service to custom_header auth for the configured name must
	// now be refused, so a later auth change cannot expose the credential slot.
	var other contract.Service
	decode(t, accepted, &other)
	path := ServicesPath + "/" + string(other.ID)
	read := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
	switched := serviceRequestForTest(
		t, handler, http.MethodPatch, path, "application/merge-patch+json",
		`{"http":{"auth":{"scheme":"custom_header","header_name":"X-Relay-Token"}}}`,
		read.Header().Get("ETag"),
	)
	if switched.Code != http.StatusUnprocessableEntity {
		t.Fatalf("auth change exposed a configured header: %d %s", switched.Code, switched.Body.String())
	}
}

func TestPatchKeepsOmittedRulesAndClearsExplicitEmptyOnes(t *testing.T) {
	_, handler := newServiceHandler(t, "service_relay")
	service := createServiceForTest(t, handler, migratedServiceBody)
	path := ServicesPath + "/" + string(service.ID)

	read := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
	etag := read.Header().Get("ETag")

	// An unrelated patch must not silently drop the saved configuration.
	patched := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", `{"name":"relay renamed"}`, etag)
	if patched.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", patched.Code, patched.Body.String())
	}
	var renamed contract.Service
	decode(t, patched, &renamed)
	if renamed.HTTP == nil || len(renamed.HTTP.ModelRules) != 3 {
		t.Fatalf("an unrelated patch dropped the rules: %+v", renamed.HTTP)
	}

	// Patching only base_url must also keep them.
	patched = serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json",
		`{"http":{"base_url":"https://relay2.example.test/v1"}}`, patched.Header().Get("ETag"))
	if patched.Code != http.StatusOK {
		t.Fatalf("base_url patch: %d %s", patched.Code, patched.Body.String())
	}
	decode(t, patched, &renamed)
	if renamed.HTTP == nil || len(renamed.HTTP.ModelRules) != 3 || renamed.HTTP.BaseURL != "https://relay2.example.test/v1" {
		t.Fatalf("base_url patch changed the rules: %+v", renamed.HTTP)
	}

	// An explicit empty list clears them, which is how the UI removes every rule.
	for _, body := range []string{`{"http":{"model_rules":[]}}`, `{"http":{"model_rules":null}}`} {
		cleared := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", body, patched.Header().Get("ETag"))
		if cleared.Code != http.StatusOK {
			t.Fatalf("clear %s: %d %s", body, cleared.Code, cleared.Body.String())
		}
		var empty contract.Service
		decode(t, cleared, &empty)
		if empty.HTTP == nil || len(empty.HTTP.ModelRules) != 0 {
			t.Fatalf("clear %s left rules: %+v", body, empty.HTTP)
		}
		// Restore for the next iteration.
		patched = serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json",
			`{"http":{"model_rules":[{"match":"gpt-6-astra","headers":{"Originator":"codex_exec"}}]}}`,
			cleared.Header().Get("ETag"))
		if patched.Code != http.StatusOK {
			t.Fatalf("restore: %d %s", patched.Code, patched.Body.String())
		}
	}

	// An invalid patch must be refused without changing the saved document.
	before := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
	var saved contract.Service
	decode(t, before, &saved)
	rejected := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json",
		`{"http":{"extra_headers":{"Authorization":"Bearer leaked"}}}`, before.Header().Get("ETag"))
	if rejected.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid patch: %d %s", rejected.Code, rejected.Body.String())
	}
	after := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
	var unchanged contract.Service
	decode(t, after, &unchanged)
	if after.Header().Get("ETag") != before.Header().Get("ETag") ||
		len(unchanged.HTTP.ModelRules) != len(saved.HTTP.ModelRules) ||
		len(unchanged.HTTP.ExtraHeaders) != 0 {
		t.Fatalf("a rejected patch changed the service: %+v", unchanged.HTTP)
	}
}

func TestConfigurationIsOperatorOnlyOnRead(t *testing.T) {
	_, handler := newServiceHandler(t, "service_relay")
	service := createServiceForTest(t, handler, migratedServiceBody)
	path := ServicesPath + "/" + string(service.ID)

	// Both the single read and the collection listing must redact: a role check
	// that only covers the detail route still leaks the whole list. Assertions
	// run on decoded values, because the JSON encoder escapes the marker's
	// angle brackets and a raw substring check would pass for the wrong reason.
	for _, route := range []string{path, ServicesPath} {
		request := httptest.NewRequest(http.MethodGet, route, nil)
		request.Header.Set("Authorization", "Bearer "+testObserverToken)
		observer := httptest.NewRecorder()
		handler.ServeHTTP(observer, request)
		if observer.Code != http.StatusOK {
			t.Fatalf("observer read %s: %d %s", route, observer.Code, observer.Body.String())
		}
		var seen contract.Service
		if route == ServicesPath {
			var page struct {
				Items []contract.Service `json:"items"`
			}
			decode(t, observer, &page)
			if len(page.Items) != 1 {
				t.Fatalf("observer listing items=%d", len(page.Items))
			}
			seen = page.Items[0]
		} else {
			decode(t, observer, &seen)
		}
		if seen.HTTP == nil || len(seen.HTTP.ModelRules) != 3 {
			t.Fatalf("observer read %s lost the rules: %+v", route, seen.HTTP)
		}
		for index, rule := range seen.HTTP.ModelRules {
			// The shape stays visible: an observer can still see THAT this service
			// rewrites requests and which models it covers.
			if rule.Match == "" || len(rule.Headers) != 2 {
				t.Fatalf("observer read %s rule %d = %+v", route, index, rule)
			}
			for _, name := range []string{"originator", "user-agent"} {
				// A configured value is chosen because it makes an upstream accept a
				// request, so it is as sensitive as the requests it shapes.
				if rule.Headers[name] != contract.RedactedConfiguredValue {
					t.Fatalf("observer read %s rule %d header %s = %q",
						route, index, name, rule.Headers[name])
				}
			}
		}
		if wire := observer.Body.String(); strings.Contains(wire, "codex_exec") ||
			strings.Contains(wire, "WindowsTerminal") || strings.Contains(wire, "0.156.0") {
			t.Fatalf("observer read %s returned configured values: %s", route, wire)
		}
	}

	// The operator still reads real values, otherwise the editor could not
	// round-trip a saved configuration.
	read := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
	var full contract.Service
	decode(t, read, &full)
	if full.HTTP == nil || full.HTTP.ModelRules[0].Headers["originator"] != "codex_exec" {
		t.Fatalf("operator read was redacted: %s", read.Body.String())
	}
}
