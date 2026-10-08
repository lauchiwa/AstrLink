package forkcheckin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNetworkCookiesHonorPathSecureExpiryAndSnapshot(t *testing.T) {
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// A response cookie is not silently adopted into a frozen Vault
		// snapshot. A later authorizer must explicitly verify/store rotation.
		http.SetCookie(w, &http.Cookie{Name: "injected", Value: "new", Path: "/"})
		_, _ = io.WriteString(w, r.Header.Get("Authorization")+"|"+r.Header.Get("Cookie"))
	}))
	defer server.Close()
	factory := networkFactory(t, networkConfig{now: func() time.Time { return now }})
	credential := NetworkCredential{Version: 1, Bearer: "expiring", BearerExpires: &expires, Cookies: []SessionCookie{
		{Name: "plain", Value: "yes", Path: "/", HTTPOnly: true},
		{Name: "secure", Value: "no", Path: "/", Secure: true},
		{Name: "scoped", Value: "yes", Path: "/app", Expires: &expires},
		{Name: "other", Value: "no", Path: "/application"},
	}}
	client := networkClient(t, factory, networkSnapshot(t, server.URL+"/app", credential))
	for index := 0; index < 2; index++ {
		if got := networkGet(t, client, server.URL+"/app/child"); got != "Bearer expiring|scoped=yes; plain=yes" {
			t.Fatal("cookie secure/path/HTTP-only rules or frozen snapshot were not respected")
		}
	}
	now = expires
	if got := networkGet(t, client, server.URL+"/app/child"); got != "|plain=yes" {
		t.Fatal("expired bearer or cookie was still forwarded")
	}
}

func TestNetworkCookiesPreferSpecificPathAndTreatEmptyURLPathAsRoot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Header.Get("Cookie"))
	}))
	defer server.Close()
	factory := networkFactory(t, networkConfig{})
	client := networkClient(t, factory, networkSnapshot(t, server.URL, NetworkCredential{
		Version: 1,
		Cookies: []SessionCookie{
			{Name: "session", Value: "root", Path: "/"},
			{Name: "session", Value: "app", Path: "/app"},
			{Name: "session", Value: "child", Path: "/app/child"},
			{Name: "other", Value: "same-path", Path: "/app"},
		},
	}))
	for _, test := range []struct{ path, want string }{
		{"", "session=root"},
		{"/", "session=root"},
		{"/application", "session=root"},
		{"/app/child", "session=child; session=app; other=same-path; session=root"},
	} {
		if got := networkGet(t, client, server.URL+test.path); got != test.want {
			t.Errorf("path %q: cookie header = %q, want %q", test.path, got, test.want)
		}
	}
}

func TestNetworkRedirectsNeverWidenAuthorityOrReplayWrites(t *testing.T) {
	var foreignHits, doneHits, postHits atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreignHits.Add(1) }))
	defer foreign.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postHits.Add(1)
		}
		switch r.URL.Path {
		case "/app/same":
			http.Redirect(w, r, "/app/done", http.StatusFound)
		case "/app/cross":
			http.Redirect(w, r, foreign.URL+"/app/done", http.StatusFound)
		case "/app/outside":
			http.Redirect(w, r, "/application", http.StatusFound)
		case "/app/loop":
			http.Redirect(w, r, "/app/loop", http.StatusFound)
		case "/app/post":
			http.Redirect(w, r, "/app/done", http.StatusFound)
		case "/app/replay":
			http.Redirect(w, r, "/app/done", http.StatusTemporaryRedirect)
		case "/app/done":
			doneHits.Add(1)
			if r.Header.Get("Authorization") != "Bearer site-token" {
				t.Error("same-origin redirect lost the scoped credential")
			}
			_, _ = io.WriteString(w, "done")
		default:
			t.Error("a request escaped the authorized path")
		}
	}))
	defer origin.Close()
	factory := networkFactory(t, networkConfig{})
	client := networkClient(t, factory, networkSnapshot(t, origin.URL+"/app", NetworkCredential{Version: 1, Bearer: "site-token"}))
	if got := networkGet(t, client, origin.URL+"/app/same"); got != "done" {
		t.Fatal("safe read-only redirect failed")
	}
	for _, path := range []string{"/app/cross", "/app/outside", "/app/loop"} {
		request, _ := http.NewRequest(http.MethodGet, origin.URL+path, nil)
		if _, err := client.Do(request); !errors.Is(err, ErrNetworkRedirect) {
			t.Fatalf("unsafe read redirect %s: %v", path, err)
		}
	}
	for _, path := range []string{"/app/post", "/app/replay"} {
		request, _ := http.NewRequest(http.MethodPost, origin.URL+path, strings.NewReader(`{}`))
		response, err := client.Do(request)
		if err != nil && !errors.Is(err, ErrNetworkRedirect) {
			t.Fatal(err)
		}
		if response != nil {
			response.Body.Close()
			if response.StatusCode != http.StatusTemporaryRedirect {
				t.Fatal("write redirect was followed")
			}
		}
	}
	if foreignHits.Load() != 0 || doneHits.Load() != 1 || postHits.Load() != 2 {
		t.Fatalf("credential scope or POST replay violation: foreign=%d done=%d posts=%d", foreignHits.Load(), doneHits.Load(), postHits.Load())
	}
}

func TestNetworkReadOnlyGETNeverFollowsSameOriginRedirects(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var hits atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hits.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/app/status" {
					t.Error("read-only endpoint scope escaped")
				}
				w.Header().Set("Location", "/app/logout")
				w.WriteHeader(code)
			}))
			defer server.Close()
			factory := networkFactory(t, networkConfig{})
			client := networkClient(t, factory, networkSnapshot(t, server.URL+"/app", NetworkCredential{Version: 1, Bearer: "token"}))
			headers := http.Header{"Accept": {"application/json"}}
			if _, err := client.ReadOnlyGET(context.Background(), server.URL+"/app/status", headers); !errors.Is(err, ErrNetworkRedirect) {
				t.Fatalf("redirect escaped: %v", err)
			}
			if hits.Load() != 1 || headers.Get("Authorization") != "" {
				t.Fatal("read-only request repeated or changed caller headers")
			}
		})
	}
}

func TestNetworkRequestScopeRejectsAlternateHostsAndAmbiguousPaths(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer server.Close()
	factory := networkFactory(t, networkConfig{})
	client := networkClient(t, factory, networkSnapshot(t, server.URL+"/app", NetworkCredential{Version: 1, Bearer: "token"}))
	for _, path := range []string{"/application", "/app/../private", "/app/%2e%2e/private", "/app%2f..%2fprivate", "/app/%252e%252e/private", "/app/%5cprivate", "/app//private", "/app/%00private"} {
		request, err := http.NewRequest(http.MethodGet, server.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Do(request); !errors.Is(err, ErrNetworkScope) {
			t.Fatalf("scope accepted %s: %v", path, err)
		}
	}
	for name, mutate := range map[string]func(*http.Request){
		"virtual host": func(r *http.Request) { r.Host = "other.example" },
		"userinfo":     func(r *http.Request) { r.URL.User = url.UserPassword("secret-canary", "password") },
		"host":         func(r *http.Request) { r.URL.Host = "other.example" },
		"scheme":       func(r *http.Request) { r.URL.Scheme = "https" },
		"fragment":     func(r *http.Request) { r.URL.Fragment = "private" },
		"method":       func(r *http.Request) { r.Method = http.MethodConnect },
		"oversize body": func(r *http.Request) {
			r.Method = http.MethodPost
			r.Body = io.NopCloser(strings.NewReader("body"))
			r.ContentLength = MaxRequestBodyBytes + 1
		},
	} {
		request, _ := http.NewRequest(http.MethodGet, server.URL+"/app", nil)
		mutate(request)
		if _, err := client.Do(request); !errors.Is(err, ErrNetworkScope) || strings.Contains(err.Error(), "canary") {
			t.Fatalf("invalid %s not safely refused: %v", name, err)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid request contacted the site")
	}
}

func TestNetworkCredentialRefusesUnsupportedOrCrossSiteMaterial(t *testing.T) {
	factory := networkFactory(t, networkConfig{})
	for _, encoded := range []string{
		`null`,
		`{"version":2,"bearer":"secret-canary"}`,
		`{"version":1,"bearer":"secret-canary","password":"not-supported"}`,
		`{"version":1,"bearer":"secret-canary","refresh_token":"not-supported"}`,
		`{"version":1,"bearer":"bad\r\nheader"}`,
		`{"version":1,"cookies":[{"name":"session","value":"secret-canary","domain":"other.example","path":"/"}]}`,
		`{"version":1,"cookies":[{"name":"session","value":"secret-canary","domain":"example","path":"/"}]}`,
		`{"version":1,"cookies":[{"name":"session","value":"secret-canary","path":""}]}`,
		`{"version":1,"cookies":[{"name":"__Host-session","value":"secret-canary","secure":true,"domain":"relay.example","path":"/"}]}`,
		`{"version":1,"cookies":[{"name":"__Secure-session","value":"secret-canary","path":"/"}]}`,
		`{"version":1,"bearer":"secret-canary","proxy":{"username":"user","password":"secret-canary"}}`,
		`{"version":1,"bearer":"secret-canary"} {}`,
	} {
		snapshot := AccountSnapshot{Account: connectedAccount(), Credential: []byte(encoded)}
		if _, err := factory.Client(snapshot); !errors.Is(err, ErrCredentialUnavailable) || strings.Contains(err.Error(), "canary") {
			t.Fatalf("unsafe credential rejection: %v", err)
		}
	}
}

func TestNetworkDashboardNormalizationIsIdempotentAndUnambiguous(t *testing.T) {
	for _, address := range []string{"https://RELAY.example/base%20name/", "https://relay.example/%E4%BD%A0%E5%A5%BD", "http://[::1]:1234/base", "https://relay.example:443/base"} {
		normalized, err := NormalizeDashboardURL(address)
		if err != nil {
			t.Fatal(err)
		}
		again, err := NormalizeDashboardURL(normalized)
		if err != nil || again != normalized {
			t.Fatalf("normalization double-encoded a path: %v", err)
		}
	}
	for _, address := range []string{"https://relay.example:0", "https://relay.example:65536", "https://relay.example:", "https://relay.example/base/%2e%2e/private", "https://relay.example/base%2fprivate", "https://relay.example/base/%252e%252e/private", "http://relay.example/base"} {
		if _, err := NormalizeDashboardURL(address); err == nil {
			t.Fatal("invalid dashboard scope accepted")
		}
	}
}

func TestNetworkCloseWaitsForCancelledRequest(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	factory := networkFactory(t, networkConfig{})
	client := networkClient(t, factory, networkSnapshot(t, server.URL, NetworkCredential{Version: 1, Bearer: "token"}))
	done := make(chan error, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		_, err := client.Do(request)
		done <- err
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := factory.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; !errors.Is(err, ErrClientInvalidated) {
		t.Fatalf("factory closed before its request stopped: %v", err)
	}
}
