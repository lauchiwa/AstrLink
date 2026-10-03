package contract

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	// MaxModelRules bounds one service's rule list. Selection is linear, and a
	// list this long already means the UI should group models instead.
	MaxModelRules = 200
	// MaxConfiguredHeaders bounds both the default overlay and one rule.
	MaxConfiguredHeaders = 32
	// MaxConfiguredHeaderValueBytes bounds one configured value.
	MaxConfiguredHeaderValueBytes = 1024
	// MaxModelRuleMatchRunes bounds one match expression.
	MaxModelRuleMatchRunes = 256
	// ModelRuleMatchAll is the only wildcard this version accepts.
	ModelRuleMatchAll = "*"
)

// HeaderClass records who owns an outbound request header. Only an overridable
// header may be configured: the remaining classes are owned by the gateway's
// credential handling, the HTTP transport, or a live session binding, and
// letting configuration reach them would leak credentials, corrupt framing, or
// silently redirect a bound channel.
type HeaderClass string

const (
	// HeaderClassOverridable covers provider compatibility and identity fields
	// a request rule may set, such as User-Agent or Originator.
	HeaderClassOverridable HeaderClass = "overridable"
	// HeaderClassAuthReserved covers caller and upstream credentials, including
	// the service's own resolved custom auth header and account bindings.
	HeaderClassAuthReserved HeaderClass = "auth_reserved"
	// HeaderClassTransportReserved covers framing, compression, connection and
	// content negotiation the gateway must control to forward a body correctly.
	HeaderClassTransportReserved HeaderClass = "transport_reserved"
	// HeaderClassSessionReserved covers per-session and per-request binding
	// markers that must describe the actual request, not configuration.
	HeaderClassSessionReserved HeaderClass = "session_reserved"
	// HeaderClassGatewayReserved covers the local X-AstrLink-* namespace, which
	// is stripped before forwarding and must never be configurable.
	HeaderClassGatewayReserved HeaderClass = "gateway_reserved"
)

// authReservedHeaders must stay aligned with the forwarder's inbound
// credential removal and with every credential an authorizer supplies.
var authReservedHeaders = map[string]struct{}{
	"Authorization":       {},
	"Proxy-Authorization": {},
	"Cookie":              {},
	"Set-Cookie":          {},
	"X-Api-Key":           {},
	"X-Goog-Api-Key":      {},
	"Api-Key":             {},
	"X-Xai-Token-Auth":    {},
	"Chatgpt-Account-Id":  {},
	"Oai-Product-Sku":     {},
	"Www-Authenticate":    {},
	"Proxy-Authenticate":  {},
}

// transportReservedHeaders covers framing, hop-by-hop control, compression and
// content negotiation. A rule that changed these would break the request the
// gateway already built, not make a provider more compatible.
var transportReservedHeaders = map[string]struct{}{
	"Host":                     {},
	"Content-Length":           {},
	"Content-Type":             {},
	"Content-Encoding":         {},
	"Content-Range":            {},
	"Transfer-Encoding":        {},
	"Connection":               {},
	"Proxy-Connection":         {},
	"Keep-Alive":               {},
	"Upgrade":                  {},
	"Te":                       {},
	"Trailer":                  {},
	"Expect":                   {},
	"Range":                    {},
	"Accept-Encoding":          {},
	"Sec-Websocket-Key":        {},
	"Sec-Websocket-Version":    {},
	"Sec-Websocket-Protocol":   {},
	"Sec-Websocket-Extensions": {},
	"Sec-Websocket-Accept":     {},
}

// sessionReservedHeaders must describe the request actually being sent. A
// configured value would pin many requests to one session or request id.
var sessionReservedHeaders = map[string]struct{}{
	"Session-Id":               {},
	"X-Claude-Code-Session-Id": {},
	"X-Session-Id":             {},
	"X-Request-Id":             {},
	"Request-Id":               {},
	"Idempotency-Key":          {},
	"X-Stainless-Retry-Count":  {},
	"X-Stainless-Timeout":      {},
}

// ClassifyRequestHeader reports who owns name for a service whose effective
// authentication is auth. Pass the auth actually used for the outgoing request,
// which for several providers is the protocol-adjusted scheme rather than the
// stored one, so a custom auth header can never be reachable by configuration.
func ClassifyRequestHeader(name string, auth ServiceAuth) HeaderClass {
	canonical := http.CanonicalHeaderKey(strings.TrimSpace(name))
	if strings.HasPrefix(canonical, "X-Astrlink-") {
		return HeaderClassGatewayReserved
	}
	if auth.Scheme == AuthSchemeCustomHeader && auth.HeaderName != "" &&
		canonical == http.CanonicalHeaderKey(auth.HeaderName) {
		return HeaderClassAuthReserved
	}
	switch auth.Scheme {
	case AuthSchemeBearer:
		if canonical == "Authorization" {
			return HeaderClassAuthReserved
		}
	case AuthSchemeAnthropicAPIKey:
		if canonical == "X-Api-Key" {
			return HeaderClassAuthReserved
		}
	case AuthSchemeGoogleAPIKey:
		if canonical == "X-Goog-Api-Key" {
			return HeaderClassAuthReserved
		}
	}
	if _, reserved := authReservedHeaders[canonical]; reserved {
		return HeaderClassAuthReserved
	}
	if _, reserved := transportReservedHeaders[canonical]; reserved {
		return HeaderClassTransportReserved
	}
	if _, reserved := sessionReservedHeaders[canonical]; reserved {
		return HeaderClassSessionReserved
	}
	return HeaderClassOverridable
}

// ModelRule overrides outbound compatibility fields for the models it matches.
// Match is compared against the model id actually sent upstream, so a routed or
// rewritten model selects the rule that the provider will really see.
type ModelRule struct {
	// Match is an exact model id or "*". Other glob syntax is rejected rather
	// than silently reinterpreted.
	Match string `json:"match"`
	// Headers overlays outbound request headers. Only overridable headers are
	// accepted; credentials, transport control and session markers are not.
	Headers map[string]string `json:"headers,omitempty"`
	// Body is reserved for a later controlled rewrite. This version accepts an
	// empty object so a headers-only rule migrates unchanged, and rejects any
	// field rather than appearing to apply one.
	Body map[string]json.RawMessage `json:"body,omitempty"`
	// IdentityProfile selects a confirmed snapshot for the matched models.
	// Empty inherits the service's bound profile.
	IdentityProfile IdentityProfileID `json:"identity_profile,omitempty"`
	// Enabled defaults to true when omitted, so an older rule keeps working
	// and the UI can turn one off without deleting it.
	Enabled *bool `json:"enabled,omitempty"`
}

// Active reports whether the rule participates in selection.
func (rule ModelRule) Active() bool {
	return rule.Enabled == nil || *rule.Enabled
}

// MatchesAll reports the catch-all tier. Exact matches are selected first.
func (rule ModelRule) MatchesAll() bool {
	return rule.Match == ModelRuleMatchAll
}

func (rule ModelRule) Validate(auth ServiceAuth) error {
	if err := validateModelRuleMatch(rule.Match); err != nil {
		return err
	}
	if err := validateConfiguredHeaders(rule.Headers, auth); err != nil {
		return err
	}
	if len(rule.Body) != 0 {
		names := make([]string, 0, len(rule.Body))
		for name := range rule.Body {
			names = append(names, name)
		}
		return fmt.Errorf(
			"body rewriting is not supported yet; remove body field(s) %s",
			strings.Join(names, ", "),
		)
	}
	if rule.IdentityProfile != "" {
		if err := rule.IdentityProfile.Validate(); err != nil {
			return fmt.Errorf("identity_profile: %w", err)
		}
	}
	return nil
}

func (rule ModelRule) Clone() ModelRule {
	cloned := rule
	if rule.Headers != nil {
		cloned.Headers = maps.Clone(rule.Headers)
	}
	if rule.Body != nil {
		cloned.Body = make(map[string]json.RawMessage, len(rule.Body))
		for name, value := range rule.Body {
			cloned.Body[name] = append(json.RawMessage(nil), value...)
		}
	}
	if rule.Enabled != nil {
		enabled := *rule.Enabled
		cloned.Enabled = &enabled
	}
	return cloned
}

func validateModelRuleMatch(match string) error {
	if match == "" {
		return fmt.Errorf("match must not be empty")
	}
	if utf8.RuneCountInString(match) > MaxModelRuleMatchRunes {
		return fmt.Errorf("match must contain at most %d characters", MaxModelRuleMatchRunes)
	}
	if strings.TrimSpace(match) != match {
		return fmt.Errorf("match %q must not have leading or trailing whitespace", match)
	}
	if match == ModelRuleMatchAll {
		return nil
	}
	// An unsupported pattern must fail loudly: silently treating it as an exact
	// model id would leave a rule that never matches anything.
	if index := strings.IndexAny(match, "*?[]"); index >= 0 {
		return fmt.Errorf(
			"match %q uses unsupported pattern syntax %q; this version accepts an exact model id or %q",
			match, match[index:index+1], ModelRuleMatchAll,
		)
	}
	return nil
}

func validateConfiguredHeaders(headers map[string]string, auth ServiceAuth) error {
	if len(headers) > MaxConfiguredHeaders {
		return fmt.Errorf("headers must contain at most %d entries", MaxConfiguredHeaders)
	}
	seen := make(map[string]string, len(headers))
	for name, value := range headers {
		if name == "" || len(name) > 128 || !headerNamePattern.MatchString(name) {
			return fmt.Errorf("header name %q is not a valid HTTP field name", name)
		}
		canonical := http.CanonicalHeaderKey(name)
		if previous, duplicate := seen[canonical]; duplicate && previous != name {
			return fmt.Errorf("headers set %q twice with different casing", canonical)
		}
		seen[canonical] = name
		if class := ClassifyRequestHeader(name, auth); class != HeaderClassOverridable {
			return fmt.Errorf("header %q is %s and cannot be configured", canonical, class)
		}
		if len(value) > MaxConfiguredHeaderValueBytes {
			return fmt.Errorf("header %q value exceeds %d bytes", canonical, MaxConfiguredHeaderValueBytes)
		}
		for index := 0; index < len(value); index++ {
			if value[index] < 0x20 || value[index] > 0x7e {
				return fmt.Errorf("header %q value must be printable ASCII", canonical)
			}
		}
		if strings.TrimSpace(value) != value {
			return fmt.Errorf("header %q value must not have leading or trailing whitespace", canonical)
		}
	}
	return nil
}

// ValidateRequestRules checks a service's default overlay and rule list
// together, so a saved configuration cannot hold a rule that would be refused
// at forward time.
func ValidateRequestRules(extraHeaders map[string]string, rules []ModelRule, auth ServiceAuth) error {
	if err := validateConfiguredHeaders(extraHeaders, auth); err != nil {
		return fmt.Errorf("extra_headers: %w", err)
	}
	if len(rules) > MaxModelRules {
		return fmt.Errorf("model_rules must contain at most %d entries", MaxModelRules)
	}
	exact := make(map[string]int, len(rules))
	catchAll := -1
	for index, rule := range rules {
		if err := rule.Validate(auth); err != nil {
			return fmt.Errorf("model_rules[%d]: %w", index, err)
		}
		if rule.MatchesAll() {
			if catchAll >= 0 {
				return fmt.Errorf("model_rules[%d] duplicates the %q rule at index %d", index, ModelRuleMatchAll, catchAll)
			}
			catchAll = index
			continue
		}
		if previous, duplicate := exact[rule.Match]; duplicate {
			return fmt.Errorf("model_rules[%d] duplicates match %q at index %d", index, rule.Match, previous)
		}
		exact[rule.Match] = index
	}
	return nil
}

// RedactedConfiguredValue replaces a configured header value for a caller that
// may not read it. It is a fixed marker rather than a length or prefix hint,
// because a compatibility value is often the whole secret of why one upstream
// accepts a request: a client build string, an account-shaped originator, or a
// token-like compatibility field an operator pasted in.
const RedactedConfiguredValue = "<redacted>"

// WithoutConfiguredValues projects the connection for a caller below operator.
// Header NAMES, rule matches, enabled flags and the bound profile id stay
// visible so an observer can still see that a service rewrites requests and
// which models it covers; every configured VALUE is replaced.
func (connection HTTPConnection) WithoutConfiguredValues() HTTPConnection {
	redacted := connection
	redacted.ExtraHeaders = redactConfiguredHeaders(connection.ExtraHeaders)
	if connection.ModelRules != nil {
		rules := make([]ModelRule, 0, len(connection.ModelRules))
		for _, rule := range connection.ModelRules {
			copied := rule.Clone()
			copied.Headers = redactConfiguredHeaders(rule.Headers)
			// Body is refused by validation today, but if a future version stores
			// one it must not become readable here by default.
			copied.Body = nil
			rules = append(rules, copied)
		}
		redacted.ModelRules = rules
	}
	return redacted
}

func redactConfiguredHeaders(headers map[string]string) map[string]string {
	if headers == nil {
		return nil
	}
	redacted := make(map[string]string, len(headers))
	for name := range headers {
		redacted[name] = RedactedConfiguredValue
	}
	return redacted
}

func cloneModelRules(rules []ModelRule) []ModelRule {
	if rules == nil {
		return nil
	}
	cloned := make([]ModelRule, 0, len(rules))
	for _, rule := range rules {
		cloned = append(cloned, rule.Clone())
	}
	return cloned
}
