package servicemodel

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
)

// redirectTarget records what a redirect destination actually received, which
// is the only thing that matters here: an intermediate header check would not
// prove the credential stayed behind.
type redirectTarget struct {
	server *httptest.Server
	mu     sync.Mutex
	hits   int
	seen   []http.Header
}

func newRedirectTarget(t *testing.T, body string) *redirectTarget {
	t.Helper()
	target := &redirectTarget{}
	target.server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		target.mu.Lock()
		target.hits++
		target.seen = append(target.seen, request.Header.Clone())
		target.mu.Unlock()
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(body))
	}))
	t.Cleanup(target.server.Close)
	return target
}

func (target *redirectTarget) received() (int, []http.Header) {
	target.mu.Lock()
	defer target.mu.Unlock()
	return target.hits, append([]http.Header(nil), target.seen...)
}

// credentialHeaderNames covers every header authorizationHeaders can produce.
// Go's own client only strips Authorization and Cookie on a cross-origin
// redirect, so provider API-key schemes need an explicit policy.
var credentialHeaderNames = []string{"Authorization", "X-Api-Key", "X-Goog-Api-Key", "X-Relay-Token"}

func credentialValues(headers http.Header) []string {
	var found []string
	for _, name := range credentialHeaderNames {
		if value := headers.Get(name); value != "" {
			found = append(found, name+": "+value)
		}
	}
	return found
}

// TestModelDiscoveryDoesNotFollowCrossOriginRedirects pins the policy for every
// credential scheme the prober can send. A redirect to another origin must fail
// the probe rather than replay the credential somewhere the operator never
// configured.
func TestModelDiscoveryDoesNotFollowCrossOriginRedirects(t *testing.T) {
	for _, test := range []struct {
		name   string
		kind   contract.ServiceKind
		auth   contract.ServiceAuth
		secret string
	}{
		{"bearer", contract.ServiceKindOpenAI, contract.ServiceAuth{Scheme: contract.AuthSchemeBearer}, "bearer-secret-value"},
		{
			"anthropic_api_key", contract.ServiceKindAnthropic,
			contract.ServiceAuth{Scheme: contract.AuthSchemeAnthropicAPIKey}, "anthropic-secret-value",
		},
		{
			"google_api_key", contract.ServiceKindGemini,
			contract.ServiceAuth{Scheme: contract.AuthSchemeGoogleAPIKey}, "google-secret-value",
		},
		{
			"custom_header", contract.ServiceKindOpenAICompatible,
			contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: "X-Relay-Token"}, "custom-secret-value",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			attacker := newRedirectTarget(t, `{"data":[{"id":"attacker-model"}],"models":[]}`)
			origin := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				http.Redirect(writer, request, attacker.server.URL+request.URL.Path, http.StatusFound)
			}))
			defer origin.Close()

			protocol := contract.ProtocolOpenAIModels
			if test.kind == contract.ServiceKindGemini {
				protocol = contract.ProtocolGoogleModels
			}
			models, err := New(nil, nil, nil).ProbeHTTP(
				context.Background(), "service_probe", test.kind,
				contract.HTTPConnection{BaseURL: origin.URL + "/v1", Auth: test.auth},
				[]byte(test.secret), protocol,
			)
			if err == nil {
				t.Fatalf("a cross-origin redirect was followed: models = %v", models)
			}
			if !errors.Is(err, ErrUpstream) {
				t.Fatalf("error = %v, want ErrUpstream", err)
			}
			hits, seen := attacker.received()
			if hits != 0 {
				t.Fatalf("the redirect destination was contacted %d times with %v", hits, seen)
			}
			for _, headers := range seen {
				if leaked := credentialValues(headers); len(leaked) > 0 {
					t.Fatalf("credential reached another origin: %v", leaked)
				}
			}
			if strings.Contains(fmt.Sprint(err), test.secret) {
				t.Fatalf("probe error echoed the credential: %v", err)
			}
		})
	}
}

// TestModelDiscoveryRejectsHTTPSDowngrade covers the case a same-origin check
// alone would miss: same host, weaker scheme.
func TestModelDiscoveryRejectsHTTPSDowngrade(t *testing.T) {
	plaintext := newRedirectTarget(t, `{"data":[{"id":"downgraded"}]}`)
	// The redirect keeps the host and changes only the scheme, so a host-only
	// comparison would accept it.
	secure := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, plaintext.server.URL+request.URL.Path, http.StatusFound)
	}))
	defer secure.Close()

	prober := New(nil, nil, secure.Client())
	_, err := prober.ProbeHTTP(
		context.Background(), "service_probe", contract.ServiceKindOpenAI,
		contract.HTTPConnection{BaseURL: secure.URL + "/v1", Auth: contract.ServiceAuth{Scheme: contract.AuthSchemeBearer}},
		[]byte("downgrade-secret-value"), contract.ProtocolOpenAIModels,
	)
	if err == nil {
		t.Fatal("an HTTPS to HTTP downgrade was followed")
	}
	if hits, seen := plaintext.received(); hits != 0 {
		t.Fatalf("plaintext destination was contacted %d times with %v", hits, seen)
	}
}

// TestModelDiscoveryFollowsSameOriginRedirect keeps the policy usable: a relay
// that redirects within its own origin still discovers models, including on a
// later pagination request.
func TestModelDiscoveryFollowsSameOriginRedirect(t *testing.T) {
	var origin *httptest.Server
	pages := 0
	origin = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !strings.HasPrefix(request.URL.Path, "/moved") {
			http.Redirect(writer, request, origin.URL+"/moved"+request.URL.Path, http.StatusFound)
			return
		}
		pages++
		writer.Header().Set("Content-Type", "application/json")
		if pages == 1 {
			_, _ = writer.Write([]byte(`{"data":[{"id":"first"}],"has_more":true,"last_id":"first"}`))
			return
		}
		_, _ = writer.Write([]byte(`{"data":[{"id":"second"}],"has_more":false}`))
	}))
	defer origin.Close()

	models, err := New(nil, nil, nil).ProbeHTTP(
		context.Background(), "service_probe", contract.ServiceKindAnthropic,
		contract.HTTPConnection{BaseURL: origin.URL + "/v1", Auth: contract.ServiceAuth{Scheme: contract.AuthSchemeBearer}},
		[]byte("same-origin-secret-value"), contract.ProtocolOpenAIModels,
	)
	if err != nil {
		t.Fatalf("a same-origin redirect was refused: %v", err)
	}
	if got := strings.Join(models, ","); got != "first,second" {
		t.Fatalf("models = %q", got)
	}
	if pages != 2 {
		t.Fatalf("pages served = %d", pages)
	}
}
