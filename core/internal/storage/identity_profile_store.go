package storage

import (
	"context"

	"github.com/QuantumNous/astrlink/core/contract"
)

// IdentityProfileRecord binds confirmation to the exact candidate reviewed by
// the operator, not to the latest mutable subscription learning result.
type IdentityProfileRecord struct {
	Profile contract.IdentityProfile
	ETag    string
}

type IdentityProfileListOptions struct {
	Limit  int
	Cursor string
}

type IdentityProfilePage struct {
	Items      []IdentityProfileRecord
	NextCursor string
}

// IdentityProfileStore owns service-scoped snapshots. Creation always produces
// an unconfirmed candidate; confirmation changes only ConfirmedAt. There is no
// fingerprint update operation. Confirming does not activate a provider binding.
// Discard is restricted to candidates so saved snapshots remain available for
// later bindings, in-flight requests and explicit rollback.
type IdentityProfileStore interface {
	CreateIdentityProfile(context.Context, contract.IdentityProfile) (IdentityProfileRecord, error)
	GetIdentityProfile(context.Context, contract.ServiceID, contract.IdentityProfileID) (IdentityProfileRecord, error)
	ListIdentityProfiles(context.Context, contract.ServiceID, IdentityProfileListOptions) (IdentityProfilePage, error)
	ConfirmIdentityProfile(context.Context, contract.ServiceID, contract.IdentityProfileID, string) (IdentityProfileRecord, error)
	DiscardIdentityProfile(context.Context, contract.ServiceID, contract.IdentityProfileID, string) error
}
