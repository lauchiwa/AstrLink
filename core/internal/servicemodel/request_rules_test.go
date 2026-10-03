package servicemodel

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

func TestDiscoveryUsesDefaultsInsteadOfModelRulesOnEveryPage(t *testing.T) {
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.Header.Get("X-Relay-Client") != "fake-default-value" || request.Header.Get("User-Agent") != "FakeDiscoveryClient/1.0" {
			t.Fatalf("defaults missing: %#v", request.Header)
		}
		if request.Header.Get("Authorization") != "Bearer fake-probe-secret" {
			t.Fatal("credential changed")
		}
		if calls == 1 {
			return probeResponse(`{"data":[{"id":"first"}],"has_more":true,"last_id":"first"}`), nil
		}
		return probeResponse(`{"data":[{"id":"second"}],"has_more":false}`), nil
	})}
	models, err := New(nil, nil, client).ProbeHTTP(context.Background(), "service_test", contract.ServiceKindAnthropic,
		contract.HTTPConnection{
			BaseURL: "https://api.example/v1", Auth: contract.ServiceAuth{Scheme: contract.AuthSchemeBearer},
			ExtraHeaders: map[string]string{"X-Relay-Client": "fake-default-value", "User-Agent": "FakeDiscoveryClient/1.0"},
			ModelRules:   []contract.ModelRule{{Match: "*", Headers: map[string]string{"X-Relay-Client": "fake-rule-value"}, IdentityProfile: "identity_unused"}},
		}, []byte("fake-probe-secret"), contract.ProtocolOpenAIModels)
	if err != nil || len(models) != 2 || calls != 2 {
		t.Fatalf("models = %v, calls = %d, error = %v", models, calls, err)
	}
}

func TestDiscoveryRejectsUnusableCompatibilityBeforeSending(t *testing.T) {
	for _, test := range []struct {
		name       string
		connection contract.HTTPConnection
		wantError  error
	}{
		{"unavailable profile", contract.HTTPConnection{IdentityProfileID: "identity_missing"}, ErrConfiguration},
		{"credential override", contract.HTTPConnection{ExtraHeaders: map[string]string{"Authorization": "fake-override"}}, ErrUnsupported},
		{"invalid model rule", contract.HTTPConnection{ModelRules: []contract.ModelRule{{Match: "model-*"}}}, ErrUnsupported},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
				calls++
				return probeResponse(`{"data":[]}`), nil
			})}
			connection := test.connection
			connection.BaseURL = "https://api.example/v1"
			connection.Auth = contract.ServiceAuth{Scheme: contract.AuthSchemeNone}
			_, err := New(nil, nil, client).ProbeHTTP(context.Background(), "service_test", contract.ServiceKindOpenAI, connection, nil, contract.ProtocolOpenAIModels)
			if !errors.Is(err, test.wantError) || calls != 0 {
				t.Fatalf("calls = %d, error = %v", calls, err)
			}
		})
	}
}
