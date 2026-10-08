package forkcheckin

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/networkproxy"
)

const (
	defaultNetworkTimeout = 15 * time.Second
	maxNetworkResponse    = 1 << 20
	maxNetworkAccounts    = 128
	maxNetworkConcurrent  = 16
)

var (
	ErrNetwork           = errors.New("check-in network request failed")
	ErrNetworkScope      = errors.New("check-in request is outside its authorized scope")
	ErrNetworkRedirect   = errors.New("check-in redirect was refused")
	ErrNetworkTLS        = errors.New("check-in TLS verification failed")
	ErrResponseTooLarge  = errors.New("check-in response exceeds its size limit")
	ErrClientInvalidated = errors.New("check-in network client was invalidated")
	ErrClientCapacity    = errors.New("check-in network client capacity reached")
)

// TransportFactory owns extension-only clients, connections and cancellation.
// The owner must call Invalidate on credential/account removal and Close before
// clearing the Vault key or shared Store. It never reads a Service or mutates
// http.DefaultTransport. Construction creates no timers, goroutines or traffic.
// C01/G02 still gate browser capture; this factory does not enable that feature.
type TransportFactory struct {
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	clients    map[AccountID]*AccountClient
	closed     bool
	done       chan struct{}
	active     sync.WaitGroup
	slots      chan struct{}
	proxySlots chan struct{}
	config     networkConfig
}

// Only package tests replace these dependencies. Production always uses the
// shared direct/system selector and verified TLS, with bounded defaults.
type networkConfig struct {
	selectProxy func(string) (networkproxy.ProxyFunc, error)
	roots       *x509.CertPool
	dial        func(context.Context, string, string) (net.Conn, error)
	now         func() time.Time
	timeout     time.Duration
	maxResponse int64
	maxAccounts int
}

func NewTransportFactory() *TransportFactory {
	return newTransportFactory(networkConfig{})
}

func newTransportFactory(config networkConfig) *TransportFactory {
	if config.selectProxy == nil {
		config.selectProxy = networkproxy.New
	}
	if config.dial == nil {
		dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
		config.dial = dialer.DialContext
	}
	if config.now == nil {
		config.now = time.Now
	}
	if config.timeout <= 0 {
		config.timeout = defaultNetworkTimeout
	}
	if config.maxResponse <= 0 {
		config.maxResponse = maxNetworkResponse
	}
	if config.maxAccounts <= 0 {
		config.maxAccounts = maxNetworkAccounts
	}
	if config.roots != nil {
		config.roots = config.roots.Clone()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TransportFactory{ctx: ctx, cancel: cancel, clients: make(map[AccountID]*AccountClient), slots: make(chan struct{}, maxNetworkConcurrent), proxySlots: make(chan struct{}, maxNetworkConcurrent), config: config}
}

// AccountClient exposes a safe Do rather than the underlying http.Client: the
// latter wraps failures in url.Error, whose URL may contain a site's secrets.
// Responses are private adapter input, fully read and size-checked before Do
// returns. No caller can replace the transport, TLS policy, jar or redirects.
type AccountClient struct {
	factory     *TransportFactory
	base        *url.URL
	revision    int64
	fingerprint [32]byte
	ctx         context.Context
	cancel      context.CancelFunc

	mu         sync.Mutex
	closed     bool
	credential NetworkCredential
	proxy      networkproxy.ProxyFunc
	system     bool
	transport  *http.Transport
	route      [32]byte
}

// Client takes the current Vault snapshot. The owner serializes account edits
// and credential writes; the factory does not independently read the database.
// A changed snapshot cancels the old client, including in-flight I/O, and closes
// its idle pool. Lower account revisions cannot replace a newer cached client.
func (factory *TransportFactory) Client(snapshot AccountSnapshot) (*AccountClient, error) {
	if err := snapshot.Validate(); err != nil {
		return nil, ErrCredentialUnavailable
	}
	normalized, err := NormalizeDashboardURL(snapshot.Account.DashboardBaseURL)
	if err != nil {
		return nil, ErrNetworkScope
	}
	base, _ := url.Parse(normalized)
	credential, err := decodeNetworkCredential(snapshot.Credential, base, snapshot.Account.Network.Mode)
	if err != nil {
		return nil, err
	}
	var proxy networkproxy.ProxyFunc
	if snapshot.Account.Network.Mode == NetworkModeCustom {
		// Network.Validate already uses this policy; keep the final outgoing
		// proxy URL subject to it too. Authentication comes only from Vault.
		if contract.ValidateProxyURL(snapshot.Account.Network.ProxyURL) != nil {
			return nil, ErrNetworkScope
		}
		address, _ := url.Parse(snapshot.Account.Network.ProxyURL)
		if credential.Proxy != nil {
			address.User = url.UserPassword(credential.Proxy.Username, credential.Proxy.Password)
		}
		proxy = http.ProxyURL(address)
	} else {
		proxy, err = factory.config.selectProxy(string(snapshot.Account.Network.Mode))
		if err != nil {
			return nil, ErrNetwork
		}
	}
	fingerprint := sha256.New()
	_, _ = io.WriteString(fingerprint, snapshot.Account.ConfigFingerprint())
	_, _ = fingerprint.Write(snapshot.Credential)
	var key [32]byte
	copy(key[:], fingerprint.Sum(nil))
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if factory.closed {
		return nil, ErrClientInvalidated
	}
	previous, exists := factory.clients[snapshot.Account.ID]
	if exists {
		if snapshot.Account.Revision < previous.revision {
			return nil, ErrRevisionChanged
		}
		if previous.fingerprint == key && previous.revision == snapshot.Account.Revision {
			return previous, nil
		}
		previous.invalidate()
	} else if len(factory.clients) >= factory.config.maxAccounts {
		return nil, ErrClientCapacity
	}
	ctx, cancel := context.WithCancel(factory.ctx)
	client := &AccountClient{factory: factory, base: base, revision: snapshot.Account.Revision,
		fingerprint: key, credential: credential, proxy: proxy, system: snapshot.Account.Network.Mode == NetworkModeSystem, ctx: ctx, cancel: cancel}
	factory.clients[snapshot.Account.ID] = client
	return client, nil
}

func (factory *TransportFactory) Invalidate(id AccountID) {
	factory.mu.Lock()
	defer factory.mu.Unlock()
	if client := factory.clients[id]; client != nil {
		client.invalidate()
		delete(factory.clients, id)
	}
}

// Close cancels all current and retired clients and waits for every admitted
// Do to finish. If ctx expires, the owner must wait again before clearing keys.
func (factory *TransportFactory) Close(ctx context.Context) error {
	factory.mu.Lock()
	if !factory.closed {
		factory.closed = true
		factory.cancel()
		for id, client := range factory.clients {
			client.invalidate()
			delete(factory.clients, id)
		}
		factory.done = make(chan struct{})
		go func() {
			factory.active.Wait()
			close(factory.done)
		}()
	}
	done := factory.done
	factory.mu.Unlock()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (client *AccountClient) invalidate() {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.closed = true
	client.cancel()
	if client.transport != nil {
		client.transport.CloseIdleConnections()
		client.transport = nil
	}
	client.proxy = nil
	client.credential = NetworkCredential{}
}

func (client *AccountClient) Do(request *http.Request) (*http.Response, error) {
	return client.do(request, true)
}

// ReadOnlyGET bounds adapter discovery to one endpoint. Redirects are refused
// even within the account's origin: a site may redirect a GET to /logout or
// another action. No caller can widen the transport policy through this API.
func (client *AccountClient) ReadOnlyGET(ctx context.Context, address string, headers http.Header) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, ErrNetworkScope
	}
	request.Header = headers.Clone()
	return client.do(request, false)
}

func (client *AccountClient) do(request *http.Request, followRedirects bool) (*http.Response, error) {
	if request == nil || request.URL == nil {
		return nil, ErrNetworkScope
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		closeNetworkRequest(request)
		return nil, ErrClientInvalidated
	}
	client.factory.active.Add(1)
	client.mu.Unlock()
	defer client.factory.active.Done()
	ctx, cancel := context.WithTimeout(request.Context(), client.factory.config.timeout)
	stop := context.AfterFunc(client.ctx, cancel)
	defer stop()
	defer cancel()
	select {
	case client.factory.slots <- struct{}{}:
		defer func() { <-client.factory.slots }()
	case <-ctx.Done():
		closeNetworkRequest(request)
		return nil, client.failure(ctx.Err())
	}
	// Parent cancellation is visible before it finishes walking the child
	// contexts. Closing one client can release a slot while another client's
	// context is not cancelled yet; that queued request must not start I/O.
	if client.factory.ctx.Err() != nil || client.ctx.Err() != nil {
		closeNetworkRequest(request)
		return nil, ErrClientInvalidated
	}
	if err := ctx.Err(); err != nil {
		closeNetworkRequest(request)
		return nil, client.failure(err)
	}
	outbound := request.Clone(ctx)
	if !client.allows(outbound) || outbound.Body != nil && outbound.Body != http.NoBody && (outbound.ContentLength <= 0 || outbound.ContentLength > MaxRequestBodyBytes) {
		closeNetworkRequest(request)
		return nil, ErrNetworkScope
	}
	// Never let net/http infer a replayable POST from an idempotency header
	// or GetBody supplied by the caller. No network-layer retry policy exists.
	outbound.GetBody = nil
	inner := &http.Client{Transport: accountRoundTripper{client}, Timeout: client.factory.config.timeout, CheckRedirect: func(next *http.Request, via []*http.Request) error {
		if !followRedirects || len(via) >= 4 || via[0].Method != http.MethodGet && via[0].Method != http.MethodHead || !client.allows(next) {
			return ErrNetworkRedirect
		}
		return nil
	}}
	response, err := inner.Do(outbound)
	if err != nil {
		return nil, client.failure(err)
	}
	defer response.Body.Close()
	if response.ContentLength > client.factory.config.maxResponse {
		return nil, ErrResponseTooLarge
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, client.factory.config.maxResponse+1))
	if err != nil {
		clear(body)
		return nil, client.failure(err)
	}
	if int64(len(body)) > client.factory.config.maxResponse {
		clear(body)
		return nil, ErrResponseTooLarge
	}
	if err := ctx.Err(); err != nil {
		clear(body)
		return nil, client.failure(err)
	}
	response.Body = &networkResponseBody{Reader: bytes.NewReader(body), data: body}
	response.ContentLength = int64(len(body))
	return response, nil
}

type networkResponseBody struct {
	*bytes.Reader
	data []byte
}

func (body *networkResponseBody) Close() error {
	clear(body.data)
	body.data = nil
	body.Reader.Reset(nil)
	return nil
}

type accountRoundTripper struct{ client *AccountClient }

// RoundTrip is used only by the private http.Client and enforces the same scope
// on every redirect hop. Resolve the proxy BEFORE accessing any pool: HTTP/2
// may otherwise reuse a route without invoking Transport.Proxy at all.
func (roundTripper accountRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	client := roundTripper.client
	if client.factory.ctx.Err() != nil || client.ctx.Err() != nil {
		closeNetworkRequest(request)
		return nil, ErrClientInvalidated
	}
	if !client.allows(request) {
		closeNetworkRequest(request)
		return nil, ErrNetworkScope
	}
	client.mu.Lock()
	if client.closed {
		client.mu.Unlock()
		closeNetworkRequest(request)
		return nil, ErrClientInvalidated
	}
	proxy, credential := client.proxy, client.credential
	client.mu.Unlock()
	var selected *url.URL
	if proxy != nil {
		var err error
		selected, err = client.resolveProxy(request, proxy)
		if err != nil {
			closeNetworkRequest(request)
			return nil, client.failure(err)
		}
	}
	if err := request.Context().Err(); err != nil {
		closeNetworkRequest(request)
		return nil, err
	}
	route := "direct"
	if selected != nil {
		// System URLs carrying userinfo are not an account Vault grant.
		// Use an explicit custom proxy with Vault authentication instead.
		if client.system && selected.User != nil {
			closeNetworkRequest(request)
			return nil, ErrNetwork
		}
		copy := *selected
		selected = &copy
		route = selected.String()
	}
	key := sha256.Sum256([]byte(route))
	client.mu.Lock()
	if client.closed || client.factory.ctx.Err() != nil || client.ctx.Err() != nil {
		client.mu.Unlock()
		closeNetworkRequest(request)
		return nil, ErrClientInvalidated
	}
	if client.transport == nil || client.route != key {
		if client.transport != nil {
			client.transport.CloseIdleConnections()
		}
		client.transport = &http.Transport{
			Proxy: http.ProxyURL(selected), DialContext: client.factory.config.dial,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: client.factory.config.roots},
			TLSHandshakeTimeout: 5 * time.Second, ResponseHeaderTimeout: 10 * time.Second,
			MaxResponseHeaderBytes: 32 << 10, MaxIdleConns: 4, MaxIdleConnsPerHost: 2,
			MaxConnsPerHost: 4, IdleConnTimeout: 30 * time.Second, ForceAttemptHTTP2: true,
		}
		client.route = key
	}
	transport := client.transport
	client.mu.Unlock()
	outbound := request.Clone(request.Context())
	outbound.RequestURI, outbound.GetBody, outbound.Trailer = "", nil, nil
	outbound.Header = networkHeaders(outbound.Header)
	credential.apply(outbound, client.factory.config.now())
	response, err := transport.RoundTrip(outbound)
	if err != nil {
		return nil, client.failure(err)
	}
	return response, nil
}

// Native system discovery has its own timeout but no context-aware public
// API. Keep these calls bounded, let Do honor its deadline, and count even a
// retired resolver when Close waits before the owner's key cleanup.
func (client *AccountClient) resolveProxy(request *http.Request, proxy networkproxy.ProxyFunc) (*url.URL, error) {
	policyRequest := (&http.Request{URL: &url.URL{Scheme: request.URL.Scheme, Host: request.URL.Host}}).WithContext(request.Context())
	if !client.system {
		return proxy(policyRequest)
	}
	select {
	case client.factory.proxySlots <- struct{}{}:
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
	type selection struct {
		url *url.URL
		err error
	}
	result := make(chan selection, 1)
	client.factory.active.Add(1) // The enclosing Do is already admitted.
	go func() {
		defer client.factory.active.Done()
		defer func() { <-client.factory.proxySlots }()
		selected, err := proxy(policyRequest)
		result <- selection{selected, err}
	}()
	select {
	case selected := <-result:
		return selected.url, selected.err
	case <-request.Context().Done():
		return nil, request.Context().Err()
	}
}

func (client *AccountClient) allows(request *http.Request) bool {
	if request == nil || request.URL == nil || request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodPost {
		return false
	}
	u := request.URL
	if u.User != nil || u.Opaque != "" || u.Fragment != "" || u.Scheme != client.base.Scheme || !strings.EqualFold(u.Hostname(), client.base.Hostname()) || networkPort(u) != networkPort(client.base) || request.Host != "" && !strings.EqualFold(request.Host, u.Host) {
		return false
	}
	if len(u.String()) > MaxDashboardURLLen || !safeNetworkPath(u.Path) || unsafeNetworkEscape(u.EscapedPath()) {
		return false
	}
	return networkPathContains(client.base.Path, u.Path)
}

func networkHeaders(source http.Header) http.Header {
	header := source.Clone()
	if header == nil {
		header = make(http.Header)
	}
	blocked := map[string]bool{
		"authorization": true, "cookie": true, "proxy-authorization": true, "proxy-connection": true,
		"originator": true, "origin": true, "referer": true, "user-agent": true, "via": true, "x-powered-by": true,
		"connection": true, "keep-alive": true, "te": true, "trailer": true, "transfer-encoding": true, "upgrade": true,
		"idempotency-key": true, "x-idempotency-key": true,
	}
	for key, values := range source {
		if strings.EqualFold(key, "Connection") {
			for _, value := range values {
				for _, token := range strings.Split(value, ",") {
					blocked[strings.ToLower(strings.TrimSpace(token))] = true
				}
			}
		}
	}
	for key := range header {
		lower := strings.ToLower(key)
		if blocked[lower] || strings.HasPrefix(lower, "x-astrlink-") || strings.HasPrefix(lower, "x-forwarded-") || lower == "forwarded" {
			delete(header, key)
		}
	}
	header.Set("User-Agent", "Go-http-client/1.1")
	return header
}

func safeNetworkPath(path string) bool {
	if path == "" {
		return true
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\\%\x00\r\n\t") || strings.Contains(path, "//") {
		return false
	}
	for _, part := range strings.Split(path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	return true
}

func unsafeNetworkEscape(path string) bool {
	lower := strings.ToLower(path)
	return strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "%25")
}

func networkPathContains(scope, path string) bool {
	if scope == "" || scope == "/" {
		return true
	}
	return path == scope || strings.HasPrefix(path, scope) && (strings.HasSuffix(scope, "/") || len(path) > len(scope) && path[len(scope)] == '/')
}

func networkPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Port()
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func closeNetworkRequest(request *http.Request) {
	if request != nil && request.Body != nil {
		_ = request.Body.Close()
	}
}

// Do not unwrap arbitrary network errors: proxy userinfo, request URLs, cookies
// and upstream response text must not become logs or public failure messages.
func (client *AccountClient) failure(err error) error {
	client.mu.Lock()
	closed := client.closed || client.factory.ctx.Err() != nil || client.ctx.Err() != nil
	client.mu.Unlock()
	if closed {
		return ErrClientInvalidated
	}
	for _, safe := range []error{context.Canceled, context.DeadlineExceeded, ErrNetworkRedirect, ErrNetworkScope, ErrClientInvalidated, ErrNetworkTLS} {
		if errors.Is(err, safe) {
			return safe
		}
	}
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	if errors.As(err, &unknown) || errors.As(err, &hostname) || errors.As(err, &invalid) {
		return ErrNetworkTLS
	}
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	return ErrNetwork
}
