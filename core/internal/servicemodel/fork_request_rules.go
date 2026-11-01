package servicemodel

import (
	"context"
	"fmt"
	"net/http"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/requestrewrite"
)

// applyDiscoveryRules writes the service's request rules onto a discovery
// request, the same overlay inference applies. Every HTTP discovery path calls
// it, including Vertex, so the editor's catalog probe and inference agree.
//
// auth must be the protocol-effective authentication. Discovery has no upstream
// model, so only service defaults apply, never a model rule and not even the
// catch-all, and the profile is resolved once for the whole probe.
func (prober *Prober) applyDiscoveryRules(
	ctx context.Context,
	serviceID contract.ServiceID,
	connection contract.HTTPConnection,
	auth contract.ServiceAuth,
	headers http.Header,
) error {
	plan, err := requestrewrite.Compile(connection, auth)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	decision := plan.DecideDefault()
	var identity http.Header
	if decision.IdentityProfile != "" {
		identity, err = accountauth.LoadIdentityProfileHeaders(
			ctx, prober.identityProfiles, serviceID, decision.IdentityProfile, auth,
		)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrConfiguration, err)
		}
	}
	if err := plan.Apply(headers, decision, identity); err != nil {
		return fmt.Errorf("%w: %w", ErrConfiguration, err)
	}
	return nil
}
