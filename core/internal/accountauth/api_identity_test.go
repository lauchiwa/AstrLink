package accountauth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

var apiIdentityClients = []contract.IdentityClient{
	contract.IdentityClientCodexCLI, contract.IdentityClientClaudeCode, contract.IdentityClientGrokCLI,
}

func confirmedAPIProfile(client contract.IdentityClient, fingerprint contract.IdentityFingerprint) contract.IdentityProfile {
	created := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	confirmed := created.Add(time.Second)
	return contract.IdentityProfile{
		ID: "identity-fixture", ServiceID: "service-fixture", Client: client,
		Source: contract.IdentityProfileBuiltin, Fingerprint: fingerprint.Clone(),
		CreatedAt: created, ConfirmedAt: &confirmed,
	}
}

func TestBuiltinAPIFingerprintsExcludeSubscriptionAndRequestFields(t *testing.T) {
	for _, client := range apiIdentityClients {
		t.Run(string(client), func(t *testing.T) {
			fingerprint, err := BuiltinIdentityFingerprint(client)
			if err != nil {
				t.Fatal(err)
			}
			if err := fingerprint.Validate(client); err != nil {
				t.Fatal(err)
			}
			profile := confirmedAPIProfile(client, fingerprint)
			header, err := IdentityProfileHeaders(profile, profile.ServiceID, contract.ServiceAuth{Scheme: contract.AuthSchemeBearer})
			if err != nil {
				t.Fatal(err)
			}
			if header.Get("User-Agent") != fingerprint.UserAgent {
				t.Fatal("renderer changed the pinned user agent")
			}
			for _, name := range []string{
				"Authorization", "Cookie", "X-Api-Key", "ChatGPT-Account-ID", "OAI-Product-Sku",
				"X-XAI-Token-Auth", "Anthropic-Beta", "Anthropic-Version", "Session-Id", ClaudeCodeSessionHeader,
				"X-Stainless-Retry-Count", "X-Stainless-Timeout", "X-Stainless-Helper-Method",
				"Anthropic-Dangerous-Direct-Browser-Access", "Accept", "Accept-Encoding", "Content-Type",
			} {
				if _, present := header[http.CanonicalHeaderKey(name)]; present {
					t.Fatalf("identity renderer supplied non-identity header %s", name)
				}
			}
			switch client {
			case contract.IdentityClientCodexCLI:
				if len(header) != 3 || header.Get("Version") != fingerprint.Version || header.Get("Originator") != fingerprint.Headers["Originator"] {
					t.Fatal("Codex tuple is inconsistent")
				}
			case contract.IdentityClientGrokCLI:
				if len(header) != 3 || header.Get("X-Grok-Client-Version") != fingerprint.Version || header.Get("X-Grok-Client-Identifier") != grokUserAgentProduct {
					t.Fatal("Grok tuple is inconsistent")
				}
			}
		})
	}
	if _, err := BuiltinIdentityFingerprint("unsupported"); err == nil {
		t.Fatal("unknown client silently fell back to a baseline")
	}
}

func TestCaptureAPIFingerprintIsAClosedReadOnlyProjection(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{\"session_id\":\"fixture-session\"}"},"messages":[{"role":"user","content":"fixture private body"}]}`)
	for _, test := range []struct {
		client      contract.IdentityClient
		header      http.Header
		wantVersion string
	}{
		{contract.IdentityClientCodexCLI, codexClientHeaders("0.160.0"), "0.160.0"},
		{contract.IdentityClientClaudeCode, claudeClientHeaders("2.1.300"), "2.1.300"},
		{contract.IdentityClientGrokCLI, http.Header{
			"User-Agent":            {"ExampleWrapper/9.0 grok-shell/1.0.52 (untrusted-platform)"},
			"X-Grok-Client-Version": {"1.0.53"}, "X-Grok-Client-Identifier": {"fixture-device"},
		}, "1.0.53"},
	} {
		t.Run(string(test.client), func(t *testing.T) {
			header := test.header.Clone()
			for _, name := range []string{"Authorization", "Cookie", "X-Stainless-Secret", "X-Stainless-Authorization", "X-AstrLink-Fixture", "X-Request-ID"} {
				header.Set(name, "fixture-sensitive-value")
			}
			before, bodyBefore := header.Clone(), bytes.Clone(body)
			fingerprint, ok := CaptureIdentityFingerprint(test.client, header, body)
			if !ok || fingerprint.Version != test.wantVersion {
				t.Fatal("recognized original client did not produce a candidate")
			}
			if !reflect.DeepEqual(header, before) || !bytes.Equal(body, bodyBefore) {
				t.Fatal("capture changed the original request")
			}
			encoded, err := json.Marshal(fingerprint)
			if err != nil {
				t.Fatal(err)
			}
			for _, excluded := range []string{"fixture-sensitive-value", "fixture private body", "fixture-session", "fixture-device", "ExampleWrapper", "untrusted-platform", "oauth-2025", "Retry-Count", "Timeout", "Helper-Method"} {
				if bytes.Contains(encoded, []byte(excluded)) {
					t.Fatalf("projection retained excluded field/content %s", excluded)
				}
			}
			if test.client != contract.IdentityClientGrokCLI && fingerprint.UserAgent != header.Get("User-Agent") {
				t.Fatal("capture changed the observed UA/platform")
			}
			if test.client == contract.IdentityClientGrokCLI && fingerprint.UserAgent != grokIdentityAt(test.wantVersion).UserAgent {
				t.Fatal("Grok version-only capture did not use the shared template")
			}
			for name := range fingerprint.Headers {
				fingerprint.Headers[name] = "changed"
			}
			if !reflect.DeepEqual(header, before) {
				t.Fatal("candidate shares mutable data with the original request")
			}
		})
	}
}

func TestCaptureAPIFingerprintRejectsAmbiguousOrUnrecognizedRequests(t *testing.T) {
	claudeBody := []byte(`{"metadata":{"user_id":"{}"}}`)
	for _, test := range []struct {
		name   string
		client contract.IdentityClient
		edit   func(http.Header)
		body   []byte
	}{
		{"codex missing session", contract.IdentityClientCodexCLI, func(h http.Header) { h.Del("Session-Id") }, nil},
		{"codex mixed custom rule", contract.IdentityClientCodexCLI, func(h http.Header) { h.Set("Originator", "codex_exec") }, nil},
		{"codex mismatched version header", contract.IdentityClientCodexCLI, func(h http.Header) { h.Set("Version", "0.161.0") }, nil},
		{"codex case duplicate UA", contract.IdentityClientCodexCLI, func(h http.Header) { h["user-agent"] = []string{h.Get("User-Agent")} }, nil},
		{"codex repeated originator", contract.IdentityClientCodexCLI, func(h http.Header) { h.Add("Originator", "codex_cli_rs") }, nil},
		{"codex empty version values", contract.IdentityClientCodexCLI, func(h http.Header) { h["Version"] = nil }, nil},
		{"codex branded UA", contract.IdentityClientCodexCLI, func(h http.Header) { h.Set("User-Agent", h.Get("User-Agent")+" AstrLink") }, nil},
		{"codex control character", contract.IdentityClientCodexCLI, func(h http.Header) { h.Set("Session-Id", "fixture\r\nextra") }, nil},
		{"codex too large UA", contract.IdentityClientCodexCLI, func(h http.Header) { h.Set("User-Agent", h.Get("User-Agent")+strings.Repeat("x", 1024)) }, nil},
		{"claude missing body signal", contract.IdentityClientClaudeCode, nil, nil},
		{"claude invalid body", contract.IdentityClientClaudeCode, nil, []byte(`{"metadata":`)},
		{"claude oversized body", contract.IdentityClientClaudeCode, nil, bytes.Repeat([]byte(" "), maxUserIDScan+1)},
		{"claude missing beta", contract.IdentityClientClaudeCode, func(h http.Header) { h.Del("Anthropic-Beta") }, claudeBody},
		{"claude oversized recognition signals", contract.IdentityClientClaudeCode, func(h http.Header) {
			for range 4 {
				h.Add("Anthropic-Beta", strings.Repeat("x", 4096))
			}
		}, claudeBody},
		{"claude missing SDK version", contract.IdentityClientClaudeCode, func(h http.Header) { h.Del("X-Stainless-Package-Version") }, claudeBody},
		{"claude invalid SDK version", contract.IdentityClientClaudeCode, func(h http.Header) { h.Set("X-Stainless-Package-Version", "invalid") }, claudeBody},
		{"claude repeated SDK", contract.IdentityClientClaudeCode, func(h http.Header) { h.Add("X-Stainless-Os", "MacOS") }, claudeBody},
		{"claude branded SDK", contract.IdentityClientClaudeCode, func(h http.Header) { h.Set("X-Stainless-Runtime", "AstrLink") }, claudeBody},
		{"claude empty SDK value", contract.IdentityClientClaudeCode, func(h http.Header) { h.Set("X-Stainless-Runtime", "") }, claudeBody},
		{"grok invalid explicit version", contract.IdentityClientGrokCLI, func(h http.Header) { h.Set("X-Grok-Client-Version", "invalid") }, nil},
		{"grok repeated explicit version", contract.IdentityClientGrokCLI, func(h http.Header) { h.Add("X-Grok-Client-Version", "1.0.54") }, nil},
		{"grok below floor", contract.IdentityClientGrokCLI, func(h http.Header) { h.Set("X-Grok-Client-Version", "1.0.1") }, nil},
		{"grok future major", contract.IdentityClientGrokCLI, func(h http.Header) { h.Set("X-Grok-Client-Version", "9.0.0") }, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			var header http.Header
			switch test.client {
			case contract.IdentityClientCodexCLI:
				header = codexClientHeaders("0.160.0")
			case contract.IdentityClientClaudeCode:
				header = claudeClientHeaders("2.1.300")
			case contract.IdentityClientGrokCLI:
				header = http.Header{"User-Agent": {"grok-shell/1.0.53 (macos; x86_64)"}, "X-Grok-Client-Version": {"1.0.53"}}
			}
			if test.edit != nil {
				test.edit(header)
			}
			fingerprint, ok := CaptureIdentityFingerprint(test.client, header, test.body)
			if ok || !reflect.DeepEqual(fingerprint, contract.IdentityFingerprint{}) {
				t.Fatal("invalid input returned a candidate or partial fingerprint")
			}
		})
	}
	if _, ok := CaptureIdentityFingerprint("unknown", codexClientHeaders("0.160.0"), nil); ok {
		t.Fatal("unknown client accepted")
	}
}

func TestCaptureAPIHeaderNamesAreCaseInsensitive(t *testing.T) {
	body := []byte(`{"metadata":{"user_id":"{}"}}`)
	for _, client := range []contract.IdentityClient{contract.IdentityClientCodexCLI, contract.IdentityClientClaudeCode} {
		original := codexClientHeaders("0.160.0")
		if client == contract.IdentityClientClaudeCode {
			original = claudeClientHeaders("2.1.300")
			original.Add("Anthropic-Beta", "fixture-feature")
		}
		lower := make(http.Header)
		for name, values := range original {
			lower[strings.ToLower(name)] = append([]string(nil), values...)
		}
		want, ok := CaptureIdentityFingerprint(client, original, body)
		if !ok {
			t.Fatal("canonical input rejected")
		}
		got, ok := CaptureIdentityFingerprint(client, lower, body)
		if !ok || !reflect.DeepEqual(got, want) {
			t.Fatal("capture depends on source header casing")
		}
	}
}

func TestAPIImportDoesNotFollowSubscriptionSettingsOrModifyRegistry(t *testing.T) {
	settings := contract.DefaultRoutingSettings()
	settings.CodexIdentityAutoLearn = false
	settings.CodexIdentityVersion = "0.999.0"
	registry := NewIdentityRegistry(fixedSettings{settings: settings}, &memoryIdentityStore{})
	if _, ok := registry.LearnedIdentityFingerprint(contract.IdentityClientCodexCLI); ok {
		t.Fatal("missing learned identity silently used a baseline")
	}
	var absent *IdentityRegistry
	if _, ok := absent.LearnedIdentityFingerprint(contract.IdentityClientCodexCLI); ok {
		t.Fatal("nil registry silently used a baseline")
	}
	if _, err := registry.LearnCodex(context.Background(), codexClientHeaders("0.160.0")); err != nil {
		t.Fatal(err)
	}
	fingerprint, ok := registry.LearnedIdentityFingerprint(contract.IdentityClientCodexCLI)
	if !ok || fingerprint.Version != "0.160.0" {
		t.Fatal("API import used subscription switches or version floor")
	}
	profile := confirmedAPIProfile(contract.IdentityClientCodexCLI, fingerprint)
	profile.Source = contract.IdentityProfileSubscriptionImport
	fingerprint.Headers["Originator"] = "changed"
	if _, err := registry.LearnCodex(context.Background(), codexClientHeaders("0.161.0")); err != nil {
		t.Fatal(err)
	}
	if _, ok := CaptureIdentityFingerprint(contract.IdentityClientCodexCLI, codexClientHeaders("0.170.0"), nil); !ok {
		t.Fatal("API capture failed")
	}
	if got := registry.ClientIdentities().Codex.LearnedVersion; got != "0.161.0" {
		t.Fatal("API candidate capture modified subscription learning")
	}
	header, err := IdentityProfileHeaders(profile, profile.ServiceID, contract.ServiceAuth{Scheme: contract.AuthSchemeNone})
	if err != nil || header.Get("Version") != "0.160.0" || header.Get("Originator") != "codex_cli_rs" {
		t.Fatalf("confirmed snapshot changed after another import/observation: %v", err)
	}
	if _, ok := registry.LearnedIdentityFingerprint("unknown"); ok {
		t.Fatal("unknown import client accepted")
	}
}

func TestAPIImportReprojectsLegacyClaudeHeaders(t *testing.T) {
	registry := NewIdentityRegistry(nil, nil)
	header := claudeClientHeaders("2.1.300")
	header.Set("X-Stainless-Secret", "fixture-sensitive-value")
	if changed, err := registry.LearnClaude(context.Background(), header); err != nil || !changed {
		t.Fatalf("legacy registry fixture: %v", err)
	}
	fingerprint, ok := registry.LearnedIdentityFingerprint(contract.IdentityClientClaudeCode)
	if !ok {
		t.Fatal("API import failed")
	}
	for _, name := range []string{"X-Stainless-Secret", "X-Stainless-Timeout", "X-Stainless-Retry-Count"} {
		if _, exists := fingerprint.Headers[name]; exists {
			t.Fatalf("API import retained non-identity field %s", name)
		}
	}
	legacy := registry.ClaudeIdentity(contract.DefaultRoutingSettings())
	if legacy.Headers["X-Stainless-Secret"] == "" || legacy.Headers["X-Stainless-Timeout"] == "" {
		t.Fatal("API projection mutated the independent subscription identity")
	}
}

func TestAPIProfileRequiresConfirmationScopeAndNonconflictingAuth(t *testing.T) {
	fingerprint, err := BuiltinIdentityFingerprint(contract.IdentityClientCodexCLI)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name      string
		edit      func(*contract.IdentityProfile)
		serviceID contract.ServiceID
		auth      contract.ServiceAuth
	}{
		{"unconfirmed", func(p *contract.IdentityProfile) { p.ConfirmedAt = nil }, "service-fixture", contract.ServiceAuth{Scheme: contract.AuthSchemeNone}},
		{"different service", nil, "service-other", contract.ServiceAuth{Scheme: contract.AuthSchemeNone}},
		{"missing scope", nil, "", contract.ServiceAuth{Scheme: contract.AuthSchemeNone}},
		{"invalid fingerprint", func(p *contract.IdentityProfile) { p.Fingerprint.Headers["Authorization"] = "fixture-only" }, "service-fixture", contract.ServiceAuth{Scheme: contract.AuthSchemeNone}},
		{"invalid auth", nil, "service-fixture", contract.ServiceAuth{Scheme: "invalid"}},
		{"UA credential", nil, "service-fixture", contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: "user-agent"}},
		{"originator credential", nil, "service-fixture", contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: "ORIGINATOR"}},
		{"version credential", nil, "service-fixture", contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: "version"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := confirmedAPIProfile(contract.IdentityClientCodexCLI, fingerprint)
			if test.edit != nil {
				test.edit(&profile)
			}
			if headers, err := IdentityProfileHeaders(profile, test.serviceID, test.auth); err == nil || headers != nil {
				t.Fatal("unsafe profile application produced headers")
			}
		})
	}
}

func TestAPIProfileProtectsEveryGeneratedHeaderFromCustomAuth(t *testing.T) {
	for _, client := range apiIdentityClients {
		fingerprint, err := BuiltinIdentityFingerprint(client)
		if err != nil {
			t.Fatal(err)
		}
		profile := confirmedAPIProfile(client, fingerprint)
		header, err := IdentityProfileHeaders(profile, profile.ServiceID, contract.ServiceAuth{Scheme: contract.AuthSchemeNone})
		if err != nil {
			t.Fatal(err)
		}
		for name := range header {
			auth := contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: strings.ToLower(name)}
			if result, err := IdentityProfileHeaders(profile, profile.ServiceID, auth); err == nil || result != nil {
				t.Fatalf("%s identity can overwrite authentication in %s", client, name)
			}
		}
		auth := contract.ServiceAuth{Scheme: contract.AuthSchemeCustomHeader, HeaderName: "X-Fixture-Credential"}
		result, err := IdentityProfileHeaders(profile, profile.ServiceID, auth)
		if err != nil || result.Get(auth.HeaderName) != "" {
			t.Fatalf("independent custom authentication rejected or rendered: %v", err)
		}
	}
}

func TestAPIProfileRenderingIsIndependentAndPinned(t *testing.T) {
	fingerprint := contract.IdentityFingerprint{UserAgent: "grok-shell/1.0.1 (frozen-platform)", Version: "1.0.1"}
	profile := confirmedAPIProfile(contract.IdentityClientGrokCLI, fingerprint)
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			header, err := IdentityProfileHeaders(profile, profile.ServiceID, contract.ServiceAuth{Scheme: contract.AuthSchemeNone})
			if err != nil || header.Get("User-Agent") != fingerprint.UserAgent || header.Get("X-Grok-Client-Version") != "1.0.1" {
				t.Errorf("pinned Grok identity was rebuilt using current defaults: %v", err)
				return
			}
			header.Set("User-Agent", "changed")
		}()
	}
	workers.Wait()
	if profile.Fingerprint.UserAgent != fingerprint.UserAgent {
		t.Fatal("rendered header map aliases the pinned profile")
	}
}

func TestAPIProfileHeadersOnActualHTTPForwarding(t *testing.T) {
	for _, client := range apiIdentityClients {
		t.Run(string(client), func(t *testing.T) {
			fingerprint, err := BuiltinIdentityFingerprint(client)
			if err != nil {
				t.Fatal(err)
			}
			profile := confirmedAPIProfile(client, fingerprint)
			overrides, err := IdentityProfileHeaders(profile, profile.ServiceID, contract.ServiceAuth{Scheme: contract.AuthSchemeBearer})
			if err != nil {
				t.Fatal(err)
			}
			// Authentication is supplied independently, never captured or rendered
			// by the identity functions. These are deliberately fake credentials.
			overrides.Set("Authorization", "Bearer provider-fixture-only")
			const payload = "{ \"model\": \"fixture-model\", \"input\": \"fixture text\", \"number\": 9007199254740993 }"
			observed := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, readErr := io.ReadAll(r.Body)
				if readErr != nil || string(body) != payload {
					t.Error("identity-only forwarding changed request bytes")
				}
				observed <- r.Header.Clone()
				w.WriteHeader(http.StatusNoContent)
			}))
			defer upstream.Close()
			base, err := url.Parse(upstream.URL)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "http://localhost/v1/responses", strings.NewReader(payload))
			request.Header.Set("User-Agent", "ExampleCaller/1.0")
			request.Header.Set("Authorization", "Bearer local-fixture-only")
			request.Header.Set("X-AstrLink-Fixture", "local-only")
			response, err := transport.New(upstream.Client().Transport).RoundTrip(request, transport.Target{
				BaseURL: base, RequestHeaders: overrides,
			})
			if err != nil {
				t.Fatal(err)
			}
			_ = response.Body.Close()
			header := <-observed
			for name, values := range overrides {
				if !reflect.DeepEqual(header.Values(name), values) {
					t.Fatalf("final outgoing header %s differs from the prepared tuple", name)
				}
			}
			for _, name := range []string{"X-AstrLink-Fixture", "ChatGPT-Account-ID", "OAI-Product-Sku", "X-XAI-Token-Auth", "Anthropic-Beta", "Session-Id"} {
				if header.Get(name) != "" {
					t.Fatalf("unexpected outgoing header %s", name)
				}
			}
			if request.Header.Get("Authorization") != "Bearer local-fixture-only" || request.Header.Get("User-Agent") != "ExampleCaller/1.0" {
				t.Fatal("forwarding mutated the inbound header map")
			}
		})
	}
}
