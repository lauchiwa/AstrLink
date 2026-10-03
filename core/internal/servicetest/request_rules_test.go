package servicetest

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/ingress"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

type testProfileReader struct{ profile contract.IdentityProfile }

func (reader testProfileReader) GetIdentityProfile(_ context.Context, serviceID contract.ServiceID, id contract.IdentityProfileID) (storage.IdentityProfileRecord, error) {
	if reader.profile.ServiceID != serviceID || reader.profile.ID != id {
		return storage.IdentityProfileRecord{}, storage.ErrNotFound
	}
	return storage.IdentityProfileRecord{Profile: reader.profile.Clone()}, nil
}

func TestConnectionTestAppliesCompatibilityAcrossNativeProtocols(t *testing.T) {
	fingerprint, err := accountauth.BuiltinIdentityFingerprint(contract.IdentityClientCodexCLI)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := time.Now().UTC()
	profile := contract.IdentityProfile{ID: "identity_test", ServiceID: "service_test", Client: contract.IdentityClientCodexCLI, Source: contract.IdentityProfileBuiltin, Fingerprint: fingerprint, CreatedAt: confirmed, ConfirmedAt: &confirmed}
	for _, test := range []struct {
		protocol contract.ProtocolID
		response string
	}{
		{contract.ProtocolOpenAIChat, `{"choices":[{"message":{"content":"OK"}}]}`},
		{contract.ProtocolOpenAIResponses, `{"status":"completed","output":[{"content":[{"type":"output_text","text":"OK"}]}]}`},
		{contract.ProtocolOpenAICompletions, `{"choices":[{"text":"OK"}]}`},
		{contract.ProtocolAnthropicMessages, `{"content":[{"type":"text","text":"OK"}]}`},
		{contract.ProtocolGoogleGenerateContent, `{"candidates":[{"content":{"parts":[{"text":"OK"}]}}]}`},
	} {
		t.Run(string(test.protocol), func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				for name, want := range map[string]string{"User-Agent": "FakeConnectionClient/1.0", "Version": fingerprint.Version, "X-Relay-Client": "fake-rule-value"} {
					if got := r.Header.Values(name); len(got) != 1 || got[0] != want {
						t.Errorf("%s = %q, want %q", name, got, want)
					}
				}
				if r.Header.Get("X-Default-Only") != "" {
					t.Error("matched rule inherited service defaults")
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, test.response)
			}))
			defer upstream.Close()
			service := contract.Service{ID: profile.ServiceID, Name: "Test provider", Kind: contract.ServiceKindOpenAICompatible,
				Capabilities: []contract.Capability{{Protocol: test.protocol, Mode: contract.CapabilityModeNative}},
				HTTP: &contract.HTTPConnection{BaseURL: upstream.URL, Auth: contract.ServiceAuth{Scheme: contract.AuthSchemeNone}, IdentityProfileID: profile.ID,
					ExtraHeaders: map[string]string{"X-Default-Only": "fake-default-value"},
					ModelRules:   []contract.ModelRule{{Match: "test-model", Headers: map[string]string{"User-Agent": "FakeConnectionClient/1.0", "X-Relay-Client": "fake-rule-value"}}},
				},
			}
			tester := NewWithDependencies(ingress.Dependencies{Authorizer: endpoint.NewServiceAuthorizer(nil, nil), IdentityProfiles: testProfileReader{profile}}, nil)
			result := tester.Test(context.Background(), service, contract.ServiceTestRequest{Protocol: test.protocol, Model: "test-model"})
			if !result.OK || result.Output != "OK" || calls.Load() != 1 {
				t.Fatalf("result = %+v, calls = %d", result, calls.Load())
			}
		})
	}
}

func TestConnectionTestFailsClosedForUnusableIdentity(t *testing.T) {
	fingerprint, err := accountauth.BuiltinIdentityFingerprint(contract.IdentityClientCodexCLI)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		reader accountauth.IdentityProfileReader
	}{
		{"storage unavailable", nil},
		{"missing", testProfileReader{}},
		{"unconfirmed", testProfileReader{contract.IdentityProfile{ID: "identity_test", ServiceID: "service_test", Client: contract.IdentityClientCodexCLI, Source: contract.IdentityProfileBuiltin, Fingerprint: fingerprint, CreatedAt: time.Now().UTC()}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer upstream.Close()
			service := contract.Service{ID: "service_test", Name: "Test provider", Kind: contract.ServiceKindOpenAICompatible,
				Capabilities: []contract.Capability{{Protocol: contract.ProtocolOpenAIChat, Mode: contract.CapabilityModeNative}},
				HTTP:         &contract.HTTPConnection{BaseURL: upstream.URL, Auth: contract.ServiceAuth{Scheme: contract.AuthSchemeNone}, IdentityProfileID: "identity_test"},
			}
			tester := NewWithDependencies(ingress.Dependencies{Authorizer: endpoint.NewServiceAuthorizer(nil, nil), IdentityProfiles: test.reader}, nil)
			result := tester.Test(context.Background(), service, contract.ServiceTestRequest{Protocol: contract.ProtocolOpenAIChat, Model: "test-model"})
			if result.OK || result.ErrorCode != "invalid_configuration" || calls.Load() != 0 {
				t.Fatalf("result = %+v, calls = %d", result, calls.Load())
			}
		})
	}
}
