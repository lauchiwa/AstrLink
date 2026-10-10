package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/console"
	"github.com/QuantumNous/astrlink/core/internal/controlapi"
	"github.com/QuantumNous/astrlink/core/internal/coreapp"
	"github.com/QuantumNous/astrlink/core/internal/ingress"
)

const identityOperatorToken = "fixture-operator-token-for-identity-tests"
const identityObserverToken = "fixture-observer-token-for-identity-tests"
const identityCapturedUA = "codex-tui/0.156.0 (Windows 10.0.26100; x86_64) WindowsTerminal"
const identityInferenceBody = `{"model":"fixture-model","input":"caller-authored AstrLink content"}`

type identityControlCall func(method, path, body, etag string) *httptest.ResponseRecorder

type identityHTTPClient struct {
	t       *testing.T
	client  *http.Client
	baseURL string
	token   string
	cookie  *http.Cookie
}

func newIdentityHTTPClient(t *testing.T, baseURL string) identityHTTPClient {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{Proxy: nil}, Timeout: 10 * time.Second}
	t.Cleanup(client.CloseIdleConnections)
	return identityHTTPClient{t: t, client: client, baseURL: baseURL}
}

func (client identityHTTPClient) send(method, path, body string, header http.Header) *httptest.ResponseRecorder {
	client.t.Helper()
	request, err := http.NewRequest(method, client.baseURL+path, strings.NewReader(body))
	if err != nil {
		client.t.Fatal(err)
	}
	request.Header = header.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	if client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	if client.cookie != nil {
		request.AddCookie(client.cookie)
	}
	response, err := client.client.Do(request)
	if err != nil {
		client.t.Fatal(err)
	}
	defer response.Body.Close()
	result := httptest.NewRecorder()
	for name, values := range response.Header {
		result.Header()[name] = values
	}
	result.WriteHeader(response.StatusCode)
	if _, err := io.Copy(result.Body, response.Body); err != nil {
		client.t.Fatal(err)
	}
	return result
}

func (client identityHTTPClient) control(method, path, body, etag string) *httptest.ResponseRecorder {
	client.t.Helper()
	header := http.Header{"Content-Type": {"application/json"}}
	if method == http.MethodPatch {
		header.Set("Content-Type", "application/merge-patch+json")
	}
	if etag != "" {
		header.Set("If-Match", etag)
	}
	if client.cookie != nil {
		header.Set(console.RequestHeader, console.RequestHeaderValue)
	}
	return client.send(method, path, body, header)
}

func identityStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status=%d want=%d body=%s", response.Code, want, response.Body.String())
	}
}

func identityDecode(t *testing.T, response *httptest.ResponseRecorder, value any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), value); err != nil {
		t.Fatal(err)
	}
}

func openIdentityCore(t *testing.T, directory string) (*persistentCore, func()) {
	t.Helper()
	core, err := openPersistentCore(context.Background(), persistentOptions{
		version:       coreapp.DefaultConfig("test", "test").Version,
		dataDirectory: directory, controlToken: identityOperatorToken, observerToken: identityObserverToken,
		maxConcurrentInspections: ingress.DefaultMaxConcurrentInspections,
		testNetwork:              offlineNetwork(t), logf: t.Logf,
	})
	if err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			if err := core.close(); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(stop)
	return core, stop
}

func identityCoreClients(t *testing.T, core *persistentCore) (identityHTTPClient, identityHTTPClient, func()) {
	t.Helper()
	control := httptest.NewServer(core.control)
	inference := httptest.NewUnstartedServer(nil)
	dependencies := core.gateway
	dependencies.AllowedHost = inference.Listener.Addr().String()
	handler, err := ingress.NewProduction(dependencies)
	if err != nil {
		t.Fatal(err)
	}
	inference.Config.Handler = handler
	inference.Start()
	stop := func() { inference.Close(); control.Close() }
	t.Cleanup(stop)
	operator := newIdentityHTTPClient(t, control.URL)
	operator.token = identityOperatorToken
	return operator, newIdentityHTTPClient(t, inference.URL), stop
}

type identityUpstreamRequest struct {
	header http.Header
	body   string
}

func identityUpstream(t *testing.T) (string, <-chan identityUpstreamRequest) {
	t.Helper()
	received := make(chan identityUpstreamRequest, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		received <- identityUpstreamRequest{r.Header.Clone(), string(body)}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/models" {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"fixture-model"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"resp_fixture","object":"response","model":"fixture-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"OK"}]}]}`)
	}))
	t.Cleanup(server.Close)
	return server.URL, received
}

func receiveIdentityRequest(t *testing.T, received <-chan identityUpstreamRequest, userAgent string) identityUpstreamRequest {
	t.Helper()
	select {
	case request := <-received:
		if request.header.Get("User-Agent") != userAgent || request.header.Get("Authorization") != "Bearer fixture-provider-secret" {
			t.Fatalf("wrong upstream identity or credential: %v", request.header)
		}
		for name := range request.header {
			if strings.HasPrefix(strings.ToLower(name), "x-astrlink-") || strings.EqualFold(name, "X-Openai-Actor-Authorization") || strings.EqualFold(name, "Cookie") {
				t.Errorf("local control header reached upstream: %s", name)
			}
		}
		return request
	case <-time.After(5 * time.Second):
		t.Fatal("no upstream request")
		return identityUpstreamRequest{}
	}
}

// Exercise actual control, production ingress, storage, discovery and tester
// consumers. CRUD alone would pass even if persistent.go forgot the readers.
func exercisePersistentIdentity(t *testing.T, call identityControlCall, inference identityHTTPClient, upstreamURL string, received <-chan identityUpstreamRequest) (contract.Service, contract.IdentityProfile, string) {
	t.Helper()
	policyPath := controlapi.PoliciesPath + "/" + string(contract.DefaultPrivacyPolicyID)
	policy := call("GET", policyPath, "", "")
	identityStatus(t, policy, 200)
	identityStatus(t, call("PATCH", policyPath, `{"enabled":false}`, policy.Header().Get("ETag")), 200)
	created := call("POST", controlapi.ServicesPath, fmt.Sprintf(`{"name":"identity fixture","kind":"openai_compatible","enabled":true,"models":["fixture-model"],"http":{"base_url":%q,"auth":{"scheme":"bearer"},"credential":{"secret":"fixture-provider-secret"}},"capabilities":[{"protocol":"openai.responses","mode":"native","streaming":true},{"protocol":"openai.models","mode":"native","streaming":false}]}`, upstreamURL+"/v1"), "")
	identityStatus(t, created, 201)
	var service contract.Service
	identityDecode(t, created, &service)
	path := controlapi.ServicesPath + "/" + string(service.ID)
	tokenResponse := call("POST", controlapi.AccessTokensPath, `{"name":"identity fixture client"}`, "")
	identityStatus(t, tokenResponse, 201)
	var token struct {
		Value string `json:"access_token"`
	}
	identityDecode(t, tokenResponse, &token)
	inference.token = token.Value
	header := http.Header{"Content-Type": {"application/json"}, "User-Agent": {identityCapturedUA}, "Originator": {"codex-tui"}, "Session-Id": {"11111111-2222-4333-8444-555555555555"}, "X-Astrlink-Console": {"local"}, "X-Openai-Actor-Authorization": {"local"}}
	infer := func() {
		t.Helper()
		identityStatus(t, inference.send("POST", "/v1/responses", identityInferenceBody, header), 200)
	}
	infer()
	unarmed := receiveIdentityRequest(t, received, identityCapturedUA)
	identityStatus(t, call("PUT", path+"/identity-capture", `{"client":"codex_cli"}`, ""), 200)
	infer()
	armed := receiveIdentityRequest(t, received, identityCapturedUA)
	if unarmed.body != identityInferenceBody || armed.body != unarmed.body {
		t.Fatal("capture changed caller content")
	}
	infer()
	receiveIdentityRequest(t, received, identityCapturedUA)
	profiles := call("GET", path+"/identity-profiles", "", "")
	identityStatus(t, profiles, 200)
	var page struct {
		Items []contract.IdentityProfile `json:"items"`
	}
	identityDecode(t, profiles, &page)
	if len(page.Items) != 1 || page.Items[0].ConfirmedAt != nil || page.Items[0].Source != contract.IdentityProfileRequestCapture {
		t.Fatalf("capture did not publish exactly one unconfirmed candidate: %+v", page.Items)
	}
	profile := page.Items[0]
	binding := fmt.Sprintf(`{"http":{"identity_profile_id":%q,"extra_headers":{"X-Fixture-Default":"saved"},"model_rules":[{"match":"*","headers":{"X-Fixture-Rule":"inference"}}]}}`, profile.ID)
	identityStatus(t, call("PATCH", path, binding, created.Header().Get("ETag")), 422)
	profilePath := path + "/identity-profiles/" + string(profile.ID)
	read := call("GET", profilePath, "", "")
	identityStatus(t, read, 200)
	confirmed := call("POST", profilePath+"/confirm", `{}`, read.Header().Get("ETag"))
	identityStatus(t, confirmed, 200)
	identityDecode(t, confirmed, &profile)
	bound := call("PATCH", path, binding, created.Header().Get("ETag"))
	identityStatus(t, bound, 200)
	identityDecode(t, bound, &service)
	header.Set("User-Agent", "ordinary-fixture-client/1.0")
	header.Del("Originator")
	infer()
	forwarded := receiveIdentityRequest(t, received, identityCapturedUA)
	if forwarded.header.Get("X-Fixture-Rule") != "inference" || forwarded.header.Get("X-Fixture-Default") != "" {
		t.Fatal("inference did not select the model rule")
	}
	for _, probe := range []struct{ path, body string }{
		{path + "/probe-models", `{"protocol":"openai.models"}`},
		{controlapi.ServiceModelProbesPath, fmt.Sprintf(`{"service_id":%q,"kind":"openai_compatible","protocol":"openai.models","http":{"base_url":%q,"auth":{"scheme":"bearer"},"identity_profile_id":%q,"extra_headers":{"X-Fixture-Default":"saved"},"model_rules":[{"match":"*","headers":{"X-Fixture-Rule":"never-for-discovery"}}]}}`, service.ID, upstreamURL+"/v1", profile.ID)},
	} {
		result := call("POST", probe.path, probe.body, "")
		identityStatus(t, result, 200)
		if !strings.Contains(result.Body.String(), "fixture-model") {
			t.Fatal("control discovery lost the model")
		}
		request := receiveIdentityRequest(t, received, identityCapturedUA)
		if request.header.Get("X-Fixture-Default") != "saved" || request.header.Get("X-Fixture-Rule") != "" {
			t.Fatal("control discovery applied inference rules instead of defaults")
		}
	}
	listing := inference.send("GET", "/v1/models", "", header)
	identityStatus(t, listing, 200)
	if !strings.Contains(listing.Body.String(), "fixture-model") {
		t.Fatal("aggregate discovery lost the model")
	}
	request := receiveIdentityRequest(t, received, identityCapturedUA)
	if request.header.Get("X-Fixture-Default") != "saved" || request.header.Get("X-Fixture-Rule") != "" {
		t.Fatal("aggregate discovery did not use service defaults")
	}
	tested := call("POST", path+"/test", `{"protocol":"openai.responses","model":"fixture-model"}`, "")
	identityStatus(t, tested, 200)
	var result contract.ServiceTestResult
	identityDecode(t, tested, &result)
	if !result.OK || result.Output != "OK" {
		t.Fatalf("control connection tester lost identity dependencies: %+v", result)
	}
	receiveIdentityRequest(t, received, identityCapturedUA)
	return service, profile, token.Value
}

func TestPersistentIdentityCompositionAndRestart(t *testing.T) {
	upstreamURL, received := identityUpstream(t)
	directory := t.TempDir()
	core, closeCore := openIdentityCore(t, directory)
	operator, inference, closeServers := identityCoreClients(t, core)
	service, profile, token := exercisePersistentIdentity(t, operator.control, inference, upstreamURL, received)
	path := controlapi.ServicesPath + "/" + string(service.ID)
	for _, caller := range []string{"", identityObserverToken, token} {
		client := operator
		client.token = caller
		want := 401
		if caller == identityObserverToken {
			want = 403
		}
		for _, suffix := range []string{"/identity-profiles", "/identity-profiles/" + string(profile.ID), "/identity-capture"} {
			identityStatus(t, client.control("GET", path+suffix, "", ""), want)
		}
		identityStatus(t, client.control("PUT", path+"/identity-capture", `{"client":"codex_cli"}`, ""), want)
	}
	pending := operator.control("POST", path+"/identity-profiles", `{"client":"codex_cli","source":"builtin"}`, "")
	identityStatus(t, pending, 201)
	pendingPath := pending.Header().Get("Location")
	identityStatus(t, operator.control("PUT", path+"/identity-capture", `{"client":"codex_cli"}`, ""), 200)
	other, _ := openIdentityCore(t, t.TempDir())
	if other.gateway.IdentityCapture.Status(service.ID).Armed {
		t.Fatal("capture consent leaked into another core")
	}
	closeServers()
	closeCore()
	restarted, _ := openIdentityCore(t, directory)
	restartedControl, restartedInference, _ := identityCoreClients(t, restarted)
	status := restartedControl.control("GET", path+"/identity-capture", "", "")
	identityStatus(t, status, 200)
	var capture contract.IdentityCaptureStatus
	identityDecode(t, status, &capture)
	if capture.Armed {
		t.Fatal("capture consent survived restart")
	}
	pendingRead := restartedControl.control("GET", pendingPath, "", "")
	identityStatus(t, pendingRead, 200)
	var candidate contract.IdentityProfile
	identityDecode(t, pendingRead, &candidate)
	if candidate.ConfirmedAt != nil {
		t.Fatal("restart confirmed a pending profile")
	}
	restartedInference.token = token
	identityStatus(t, restartedInference.send("POST", "/v1/responses", identityInferenceBody, http.Header{"Content-Type": {"application/json"}}), 200)
	receiveIdentityRequest(t, received, profile.Fingerprint.UserAgent)
}

// Unlike the Unix control-socket suite, this exercises the actual server entry
// and trusted console-session boundary on Windows too.
func TestServePersistentIdentityWithConsoleSession(t *testing.T) {
	upstreamURL, received := identityUpstream(t)
	running := startServe(t, serveConfig{dataDirectory: shortDataDir(t), listen: "127.0.0.1:0", outboundProxy: "direct"})
	client := newIdentityHTTPClient(t, "http://"+running.address)
	setup := client.send("POST", console.SetupPath, `{"password":"`+servePassword+`"}`, http.Header{"Content-Type": {"application/json"}, console.RequestHeader: {console.RequestHeaderValue}})
	identityStatus(t, setup, 200)
	client.cookie = sessionCookieOf(t, setup.Result())
	service, _, token := exercisePersistentIdentity(t, client.control, newIdentityHTTPClient(t, client.baseURL), upstreamURL, received)
	path := controlapi.ServicesPath + "/" + string(service.ID) + "/identity-capture"
	identityStatus(t, client.control("PUT", path, `{"client":"codex_cli"}`, ""), 200)
	identityStatus(t, client.send("PUT", path, `{"client":"codex_cli"}`, http.Header{"Content-Type": {"application/json"}}), 403)
	identityStatus(t, client.send("GET", "/v1/models", "", nil), 401)
	inference := client
	inference.cookie = nil
	inference.token = token
	identityStatus(t, inference.control("GET", path, "", ""), 401)
	identityStatus(t, inference.send("POST", "/v1/responses", identityInferenceBody, http.Header{"Content-Type": {"application/json"}, "Origin": {"https://browser.example"}}), 403)
	status := client.control("GET", path, "", "")
	identityStatus(t, status, 200)
	var capture contract.IdentityCaptureStatus
	identityDecode(t, status, &capture)
	if !capture.Armed || capture.Rejected != 0 || capture.CapturedProfile != "" {
		t.Fatal("pre-auth network requests reached identity capture")
	}
	identityStatus(t, client.control("POST", console.LogoutPath, `{}`, ""), 204)
	identityStatus(t, client.control("GET", path, "", ""), 401)
}

func TestPersistentIdentityInitializationFailsClosed(t *testing.T) {
	for _, mode := range []string{"invalid_key", "short_control_token", "equal_tokens"} {
		t.Run(mode, func(t *testing.T) {
			key := make([]byte, 32)
			for index := range key {
				key[index] = 1
			}
			options := persistentOptions{
				version:       coreapp.DefaultConfig("test", "test").Version,
				dataDirectory: t.TempDir(), stdinLocalKey: key,
				controlToken: identityOperatorToken, observerToken: identityObserverToken,
				testNetwork: offlineNetwork(t), logf: t.Logf,
			}
			want := "configure persistent control API"
			switch mode {
			case "invalid_key":
				options.stdinLocalKey = key[:1]
				want = "load local key"
			case "short_control_token":
				options.controlToken = "short"
			case "equal_tokens":
				options.observerToken = options.controlToken
			}
			core, err := openPersistentCore(context.Background(), options)
			if core != nil {
				_ = core.close()
				t.Fatal("failed initialization returned a usable core")
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("error=%v, want %s", err, want)
			}
			for _, value := range options.stdinLocalKey {
				if value != 0 {
					t.Fatal("failed initialization retained injected key material")
				}
			}
		})
	}
}
