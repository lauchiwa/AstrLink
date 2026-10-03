package ingress

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/requestrewrite"
)

// IdentityProfileReader is the shared, read-only profile dependency.
type IdentityProfileReader = accountauth.IdentityProfileReader

// applyRequestRules selects this service's outbound compatibility
// configuration for upstreamModel and writes it onto the authorizer's overlay.
//
// auth must be the auth effective for the outgoing protocol, not the stored
// scheme, so a rule can never reach the credential header this request really
// uses. The returned Decision is a frozen value: the caller keeps it for the
// whole attempt, so a retry replays what it already decided instead of
// re-reading a configuration that may have changed mid-request.
//
// A configured but unusable identity profile is an error rather than a silent
// omission: forwarding without the identity the operator pinned would look like
// success while presenting the wrong client to the upstream.
func (handler *Handler) applyRequestRules(
	ctx context.Context,
	service contract.Service,
	auth contract.ServiceAuth,
	upstreamModel string,
	overlay *http.Header,
) (requestrewrite.Decision, []string, error) {
	return handler.applyCompiledRules(ctx, service, auth, overlay,
		func(plan *requestrewrite.Plan) requestrewrite.Decision {
			return plan.Decide(upstreamModel)
		})
}

// applyDiscoveryRequestRules applies the same configuration to the aggregated
// model listing, which has no upstream model to match.
//
// Discovery reaches the same provider over the same connection, so a service
// that only answers a recognized client must be able to list its models at all.
// Selection deliberately uses the service tier alone: a per-model rule, even a
// catch-all one, describes an inference request and must not be attributed to a
// listing that names no model. This mirrors the control-plane prober, so the
// models shown while configuring a provider match the ones its clients see.
func (handler *Handler) applyDiscoveryRequestRules(
	ctx context.Context,
	service contract.Service,
	auth contract.ServiceAuth,
	overlay *http.Header,
) (requestrewrite.Decision, []string, error) {
	return handler.applyCompiledRules(ctx, service, auth, overlay, (*requestrewrite.Plan).DecideDefault)
}

func (handler *Handler) applyCompiledRules(
	ctx context.Context,
	service contract.Service,
	auth contract.ServiceAuth,
	overlay *http.Header,
	decide func(*requestrewrite.Plan) requestrewrite.Decision,
) (requestrewrite.Decision, []string, error) {
	if !service.Kind.IsHTTP() || service.HTTP == nil {
		return requestrewrite.Decision{RuleIndex: -1, Source: requestrewrite.SourceNone}, nil, nil
	}
	plan, err := requestrewrite.Compile(*service.HTTP, auth)
	if err != nil {
		return requestrewrite.Decision{}, nil, fmt.Errorf("request rules: %w", err)
	}
	decision := decide(plan)
	if !decision.Configured() {
		return decision, nil, nil
	}
	var identity http.Header
	if decision.IdentityProfile != "" {
		identity, err = handler.identityProfileHeaders(ctx, service.ID, decision.IdentityProfile, auth)
		if err != nil {
			return requestrewrite.Decision{}, nil, err
		}
	}
	if *overlay == nil {
		*overlay = make(http.Header)
	}
	if err := plan.Apply(*overlay, decision, identity); err != nil {
		return requestrewrite.Decision{}, nil, fmt.Errorf("request rules: %w", err)
	}
	// Every name this attempt injected, from either source. Their values are
	// operator-only on the service document and the profile, so audit must not
	// become a second, observer-readable copy of them.
	protected := decision.HeaderNames()
	for name := range identity {
		protected = append(protected, http.CanonicalHeaderKey(name))
	}
	return decision, protected, nil
}

// noteRequestRules records which configuration tier won for this attempt.
//
// It reports header NAMES only. A configured value can carry as much detail as
// the request it describes, and the full configuration is already operator-only
// on the service document, so an event summary visible to an observer must not
// repeat it.
//
// protected lists the header names this attempt injected. It replaces the
// previous attempt's list unconditionally, so a fallback candidate without
// rules does not inherit masking it no longer needs, and one with rules never
// starts unmasked.
func (session *recordSession) noteRequestRules(decision requestrewrite.Decision, protected []string) {
	if session == nil {
		return
	}
	session.protectedUpstreamHeaders = nil
	if len(protected) > 0 {
		session.protectedUpstreamHeaders = make(map[string]struct{}, len(protected))
		for _, name := range protected {
			session.protectedUpstreamHeaders[strings.ToLower(name)] = struct{}{}
		}
	}
	if !decision.Configured() {
		return
	}
	summary := string(decision.Source)
	if decision.Source == requestrewrite.SourceModelRule {
		// Index plus match identifies the rule without a separate persisted id,
		// and stays meaningful when several rules share a prefix.
		summary = fmt.Sprintf("model_rule[%d] %q", decision.RuleIndex, decision.RuleMatch)
	}
	if names := decision.HeaderNames(); len(names) > 0 {
		summary += " headers: " + strings.Join(names, ", ")
	}
	if decision.IdentityProfile != "" {
		summary += " identity: " + string(decision.IdentityProfile)
	}
	// A closed point event: the configuration was selected at this instant and
	// does not span the upstream attempt it precedes.
	session.addEvent(contract.RequestEventRouted, contract.RequestStatusSucceeded, summary)
	event := &session.events[len(session.events)-1]
	ended := event.StartedAt
	event.EndedAt = &ended
}

// identityProfileHeaders renders a pinned snapshot. The profile must belong to
// this service and be confirmed; an unconfirmed candidate is never forwarded.
func (handler *Handler) identityProfileHeaders(
	ctx context.Context,
	serviceID contract.ServiceID,
	id contract.IdentityProfileID,
	auth contract.ServiceAuth,
) (http.Header, error) {
	return accountauth.LoadIdentityProfileHeaders(ctx, handler.identityProfiles, serviceID, id, auth)
}
