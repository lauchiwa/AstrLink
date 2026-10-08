package forkcheckin

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
)

func TestNetworkSystemDiscoveryDeadlineAndCloseWait(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var dials atomic.Int32
	factory := networkFactory(t, networkConfig{
		timeout: 50 * time.Millisecond,
		dial: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, errors.New("unexpected dial")
		},
		selectProxy: func(string) (networkproxy.ProxyFunc, error) {
			return func(*http.Request) (*url.URL, error) {
				close(started)
				<-release // Models a native discovery API without cancellation.
				return nil, nil
			}, nil
		},
	})
	snapshot := networkSnapshot(t, "https://relay.example", NetworkCredential{Version: 1, Bearer: "token"})
	snapshot.Account.Network.Mode = NetworkModeSystem
	client := networkClient(t, factory, snapshot)
	result := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, snapshot.Account.DashboardBaseURL, nil)
		_, err := client.Do(request)
		result <- err
	}()
	<-started
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("system discovery ignored Do deadline: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Do blocked on an uncancellable system query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := factory.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close claimed workers stopped while a native query was active: %v", err)
	}
	releaseOnce.Do(func() { close(release) })
	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 0 {
		t.Fatal("expired system query later started network I/O")
	}
}

func TestNetworkPOSTDisconnectIsNeverReplayed(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, "warm pool")
			return
		}
		posts.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		connection, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		connection.Close() // The server accepted the POST but lost the response.
	}))
	defer server.Close()
	factory := networkFactory(t, networkConfig{})
	client := networkClient(t, factory, networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "token"}))
	networkGet(t, client, server.URL)
	request, _ := http.NewRequest(http.MethodPost, server.URL, strings.NewReader(`{"checkin":true}`))
	request.Header.Set("Idempotency-Key", "caller-retry-key")
	request.Header.Set("X-Idempotency-Key", "caller-retry-key")
	if request.GetBody == nil {
		t.Fatal("fixture must be replayable before the client removes that capability")
	}
	if _, err := client.Do(request); !errors.Is(err, ErrNetwork) {
		t.Fatalf("dropped response was not a network failure: %v", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("POST was replayed %d times", posts.Load())
	}
}

func TestNetworkCredentialOnlyRotationAndStaleRevision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Authorization"))
	}))
	defer server.Close()
	factory := networkFactory(t, networkConfig{})
	snapshot := networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "first"})
	old := networkClient(t, factory, snapshot)
	snapshot.Credential, _ = EncodeNetworkCredential(NetworkCredential{Version: 1, Bearer: "second"})
	defer clear(snapshot.Credential)
	current := networkClient(t, factory, snapshot)
	if old == current || networkGet(t, current, server.URL) != "Bearer second" {
		t.Fatal("credential-only change did not replace the client")
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
	if _, err := old.Do(request); !errors.Is(err, ErrClientInvalidated) {
		t.Fatalf("old credential client is still active: %v", err)
	}
	snapshot.Account.Revision = 2
	networkClient(t, factory, snapshot)
	snapshot.Account.Revision = 1
	if _, err := factory.Client(snapshot); !errors.Is(err, ErrRevisionChanged) {
		t.Fatal("an older account revision replaced the current client")
	}
}

func TestNetworkGlobalAdmissionIsBoundedAndCancellable(t *testing.T) {
	started := make(chan struct{}, maxNetworkConcurrent+1)
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	factory := networkFactory(t, networkConfig{})
	results := make(chan error, maxNetworkConcurrent+1)
	for index := 0; index <= maxNetworkConcurrent; index++ {
		snapshot := networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "token"})
		snapshot.Account.ID = AccountID("account_" + string(rune('a'+index)))
		client := networkClient(t, factory, snapshot)
		go func() {
			request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
			_, err := client.Do(request)
			results <- err
		}()
	}
	for index := 0; index < maxNetworkConcurrent; index++ {
		select {
		case <-started:
		case <-time.After(3 * time.Second):
			t.Fatal("requests did not reach the admission limit")
		}
	}
	if len(factory.slots) != maxNetworkConcurrent {
		t.Fatal("admission accounting is not bounded")
	}
	if err := factory.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= maxNetworkConcurrent; index++ {
		if err := <-results; !errors.Is(err, ErrClientInvalidated) {
			t.Fatalf("admitted or queued request did not stop: %v", err)
		}
	}
	if hits.Load() > maxNetworkConcurrent {
		t.Fatal("queued request bypassed admission while closing")
	}
}

func TestNetworkHTTPSProxyValidatesProxyTLS(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if !r.URL.IsAbs() || r.Header.Get("Proxy-Authorization") == "" {
			t.Error("HTTPS proxy did not receive its scoped authentication")
		}
		_, _ = io.WriteString(w, "secure-proxy")
	}))
	defer proxy.Close()
	snapshot := networkSnapshot(t, "http://localhost:12345", NetworkCredential{Version: 1, Bearer: "site-token", Proxy: &contract.ProxyCredential{Username: "user", Password: "proxy-password"}})
	snapshot.Account.Network = Network{Mode: NetworkModeCustom, ProxyURL: proxy.URL}
	client := networkClient(t, networkFactory(t, networkConfig{roots: networkRoots(proxy)}), snapshot)
	if got := networkGet(t, client, snapshot.Account.DashboardBaseURL); got != "secure-proxy" {
		t.Fatal("HTTPS proxy was not used")
	}
	untrusted := networkClient(t, networkFactory(t, networkConfig{}), snapshot)
	request, _ := http.NewRequest(http.MethodGet, snapshot.Account.DashboardBaseURL, nil)
	if _, err := untrusted.Do(request); !errors.Is(err, ErrNetworkTLS) {
		t.Fatalf("untrusted HTTPS proxy certificate accepted: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatal("untrusted proxy received credentials")
	}
}
