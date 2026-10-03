package ingress

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/relaykitbridge"
)

func TestRequestRulesReachConvertedUpstreamRequests(t *testing.T) {
	const upstreamModel = "gpt-6-astra"
	profile := confirmedCodexProfile("service_conversion", "identity_conversion")
	for _, test := range []struct {
		kind              contract.ServiceKind
		ingress, upstream contract.ProtocolID
		auth              contract.AuthScheme
		path, keyHeader   string
		bodyField         string
	}{
		{contract.ServiceKindDeepSeek, contract.ProtocolOpenAIChat, contract.ProtocolAnthropicMessages, contract.AuthSchemeBearer, "/anthropic/v1/messages", "X-Api-Key", "messages"},
		{contract.ServiceKindGemini, contract.ProtocolOpenAIChat, contract.ProtocolGoogleGenerateContent, contract.AuthSchemeGoogleAPIKey, "/v1beta/models/" + upstreamModel + ":generateContent", "X-Goog-Api-Key", "contents"},
		{contract.ServiceKindOpenAICompatible, contract.ProtocolAnthropicMessages, contract.ProtocolOpenAIResponses, contract.AuthSchemeBearer, "/v1/responses", "Authorization", "input"},
		{contract.ServiceKindOpenAICompatible, contract.ProtocolOpenAIResponses, contract.ProtocolOpenAIChat, contract.AuthSchemeBearer, "/v1/chat/completions", "Authorization", "messages"},
	} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s-to-%s/stream=%t", test.ingress, test.upstream, streaming), func(t *testing.T) {
				var calls atomic.Int32
				upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					calls.Add(1)
					wantPath := test.path
					if test.upstream == contract.ProtocolGoogleGenerateContent && streaming {
						wantPath = strings.Replace(wantPath, ":generateContent", ":streamGenerateContent", 1)
						if request.URL.Query().Get("alt") != "sse" {
							t.Error("converted Gemini stream lost SSE query")
						}
					}
					if request.URL.Path != wantPath {
						t.Errorf("path = %s, want %s", request.URL.Path, wantPath)
					}
					for name, want := range map[string]string{
						"User-Agent": ruleUserAgent, "Originator": ruleOriginator,
						"Version": profile.Fingerprint.Version, "X-Relay-Rule": "upstream-model",
					} {
						if got := request.Header.Values(name); len(got) != 1 || got[0] != want {
							t.Errorf("%s = %q, want single %q", name, got, want)
						}
					}
					if request.Header.Get("X-Default-Only") != "" || request.Header.Get("X-AstrLink-Caller") != "" || request.Header.Get("Cookie") != "" {
						t.Error("converted request retained a replaced default or local header")
					}
					for _, name := range []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key"} {
						want := ""
						if name == test.keyHeader {
							want = "plan-key"
							if name == "Authorization" {
								want = "Bearer " + want
							}
						}
						if request.Header.Get(name) != want {
							t.Errorf("wrong effective upstream credential in %s", name)
						}
					}
					body, err := io.ReadAll(request.Body)
					var payload map[string]any
					if err != nil || json.Unmarshal(body, &payload) != nil || payload[test.bodyField] == nil || !strings.Contains(string(body), "hello") {
						t.Errorf("invalid converted body: %s, %v", body, err)
					}
					if test.upstream != contract.ProtocolGoogleGenerateContent && payload["model"] != upstreamModel {
						t.Errorf("model = %v, want %s", payload["model"], upstreamModel)
					}
					_, _, responseBody := nativeProviderExchange(test.upstream, streaming)
					if test.upstream == contract.ProtocolGoogleGenerateContent {
						responseBody = `{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP","index":0}]}`
						if streaming {
							responseBody = "data: " + responseBody + "\n\n"
						}
					}
					writer.Header().Set("Content-Type", "application/json")
					if streaming {
						writer.Header().Set("Content-Type", "text/event-stream")
					}
					_, _ = io.WriteString(writer, responseBody)
				}))
				defer upstream.Close()
				service := contract.Service{
					ID: profile.ServiceID, Name: "Conversion fixture", Kind: test.kind, Enabled: true,
					Models: []string{upstreamModel},
					Capabilities: []contract.Capability{
						{Protocol: test.upstream, Mode: contract.CapabilityModeNative, Streaming: true},
						{Protocol: test.ingress, Mode: contract.CapabilityModeNative, Streaming: true, ConvertTo: test.upstream},
					},
					HTTP: &contract.HTTPConnection{
						BaseURL: upstream.URL, Auth: contract.ServiceAuth{Scheme: test.auth}, CredentialRef: "local://service/service_conversion",
						IdentityProfileID: profile.ID, ExtraHeaders: map[string]string{"X-Default-Only": "fake-default-value"},
						ModelRules: []contract.ModelRule{
							{Match: "model-test", Headers: map[string]string{"X-Relay-Rule": "requested-model"}},
							{Match: "*", Headers: map[string]string{"X-Relay-Rule": "catch-all"}},
							{Match: upstreamModel, Headers: map[string]string{"X-Relay-Rule": "upstream-model", "User-Agent": ruleUserAgent, "Originator": ruleOriginator}},
						},
					},
				}
				if err := service.Validate(); err != nil {
					t.Fatal(err)
				}
				handler := NewWithDependencies(Dependencies{
					Resolver: candidateResolver{candidates: []endpoint.Resolved{{Service: service, BaseURL: upstream.URL,
						PlanType: contract.PlanTypeRelayKit, UpstreamProtocol: test.upstream, UpstreamModel: upstreamModel}}},
					Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, nil), ConversionEngine: relaykitbridge.NewEngine(),
					IdentityProfiles: &fixedProfiles{profiles: map[contract.IdentityProfileID]contract.IdentityProfile{profile.ID: profile}},
				})
				path, body, _ := nativeProviderExchange(test.ingress, streaming)
				request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("User-Agent", "FakeSourceClient/1.0")
				request.Header.Set("Originator", "fake_source")
				request.Header.Set("Authorization", "Bearer fake-caller-key")
				request.Header.Set("X-Api-Key", "fake-caller-key")
				request.Header.Set("X-Goog-Api-Key", "fake-caller-key")
				request.Header.Set("Cookie", "fake-caller-cookie")
				request.Header.Set("X-AstrLink-Caller", "local-only")
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusOK || calls.Load() != 1 {
					t.Fatalf("calls = %d, status = %d %s", calls.Load(), response.Code, response.Body.String())
				}
			})
		}
	}
}

func TestConvertedRequestFailsClosedForUnusableIdentity(t *testing.T) {
	for _, test := range []struct {
		name string
		http contract.HTTPConnection
	}{
		{"missing service binding", contract.HTTPConnection{IdentityProfileID: "identity_missing"}},
		{"missing rule binding", contract.HTTPConnection{ModelRules: []contract.ModelRule{{Match: "model-test", IdentityProfile: "identity_missing"}}}},
		{"reserved effective auth header", contract.HTTPConnection{ModelRules: []contract.ModelRule{{Match: "*", Headers: map[string]string{"X-Api-Key": "fake-override"}}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				calls.Add(1)
				writer.WriteHeader(http.StatusInternalServerError)
			}))
			defer upstream.Close()
			connection := test.http
			connection.BaseURL, connection.Auth, connection.CredentialRef = upstream.URL, contract.ServiceAuth{Scheme: contract.AuthSchemeBearer}, "local://service/service_conversion"
			service := contract.Service{
				ID: "service_conversion", Name: "Conversion fixture", Kind: contract.ServiceKindDeepSeek, Enabled: true,
				Models: []string{"model-test"}, HTTP: &connection,
				Capabilities: []contract.Capability{
					{Protocol: contract.ProtocolAnthropicMessages, Mode: contract.CapabilityModeNative},
					{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeNative, ConvertTo: contract.ProtocolAnthropicMessages},
				},
			}
			handler := NewWithDependencies(Dependencies{
				Resolver: candidateResolver{candidates: []endpoint.Resolved{{Service: service, BaseURL: upstream.URL,
					PlanType: contract.PlanTypeRelayKit, UpstreamProtocol: contract.ProtocolAnthropicMessages}}},
				Authorizer: endpoint.NewServiceAuthorizer(codingPlanCredentials{}, nil), ConversionEngine: relaykitbridge.NewEngine(),
				IdentityProfiles: &fixedProfiles{},
			})
			path, body, _ := nativeProviderExchange(contract.ProtocolOpenAIChat, false)
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code == http.StatusOK || calls.Load() != 0 || strings.Contains(response.Body.String(), "plan-key") || strings.Contains(response.Body.String(), "fake-override") {
				t.Fatalf("calls = %d, status = %d %s", calls.Load(), response.Code, response.Body.String())
			}
		})
	}
}
