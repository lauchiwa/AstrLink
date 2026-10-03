package ingress

import (
	"encoding/json"
	"net/http"

	"github.com/QuantumNous/astrlink/core/internal/requestrewrite"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// auditedRuleHandler forwards through real request rules with upstream HTTP
// metadata capture on, so the stored envelope can be inspected.
func auditedRuleHandler(
	t *testing.T, service contract.Service, profiles IdentityProfileReader, upstreamModel string,
) (*Handler, *memoryAuditBlobs) {
	t.Helper()
	if err := service.Validate(); err != nil {
		t.Fatalf("configuration rejected: %v", err)
	}
	records := &memoryRequestRecordStore{}
	blobs := &memoryAuditBlobs{records: records}
	settings := &memoryAuditSettings{settings: contract.AuditSettings{
		RequestBodyEnabled: true, ResponseContentEnabled: true, HTTPMetaEnabled: true,
		RequestBodyMaxBytes: 1024, ResponseContentMaxBytes: 1024,
		MetadataRetentionDays: 30, ContentRetentionDays: 7,
	}}
	candidate := endpoint.Resolved{
		Service: service, BaseURL: service.HTTP.BaseURL,
		UpstreamProtocol: contract.ProtocolOpenAIChat, UpstreamModel: upstreamModel,
	}
	handler := NewWithDependencies(Dependencies{
		Resolver:         candidateResolver{candidates: []endpoint.Resolved{candidate}},
		Authorizer:       endpoint.NewServiceAuthorizer(codingPlanCredentials{}, nil),
		IdentityProfiles: profiles,
		RequestRecords:   records,
		AuditSettings:    settings,
		AuditBlobs:       blobs,
	})
	return handler, blobs
}

func storedUpstreamMeta(t *testing.T, blobs *memoryAuditBlobs) (contract.AuditHTTPMeta, storage.AuditBlob) {
	t.Helper()
	for _, blob := range blobs.blobs {
		if blob.Direction != storage.AuditDirectionUpstreamHTTPMeta {
			continue
		}
		plain, err := storage.OpenAuditBlob(blobs.key, blob.Nonce, blob.Ciphertext)
		if err != nil {
			t.Fatal(err)
		}
		var meta contract.AuditHTTPMeta
		if err := json.Unmarshal(plain, &meta); err != nil {
			t.Fatal(err)
		}
		return meta, blob
	}
	t.Fatalf("no upstream HTTP metadata among %d blobs", len(blobs.blobs))
	return contract.AuditHTTPMeta{}, storage.AuditBlob{}
}

// TestAuditNeverStoresConfiguredHeaderValues closes the audit half of the
// operator-only rule. Upstream metadata is labelled shareable, so a configured
// value captured verbatim there would be readable by an observer even though
// the service document hides it.
func TestAuditNeverStoresConfiguredHeaderValues(t *testing.T) {
	const profileID contract.IdentityProfileID = "identity_audit"
	upstream := newRuleUpstream(t)
	rules := migratedModelRules()
	rules[2].IdentityProfile = profileID
	service := ruleService(upstream.url, contract.HTTPConnection{
		ExtraHeaders: map[string]string{"X-Relay-Client": "relay-client-value"},
		ModelRules:   rules,
	})
	profiles := &fixedProfiles{profiles: map[contract.IdentityProfileID]contract.IdentityProfile{
		profileID: confirmedCodexProfile(service.ID, profileID),
	}}
	handler, blobs := auditedRuleHandler(t, service, profiles, "gpt-6-astra")
	if response := serveRuleRequest(t, handler, "gpt-6-astra"); response.Code != 200 {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	// The value really went upstream; only the stored copy is masked.
	sent, _ := upstream.sent(t)
	if sent.Get("User-Agent") != ruleUserAgent {
		t.Fatalf("User-Agent sent = %q", sent.Get("User-Agent"))
	}

	meta, blob := storedUpstreamMeta(t, blobs)
	if blob.Exposure != storage.AuditExposureShareable {
		t.Fatalf("exposure = %q; this test assumes an observer-readable part", blob.Exposure)
	}
	encoded, _ := json.Marshal(meta)
	for _, secret := range []string{ruleOriginator, ruleUserAgent, "codex_cli_rs", "0.160.0", "plan-key"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("stored upstream metadata contains %q: %s", secret, encoded)
		}
	}
	masked := map[string]bool{}
	for _, header := range meta.RequestHeaders {
		if header.Value == contract.RedactedConfiguredValue {
			if !header.Redacted {
				t.Fatalf("%s carries the marker but is not flagged redacted", header.Name)
			}
			masked[header.Name] = true
		}
	}
	// The rule's two headers and the profile's own Version header are all
	// injected configuration; the name stays so the record is still diagnosable.
	for _, name := range []string{"originator", "user-agent", "version"} {
		if !masked[name] {
			t.Fatalf("%s was not masked with the fixed marker: %#v", name, meta.RequestHeaders)
		}
	}
}

func TestAuditProtectionTracksEachAttempt(t *testing.T) {
	session := &recordSession{upstreamHTTPMetaEnabled: true}
	request := &http.Request{Header: http.Header{
		"X-First": {"fake-first-value"}, "X-Second": {"fake-second-value"},
	}}
	for _, names := range [][]string{{"X-First"}, {"x-second"}, nil} {
		session.noteRequestRules(requestrewrite.Decision{Source: requestrewrite.SourceNone}, names)
		session.observeOutboundCapture(request)
		for _, header := range session.upstreamHTTPMeta.RequestHeaders {
			protected := len(names) > 0 && strings.EqualFold(header.Name, names[0])
			if header.Redacted != protected {
				t.Fatalf("protected %v: header %#v", names, header)
			}
			if protected && header.Value != contract.RedactedConfiguredValue {
				t.Fatalf("configured value stored: %#v", header)
			}
		}
	}
}

func TestAuditMasksServiceDefaultHeaders(t *testing.T) {
	upstream := newRuleUpstream(t)
	service := ruleService(upstream.url, contract.HTTPConnection{
		ExtraHeaders: map[string]string{"X-Relay-Client": "fake-relay-value"},
	})
	handler, blobs := auditedRuleHandler(t, service, nil, "gpt-6-nova")
	if response := serveRuleRequest(t, handler, "gpt-6-nova"); response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	sent, _ := upstream.sent(t)
	if sent.Get("X-Relay-Client") != "fake-relay-value" {
		t.Fatal("default header did not reach upstream")
	}
	meta, _ := storedUpstreamMeta(t, blobs)
	for _, header := range meta.RequestHeaders {
		if header.Name == "x-relay-client" {
			if !header.Redacted || header.Value != contract.RedactedConfiguredValue {
				t.Fatalf("configured header leaked: %#v", header)
			}
			return
		}
	}
	t.Fatal("configured header missing from audit")
}

// TestAuditKeepsUnconfiguredHeadersReadable keeps the masking scoped: a service
// without rules records the same metadata as before.
func TestAuditKeepsUnconfiguredHeadersReadable(t *testing.T) {
	upstream := newRuleUpstream(t)
	service := ruleService(upstream.url, contract.HTTPConnection{ModelRules: migratedModelRules()})
	handler, blobs := auditedRuleHandler(t, service, nil, "gpt-6-nova")
	if response := serveRuleRequest(t, handler, "gpt-6-nova"); response.Code != 200 {
		t.Fatalf("status = %d", response.Code)
	}
	meta, _ := storedUpstreamMeta(t, blobs)
	for _, header := range meta.RequestHeaders {
		if header.Value == contract.RedactedConfiguredValue {
			t.Fatalf("unmatched model masked %s", header.Name)
		}
		if header.Name == "content-type" && header.Value != "application/json" {
			t.Fatalf("content-type = %q", header.Value)
		}
	}
}
