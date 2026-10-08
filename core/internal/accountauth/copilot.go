package accountauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/transport"
)

// GitHub Copilot login follows OpenCode, the third-party client GitHub
// supports for Copilot plans: RFC 8628 device authorization against
// github.com with OpenCode's public OAuth app, after which the GitHub token
// itself authenticates Copilot API calls. AstrLink never embeds a client
// secret; the client ID below is the public one shipped in OpenCode.
const (
	DefaultCopilotIssuer        = "https://github.com"
	DefaultCopilotClientID      = "Ov23li8tweQw6odWQebz"
	DefaultCopilotAPIBaseURL    = "https://api.githubcopilot.com"
	DefaultGitHubAPIBaseURL     = "https://api.github.com"
	DefaultCopilotDeviceCodeTTL = 15 * time.Minute

	// copilotTokenLifetime stands in for "until revoked": GitHub OAuth app
	// tokens do not expire, so the account is only re-checked when Copilot
	// rejects the token (see Manager.HandleUnauthorized).
	copilotTokenLifetime = 10 * 365 * 24 * time.Hour

	ErrCodeCopilotNotEntitled = "copilot_not_entitled"
)

// ErrCopilotNotEntitled reports a GitHub account without Copilot access.
var ErrCopilotNotEntitled = errors.New("github account has no copilot access")

// DefaultCopilotScopes is what OpenCode requests: reading the user profile.
func DefaultCopilotScopes() []string {
	return []string{"read:user"}
}

func normalizeCopilotConfig(config OAuthConfig) OAuthConfig {
	if strings.TrimSpace(config.ClientID) == "" {
		config.ClientID = DefaultCopilotClientID
	}
	if config.Issuer == "" {
		config.Issuer = DefaultCopilotIssuer
	}
	if config.TokenURL == "" {
		config.TokenURL = strings.TrimRight(config.Issuer, "/") + "/login/oauth/access_token"
	}
	if config.APIBaseURL == "" {
		config.APIBaseURL = DefaultCopilotAPIBaseURL
	}
	if config.UserInfoURL == "" {
		config.UserInfoURL = DefaultGitHubAPIBaseURL + "/user"
	}
	if config.UsageURL == "" {
		config.UsageURL = DefaultGitHubAPIBaseURL + "/copilot_internal/user"
	}
	if len(config.Scopes) == 0 {
		config.Scopes = DefaultCopilotScopes()
	}
	if config.DeviceCodeTTL <= 0 {
		config.DeviceCodeTTL = DefaultCopilotDeviceCodeTTL
	}
	return config
}

// ApplyCopilotAPIHeaders writes OpenCode's Copilot API identity: the GitHub
// token as Bearer, its User-Agent, pinned API version and chat intent.
// providerapi.CopilotRequest adds the per-request headers.
func ApplyCopilotAPIHeaders(header http.Header, tokens AccountTokens) {
	if header == nil {
		return
	}
	identity := DefaultCopilotIdentity()
	header.Set("Authorization", "Bearer "+tokens.AccessToken)
	header.Set("User-Agent", identity.UserAgent)
	for name, value := range identity.Headers {
		header.Set(name, value)
	}
	header.Set("Openai-Intent", "conversation-edits")
}

// copilotGitHubRequest is an api.github.com GET as OpenCode's token would
// make it: the GitHub token scheme and the OpenCode User-Agent alone.
func copilotGitHubRequest(ctx context.Context, endpoint, token string) (*http.Request, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "token "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", DefaultCopilotIdentity().UserAgent)
	request.Header.Set("Accept-Encoding", transport.SupportedResponseEncodings)
	return request, nil
}

// CopilotEntitlement is the part of GET /copilot_internal/user AstrLink reads:
// the plan and the quota snapshots keyed by quota (chat, completions,
// premium_interactions).
type CopilotEntitlement struct {
	Plan           string                          `json:"copilot_plan"`
	QuotaResetDate string                          `json:"quota_reset_date"`
	QuotaResetUTC  string                          `json:"quota_reset_date_utc"`
	QuotaSnapshots map[string]CopilotQuotaSnapshot `json:"quota_snapshots"`
}

type CopilotQuotaSnapshot struct {
	Entitlement      *float64 `json:"entitlement"`
	Remaining        *float64 `json:"remaining"`
	PercentRemaining *float64 `json:"percent_remaining"`
	Unlimited        bool     `json:"unlimited"`
}

// FetchCopilotEntitlement reads the account's Copilot plan with its GitHub
// token. A rejected token is ErrInvalidGrant; an account without Copilot
// access is ErrCopilotNotEntitled.
func FetchCopilotEntitlement(ctx context.Context, client *http.Client, endpoint, token string) (CopilotEntitlement, error) {
	request, err := copilotGitHubRequest(ctx, endpoint, token)
	if err != nil {
		return CopilotEntitlement{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return CopilotEntitlement{}, err
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return CopilotEntitlement{}, ErrInvalidGrant
	case http.StatusForbidden, http.StatusNotFound:
		return CopilotEntitlement{}, ErrCopilotNotEntitled
	default:
		return CopilotEntitlement{}, fmt.Errorf("copilot entitlement returned status %d", response.StatusCode)
	}
	body, err := transport.ReadResponseBody(response, 1<<20)
	if err != nil {
		return CopilotEntitlement{}, err
	}
	var entitlement CopilotEntitlement
	if err := json.Unmarshal(body, &entitlement); err != nil {
		return CopilotEntitlement{}, fmt.Errorf("decode copilot entitlement: %w", err)
	}
	return entitlement, nil
}

// completeCopilotAccount turns a GitHub token into stored account tokens:
// the numeric GitHub user id identifies the account and the Copilot plan
// must exist, or the login fails instead of saving an account that cannot
// serve requests.
func (client *TokenClient) completeCopilotAccount(ctx context.Context, token string) (AccountTokens, error) {
	request, err := copilotGitHubRequest(ctx, client.config.UserInfoURL, token)
	if err != nil {
		return AccountTokens{}, err
	}
	response, err := client.config.HTTPClient.Do(request)
	if err != nil {
		return AccountTokens{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return AccountTokens{}, fmt.Errorf("github user returned status %d", response.StatusCode)
	}
	body, err := transport.ReadResponseBody(response, 1<<20)
	if err != nil {
		return AccountTokens{}, err
	}
	var user struct {
		ID json.Number `json:"id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&user); err != nil {
		return AccountTokens{}, fmt.Errorf("decode github user: %w", err)
	}
	if _, err := strconv.ParseUint(user.ID.String(), 10, 64); err != nil {
		return AccountTokens{}, fmt.Errorf("github user omitted its id")
	}
	entitlement, err := FetchCopilotEntitlement(ctx, client.config.HTTPClient, client.config.UsageURL, token)
	if err != nil {
		return AccountTokens{}, err
	}
	return AccountTokens{
		AccessToken:  token,
		RefreshToken: token,
		TokenType:    "bearer",
		AccountID:    user.ID.String(),
		PlanType:     strings.TrimSpace(entitlement.Plan),
		ExpiresAt:    client.config.Now().UTC().Add(copilotTokenLifetime),
	}, nil
}

// refreshCopilot re-checks a GitHub token that Copilot rejected. The token
// is its own refresh token, so a still-valid one is returned unchanged; a
// revoked one is ErrInvalidGrant and the account needs a new sign-in.
func (client *TokenClient) refreshCopilot(ctx context.Context, token string) (AccountTokens, error) {
	entitlement, err := FetchCopilotEntitlement(ctx, client.config.HTTPClient, client.config.UsageURL, token)
	if err != nil {
		return AccountTokens{}, err
	}
	return AccountTokens{
		AccessToken:  token,
		RefreshToken: token,
		TokenType:    "bearer",
		PlanType:     strings.TrimSpace(entitlement.Plan),
		ExpiresAt:    client.config.Now().UTC().Add(copilotTokenLifetime),
	}, nil
}

func applyCopilotOAuthHeaders(header http.Header) {
	header.Set("Accept", "application/json")
	header.Set("Content-Type", "application/json")
	header.Set("User-Agent", DefaultCopilotIdentity().UserAgent)
}

type copilotDeviceCodeResponse struct {
	DeviceCode      string          `json:"device_code"`
	UserCode        string          `json:"user_code"`
	VerificationURI string          `json:"verification_uri"`
	ExpiresIn       int64           `json:"expires_in"`
	Interval        json.RawMessage `json:"interval"`
}

func (manager *SessionManager) requestCopilotDeviceAuthorization(ctx context.Context) (polledDeviceAuthorization, error) {
	body, err := json.Marshal(map[string]string{
		"client_id": manager.config.ClientID,
		"scope":     strings.Join(manager.config.Scopes, " "),
	})
	if err != nil {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	endpoint := strings.TrimRight(manager.config.Issuer, "/") + "/login/device/code"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	applyCopilotOAuthHeaders(request.Header)
	response, err := manager.config.HTTPClient.Do(request)
	if err != nil {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return polledDeviceAuthorization{}, ErrDeviceCodeUnavailable
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	var parsed copilotDeviceCodeResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	verification := strings.TrimSpace(parsed.VerificationURI)
	if strings.TrimSpace(parsed.DeviceCode) == "" || len(parsed.DeviceCode) > 4096 ||
		!validDeviceUserCode(parsed.UserCode) || len(verification) > 4096 {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	public := contract.AuthorizationDeviceCode{VerificationURL: verification, UserCode: parsed.UserCode}
	if public.Validate() != nil {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	interval, err := parseDevicePollInterval(parsed.Interval)
	if err != nil {
		return polledDeviceAuthorization{}, fmt.Errorf("%w", ErrDeviceCodeRequestFailed)
	}
	if interval <= 0 {
		interval = 5 * time.Second
	}
	device := polledDeviceAuthorization{
		VerificationURL: verification,
		UserCode:        parsed.UserCode,
		DeviceCode:      parsed.DeviceCode,
		PollInterval:    manager.clampDevicePollInterval(interval),
	}
	if parsed.ExpiresIn > 0 {
		device.ExpiresIn = time.Duration(parsed.ExpiresIn) * time.Second
	}
	return device, nil
}

// pollCopilotDeviceAuthorizationOnce exchanges the device code once. GitHub
// answers pending and slow_down with HTTP 200 and an error field, so the
// error is read before the token.
func (manager *SessionManager) pollCopilotDeviceAuthorizationOnce(
	ctx context.Context,
	device polledDeviceAuthorization,
) (AccountTokens, bool, bool, error) {
	body, err := json.Marshal(map[string]string{
		"client_id":   manager.config.ClientID,
		"device_code": device.DeviceCode,
		"grant_type":  standardDeviceGrantType,
	})
	if err != nil {
		return AccountTokens{}, false, false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, manager.tokens.config.TokenURL, bytes.NewReader(body))
	if err != nil {
		return AccountTokens{}, false, false, err
	}
	applyCopilotOAuthHeaders(request.Header)
	response, err := manager.config.HTTPClient.Do(request)
	if err != nil {
		return AccountTokens{}, false, false, err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return AccountTokens{}, false, false, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return AccountTokens{}, false, false, fmt.Errorf("device token endpoint returned status %d", response.StatusCode)
	}
	var parsed struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(raw), &parsed); err != nil {
		return AccountTokens{}, false, false, err
	}
	switch parsed.Error {
	case "":
	case "authorization_pending":
		return AccountTokens{}, true, false, nil
	case "slow_down":
		return AccountTokens{}, true, true, nil
	default:
		// access_denied, expired_token and unknown codes end the session.
		return AccountTokens{}, false, false, fmt.Errorf("device authorization ended: %s", parsed.Error)
	}
	token := strings.TrimSpace(parsed.AccessToken)
	if token == "" || len(token) > 4096 || !validHeaderToken(token) {
		return AccountTokens{}, false, false, fmt.Errorf("device token response is incomplete")
	}
	tokens, err := manager.tokens.completeCopilotAccount(ctx, token)
	return tokens, false, false, err
}

func validHeaderToken(token string) bool {
	for _, r := range token {
		if r <= 0x20 || r >= 0x7f {
			return false
		}
	}
	return true
}
