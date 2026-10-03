package transport

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/gorilla/websocket"
)

// bindingUpstream answers every turn and records the headers of each handshake,
// so a test can tell a reused connection from a second dial.
func bindingUpstream(t *testing.T, handshakes *[]http.Header, dials, turns *atomic.Int32) *httptest.Server {
	t.Helper()
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		dials.Add(1)
		*handshakes = append(*handshakes, request.Header.Clone())
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			turns.Add(1)
			_ = conn.WriteJSON(map[string]any{
				"type":     "response.completed",
				"response": map[string]any{"id": "resp_bind", "status": "completed", "output": []any{}},
			})
		}
	}))
	t.Cleanup(server.Close)
	return server
}

// bindingTarget builds a target for an upstream the dialer can actually reach.
func bindingTarget(t *testing.T, serverURL string, overlay http.Header) Target {
	t.Helper()
	return Target{
		BaseURL:        mustParseURL(t, serverURL),
		Service:        contract.Service{ID: "service_socket"},
		RequestHeaders: overlay,
	}
}

// forwardTurn runs one turn to completion. observed reports whether
// ObserveOutbound fired, which must mean "this turn is being sent upstream".
func forwardTurn(
	t *testing.T,
	socket *ResponsesSocket,
	target Target,
	body string,
	inbound http.Header,
) (error, bool) {
	t.Helper()
	observed := false
	target.ObserveOutbound = func(*http.Request) { observed = true }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(body)).WithContext(ctx)
	for name, values := range inbound {
		for _, value := range values {
			request.Header.Add(name, value)
		}
	}
	return socket.Forward(httptest.NewRecorder(), request, target, "service_socket/test-model/"+target.BaseURL.String(), nil), observed
}

// TestSocketReuseIgnoresConfigurationStrippedBeforeHandshake pins the digest to
// the EFFECTIVE handshake configuration. A value the socket deletes before
// dialing cannot change the connection, so it must not force a reconnect: the
// turn would be refused while the already-open channel is still correct.
func TestSocketReuseIgnoresConfigurationStrippedBeforeHandshake(t *testing.T) {
	var handshakes []http.Header
	var dials, turns atomic.Int32
	upstream := bindingUpstream(t, &handshakes, &dials, &turns)
	socket := &ResponsesSocket{}
	defer socket.Close()

	first := bindingTarget(t, upstream.URL, http.Header{"Originator": {"codex_exec"}})
	if err, observed := forwardTurn(t, socket, first, `{"model":"test","input":"one"}`, nil); err != nil || !observed {
		t.Fatalf("first turn err=%v observed=%v", err, observed)
	}

	// Content negotiation and WebSocket handshake fields are deleted before the
	// dial, so adding them changes nothing that reaches upstream.
	second := bindingTarget(t, upstream.URL, http.Header{
		"Originator":            {"codex_exec"},
		"Accept":                {"application/json"},
		"Content-Type":          {"application/json"},
		"Accept-Encoding":       {"gzip"},
		"Sec-Websocket-Version": {"13"},
	})
	err, observed := forwardTurn(t, socket, second, `{"model":"test","input":"a completely different body"}`, nil)
	if err != nil {
		t.Fatalf("a stripped-header difference refused the turn: %v", err)
	}
	if !observed {
		t.Fatal("the second turn was not reported as sent upstream")
	}
	if dials.Load() != 1 {
		t.Fatalf("dials = %d, want the socket reused", dials.Load())
	}
	if turns.Load() != 2 {
		t.Fatalf("turns delivered = %d", turns.Load())
	}
}

// TestSocketReuseRefusesRealHandshakeChange is the other half: a value that does
// reach the handshake identifies a different channel, so reusing the open socket
// would send the turn under an identity the operator did not configure for it.
func TestSocketReuseRefusesRealHandshakeChange(t *testing.T) {
	for _, test := range []struct {
		name    string
		overlay http.Header
	}{
		{"identity", http.Header{"Originator": {"codex_exec"}, "User-Agent": {"codex-tui/0.156.0"}}},
		{"credential", http.Header{"Originator": {"codex_exec"}, "Authorization": {"Bearer second-secret-value"}}},
		{"removed", http.Header{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var handshakes []http.Header
			var dials, turns atomic.Int32
			upstream := bindingUpstream(t, &handshakes, &dials, &turns)
			socket := &ResponsesSocket{}
			defer socket.Close()

			first := bindingTarget(t, upstream.URL, http.Header{"Originator": {"codex_exec"}})
			if err, _ := forwardTurn(t, socket, first, `{"model":"test","input":"one"}`, nil); err != nil {
				t.Fatalf("first turn: %v", err)
			}
			changed := bindingTarget(t, upstream.URL, test.overlay)
			err, observed := forwardTurn(t, socket, changed, `{"model":"test","input":"two"}`, nil)
			if err == nil {
				t.Fatal("a changed handshake configuration reused the socket")
			}
			// A refused turn never reached this provider, so it must not be
			// reported as dispatched: that observation starts the attempt record
			// and arms identity capture.
			if observed {
				t.Fatal("a refused turn was reported as sent upstream")
			}
			if dials.Load() != 1 || turns.Load() != 1 {
				t.Fatalf("dials=%d turns=%d, want no silent reconnect", dials.Load(), turns.Load())
			}
		})
	}
}
