package forkcheckin

import "context"

// NewAuthorizationVerifier builds a bounded, request-scoped identity verifier.
// Pending login credentials must not replace or invalidate a running job's
// cached client, or another login's client for the same account. Each admitted
// verification owns a separate factory and drains it before returning. The
// module's active-call tracking and cancellation cover this entire lifetime.
func NewAuthorizationVerifier(build func(*TransportFactory) (SiteAdapter, error)) func(context.Context, AccountSnapshot) (SiteIdentity, error) {
	slots := make(chan struct{}, maxAuthorizations)
	return func(ctx context.Context, snapshot AccountSnapshot) (SiteIdentity, error) {
		if err := ctx.Err(); err != nil {
			return SiteIdentity{}, err
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		case <-ctx.Done():
			return SiteIdentity{}, ctx.Err()
		}
		if build == nil {
			return SiteIdentity{}, ErrUnsupported
		}
		factory := NewTransportFactory()
		defer func() { _ = factory.Close(context.WithoutCancel(ctx)) }()
		adapter, err := build(factory)
		if err != nil || adapter == nil {
			return SiteIdentity{}, ErrUnsupported
		}
		return adapter.ValidateIdentity(ctx, snapshot)
	}
}
