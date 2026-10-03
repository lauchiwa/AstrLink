package ingress

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/gorilla/websocket"
)

// TestResponsesWebSocketHandshakeCarriesModelRules checks the native WebSocket
// path against the same requirement as HTTP forwarding: the configured
// compatibility headers must reach the upstream handshake verbatim, and the
// caller's own client identity must not survive.
func TestResponsesWebSocketHandshakeCarriesModelRules(t *testing.T) {
	var dials atomic.Int32
	handshakes := make(chan http.Header, 2)
	upstream := wsUpstream(t, func(conn *websocket.Conn, request *http.Request) {
		dials.Add(1)
		handshakes <- request.Header.Clone()
		var event map[string]any
		if err := conn.ReadJSON(&event); err != nil {
			t.Error(err)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"type":     "response.completed",
			"response": map[string]any{"id": "resp_rules", "status": "completed", "output": []any{}},
		})
		_, _, _ = conn.ReadMessage()
	})

	candidate := wsCandidate(upstream.URL)
	candidate.UpstreamModel = "gpt-6-astra"
	candidate.Service.HTTP.ModelRules = migratedModelRules()
	handler := NewWithDependencies(Dependencies{
		Resolver: candidateResolver{candidates: []endpoint.Resolved{candidate}},
		AccessTokenAuthenticator: AccessTokenAuthenticatorFunc(
			func(context.Context, string) (contract.AccessTokenID, error) { return "token_test", nil },
		),
	})

	// The caller presents its own identity, which the matched rule replaces.
	client := dialResponses(t, handler, http.Header{
		"Authorization": {"Bearer local-token"},
		"User-Agent":    {"ExampleIDE/1.2.3"},
		"Originator":    {"example_ide"},
	})
	sendWS(t, client, `{"type":"response.create","model":"gpt-6-astra","input":"hello"}`)
	if event := readWS(t, client); event["type"] != "response.completed" {
		t.Fatalf("turn = %#v", event)
	}

	headers := <-handshakes
	const wantAgent = "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal (codex-tui; 0.156.0)"
	if got := headers.Values("Originator"); len(got) != 1 || got[0] != "codex_exec" {
		t.Fatalf("Originator = %#v", got)
	}
	if got := headers.Values("User-Agent"); len(got) != 1 || got[0] != wantAgent {
		t.Fatalf("User-Agent = %#v", got)
	}
	if got := headers.Get("Authorization"); got == "Bearer local-token" {
		t.Fatalf("the caller credential reached upstream: %q", got)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d", dials.Load())
	}
}

// TestResponsesWebSocketReconnectsWhenRulesChangeTheIdentity makes the digest
// observable from the ingress side: once a later turn resolves to a model whose
// rule sends a different identity, the open channel must be refused instead of
// carrying the turn under the previous one.
func TestResponsesWebSocketReconnectsWhenRulesChangeTheIdentity(t *testing.T) {
	var dials atomic.Int32
	upstream := wsUpstream(t, func(conn *websocket.Conn, _ *http.Request) {
		dials.Add(1)
		for {
			var event map[string]any
			if err := conn.ReadJSON(&event); err != nil {
				return
			}
			_ = conn.WriteJSON(map[string]any{
				"type":     "response.completed",
				"response": map[string]any{"id": "resp_switch", "status": "completed", "output": []any{}},
			})
		}
	})

	service := contract.ServiceFromEndpoint(validEndpoint(contract.ProtocolOpenAIResponses, true))
	service.Kind = contract.ServiceKindOpenAI
	service.HTTP.BaseURL = upstream.URL
	enabled := true
	service.ResponsesWebSocketEnabled = &enabled
	service.HTTP.ModelRules = []contract.ModelRule{
		{Match: "model-a", Headers: map[string]string{"Originator": "originator_a"}},
		{Match: "model-b", Headers: map[string]string{"Originator": "originator_b"}},
	}
	handler := NewWithDependencies(Dependencies{
		Resolver: resolverFunc(func(_ context.Context, request endpoint.ResolveRequest) (endpoint.Resolved, error) {
			return endpoint.Resolved{
				Service:       service,
				BaseURL:       upstream.URL,
				UpstreamModel: request.Model,
			}, nil
		}),
		AccessTokenAuthenticator: AccessTokenAuthenticatorFunc(
			func(context.Context, string) (contract.AccessTokenID, error) { return "token_test", nil },
		),
	})

	client := dialResponses(t, handler, http.Header{"Authorization": {"Bearer local-token"}})
	sendWS(t, client, `{"type":"response.create","model":"model-a","input":"one"}`)
	if event := readWS(t, client); event["type"] != "response.completed" {
		t.Fatalf("first turn = %#v", event)
	}

	// A different model selects a rule with a different identity. The session is
	// already bound to model-a, so this is refused rather than silently sent
	// under the previous identity.
	sendWS(t, client, `{"type":"response.create","model":"model-b","input":"two"}`)
	event := readWS(t, client)
	if event["type"] != "error" {
		t.Fatalf("a turn with a different identity was accepted on the bound channel: %#v", event)
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want no silent reconnect", dials.Load())
	}
}
