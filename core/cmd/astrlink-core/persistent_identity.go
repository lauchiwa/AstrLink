package main

import (
	"github.com/QuantumNous/astrlink/core/internal/identitycapture"
	"github.com/QuantumNous/astrlink/core/internal/ingress"
	"github.com/QuantumNous/astrlink/core/internal/servicemodel"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
	"github.com/QuantumNous/astrlink/core/internal/subscription"
)

// configurePersistentIdentity connects the fork's HTTP identity support before
// gateway dependencies are copied into control-plane testers. Capture consent is
// per core and memory-only; profiles use the already opened persistent store.
func configurePersistentIdentity(store *sqlite.Store, gateway *ingress.Dependencies) (*identitycapture.Registry, error) {
	capture, err := identitycapture.New(store)
	if err != nil {
		return nil, err
	}
	gateway.IdentityCapture = capture
	gateway.IdentityProfiles = store
	return capture, nil
}

func persistentServiceModels(store *sqlite.Store, subscriptions *subscription.Manager) *servicemodel.Prober {
	return servicemodel.NewWithDependencies(servicemodel.Dependencies{
		Secrets: store, Subscriptions: subscriptions, IdentityProfiles: store,
	})
}
