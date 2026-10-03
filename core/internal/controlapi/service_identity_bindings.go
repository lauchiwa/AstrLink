package controlapi

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// requireServiceIdentityBindings checks references before a service or its
// credentials are written. Confirmation is not activation: every binding must
// explicitly select a confirmed snapshot belonging to this service. Disabled
// rules are checked too, so they cannot retain discardable candidates.
// Forwarding still validates again when loading its frozen snapshot.
func (handler *Handler) requireServiceIdentityBindings(writer http.ResponseWriter, request *http.Request, service contract.Service) bool {
	connection := service.HTTP
	if connection == nil {
		return true
	}
	// Bound the reference walk before doing storage I/O, including on create
	// where the service store has not validated the connection yet.
	if err := contract.ValidateRequestRules(connection.ExtraHeaders, connection.ModelRules, connection.Auth); err != nil {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_service", err.Error())
		return false
	}
	invalid := func(field string) bool {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_identity_binding",
			field+": select a confirmed identity profile belonging to this service and compatible with its authentication header")
		return false
	}
	unavailable := func() bool {
		writeError(writer, http.StatusServiceUnavailable, "identity_profiles_unavailable", "identity profile storage is unavailable")
		return false
	}
	checked := make(map[contract.IdentityProfileID]bool)
	check := func(id contract.IdentityProfileID, field string) bool {
		if id == "" || checked[id] {
			return true
		}
		if id.Validate() != nil {
			return invalid(field)
		}
		profiles, ok := handler.serviceStore.(accountauth.IdentityProfileReader)
		if !ok {
			return unavailable()
		}
		record, err := profiles.GetIdentityProfile(request.Context(), service.ID, id)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) || errors.Is(err, storage.ErrInvalidRecord) {
				return invalid(field)
			}
			return unavailable()
		}
		if record.Profile.ID != id {
			return invalid(field)
		}
		// Provider-specific auth selection changes only standard credential
		// schemes. Explicit custom headers are preserved, so checking the saved
		// auth here also catches conflicts after a later authentication edit.
		if _, err := accountauth.IdentityProfileHeaders(record.Profile, service.ID, connection.Auth); err != nil {
			return invalid(field)
		}
		checked[id] = true
		return true
	}
	if !check(connection.IdentityProfileID, "http.identity_profile_id") {
		return false
	}
	for index, rule := range connection.ModelRules {
		if !check(rule.IdentityProfile, fmt.Sprintf("http.model_rules[%d].identity_profile", index)) {
			return false
		}
	}
	return true
}
