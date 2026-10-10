package transport

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// Check the emitted request, not just the intermediate overlay: local switches
// can arrive from either a caller or a provider adapter, in arbitrary casing.
func TestLocalControlsNeverReachHTTPOrWebSocketUpstream(t *testing.T) {
	for _, socketMode := range []bool{false, true} {
		name := "http"
		if socketMode {
			name = "websocket"
		}
		t.Run(name, func(t *testing.T) {
			received := make(chan http.Header, 1)
			const body = `{"model":"fixture","input":"AstrLink is caller-authored content"}`
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- r.Header.Clone()
				if socketMode {
					upgrader := websocket.Upgrader{}
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					_, payload, err := conn.ReadMessage()
					if err != nil {
						t.Error(err)
						return
					}
					if !strings.Contains(string(payload), "AstrLink is caller-authored content") {
						t.Error("caller content was removed")
					}
					_ = conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_local", "status": "completed", "output": []any{}}})
					_, _, _ = conn.ReadMessage()
					return
				}
				payload, err := io.ReadAll(r.Body)
				if err != nil || string(payload) != body {
					t.Error("caller content was changed")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{}`)
			}))
			defer upstream.Close()
			overlay := http.Header{
				"x-openai-actor-authorization": {"overlay-local-switch"},
				"X-AstrLink-Console":           {"overlay-local-console"},
				"x-astrlink-future":            {"overlay-local-marker"},
				"Authorization":                {"Bearer fixture-provider-credential"},
				"User-Agent":                   {"fixture-client/1.0"},
			}
			target := Target{BaseURL: mustParseURL(t, upstream.URL), RequestHeaders: overlay}
			request := httptest.NewRequest(http.MethodPost, "http://gateway.example/v1/responses", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer fixture-local-access-token")
			request.Header.Set("Cookie", "fixture-console-session=local")
			request.Header.Set(OpenAIActorAuthorizationHeader, "inbound-local-switch")
			request.Header["X-OPENAI-ACTOR-AUTHORIZATION"] = []string{"another-local-switch"}
			request.Header.Set("X-AstrLink-Console", "local-console")
			response := httptest.NewRecorder()
			if socketMode {
				socket := &ResponsesSocket{}
				defer socket.Close()
				if err := socket.Forward(response, request, target, "fixture", nil); err != nil {
					t.Fatal(err)
				}
			} else {
				forwarder := New(nil)
				defer forwarder.proxyTransport.(interface{ CloseIdleConnections() }).CloseIdleConnections()
				if err := forwarder.Forward(response, request, target); err != nil {
					t.Fatal(err)
				}
			}
			header := <-received
			for name := range header {
				if strings.HasPrefix(strings.ToLower(name), "x-astrlink-") || strings.EqualFold(name, OpenAIActorAuthorizationHeader) || strings.EqualFold(name, "Cookie") {
					t.Errorf("local header reached upstream: %s", name)
				}
			}
			if header.Get("Authorization") != "Bearer fixture-provider-credential" || header.Get("User-Agent") != "fixture-client/1.0" {
				t.Error("provider credential or client identity was changed")
			}
		})
	}
}
