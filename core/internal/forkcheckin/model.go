// Package forkcheckin holds the aggregated relay check-in extension. It is
// self-contained on purpose: a check-in account is not a contract.Service, so
// enabling, breaking or removing this package cannot change service
// documents, inference, stored provider credentials or the control contract.
//
// This file defines the extension's own account model and the boundary
// between private values and the public DTO. A site session is never a field
// of a public type: it lives only in [AccountSnapshot], which has no JSON
// representation, and is read through a [Vault].
package forkcheckin

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	// Resolves IANA zones without a system zoneinfo database, as the usage
	// summary and pricing already do.
	_ "time/tzdata"

	"github.com/QuantumNous/astrlink/core/contract"
)

const (
	// MaxDashboardURLLen matches the service base_url limit so a relay
	// address accepted here cannot be rejected when it is displayed.
	MaxDashboardURLLen = 2048
	// MaxCredentialBytes bounds one account's sealed session. Site sessions
	// are cookies and short tokens; anything larger is a mistake or an
	// attempt to use the vault as storage.
	MaxCredentialBytes = 64 << 10
	// MaxBoundServices bounds the services one account may be bound to.
	MaxBoundServices = 32
	maxAccountIDLen  = 64
	minAccountIDLen  = 3
)

// AccountID identifies a check-in account. It is the extension's own
// identifier: it is never a contract.ServiceID and never appears in a
// service document.
type AccountID string

// Validate accepts the same shape as the core resource identifiers, so an
// account ID is safe in a URL path, a window label and a sealing AAD.
func (id AccountID) Validate() error {
	value := string(id)
	if len(value) < minAccountIDLen || len(value) > maxAccountIDLen {
		return fmt.Errorf("account id must be %d-%d characters", minAccountIDLen, maxAccountIDLen)
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z',
			character >= '0' && character <= '9',
			character == '_':
		default:
			return fmt.Errorf("account id must use lowercase letters, digits or underscores")
		}
	}
	if value[0] < 'a' || value[0] > 'z' {
		return fmt.Errorf("account id must start with a letter")
	}
	return nil
}

// AccountState is the account's lifecycle, not the result of a check-in.
type AccountState string

const (
	// AccountStateDraft has been created but never verified against the
	// site, so it has no remote user and no stored session.
	AccountStateDraft AccountState = "draft"
	// AccountStateConnected has a session the site accepted, with the
	// remote user it reported.
	AccountStateConnected AccountState = "connected"
	// AccountStateAuthRequired had a session that the site has since
	// rejected. The account keeps its remote user so the operator can see
	// which login to repeat.
	AccountStateAuthRequired AccountState = "auth_required"
	// AccountStateManualRequired means the site needs a human: a challenge
	// page, or a dialect this build does not support.
	AccountStateManualRequired AccountState = "manual_required"
)

func (state AccountState) Validate() error {
	switch state {
	case AccountStateDraft, AccountStateConnected, AccountStateAuthRequired, AccountStateManualRequired:
		return nil
	default:
		return fmt.Errorf("unknown account state %q", string(state))
	}
}

// requiresIdentity reports whether the state may only exist for an account
// the site has already identified.
func (state AccountState) requiresIdentity() bool {
	return state != AccountStateDraft
}

// NetworkMode selects how check-in requests leave the machine. The extension
// resolves it with the shared proxy helpers but keeps its own value, so the
// routing of inference traffic is never changed from here.
type NetworkMode string

const (
	// NetworkModeDirect ignores any system proxy.
	NetworkModeDirect NetworkMode = "direct"
	// NetworkModeSystem uses the operating system's proxy configuration.
	NetworkModeSystem NetworkMode = "system"
	// NetworkModeCustom uses Network.ProxyURL.
	NetworkModeCustom NetworkMode = "custom"
)

// Network is the account's public egress configuration. Proxy authentication
// is not part of it; those secrets live in the vault, like the session.
type Network struct {
	Mode NetworkMode `json:"mode"`
	// ProxyURL is set only for NetworkModeCustom and carries no userinfo.
	ProxyURL string `json:"proxy_url,omitempty"`
}

func (network Network) Validate() error {
	switch network.Mode {
	case NetworkModeDirect, NetworkModeSystem:
		if network.ProxyURL != "" {
			return fmt.Errorf("only custom network mode may specify a proxy url")
		}
	case NetworkModeCustom:
		if network.ProxyURL == "" {
			return fmt.Errorf("custom network mode requires a proxy url")
		}
		// Reuses the service proxy rules, so a URL accepted here is one the
		// shared proxy resolver can already dial.
		if err := contract.ValidateProxyURL(network.ProxyURL); err != nil {
			return err
		}
	default:
		return fmt.Errorf("network mode must be direct, system, or custom")
	}
	return nil
}

// Account is the extension's private account record. Only the fields in
// [AccountView] are ever published.
type Account struct {
	ID AccountID
	// DashboardBaseURL is the site root the operator signs in to, including
	// any sub-path. Check-in requests are built under it.
	DashboardBaseURL string
	State            AccountState
	// Revision increases on every change the operator can make. A check-in
	// or an authorization that was prepared for an older revision is
	// refused rather than applied to a changed account.
	Revision int64
	Network  Network
	// TimeZone is the explicit IANA zone the daily schedule uses. The site's
	// own date still wins when it reports one.
	TimeZone string
	// Automatic is off until the operator turns it on for this account.
	Automatic bool
	// RemoteUserID is what the site reported for the stored session. It is
	// empty only for a draft.
	RemoteUserID string
	// BoundServices are the services this account funds. The binding is
	// stored here; the services themselves are read-only to this package.
	BoundServices []contract.ServiceID
}

// Validate checks the account on its own. Uniqueness across accounts is a
// storage concern; see [AccountIdentity].
func (account Account) Validate() error {
	if err := account.ID.Validate(); err != nil {
		return err
	}
	if err := account.State.Validate(); err != nil {
		return err
	}
	if _, err := NormalizeDashboardURL(account.DashboardBaseURL); err != nil {
		return err
	}
	if err := account.Network.Validate(); err != nil {
		return err
	}
	if err := ValidateTimeZone(account.TimeZone); err != nil {
		return err
	}
	if account.Revision < 1 {
		return fmt.Errorf("revision must be positive")
	}
	if account.State.requiresIdentity() && account.RemoteUserID == "" {
		return fmt.Errorf("a %s account requires the remote user the site reported", account.State)
	}
	if account.State == AccountStateDraft && account.RemoteUserID != "" {
		return fmt.Errorf("a draft account has no verified remote user")
	}
	if err := validateRemoteUserID(account.RemoteUserID); err != nil {
		return err
	}
	// Automatic check-in needs a session the site has accepted; otherwise
	// the scheduler would wake up only to report that login is required.
	if account.Automatic && account.State != AccountStateConnected {
		return fmt.Errorf("automatic check-in requires a connected account")
	}
	return validateBoundServices(account.BoundServices)
}

func validateRemoteUserID(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 128 {
		return fmt.Errorf("remote user id exceeds 128 characters")
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("remote user id must not be padded")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("remote user id must not contain control characters")
		}
	}
	return nil
}

func validateBoundServices(services []contract.ServiceID) error {
	if len(services) > MaxBoundServices {
		return fmt.Errorf("an account may fund at most %d services", MaxBoundServices)
	}
	seen := make(map[contract.ServiceID]struct{}, len(services))
	for _, id := range services {
		if err := id.Validate(); err != nil {
			return err
		}
		if _, duplicate := seen[id]; duplicate {
			return fmt.Errorf("service %q is bound twice", id)
		}
		seen[id] = struct{}{}
	}
	return nil
}

// ValidateTimeZone requires an explicit IANA zone. "Local" is refused
// because a scheduled check-in must not silently change its day when the
// host's zone changes.
func ValidateTimeZone(value string) error {
	if value == "" {
		return fmt.Errorf("time zone is required")
	}
	if value == "Local" {
		return fmt.Errorf("time zone must be an explicit IANA zone, not %q", value)
	}
	if strings.TrimSpace(value) != value || len(value) > 64 {
		return fmt.Errorf("invalid time zone %q", value)
	}
	if _, err := time.LoadLocation(value); err != nil {
		return fmt.Errorf("unknown time zone %q", value)
	}
	return nil
}

// NormalizeDashboardURL validates a relay dashboard address and returns its
// canonical form: lowercase host, no trailing slash, sub-path preserved.
//
// Plaintext http is accepted only for an explicit loopback host, matching
// the loopback exception the subscription issuers already use; a remote site
// must be https so a session is never sent in the clear.
func NormalizeDashboardURL(value string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("dashboard url is required")
	}
	if len(value) > MaxDashboardURLLen {
		return "", fmt.Errorf("dashboard url exceeds %d characters", MaxDashboardURLLen)
	}
	if strings.TrimSpace(value) != value || strings.ContainsAny(value, " \r\n\t") {
		return "", fmt.Errorf("dashboard url must not contain whitespace")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("parse dashboard url: %w", err)
	}
	if parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("dashboard url must be an absolute http(s) url")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("dashboard url must not contain credentials")
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" {
		return "", fmt.Errorf("dashboard url must not contain a query or fragment")
	}
	host := strings.ToLower(parsed.Hostname())
	if parsed.Scheme == "http" && !isLoopbackHost(host) {
		return "", fmt.Errorf("dashboard url must use https outside loopback")
	}
	if parsed.Opaque != "" {
		return "", fmt.Errorf("dashboard url must not be opaque")
	}
	if port := parsed.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return "", fmt.Errorf("dashboard url has an invalid port")
		}
	} else if strings.HasSuffix(parsed.Host, ":") {
		return "", fmt.Errorf("dashboard url has an invalid port")
	}
	if !safeNetworkPath(parsed.Path) || unsafeNetworkEscape(parsed.EscapedPath()) {
		return "", fmt.Errorf("dashboard url path is ambiguous or contains relative segments")
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	normalized := url.URL{Scheme: parsed.Scheme, Host: strings.ToLower(parsed.Host), Path: path}
	return normalized.String(), nil
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

// AccountIdentity is the uniqueness key for a connected account: one site
// plus one remote user. The same relay may hold several accounts, and the
// same account may fund several services, but a site and user pair must not
// exist twice, or one day would produce two check-ins.
type AccountIdentity struct {
	DashboardBaseURL string
	RemoteUserID     string
}

// Identity returns the uniqueness key, or false for a draft that the site
// has not identified yet.
func (account Account) Identity() (AccountIdentity, bool) {
	if account.RemoteUserID == "" {
		return AccountIdentity{}, false
	}
	normalized, err := NormalizeDashboardURL(account.DashboardBaseURL)
	if err != nil {
		return AccountIdentity{}, false
	}
	return AccountIdentity{DashboardBaseURL: normalized, RemoteUserID: account.RemoteUserID}, true
}

// ConfigFingerprint covers everything that decides where a stored session is
// sent and whose session it is. A stored authorization is only reused while
// the fingerprint is unchanged, so editing the address, the egress or the
// bound services cannot quietly redirect an existing session.
//
// The fingerprint is derived here and stored by this extension; it is never
// written into a service document.
func (account Account) ConfigFingerprint() string {
	normalized, err := NormalizeDashboardURL(account.DashboardBaseURL)
	if err != nil {
		normalized = "invalid"
	}
	services := make([]string, 0, len(account.BoundServices))
	for _, id := range account.BoundServices {
		services = append(services, string(id))
	}
	sort.Strings(services)
	digest := sha256.New()
	for _, field := range []string{
		string(account.ID),
		normalized,
		string(account.Network.Mode),
		account.Network.ProxyURL,
		account.RemoteUserID,
		strings.Join(services, ","),
	} {
		// Length-prefixed so no two field layouts share a digest.
		fmt.Fprintf(digest, "%d:%s\n", len(field), field)
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// AccountView is the only account shape that leaves the process. It has no
// session, no cookie, no proxy password and no vault handle; the fields it
// does carry are the ones the workspace renders.
type AccountView struct {
	ID               AccountID            `json:"id"`
	DashboardBaseURL string               `json:"dashboard_base_url"`
	State            AccountState         `json:"state"`
	Revision         int64                `json:"revision"`
	Network          Network              `json:"network"`
	TimeZone         string               `json:"time_zone"`
	Automatic        bool                 `json:"automatic"`
	RemoteUserID     string               `json:"remote_user_id,omitempty"`
	BoundServices    []contract.ServiceID `json:"bound_services"`
	// ConfigFingerprint lets the UI resume an authorization only while the
	// account it was started for is unchanged.
	ConfigFingerprint string `json:"config_fingerprint"`
}

// View projects the public DTO. Adding a secret to [Account] cannot leak it
// through this method, because every published field is listed here.
func (account Account) View() AccountView {
	services := account.BoundServices
	if services == nil {
		services = []contract.ServiceID{}
	}
	return AccountView{
		ID:                account.ID,
		DashboardBaseURL:  account.DashboardBaseURL,
		State:             account.State,
		Revision:          account.Revision,
		Network:           account.Network,
		TimeZone:          account.TimeZone,
		Automatic:         account.Automatic,
		RemoteUserID:      account.RemoteUserID,
		BoundServices:     services,
		ConfigFingerprint: account.ConfigFingerprint(),
	}
}

// AccountSnapshot is what an adapter receives: a validated account plus the
// session bytes for this one call. Generic JSON encoding is refused and fmt
// formatting is redacted; only the explicit Vault codec serializes secrets.
type AccountSnapshot struct {
	Account Account
	// Credential is opaque to storage and adapters' callers. The network
	// layer decodes its versioned envelope; site protocols receive only
	// credentials scoped to this snapshot's authorized destination.
	Credential []byte
}

// Validate refuses a snapshot that could not produce a meaningful request.
func (snapshot AccountSnapshot) Validate() error {
	if err := snapshot.Account.Validate(); err != nil {
		return err
	}
	if len(snapshot.Credential) == 0 {
		return fmt.Errorf("%w: account %q has no stored session", ErrCredentialUnavailable, snapshot.Account.ID)
	}
	if len(snapshot.Credential) > MaxCredentialBytes {
		return fmt.Errorf("stored session exceeds %d bytes", MaxCredentialBytes)
	}
	return nil
}
