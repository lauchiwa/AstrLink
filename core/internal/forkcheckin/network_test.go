package forkcheckin

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
)

func networkSnapshot(t *testing.T, address string, credential NetworkCredential) AccountSnapshot {
	t.Helper()
	account := connectedAccount()
	account.DashboardBaseURL = address
	encoded, err := EncodeNetworkCredential(credential)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { clear(encoded) })
	return AccountSnapshot{Account: account, Credential: encoded}
}

func networkFactory(t *testing.T, config networkConfig) *TransportFactory {
	t.Helper()
	factory := newTransportFactory(config)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := factory.Close(ctx); err != nil {
			t.Errorf("close factory: %v", err)
		}
	})
	return factory
}

func networkClient(t *testing.T, factory *TransportFactory, snapshot AccountSnapshot) *AccountClient {
	t.Helper()
	client, err := factory.Client(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func networkGet(t *testing.T, client *AccountClient, address string) string {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func networkRoots(server *httptest.Server) *x509.CertPool {
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return roots
}

func TestNetworkDirectScopesSessionAndFinalIdentity(t *testing.T) {
	var hits atomic.Int32
	expires := time.Now().Add(-time.Hour)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("Authorization") != "Bearer dashboard-access" || r.Header.Get("New-Api-User") != "7" {
			t.Error("dashboard session was not applied independently of the caller's API key")
		}
		cookies := r.Cookies()
		if len(cookies) != 2 || cookies[0].Name != "scoped" || cookies[1].Name != "session" {
			t.Error("cookie path, expiry filtering or longest-path-first ordering failed")
		}
		for name := range r.Header {
			lower := strings.ToLower(name)
			if strings.HasPrefix(lower, "x-astrlink-") || lower == "originator" || lower == "via" || lower == "x-powered-by" || lower == "proxy-authorization" || lower == "idempotency-key" || lower == "x-hidden" {
				t.Errorf("forbidden outgoing header %s", name)
			}
		}
		if strings.Contains(strings.ToLower(r.UserAgent()), "astrlink") {
			t.Error("gateway-branded User-Agent reached site")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"note":"AstrLink caller content"}` {
			t.Error("caller-authored body was rewritten")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer server.Close()
	original := http.DefaultTransport
	originalProxy := reflect.ValueOf(original.(*http.Transport).Proxy).Pointer()
	factory := networkFactory(t, networkConfig{roots: networkRoots(server)})
	credential := NetworkCredential{Version: 1, Bearer: "dashboard-access", Cookies: []SessionCookie{
		{Name: "session", Value: "private", Path: "/", Secure: true, HTTPOnly: true},
		{Name: "scoped", Value: "private", Path: "/app"},
		{Name: "expired", Value: "private", Path: "/", Expires: &expires},
		{Name: "other", Value: "private", Path: "/elsewhere"},
	}}
	client := networkClient(t, factory, networkSnapshot(t, server.URL+"/app", credential))
	if hits.Load() != 0 {
		t.Fatal("client construction contacted the site")
	}
	request, _ := http.NewRequest(http.MethodPost, server.URL+"/app/checkin", strings.NewReader(`{"note":"AstrLink caller content"}`))
	request.Header = http.Header{
		"Authorization": {"Bearer inference-api-key"}, "Cookie": {"injected=wrong"}, "Proxy-Authorization": {"secret"},
		"User-Agent": {"AstrLink"}, "Originator": {"AstrLink"}, "Via": {"AstrLink"}, "X-Powered-By": {"AstrLink"},
		"x-AsTrLiNk-private": {"local-only"}, "Connection": {"X-Hidden"}, "X-Hidden": {"local"},
		"Idempotency-Key": {"local-write-receipt"}, "New-Api-User": {"7"},
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if request.Header.Get("Authorization") != "Bearer inference-api-key" || request.GetBody == nil {
		t.Fatal("client mutated the caller-owned request")
	}
	if http.DefaultTransport != original || reflect.ValueOf(original.(*http.Transport).Proxy).Pointer() != originalProxy {
		t.Fatal("factory changed global outbound transport")
	}
}

func TestNetworkModesCaptureExitAndNeverFallback(t *testing.T) {
	var directHits, proxyHits atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directHits.Add(1)
		_, _ = io.WriteString(w, "direct")
	}))
	defer origin.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits.Add(1)
		if !r.URL.IsAbs() {
			t.Error("request did not reach the configured HTTP proxy")
		}
		if r.Header.Get("Proxy-Authorization") != "" && r.Header.Get("Proxy-Authorization") != "Basic "+base64.StdEncoding.EncodeToString([]byte("proxy-user:proxy-password-canary")) {
			t.Error("unexpected proxy authentication")
		}
		_, _ = io.WriteString(w, "proxy")
	}))
	defer proxy.Close()
	proxyURL, _ := url.Parse(proxy.URL)
	var systemFailure atomic.Bool
	var selections atomic.Int32
	factory := networkFactory(t, networkConfig{selectProxy: func(mode string) (networkproxy.ProxyFunc, error) {
		if mode != "system" {
			return networkproxy.New(mode)
		}
		return func(*http.Request) (*url.URL, error) {
			selections.Add(1)
			if systemFailure.Load() {
				return nil, errors.New("system query secret-canary")
			}
			return proxyURL, nil
		}, nil
	}})
	// A poisoned environment must not override direct or custom policies.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	snapshot := networkSnapshot(t, origin.URL, NetworkCredential{Version: 1, Bearer: "dashboard-token"})
	if got := networkGet(t, networkClient(t, factory, snapshot), origin.URL); got != "direct" {
		t.Fatal("direct mode used a proxy")
	}
	snapshot.Account.Revision++
	snapshot.Account.Network.Mode = NetworkModeSystem
	system := networkClient(t, factory, snapshot)
	if selections.Load() != 0 {
		t.Fatal("constructing a client performed system discovery")
	}
	if got := networkGet(t, system, origin.URL); got != "proxy" {
		t.Fatal("system mode ignored its shared selector")
	}
	systemFailure.Store(true)
	request, _ := http.NewRequest(http.MethodGet, origin.URL+"?session=error-canary", nil)
	if _, err := system.Do(request); !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), "canary") {
		t.Fatalf("unsafe system failure classification: %v", err)
	}
	credential := NetworkCredential{Version: 1, Bearer: "dashboard-token", Proxy: &contract.ProxyCredential{Username: "proxy-user", Password: "proxy-password-canary"}}
	customSnapshot := networkSnapshot(t, origin.URL, credential)
	customSnapshot.Account.Revision = 3
	customSnapshot.Account.Network = Network{Mode: NetworkModeCustom, ProxyURL: proxy.URL}
	custom := networkClient(t, factory, customSnapshot)
	if got := networkGet(t, custom, origin.URL); got != "proxy" {
		t.Fatal("custom mode ignored its own proxy")
	}
	proxy.Close()
	if _, err := custom.Do(request); !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), "canary") {
		t.Fatalf("unsafe proxy failure classification: %v", err)
	}
	if directHits.Load() != 1 || proxyHits.Load() != 2 {
		t.Fatalf("proxy failure fell back or replayed: direct=%d proxy=%d", directHits.Load(), proxyHits.Load())
	}
}

// CONNECT tunnels are real sockets, not a fake RoundTripper. The destination
// is fixed to a local fixture; this test helper is not a general-purpose proxy.
func networkTunnel(t *testing.T, target, authorization string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect || r.Host != target || r.Header.Get("Proxy-Authorization") != authorization {
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		hits.Add(1)
		upstream, err := net.DialTimeout("tcp", target, time.Second)
		if err != nil {
			t.Error("fixture tunnel could not connect")
			return
		}
		downstream, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			t.Error("fixture tunnel could not hijack")
			return
		}
		defer upstream.Close()
		defer downstream.Close()
		_, _ = io.WriteString(downstream, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() {
			_, _ = io.Copy(upstream, buffered)
			upstream.Close()
			close(done)
		}()
		_, _ = io.Copy(downstream, upstream)
		downstream.Close()
		<-done
	}))
}

func TestNetworkHTTP2ResolvesSystemRouteBeforePoolReuse(t *testing.T) {
	var originHits, firstHits, secondHits atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		originHits.Add(1)
		if r.ProtoMajor != 2 || r.Header.Get("Proxy-Authorization") != "" {
			t.Error("expected HTTP/2 without proxy credentials at the site")
		}
		_, _ = io.WriteString(w, "h2")
	}))
	origin.EnableHTTP2 = true
	origin.StartTLS()
	defer origin.Close()
	target, _ := url.Parse(origin.URL)
	first := networkTunnel(t, target.Host, "", &firstHits)
	second := networkTunnel(t, target.Host, "", &secondHits)
	defer first.Close()
	defer second.Close()
	firstURL, _ := url.Parse(first.URL)
	secondURL, _ := url.Parse(second.URL)
	var route atomic.Int32
	factory := networkFactory(t, networkConfig{roots: networkRoots(origin), selectProxy: func(mode string) (networkproxy.ProxyFunc, error) {
		if mode != "system" {
			return networkproxy.New(mode)
		}
		return func(*http.Request) (*url.URL, error) {
			switch route.Load() {
			case 0:
				return firstURL, nil
			case 1:
				return secondURL, nil
			case 2:
				return nil, errors.New("system discovery unavailable")
			default:
				return nil, nil
			}
		}, nil
	}})
	snapshot := networkSnapshot(t, origin.URL, NetworkCredential{Version: 1, Bearer: "token"})
	snapshot.Account.Network.Mode = NetworkModeSystem
	client := networkClient(t, factory, snapshot)
	networkGet(t, client, origin.URL)
	networkGet(t, client, origin.URL)
	if firstHits.Load() != 1 {
		t.Fatal("same-route client did not reuse its own pool")
	}
	route.Store(1)
	networkGet(t, client, origin.URL)
	route.Store(2)
	request, _ := http.NewRequest(http.MethodGet, origin.URL, nil)
	if _, err := client.Do(request); !errors.Is(err, ErrNetwork) {
		t.Fatalf("HTTP/2 reused a stale proxy after discovery failed: %v", err)
	}
	route.Store(3)
	networkGet(t, client, origin.URL)
	if firstHits.Load() != 1 || secondHits.Load() != 1 || originHits.Load() != 4 {
		t.Fatalf("incorrect route capture: first=%d second=%d origin=%d", firstHits.Load(), secondHits.Load(), originHits.Load())
	}
	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestNetworkAuthenticatedCONNECTAndRefusal(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Proxy-Authorization") != "" || r.Header.Get("Authorization") != "Bearer site-token" {
			t.Error("proxy and site credentials were mixed")
		}
		_, _ = io.WriteString(w, "ok")
	}))
	defer origin.Close()
	target, _ := url.Parse(origin.URL)
	var connects atomic.Int32
	proxy := networkTunnel(t, target.Host, "Basic "+base64.StdEncoding.EncodeToString([]byte("user:proxy-canary")), &connects)
	defer proxy.Close()
	factory := networkFactory(t, networkConfig{roots: networkRoots(origin)})
	snapshot := networkSnapshot(t, origin.URL, NetworkCredential{Version: 1, Bearer: "site-token", Proxy: &contract.ProxyCredential{Username: "user", Password: "proxy-canary"}})
	snapshot.Account.Network = Network{Mode: NetworkModeCustom, ProxyURL: proxy.URL}
	client := networkClient(t, factory, snapshot)
	networkGet(t, client, origin.URL)
	wrong := networkSnapshot(t, origin.URL, NetworkCredential{Version: 1, Bearer: "site-token", Proxy: &contract.ProxyCredential{Username: "user", Password: "wrong-canary"}})
	wrong.Account.Network = snapshot.Account.Network
	wrong.Account.Revision++
	client = networkClient(t, factory, wrong)
	request, _ := http.NewRequest(http.MethodGet, origin.URL, nil)
	if _, err := client.Do(request); !errors.Is(err, ErrNetwork) || strings.Contains(err.Error(), "canary") {
		t.Fatalf("CONNECT refusal leaked authentication or succeeded: %v", err)
	}
	if connects.Load() != 1 {
		t.Fatal("rejected proxy authentication fell back")
	}
	factory.Close(context.Background())
}

func TestNetworkTLSVerificationIsMandatory(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		_, _ = io.WriteString(w, "trusted")
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	snapshot := networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "private-canary"})
	untrusted := networkClient(t, networkFactory(t, networkConfig{}), snapshot)
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	if _, err := untrusted.Do(request); !errors.Is(err, ErrNetworkTLS) {
		t.Fatalf("untrusted TLS was not refused: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("credentials reached a server before certificate verification")
	}
	trusted := networkClient(t, networkFactory(t, networkConfig{roots: networkRoots(server)}), snapshot)
	if got := networkGet(t, trusted, server.URL); got != "trusted" {
		t.Fatal("explicit test CA did not validate")
	}
}

func TestNetworkResponseLimitsAndDeadlines(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/headers":
			<-r.Context().Done()
		case "/body":
			_, _ = io.WriteString(w, "x")
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		case "/gzip":
			w.Header().Set("Content-Encoding", "gzip")
			writer := gzip.NewWriter(w)
			_, _ = io.WriteString(writer, strings.Repeat("x", 256))
			writer.Close()
		case "/chunked":
			w.(http.Flusher).Flush()
			_, _ = io.WriteString(w, strings.Repeat("x", 64))
		default:
			_, _ = io.WriteString(w, strings.Repeat("x", 64))
		}
	}))
	defer server.Close()
	factory := networkFactory(t, networkConfig{timeout: 150 * time.Millisecond, maxResponse: 32})
	client := networkClient(t, factory, networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "token"}))
	for _, path := range []string{"/fixed", "/chunked", "/gzip", "/headers", "/body"} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+path, nil)
		_, err := client.Do(request)
		want := ErrResponseTooLarge
		if path == "/headers" || path == "/body" {
			want = context.DeadlineExceeded
		}
		if !errors.Is(err, want) {
			t.Fatalf("path %s: %v, want %v", path, err, want)
		}
	}
}

func TestNetworkRotationInvalidatesPoolAndCancelsInFlight(t *testing.T) {
	started := make(chan struct{})
	closed := make(chan string, 8)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/wait" {
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	}))
	server.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		if state == http.StateClosed {
			closed <- conn.RemoteAddr().String()
		}
	}
	server.Start()
	defer server.Close()
	factory := networkFactory(t, networkConfig{})
	snapshot := networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "old-session"})
	old := networkClient(t, factory, snapshot)
	if got := networkGet(t, old, server.URL); got != "Bearer old-session" {
		t.Fatal("old credential missing")
	}
	snapshot.Credential, _ = EncodeNetworkCredential(NetworkCredential{Version: 1, Bearer: "new-session"})
	defer clear(snapshot.Credential)
	snapshot.Account.Revision++
	current := networkClient(t, factory, snapshot)
	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("rotation did not close the old idle pool")
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	if _, err := old.Do(request); !errors.Is(err, ErrClientInvalidated) {
		t.Fatalf("old client survived rotation: %v", err)
	}
	if got := networkGet(t, current, server.URL); got != "Bearer new-session" {
		t.Fatal("new credential missing")
	}
	result := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/wait", nil)
		_, err := current.Do(request)
		result <- err
	}()
	<-started
	factory.Invalidate(snapshot.Account.ID)
	if err := <-result; !errors.Is(err, ErrClientInvalidated) {
		t.Fatalf("revocation did not cancel active I/O: %v", err)
	}
	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Client(snapshot); !errors.Is(err, ErrClientInvalidated) {
		t.Fatal("closed factory accepted new work")
	}
}

func TestNetworkCapacityAndAccountIsolation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	factory := networkFactory(t, networkConfig{maxAccounts: 2})
	one := networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "first"})
	two := networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "second"})
	two.Account.ID = "account_two"
	first, second := networkClient(t, factory, one), networkClient(t, factory, two)
	var wg sync.WaitGroup
	for index := 0; index < 10; index++ {
		for _, test := range []struct {
			client *AccountClient
			want   string
		}{{first, "Bearer first"}, {second, "Bearer second"}} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if got := networkGet(t, test.client, server.URL); got != test.want {
					t.Error("account sessions crossed")
				}
			}()
		}
	}
	wg.Wait()
	third := one
	third.Account.ID = "account_three"
	if _, err := factory.Client(third); !errors.Is(err, ErrClientCapacity) {
		t.Fatal("unbounded account pools")
	}
	factory.Invalidate(one.Account.ID)
	networkClient(t, factory, third)
}

func TestNetworkPrivateSnapshotsCannotBePublished(t *testing.T) {
	credential := NetworkCredential{Version: 1, Bearer: "private-canary"}
	snapshot := networkSnapshot(t, "https://relay.example", credential)
	for _, value := range []any{snapshot, credential} {
		if _, err := json.Marshal(value); !errors.Is(err, ErrCredentialUnavailable) {
			t.Fatal("private snapshot was JSON serializable")
		}
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(format, value), "private-canary") {
				t.Fatal("private snapshot leaked through fmt")
			}
		}
	}
	// Explicit Vault serialization is intentionally still available.
	encoded, err := EncodeNetworkCredential(credential)
	defer clear(encoded)
	if err != nil || !bytes.Contains(encoded, []byte("private-canary")) {
		t.Fatal("explicit Vault codec did not encode the credential")
	}
}
