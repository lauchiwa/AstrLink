package accountauth

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// BuiltinIdentityFingerprint projects the shared baseline into an API-safe
// snapshot. It does not read Routing settings, version overrides or OAuth state.
func BuiltinIdentityFingerprint(client contract.IdentityClient) (contract.IdentityFingerprint, error) {
	var identity ClientIdentity
	switch client {
	case contract.IdentityClientCodexCLI:
		identity = DefaultCodexIdentity()
	case contract.IdentityClientClaudeCode:
		identity = DefaultClaudeIdentity()
	case contract.IdentityClientGrokCLI:
		identity = DefaultGrokIdentity()
	default:
		return contract.IdentityFingerprint{}, fmt.Errorf("identity client is invalid")
	}
	return projectAPIFingerprint(client, identity)
}

// CaptureIdentityFingerprint extracts an untrusted candidate from ORIGINAL
// inbound data. The caller must enforce local authentication, capture scope,
// privacy admission and operator confirmation; recognition is not attestation.
// It never changes the request, persists data, or updates subscription learning.
// Body is used only for Claude's bounded recognition check and is not retained.
func CaptureIdentityFingerprint(client contract.IdentityClient, original http.Header, body []byte) (contract.IdentityFingerprint, bool) {
	provider, ok := identitySubscriptionProvider(client)
	if !ok {
		return contract.IdentityFingerprint{}, false
	}
	header, ok := identityRecognitionHeaders(client, original)
	if !ok {
		return contract.IdentityFingerprint{}, false
	}
	var identity ClientIdentity
	switch client {
	case contract.IdentityClientCodexCLI:
		if !RecognizedCodexOfficialClient(header) {
			return contract.IdentityFingerprint{}, false
		}
		identity, ok = codexIdentityFromHeaders(header)
		if version := header.Get("Version"); version != "" && version != identity.Version {
			return contract.IdentityFingerprint{}, false
		}
	case contract.IdentityClientClaudeCode:
		if !RecognizedClaudeOfficialHeaders(header) || !ClaudeMetadataUserIDRecognized(body) {
			return contract.IdentityFingerprint{}, false
		}
		identity, ok = claudeIdentityFromHeaders(header)
	case contract.IdentityClientGrokCLI:
		if version := header.Get("X-Grok-Client-Version"); version != "" && !contract.ValidClientVersion(version) {
			return contract.IdentityFingerprint{}, false
		}
		identity, ok = grokIdentityFromHeaders(header)
		if ok {
			// Match subscription learning: learn the release, never the wrapper,
			// user-supplied identifier, or device/platform fingerprint.
			identity = grokIdentityAt(identity.Version)
		}
	}
	if !ok || !validLearnedIdentity(provider, identity) {
		return contract.IdentityFingerprint{}, false
	}
	fingerprint, err := projectAPIFingerprint(client, identity)
	return fingerprint, err == nil
}

// LearnedIdentityFingerprint copies a raw learned subscription identity for
// explicit API-profile import. Routing switches and version floors do not
// participate, and modifying the returned value cannot affect the registry.
// No learned entry means unavailable, not an implicit built-in fallback.
func (registry *IdentityRegistry) LearnedIdentityFingerprint(client contract.IdentityClient) (contract.IdentityFingerprint, bool) {
	provider, ok := identitySubscriptionProvider(client)
	if !ok {
		return contract.IdentityFingerprint{}, false
	}
	identity, ok := registry.learnedIdentity(provider)
	if !ok || !validLearnedIdentity(provider, identity) {
		return contract.IdentityFingerprint{}, false
	}
	fingerprint, err := projectAPIFingerprint(client, identity)
	return fingerprint, err == nil
}

// IdentityProfileReader reads a pinned snapshot without granting forwarding
// paths permission to list, create or confirm profiles.
type IdentityProfileReader interface {
	GetIdentityProfile(context.Context, contract.ServiceID, contract.IdentityProfileID) (storage.IdentityProfileRecord, error)
}

// LoadIdentityProfileHeaders shares the fail-closed profile lookup between
// inference, connection tests and model discovery.
func LoadIdentityProfileHeaders(
	ctx context.Context,
	reader IdentityProfileReader,
	serviceID contract.ServiceID,
	id contract.IdentityProfileID,
	auth contract.ServiceAuth,
) (http.Header, error) {
	if reader == nil {
		return nil, fmt.Errorf("identity profile %q is configured but profile storage is unavailable", id)
	}
	record, err := reader.GetIdentityProfile(ctx, serviceID, id)
	if err != nil {
		return nil, fmt.Errorf("load identity profile %q: %w", id, err)
	}
	if record.Profile.ID != id {
		return nil, fmt.Errorf("identity profile %q: snapshot ID does not match", id)
	}
	header, err := IdentityProfileHeaders(record.Profile, serviceID, auth)
	if err != nil {
		return nil, fmt.Errorf("identity profile %q: %w", id, err)
	}
	return header, nil
}

// IdentityProfileHeaders renders a confirmed, service-scoped frozen snapshot.
// auth must be the effective auth selected for the upstream protocol. This
// returns ONLY identity overrides, never credentials, OAuth markers, feature
// betas or request/session identifiers. It neither mutates inbound headers nor
// classifies the caller as an official client. The execution layer still owns
// conflict cleanup, protocol headers and final HTTP/WebSocket filtering.
func IdentityProfileHeaders(profile contract.IdentityProfile, serviceID contract.ServiceID, auth contract.ServiceAuth) (http.Header, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	if profile.ServiceID != serviceID {
		return nil, fmt.Errorf("identity profile belongs to a different service")
	}
	if profile.ConfirmedAt == nil {
		return nil, fmt.Errorf("identity profile requires confirmation")
	}
	if err := auth.Validate(); err != nil {
		return nil, fmt.Errorf("identity profile auth configuration is invalid")
	}
	header := make(http.Header)
	header.Set("User-Agent", profile.Fingerprint.UserAgent)
	for name, value := range profile.Fingerprint.Headers {
		header.Set(name, value)
	}
	switch profile.Client {
	case contract.IdentityClientCodexCLI:
		header.Set("Version", profile.Fingerprint.Version)
	case contract.IdentityClientGrokCLI:
		header.Set("X-Grok-Client-Version", profile.Fingerprint.Version)
		header.Set("X-Grok-Client-Identifier", grokUserAgentProduct)
	}
	if auth.Scheme == contract.AuthSchemeCustomHeader && len(header.Values(auth.HeaderName)) > 0 {
		return nil, fmt.Errorf("identity profile conflicts with the service authentication header")
	}
	return header, nil
}

func identitySubscriptionProvider(client contract.IdentityClient) (contract.SubscriptionProvider, bool) {
	switch client {
	case contract.IdentityClientCodexCLI:
		return contract.SubscriptionProviderOpenAICodex, true
	case contract.IdentityClientClaudeCode:
		return contract.SubscriptionProviderClaudeCode, true
	case contract.IdentityClientGrokCLI:
		return contract.SubscriptionProviderXAIGrok, true
	default:
		return "", false
	}
}

func projectAPIFingerprint(client contract.IdentityClient, identity ClientIdentity) (contract.IdentityFingerprint, error) {
	fingerprint := contract.IdentityFingerprint{
		UserAgent: identity.UserAgent,
		Version:   identity.Version,
		Headers:   make(map[string]string),
	}
	for name, value := range identity.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if !contract.IdentityFingerprintHeaderAllowed(client, canonical) {
			continue
		}
		if _, exists := fingerprint.Headers[canonical]; exists {
			return contract.IdentityFingerprint{}, fmt.Errorf("identity contains a repeated header")
		}
		fingerprint.Headers[canonical] = value
	}
	if err := fingerprint.Validate(client); err != nil {
		return contract.IdentityFingerprint{}, err
	}
	return fingerprint, nil
}

// identityRecognitionHeaders canonicalizes only fields used for recognition or
// the safe projection. Ambiguous repeated identity fields are rejected even if
// their map keys differ in case. Multiple Anthropic-Beta lines are legitimate
// recognition signals; none of their values are included in the snapshot.
func identityRecognitionHeaders(client contract.IdentityClient, original http.Header) (http.Header, bool) {
	header := make(http.Header)
	totalBytes := 0
	for name, values := range original {
		canonical := http.CanonicalHeaderKey(name)
		if !identityRecognitionHeader(client, canonical) {
			continue
		}
		if len(values) == 0 {
			return nil, false
		}
		limit := contract.MaxIdentityHeaderValueBytes
		if canonical == "User-Agent" {
			limit = contract.MaxIdentityUserAgentBytes
		} else if canonical == "Anthropic-Beta" {
			limit = 4096
		}
		for _, value := range values {
			totalBytes += len(value)
			if totalBytes > maxLearnedDocument || len(value) == 0 || len(value) > limit || strings.TrimSpace(value) != value {
				return nil, false
			}
			for _, character := range value {
				if character < 0x20 || character > 0x7e {
					return nil, false
				}
			}
			header[canonical] = append(header[canonical], value)
		}
		if canonical != "Anthropic-Beta" && len(header[canonical]) != 1 {
			return nil, false
		}
	}
	return header, true
}

func identityRecognitionHeader(client contract.IdentityClient, name string) bool {
	if name == "User-Agent" || contract.IdentityFingerprintHeaderAllowed(client, name) {
		return true
	}
	switch client {
	case contract.IdentityClientCodexCLI:
		return name == "Session-Id" || name == "Version"
	case contract.IdentityClientClaudeCode:
		return name == "Anthropic-Beta" || name == http.CanonicalHeaderKey(ClaudeCodeSessionHeader)
	case contract.IdentityClientGrokCLI:
		return name == "X-Grok-Client-Version"
	default:
		return false
	}
}
