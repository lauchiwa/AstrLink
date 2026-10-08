package forkcheckin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

const maxSessionCookies = 32

// NetworkCredential is the extension-owned, versioned envelope in Vault. It
// holds a dashboard session, never a Service API key or a site login password.
// Refresh tokens and browser-specific fields are deliberately unsupported.
// Use EncodeNetworkCredential for sealing; generic JSON/log output is denied.
type NetworkCredential struct {
	Version       int                       `json:"version"`
	Bearer        string                    `json:"bearer,omitempty"`
	BearerExpires *time.Time                `json:"bearer_expires,omitempty"`
	Cookies       []SessionCookie           `json:"cookies,omitempty"`
	Proxy         *contract.ProxyCredential `json:"proxy,omitempty"`
}

// SessionCookie is an immutable captured cookie. Empty Domain means host-only;
// empty Expires means a session cookie. Max-Age must be resolved to an absolute
// expiry by the importing layer, not restarted on every outgoing request.
type SessionCookie struct {
	Name     string     `json:"name"`
	Value    string     `json:"value"`
	Domain   string     `json:"domain,omitempty"`
	Path     string     `json:"path"`
	Secure   bool       `json:"secure"`
	HTTPOnly bool       `json:"http_only"`
	Expires  *time.Time `json:"expires,omitempty"`
}

// AccountSnapshot contains private Vault material. No-tags alone would not
// prevent encoding/json or fmt from exposing its exported Credential field.
func (AccountSnapshot) MarshalJSON() ([]byte, error) {
	return nil, ErrCredentialUnavailable
}

func (AccountSnapshot) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[private check-in snapshot]")
}

type networkCredentialWire NetworkCredential

func (NetworkCredential) MarshalJSON() ([]byte, error) {
	return nil, ErrCredentialUnavailable
}

func (NetworkCredential) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[private check-in credential]")
}

func (SessionCookie) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[private check-in cookie]")
}

// EncodeNetworkCredential is the sole explicit serialization path for a Vault
// write. The caller owns the returned plaintext and must clear it after Put.
func EncodeNetworkCredential(credential NetworkCredential) ([]byte, error) {
	if err := validateNetworkCredential(credential); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(networkCredentialWire(credential))
	if err != nil || len(encoded) > MaxCredentialBytes {
		clear(encoded)
		return nil, ErrCredentialUnavailable
	}
	return encoded, nil
}

func decodeNetworkCredential(encoded []byte, base *url.URL, mode NetworkMode) (NetworkCredential, error) {
	var wire networkCredentialWire
	if len(encoded) == 0 || len(encoded) > MaxCredentialBytes {
		return NetworkCredential{}, ErrCredentialUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return NetworkCredential{}, ErrCredentialUnavailable
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return NetworkCredential{}, ErrCredentialUnavailable
	}
	credential := NetworkCredential(wire)
	if err := validateNetworkCredential(credential); err != nil {
		return NetworkCredential{}, err
	}
	if credential.Proxy != nil && mode != NetworkModeCustom {
		return NetworkCredential{}, ErrCredentialUnavailable
	}
	for _, cookie := range credential.Cookies {
		domain := strings.TrimPrefix(strings.ToLower(cookie.Domain), ".")
		// Even a parent-domain cookie is narrowed to the exact authorized
		// origin. Never borrow a browser's domain-wide authority here.
		if domain != "" && domain != strings.ToLower(base.Hostname()) {
			return NetworkCredential{}, ErrCredentialUnavailable
		}
	}
	return credential, nil
}

func validateNetworkCredential(credential NetworkCredential) error {
	if credential.Version != 1 || (credential.Bearer == "" && len(credential.Cookies) == 0) || len(credential.Cookies) > maxSessionCookies {
		return ErrCredentialUnavailable
	}
	if len(credential.Bearer) > MaxCredentialBytes || credential.Bearer == "" && credential.BearerExpires != nil {
		return ErrCredentialUnavailable
	}
	for _, character := range credential.Bearer {
		if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("-._~+/=", character)) {
			return ErrCredentialUnavailable
		}
	}
	if credential.Proxy != nil && credential.Proxy.Validate() != nil {
		return ErrCredentialUnavailable
	}
	seen := make(map[string]bool)
	for _, cookie := range credential.Cookies {
		candidate := &http.Cookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path}
		if candidate.Valid() != nil || cookie.Path == "" || !safeNetworkPath(cookie.Path) {
			return ErrCredentialUnavailable
		}
		key := cookie.Name + "\x00" + cookie.Path
		if seen[key] {
			return ErrCredentialUnavailable
		}
		seen[key] = true
		if strings.HasPrefix(cookie.Name, "__Secure-") && !cookie.Secure {
			return ErrCredentialUnavailable
		}
		if strings.HasPrefix(cookie.Name, "__Host-") && (!cookie.Secure || cookie.Domain != "" || cookie.Path != "/") {
			return ErrCredentialUnavailable
		}
	}
	return nil
}

func (credential NetworkCredential) apply(request *http.Request, now time.Time) {
	if credential.Bearer != "" && (credential.BearerExpires == nil || now.Before(*credential.BearerExpires)) {
		request.Header.Set("Authorization", "Bearer "+credential.Bearer)
	}
	cookies := make([]SessionCookie, 0, len(credential.Cookies))
	for _, cookie := range credential.Cookies {
		if cookie.Secure && request.URL.Scheme != "https" || cookie.Expires != nil && !now.Before(*cookie.Expires) || !networkPathContains(cookie.Path, request.URL.Path) {
			continue
		}
		cookies = append(cookies, cookie)
	}
	// Match browser ordering for duplicate names at different paths. A root
	// cookie must not mask a more specific session on a mounted dashboard.
	sort.SliceStable(cookies, func(i, j int) bool { return len(cookies[i].Path) > len(cookies[j].Path) })
	for _, cookie := range cookies {
		request.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
}
