package contract

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func profileFixture() IdentityProfile {
	created := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	observed, confirmed := created.Add(-time.Second), created.Add(time.Second)
	return IdentityProfile{
		ID: "identity-fixture", ServiceID: "service-fixture", Client: IdentityClientCodexCLI,
		Source: IdentityProfileRequestCapture,
		Fingerprint: IdentityFingerprint{
			UserAgent: "codex-tui/0.156.0 (Windows 10.0.26100; x86_64)", Version: "0.156.0",
			Headers: map[string]string{"Originator": "codex-tui"},
		},
		CreatedAt: created, ObservedAt: &observed, ConfirmedAt: &confirmed,
	}
}

func TestIdentityFingerprintValidation(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*IdentityProfile)
	}{
		{"unknown client", func(p *IdentityProfile) { p.Client = "cursor" }},
		{"bad version", func(p *IdentityProfile) { p.Fingerprint.Version = "0.156" }},
		{"version mismatch", func(p *IdentityProfile) { p.Fingerprint.Version = "0.157.0" }},
		{"unrecognized product", func(p *IdentityProfile) {
			p.Fingerprint.UserAgent = "other/0.156.0"
			p.Fingerprint.Headers["Originator"] = "other"
		}},
		{"manual mixed identity", func(p *IdentityProfile) { p.Fingerprint.Headers["Originator"] = "codex_exec" }},
		{"missing originator", func(p *IdentityProfile) { p.Fingerprint.Headers = nil }},
		{"noncanonical header", func(p *IdentityProfile) {
			p.Fingerprint.Headers = map[string]string{"originator": "codex-tui"}
		}},
		{"case duplicate", func(p *IdentityProfile) { p.Fingerprint.Headers["originator"] = "codex-tui" }},
		{"credential header", func(p *IdentityProfile) { p.Fingerprint.Headers["Authorization"] = "fixture-only" }},
		{"gateway header", func(p *IdentityProfile) { p.Fingerprint.Headers["X-AstrLink-Fixture"] = "fixture" }},
		{"session header", func(p *IdentityProfile) { p.Fingerprint.Headers["Session-Id"] = "fixture-session" }},
		{"transport header", func(p *IdentityProfile) { p.Fingerprint.Headers["Accept-Encoding"] = "gzip" }},
		{"oversized agent", func(p *IdentityProfile) { p.Fingerprint.UserAgent += strings.Repeat("x", MaxIdentityUserAgentBytes) }},
		{"branded agent", func(p *IdentityProfile) { p.Fingerprint.UserAgent += " AstrLink" }},
		{"newline agent", func(p *IdentityProfile) { p.Fingerprint.UserAgent += "\r\nX-Fixture: value" }},
		{"non ASCII agent", func(p *IdentityProfile) { p.Fingerprint.UserAgent += " é" }},
		{"whitespace agent", func(p *IdentityProfile) { p.Fingerprint.UserAgent += " " }},
		{"empty value", func(p *IdentityProfile) { p.Fingerprint.Headers["Originator"] = "" }},
		{"branded value", func(p *IdentityProfile) { p.Fingerprint.Headers["Originator"] = "astrlink-fixture" }},
		{"oversized value", func(p *IdentityProfile) {
			p.Fingerprint.Headers["Originator"] = strings.Repeat("x", MaxIdentityHeaderValueBytes+1)
		}},
		{"invalid value", func(p *IdentityProfile) { p.Fingerprint.Headers["Originator"] = "codex-tui\x00" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := profileFixture()
			test.edit(&profile)
			if err := profile.Fingerprint.Validate(profile.Client); err == nil {
				t.Fatal("invalid fingerprint accepted")
			}
		})
	}
}

func TestIdentityFingerprintClientFamilies(t *testing.T) {
	for _, product := range []string{"codex-tui", "codex_cli_rs", "codex_vscode", "codex_vscode_copilot", "codex_app", "codex_chatgpt_desktop", "codex_atlas", "codex_exec", "codex_sdk_ts"} {
		fingerprint := IdentityFingerprint{
			UserAgent: product + "/0.156.0", Version: "0.156.0",
			Headers: map[string]string{"Originator": product},
		}
		if err := fingerprint.Validate(IdentityClientCodexCLI); err != nil {
			t.Fatalf("supported Codex product: %v", err)
		}
	}
	claude := IdentityFingerprint{
		UserAgent: "claude-cli/2.1.300 (external, cli)", Version: "2.1.300",
		Headers: map[string]string{"X-App": "cli", "X-Stainless-Package-Version": "0.95.1"},
	}
	if err := claude.Validate(IdentityClientClaudeCode); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"X-Stainless-Secret", "X-Stainless-Retry-Count", "X-Stainless-Timeout", "X-Stainless-Helper-Method", "Anthropic-Beta", "Anthropic-Dangerous-Direct-Browser-Access"} {
		copy := claude.Clone()
		copy.Headers[name] = "fixture"
		if err := copy.Validate(IdentityClientClaudeCode); err == nil {
			t.Fatalf("request-specific or unapproved Claude header %s accepted", name)
		}
	}
	for _, edit := range []func(*IdentityFingerprint){
		func(f *IdentityFingerprint) { delete(f.Headers, "X-Stainless-Package-Version") },
		func(f *IdentityFingerprint) { f.Headers["X-Stainless-Package-Version"] = "invalid" },
		func(f *IdentityFingerprint) { f.Headers["X-App"] = "web" },
	} {
		copy := claude.Clone()
		edit(&copy)
		if err := copy.Validate(IdentityClientClaudeCode); err == nil {
			t.Fatal("inconsistent Claude tuple accepted")
		}
	}
	grok := IdentityFingerprint{UserAgent: "grok-shell/1.0.53 (macos; x86_64)", Version: "1.0.53"}
	if err := grok.Validate(IdentityClientGrokCLI); err != nil {
		t.Fatal(err)
	}
	grok.Headers = map[string]string{"X-Grok-Client-Identifier": "fixture-device"}
	if err := grok.Validate(IdentityClientGrokCLI); err == nil {
		t.Fatal("captured Grok identifier accepted")
	}
}

func TestIdentityProfileMetadataAndJSON(t *testing.T) {
	profile := profileFixture()
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	var decoded IdentityProfile
	if err := json.Unmarshal(encoded, &decoded); err != nil || !reflect.DeepEqual(profile, decoded) {
		t.Fatalf("profile round trip failed: %v", err)
	}
	candidate := profile.Clone()
	candidate.ConfirmedAt = nil
	if err := candidate.Validate(); err != nil {
		t.Fatalf("a valid unconfirmed candidate must remain representable: %v", err)
	}
	for _, source := range []IdentityProfileSource{IdentityProfileBuiltin, IdentityProfileSubscriptionImport} {
		copy := profile.Clone()
		copy.Source, copy.ObservedAt = source, nil
		if err := copy.Validate(); err != nil {
			t.Fatalf("source without invented observation time: %v", err)
		}
	}
	for _, test := range []struct {
		name string
		edit func(*IdentityProfile)
	}{
		{"invalid ID", func(p *IdentityProfile) { p.ID = "!" }},
		{"missing scope", func(p *IdentityProfile) { p.ServiceID = "" }},
		{"unknown source", func(p *IdentityProfile) { p.Source = "unknown" }},
		{"missing creation", func(p *IdentityProfile) { p.CreatedAt = time.Time{} }},
		{"missing observation", func(p *IdentityProfile) { p.ObservedAt = nil }},
		{"zero observation", func(p *IdentityProfile) { p.ObservedAt = new(time.Time) }},
		{"observation after creation", func(p *IdentityProfile) { value := p.CreatedAt.Add(time.Second); p.ObservedAt = &value }},
		{"builtin observation", func(p *IdentityProfile) { p.Source = IdentityProfileBuiltin }},
		{"zero confirmation", func(p *IdentityProfile) { p.ConfirmedAt = new(time.Time) }},
		{"confirmation before creation", func(p *IdentityProfile) { value := p.CreatedAt.Add(-time.Second); p.ConfirmedAt = &value }},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := profile.Clone()
			test.edit(&copy)
			if err := copy.Validate(); err == nil {
				t.Fatal("invalid profile metadata accepted")
			}
		})
	}
}

func TestIdentityProfileCloneIsIndependent(t *testing.T) {
	profile := profileFixture()
	copy := profile.Clone()
	copy.Fingerprint.Headers["Originator"] = "changed"
	*copy.ObservedAt = time.Time{}
	*copy.ConfirmedAt = time.Time{}
	if profile.Fingerprint.Headers["Originator"] != "codex-tui" || profile.ObservedAt.IsZero() || profile.ConfirmedAt.IsZero() {
		t.Fatal("clone shares mutable data with its source")
	}
}

func TestPinnedIdentityDoesNotFollowLearningVersionFloors(t *testing.T) {
	for _, test := range []struct {
		client      IdentityClient
		fingerprint IdentityFingerprint
	}{
		{IdentityClientCodexCLI, IdentityFingerprint{UserAgent: "codex-tui/0.100.0", Version: "0.100.0", Headers: map[string]string{"Originator": "codex-tui"}}},
		{IdentityClientGrokCLI, IdentityFingerprint{UserAgent: "grok-shell/1.0.1 (macos; x86_64)", Version: "1.0.1"}},
	} {
		if err := test.fingerprint.Validate(test.client); err != nil {
			t.Fatalf("pinned tuple should not consult mutable learning admission: %v", err)
		}
	}
}
