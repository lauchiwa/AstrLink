package contract

import (
	"fmt"
	"maps"
	"net/http"
	"strings"
	"time"
)

// IdentityClient selects a reusable outbound fingerprint. Unlike ClientType,
// it is explicit configuration, not an inbound classification or proof of trust.
type IdentityClient string

const (
	IdentityClientCodexCLI   IdentityClient = "codex_cli"
	IdentityClientClaudeCode IdentityClient = "claude_code"
	IdentityClientGrokCLI    IdentityClient = "grok_cli"

	MaxIdentityUserAgentBytes   = 1024
	MaxIdentityHeaderValueBytes = 256
	MaxIdentityHeaders          = 16
)

func (client IdentityClient) Valid() bool {
	return client == IdentityClientCodexCLI || client == IdentityClientClaudeCode || client == IdentityClientGrokCLI
}

// IsCodexIdentityProduct is shared by recognition and saved-profile validation.
// A matching product is a format check, never authentication of a client binary.
func IsCodexIdentityProduct(product string) bool {
	switch product {
	case "codex-tui", "codex_cli_rs", "codex_vscode", "codex_vscode_copilot",
		"codex_app", "codex_chatgpt_desktop", "codex_atlas", "codex_exec", "codex_sdk_ts":
		return true
	default:
		return false
	}
}

// IdentityFingerprint contains only reusable identity fields. Versions of
// generated companion headers are derived from Version, not stored twice.
// Headers use canonical HTTP names. Credentials, session markers, feature
// betas, retry counts, timeouts and request bodies never belong here.
type IdentityFingerprint struct {
	UserAgent string            `json:"user_agent"`
	Version   string            `json:"version"`
	Headers   map[string]string `json:"headers,omitempty"`
}

// IdentityFingerprintHeaderAllowed is a closed projection, not a prefix rule.
// In particular, arbitrary X-Stainless-* fields are not safe to learn.
func IdentityFingerprintHeaderAllowed(client IdentityClient, name string) bool {
	switch client {
	case IdentityClientCodexCLI:
		return name == "Originator"
	case IdentityClientClaudeCode:
		switch name {
		case "X-App", "X-Stainless-Lang", "X-Stainless-Package-Version",
			"X-Stainless-Os", "X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version":
			return true
		}
	}
	return false
}

// Validate checks the frozen tuple, not today's subscription version floor.
// Learning admission can reject implausible versions; loading a pinned profile
// must not silently upgrade or invalidate it when a built-in version changes.
func (fingerprint IdentityFingerprint) Validate(client IdentityClient) error {
	if !client.Valid() {
		return fmt.Errorf("identity client is invalid")
	}
	if !ValidClientVersion(fingerprint.Version) {
		return fmt.Errorf("identity version is invalid")
	}
	if !identityText(fingerprint.UserAgent, MaxIdentityUserAgentBytes) {
		return fmt.Errorf("identity user_agent is invalid")
	}
	product, tail, found := strings.Cut(fingerprint.UserAgent, "/")
	version, _, _ := strings.Cut(tail, " ")
	if !found || version != fingerprint.Version {
		return fmt.Errorf("identity user_agent and version disagree")
	}
	if len(fingerprint.Headers) > MaxIdentityHeaders {
		return fmt.Errorf("identity has too many headers")
	}
	for name, value := range fingerprint.Headers {
		if name != http.CanonicalHeaderKey(name) || !IdentityFingerprintHeaderAllowed(client, name) {
			return fmt.Errorf("identity contains an unsupported header")
		}
		if !identityText(value, MaxIdentityHeaderValueBytes) {
			return fmt.Errorf("identity contains an invalid header value")
		}
	}
	switch client {
	case IdentityClientCodexCLI:
		if !IsCodexIdentityProduct(product) || fingerprint.Headers["Originator"] != product {
			return fmt.Errorf("Codex identity product and originator disagree")
		}
	case IdentityClientClaudeCode:
		if product != "claude-cli" || !ValidClientVersion(fingerprint.Headers["X-Stainless-Package-Version"]) {
			return fmt.Errorf("Claude identity requires its client product and SDK version")
		}
		if app, present := fingerprint.Headers["X-App"]; present && app != "cli" {
			return fmt.Errorf("Claude identity app is invalid")
		}
	case IdentityClientGrokCLI:
		if product != "grok-shell" {
			return fmt.Errorf("Grok identity product is invalid")
		}
	}
	return nil
}

func (fingerprint IdentityFingerprint) Clone() IdentityFingerprint {
	fingerprint.Headers = maps.Clone(fingerprint.Headers)
	return fingerprint
}

func identityText(value string, limit int) bool {
	if value == "" || len(value) > limit || strings.TrimSpace(value) != value ||
		strings.Contains(strings.ToLower(value), "astrlink") {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character > 0x7e {
			return false
		}
	}
	return true
}

type IdentityProfileID string

func (id IdentityProfileID) Validate() error {
	return validateResourceID("identity profile", string(id))
}

type IdentityProfileSource string

const (
	IdentityProfileBuiltin            IdentityProfileSource = "builtin"
	IdentityProfileSubscriptionImport IdentityProfileSource = "subscription_import"
	IdentityProfileRequestCapture     IdentityProfileSource = "request_capture"
)

func (source IdentityProfileSource) Valid() bool {
	return source == IdentityProfileBuiltin || source == IdentityProfileSubscriptionImport || source == IdentityProfileRequestCapture
}

// IdentityProfile is one service-scoped snapshot, not a link to the mutable
// subscription registry. A replacement snapshot gets a new ID. A candidate is
// not usable for forwarding until ConfirmedAt is set by an operator action.
// This type intentionally has no credentials, body, session ID or device ID.
type IdentityProfile struct {
	ID          IdentityProfileID     `json:"id"`
	ServiceID   ServiceID             `json:"service_id"`
	Client      IdentityClient        `json:"client"`
	Source      IdentityProfileSource `json:"source"`
	Fingerprint IdentityFingerprint   `json:"fingerprint"`
	CreatedAt   time.Time             `json:"created_at"`
	ObservedAt  *time.Time            `json:"observed_at,omitempty"`
	ConfirmedAt *time.Time            `json:"confirmed_at,omitempty"`
}

func (profile IdentityProfile) Validate() error {
	if err := profile.ID.Validate(); err != nil {
		return err
	}
	if err := profile.ServiceID.Validate(); err != nil {
		return err
	}
	if !profile.Source.Valid() {
		return fmt.Errorf("identity profile source is invalid")
	}
	if err := profile.Fingerprint.Validate(profile.Client); err != nil {
		return err
	}
	if profile.CreatedAt.IsZero() {
		return fmt.Errorf("identity profile created_at is required")
	}
	if profile.Source == IdentityProfileRequestCapture && profile.ObservedAt == nil {
		return fmt.Errorf("captured identity requires observed_at")
	}
	if profile.Source == IdentityProfileBuiltin && profile.ObservedAt != nil {
		return fmt.Errorf("built-in identity cannot have an observation time")
	}
	if profile.ObservedAt != nil && (profile.ObservedAt.IsZero() || profile.ObservedAt.After(profile.CreatedAt)) {
		return fmt.Errorf("identity observation must precede creation")
	}
	if profile.ConfirmedAt != nil && (profile.ConfirmedAt.IsZero() || profile.ConfirmedAt.Before(profile.CreatedAt)) {
		return fmt.Errorf("identity confirmation must follow creation")
	}
	return nil
}

// IdentityCaptureStatus reports one service's explicitly armed capture window.
// Arming is deliberately in-memory: consent does not survive a restart, and a
// window closes as soon as one candidate is published. A window never changes
// forwarding, and a published candidate still requires operator confirmation.
type IdentityCaptureStatus struct {
	ServiceID ServiceID      `json:"service_id"`
	Armed     bool           `json:"armed"`
	Client    IdentityClient `json:"client,omitempty"`
	ArmedAt   *time.Time     `json:"armed_at,omitempty"`
	ExpiresAt *time.Time     `json:"expires_at,omitempty"`
	// CapturedProfile names the candidate this window published, so the UI can
	// show which snapshot to review instead of re-reading the request.
	CapturedProfile IdentityProfileID `json:"captured_profile,omitempty"`
	// Rejected counts observations that did not match the selected client's
	// recognized shape. It is a format signal, never proof of a real client.
	Rejected int `json:"rejected"`
}

func (profile IdentityProfile) Clone() IdentityProfile {
	profile.Fingerprint = profile.Fingerprint.Clone()
	if profile.ObservedAt != nil {
		observed := *profile.ObservedAt
		profile.ObservedAt = &observed
	}
	if profile.ConfirmedAt != nil {
		confirmed := *profile.ConfirmedAt
		profile.ConfirmedAt = &confirmed
	}
	return profile
}
