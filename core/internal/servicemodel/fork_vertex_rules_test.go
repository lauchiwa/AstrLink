package servicemodel

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

type vertexProfileReader struct{ profile contract.IdentityProfile }

func (reader vertexProfileReader) GetIdentityProfile(_ context.Context, serviceID contract.ServiceID, id contract.IdentityProfileID) (storage.IdentityProfileRecord, error) {
	if reader.profile.ServiceID != serviceID || reader.profile.ID != id {
		return storage.IdentityProfileRecord{}, storage.ErrNotFound
	}
	return storage.IdentityProfileRecord{Profile: reader.profile.Clone()}, nil
}

func vertexServiceAccountSecret(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	secret, err := json.Marshal(map[string]string{"type": "service_account", "project_id": "vertex-test", "client_email": "test@vertex-test.iam.gserviceaccount.com", "private_key": string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))})
	if err != nil {
		t.Fatal(err)
	}
	return secret
}

// The editor's Vertex catalog probe carries the service's request rules, as
// inference does. Credential and project stay with the connection.
func TestVertexCatalogProbeAppliesServiceRules(t *testing.T) {
	fingerprint, err := accountauth.BuiltinIdentityFingerprint(contract.IdentityClientCodexCLI)
	if err != nil {
		t.Fatal(err)
	}
	confirmed := time.Now().UTC()
	profile := contract.IdentityProfile{ID: "identity_vertex", ServiceID: "service_vertex", Client: contract.IdentityClientCodexCLI, Source: contract.IdentityProfileBuiltin, Fingerprint: fingerprint, CreatedAt: confirmed, ConfirmedAt: &confirmed}
	secret := vertexServiceAccountSecret(t)
	vertex := &contract.VertexConnection{ProjectID: "vertex-test", Location: "global"}
	base := contract.HTTPConnection{BaseURL: vertex.BaseURL(), Auth: contract.ServiceAuth{Scheme: contract.AuthSchemeGoogleServiceAccount}, Vertex: vertex}

	t.Run("applied", func(t *testing.T) {
		calls := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			for name := range r.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Astrlink-") {
					t.Errorf("%s reached %s", name, r.URL.Host)
				}
			}
			if strings.Contains(strings.ToLower(r.Header.Get("User-Agent")), "astrlink") {
				t.Errorf("AstrLink User-Agent reached %s", r.URL.Host)
			}
			if r.URL.Host == "oauth2.googleapis.com" {
				return probeResponse(`{"access_token":"google-token","token_type":"Bearer","expires_in":3600}`), nil
			}
			calls++
			for name, want := range map[string]string{
				"Authorization":       "Bearer google-token",
				"X-Goog-User-Project": "vertex-test",
				"X-Relay-Client":      "fake-default-value",
				"User-Agent":          fingerprint.UserAgent,
				"Version":             fingerprint.Version,
			} {
				if got := r.Header.Values(name); len(got) != 1 || got[0] != want {
					t.Errorf("%s = %q, want %q", name, got, want)
				}
			}
			if r.Header.Get("X-Catch-All") != "" {
				t.Error("catalog probe applied the catch-all model rule")
			}
			return probeResponse(vertexCatalog), nil
		})}
		connection := base
		connection.IdentityProfileID = profile.ID
		connection.ExtraHeaders = map[string]string{"X-Relay-Client": "fake-default-value", "X-Goog-User-Project": "fake-rule-project"}
		connection.ModelRules = []contract.ModelRule{{Match: "*", Headers: map[string]string{"X-Catch-All": "fake-rule-value"}}}
		prober := NewWithDependencies(Dependencies{Client: client, IdentityProfiles: vertexProfileReader{profile}})
		got, err := prober.ProbeHTTP(context.Background(), profile.ServiceID, contract.ServiceKindVertex, connection, secret, contract.ProtocolOpenAIModels)
		if err != nil || len(got.Models) != 3 || calls != 1 {
			t.Fatalf("models = %v, calls = %d, error = %v", got, calls, err)
		}
	})

	t.Run("missing profile", func(t *testing.T) {
		requests := 0
		client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			return probeResponse(vertexCatalog), nil
		})}
		connection := base
		connection.IdentityProfileID = "identity_missing"
		prober := NewWithDependencies(Dependencies{Client: client, IdentityProfiles: vertexProfileReader{profile}})
		_, err := prober.ProbeHTTP(context.Background(), profile.ServiceID, contract.ServiceKindVertex, connection, secret, contract.ProtocolOpenAIModels)
		if !errors.Is(err, ErrConfiguration) || requests != 0 {
			t.Fatalf("requests = %d, error = %v", requests, err)
		}
	})
}
