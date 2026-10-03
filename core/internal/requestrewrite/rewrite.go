// Package requestrewrite selects and applies a provider's outbound
// compatibility configuration for one upstream attempt.
//
// It owns four things and nothing else: validating and compiling a saved
// configuration, selecting at most one rule for the model actually sent
// upstream, producing a header overlay, and reporting what it decided. It never
// reads credentials, never performs I/O, and never rewrites a request body in
// this version. Selection is frozen per attempt by the caller, so a retry
// cannot silently pick a different rule than the attempt it is replaying.
package requestrewrite

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
)

// Source records which tier produced the overlay, for diagnostics the operator
// can act on. The three tiers are mutually exclusive by design: a matched rule
// replaces the service default rather than layering on top of it, which is the
// behavior operators already rely on in the configuration this migrates from.
type Source string

const (
	// SourceNone means nothing was configured for this model.
	SourceNone Source = "none"
	// SourceServiceDefault means the service-wide overlay applied.
	SourceServiceDefault Source = "extra_headers"
	// SourceModelRule means a rule matched and replaced the service default.
	SourceModelRule Source = "model_rule"
)

// Decision is the frozen outcome for one upstream attempt. An empty Decision is
// valid and applies nothing, so callers may use the zero value for services
// without configuration.
type Decision struct {
	// Model is the model id the rule was selected against: the id actually sent
	// upstream, not the one the client asked for.
	Model string
	// Source reports which tier won.
	Source Source
	// RuleIndex is the position of the matched rule in the saved list, or -1.
	// Paired with RuleMatch it identifies the rule in diagnostics without
	// requiring a separate persisted rule id.
	RuleIndex int
	// RuleMatch is the matched rule's expression, or empty.
	RuleMatch string
	// IdentityProfile is the snapshot the caller must resolve and confirm
	// before Apply. Empty means no identity override.
	IdentityProfile contract.IdentityProfileID
	// headers is the already-validated overlay. It is unexported so a caller
	// cannot mutate a compiled plan's data through a returned Decision.
	headers map[string]string
}

// Configured reports whether anything would be applied.
func (decision Decision) Configured() bool {
	return len(decision.headers) > 0 || decision.IdentityProfile != ""
}

// HeaderNames lists the overlay's canonical names in a stable order. It exists
// for diagnostics and the audit record; values are deliberately not exposed
// here, because a configured value may be as sensitive as the request it
// describes and has its own operator-only exposure rules.
func (decision Decision) HeaderNames() []string {
	if len(decision.headers) == 0 {
		return nil
	}
	names := make([]string, 0, len(decision.headers))
	for name := range decision.headers {
		names = append(names, http.CanonicalHeaderKey(name))
	}
	sort.Strings(names)
	return names
}

// Plan is an immutable compiled configuration. A nil *Plan is valid and decides
// nothing, so an unconfigured service needs no special case at the call site.
type Plan struct {
	// auth is the effective authentication for the outgoing request. It is
	// stored so Apply re-checks ownership against the same auth that was
	// validated, instead of trusting the saved configuration.
	auth     contract.ServiceAuth
	defaults map[string]string
	// exact indexes single-model rules; order is preserved through index.
	exact map[string]int
	// catchAll is the index of the "*" rule, or -1.
	catchAll int
	rules    []contract.ModelRule
	identity contract.IdentityProfileID
}

// Compile validates and freezes a service's configuration. auth must be the
// effective authentication for the outgoing request, which for several
// providers differs from the stored scheme; passing the stored scheme would let
// a rule reach a credential header on the protocol that actually uses it.
//
// Compile rejects the same configurations the control API rejects, so a stored
// document written by an older or hand-edited path cannot start applying
// something the current validator would refuse.
func Compile(connection contract.HTTPConnection, auth contract.ServiceAuth) (*Plan, error) {
	if err := contract.ValidateRequestRules(connection.ExtraHeaders, connection.ModelRules, auth); err != nil {
		return nil, err
	}
	if connection.IdentityProfileID != "" {
		if err := connection.IdentityProfileID.Validate(); err != nil {
			return nil, fmt.Errorf("identity_profile_id: %w", err)
		}
	}
	plan := &Plan{
		auth: auth, catchAll: -1,
		identity: connection.IdentityProfileID,
		exact:    make(map[string]int, len(connection.ModelRules)),
	}
	if len(connection.ExtraHeaders) > 0 {
		plan.defaults = canonicalHeaders(connection.ExtraHeaders)
	}
	for _, rule := range connection.ModelRules {
		// Retain disabled slots so RuleIndex still points into the saved list.
		plan.rules = append(plan.rules, rule.Clone())
		if !rule.Active() {
			continue
		}
		position := len(plan.rules) - 1
		if rule.MatchesAll() {
			if plan.catchAll < 0 {
				plan.catchAll = position
			}
			continue
		}
		// First listed wins within one specificity tier.
		if _, taken := plan.exact[rule.Match]; !taken {
			plan.exact[rule.Match] = position
		}
	}
	if len(plan.defaults) == 0 && len(plan.rules) == 0 && plan.identity == "" {
		return nil, nil
	}
	return plan, nil
}

// Decide selects at most one rule for upstreamModel. Exact matches win over
// "*"; a disabled rule never matches. The returned Decision is a value snapshot
// and stays stable even if the service is reconfigured mid-request.
func (plan *Plan) Decide(upstreamModel string) Decision {
	decision := Decision{Model: upstreamModel, Source: SourceNone, RuleIndex: -1}
	if plan == nil {
		return decision
	}
	position := -1
	if index, matched := plan.exact[upstreamModel]; matched {
		position = index
	} else if plan.catchAll >= 0 {
		position = plan.catchAll
	}
	if position < 0 {
		decision = plan.DecideDefault()
		decision.Model = upstreamModel
		return decision
	}
	rule := plan.rules[position]
	decision.Source = SourceModelRule
	decision.RuleIndex = position
	decision.RuleMatch = rule.Match
	decision.headers = canonicalHeaders(rule.Headers)
	// A rule without its own profile inherits the service binding, so pinning a
	// service identity does not have to be repeated in every rule.
	decision.IdentityProfile = rule.IdentityProfile
	if decision.IdentityProfile == "" {
		decision.IdentityProfile = plan.identity
	}
	if len(decision.headers) == 0 && decision.IdentityProfile == "" {
		decision.Source = SourceNone
	}
	return decision
}

// DecideDefault selects only the service binding and extra_headers. Model
// discovery has no model to match, so even a "*" rule must not participate.
func (plan *Plan) DecideDefault() Decision {
	decision := Decision{Source: SourceNone, RuleIndex: -1}
	if plan == nil {
		return decision
	}
	decision.headers = plan.defaults
	decision.IdentityProfile = plan.identity
	if decision.Configured() {
		decision.Source = SourceServiceDefault
	}
	return decision
}

// Apply writes the decision onto an outbound overlay. identity is the already
// resolved and confirmed profile headers, or nil; configured headers win over
// identity headers so an explicit rule is never silently overridden.
//
// Apply re-checks ownership of every name it is about to write. A header the
// gateway owns is refused here even if it somehow passed validation, so a
// stored document can never reach a credential, framing, or session field.
func (plan *Plan) Apply(overlay http.Header, decision Decision, identity http.Header) error {
	if overlay == nil {
		return fmt.Errorf("overlay header is nil")
	}
	auth := contract.ServiceAuth{}
	if plan != nil {
		auth = plan.auth
	}
	for name := range identity {
		if class := contract.ClassifyRequestHeader(name, auth); class != contract.HeaderClassOverridable {
			return fmt.Errorf("identity header %q is %s", http.CanonicalHeaderKey(name), class)
		}
	}
	for name := range decision.headers {
		if class := contract.ClassifyRequestHeader(name, auth); class != contract.HeaderClassOverridable {
			return fmt.Errorf("configured header %q is %s", http.CanonicalHeaderKey(name), class)
		}
	}
	for name, values := range identity {
		canonical := http.CanonicalHeaderKey(name)
		overlay.Del(canonical)
		for _, value := range values {
			overlay.Add(canonical, value)
		}
	}
	for name, value := range decision.headers {
		// Set replaces rather than appends: a compatibility override must send
		// exactly one value, not fold onto whatever the client already sent.
		overlay.Set(http.CanonicalHeaderKey(name), value)
	}
	return nil
}

func canonicalHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	canonical := make(map[string]string, len(headers))
	for name, value := range headers {
		canonical[http.CanonicalHeaderKey(strings.TrimSpace(name))] = value
	}
	return canonical
}
