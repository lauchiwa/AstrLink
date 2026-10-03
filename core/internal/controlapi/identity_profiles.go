package controlapi

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// Import is deliberately a separate capability from the observer-facing
// version reporter. The registry returns a copy without applying Routing floors.
type identityFingerprintImporter interface {
	LearnedIdentityFingerprint(contract.IdentityClient) (contract.IdentityFingerprint, bool)
}

type identityProfileCreateInput struct {
	Client contract.IdentityClient        `json:"client"`
	Source contract.IdentityProfileSource `json:"source"`
}

// IdentityCaptureController is satisfied by *identitycapture.Registry. Arming
// is in-memory, so consent is not restored after a restart.
type IdentityCaptureController interface {
	Arm(contract.ServiceID, contract.IdentityClient, time.Duration) (contract.IdentityCaptureStatus, error)
	Disarm(contract.ServiceID) contract.IdentityCaptureStatus
	Status(contract.ServiceID) contract.IdentityCaptureStatus
}

type identityCaptureArmInput struct {
	Client contract.IdentityClient `json:"client"`
	// TTLSeconds is optional. Omitted or out-of-range selects the default
	// window; a window is never unbounded.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
}

// serviceIdentityCapture arms, reads, or closes one service's capture window.
// Arming records consent only: it does not change what is forwarded, does not
// grant official-client treatment, and the candidate it may publish still needs
// explicit confirmation. Matching a client's shape is never authentication.
func (handler *Handler) serviceIdentityCapture(writer http.ResponseWriter, request *http.Request, serviceID contract.ServiceID) {
	if handler.identityCapture == nil {
		writeError(writer, http.StatusServiceUnavailable, "identity_capture_unavailable", "identity capture is unavailable")
		return
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_query", "identity capture does not accept query parameters")
		return
	}
	if request.Method != http.MethodGet && request.Method != http.MethodPut && request.Method != http.MethodDelete {
		writeMethodNotAllowed(writer, "GET, PUT, DELETE")
		return
	}
	service, err := handler.serviceStore.GetService(request.Context(), serviceID)
	if err != nil {
		handler.writeStoreError(writer, err)
		return
	}
	if !service.Service.Kind.IsHTTP() || service.Service.HTTP == nil {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_service", "identity capture requires an HTTP service")
		return
	}
	switch request.Method {
	case http.MethodGet:
		writeJSON(writer, http.StatusOK, handler.identityCapture.Status(serviceID))
	case http.MethodDelete:
		writeJSON(writer, http.StatusOK, handler.identityCapture.Disarm(serviceID))
	default:
		if !requireMediaType(writer, request, "application/json") {
			return
		}
		var input identityCaptureArmInput
		if !decodeControlJSON(writer, request, &input) {
			return
		}
		if !input.Client.Valid() {
			writeError(writer, http.StatusUnprocessableEntity, "invalid_identity_client", "select a supported identity client")
			return
		}
		if input.TTLSeconds < 0 {
			writeError(writer, http.StatusUnprocessableEntity, "invalid_identity_window", "ttl_seconds must not be negative")
			return
		}
		status, err := handler.identityCapture.Arm(serviceID, input.Client, time.Duration(input.TTLSeconds)*time.Second)
		if err != nil {
			writeError(writer, http.StatusUnprocessableEntity, "invalid_identity_client", "identity capture could not be armed")
			return
		}
		writeJSON(writer, http.StatusOK, status)
	}
}

type identityProfilePageResponse struct {
	Items      []contract.IdentityProfile `json:"items"`
	NextCursor *string                    `json:"next_cursor"`
}

// serviceItemRole protects all these paths with RoleOperator. No endpoint
// accepts raw fingerprints, timestamps, confirmation flags or captured requests.
// Confirmation persists consent for one snapshot; it does not enable forwarding.
func (handler *Handler) serviceIdentityProfiles(writer http.ResponseWriter, request *http.Request, serviceID contract.ServiceID, parts []string) {
	profiles, ok := handler.serviceStore.(storage.IdentityProfileStore)
	if !ok {
		writeError(writer, http.StatusServiceUnavailable, "identity_profiles_unavailable", "identity profile storage is unavailable")
		return
	}
	service, err := handler.serviceStore.GetService(request.Context(), serviceID)
	if err != nil {
		handler.writeStoreError(writer, err)
		return
	}
	if !service.Service.Kind.IsHTTP() || service.Service.HTTP == nil {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_service", "identity profiles require an HTTP service")
		return
	}
	if len(parts) == 0 && request.Method == http.MethodGet {
		handler.listIdentityProfiles(writer, request, profiles, serviceID)
		return
	}
	if request.URL.RawQuery != "" {
		writeError(writer, http.StatusBadRequest, "invalid_query", "this identity profile operation does not accept query parameters")
		return
	}
	if len(parts) == 0 {
		if request.Method != http.MethodPost {
			writeMethodNotAllowed(writer, "GET, POST")
			return
		}
		handler.createIdentityProfile(writer, request, profiles, serviceID)
		return
	}
	id := contract.IdentityProfileID(parts[0])
	if id.Validate() != nil {
		writeError(writer, http.StatusBadRequest, "invalid_id", "identity profile id is invalid")
		return
	}
	if len(parts) == 1 && request.Method == http.MethodGet {
		record, err := profiles.GetIdentityProfile(request.Context(), serviceID, id)
		if err != nil {
			handler.writeStoreError(writer, err)
			return
		}
		writer.Header().Set("ETag", record.ETag)
		writeJSON(writer, http.StatusOK, record.Profile)
		return
	}
	confirm := len(parts) == 2 && parts[1] == "confirm"
	if !confirm && len(parts) != 1 {
		writeError(writer, http.StatusNotFound, "not_found", "identity profile operation not found")
		return
	}
	if confirm && request.Method != http.MethodPost {
		writeMethodNotAllowed(writer, http.MethodPost)
		return
	}
	if !confirm && request.Method != http.MethodDelete {
		writeMethodNotAllowed(writer, "GET, DELETE")
		return
	}
	etag := request.Header.Get("If-Match")
	if etag == "" {
		writeError(writer, http.StatusBadRequest, "if_match_required", "If-Match is required")
		return
	}
	if !confirm {
		if err := profiles.DiscardIdentityProfile(request.Context(), serviceID, id, etag); err != nil {
			handler.writeStoreError(writer, err)
			return
		}
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	if !requireMediaType(writer, request, "application/json") {
		return
	}
	var input *struct{}
	if !decodeControlJSON(writer, request, &input) {
		return
	}
	if input == nil {
		writeError(writer, http.StatusBadRequest, "invalid_json", "confirmation body must be an empty object")
		return
	}
	record, err := profiles.ConfirmIdentityProfile(request.Context(), serviceID, id, etag)
	if err != nil {
		handler.writeStoreError(writer, err)
		return
	}
	writer.Header().Set("ETag", record.ETag)
	writeJSON(writer, http.StatusOK, record.Profile)
}

func (handler *Handler) createIdentityProfile(writer http.ResponseWriter, request *http.Request, profiles storage.IdentityProfileStore, serviceID contract.ServiceID) {
	if !requireMediaType(writer, request, "application/json") {
		return
	}
	var input identityProfileCreateInput
	if !decodeControlJSON(writer, request, &input) {
		return
	}
	if !input.Client.Valid() || (input.Source != contract.IdentityProfileBuiltin && input.Source != contract.IdentityProfileSubscriptionImport) {
		writeError(writer, http.StatusUnprocessableEntity, "invalid_identity_source", "select a supported client and builtin or subscription_import source")
		return
	}
	var fingerprint contract.IdentityFingerprint
	if input.Source == contract.IdentityProfileBuiltin {
		var err error
		fingerprint, err = accountauth.BuiltinIdentityFingerprint(input.Client)
		if err != nil {
			writeError(writer, http.StatusInternalServerError, "identity_unavailable", "built-in identity is unavailable")
			return
		}
	} else {
		importer, ok := handler.clientIdentities.(identityFingerprintImporter)
		if !ok {
			writeError(writer, http.StatusServiceUnavailable, "identity_learning_unavailable", "identity learning is unavailable")
			return
		}
		var found bool
		fingerprint, found = importer.LearnedIdentityFingerprint(input.Client)
		if !found {
			writeError(writer, http.StatusConflict, "identity_not_learned", "no eligible learned identity is available")
			return
		}
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		writeError(writer, http.StatusInternalServerError, "identity_unavailable", "identity profile id could not be created")
		return
	}
	candidate := contract.IdentityProfile{
		ID:        contract.IdentityProfileID("identity_" + hex.EncodeToString(random[:])),
		ServiceID: serviceID, Client: input.Client, Source: input.Source, Fingerprint: fingerprint,
	}
	record, err := profiles.CreateIdentityProfile(request.Context(), candidate)
	if err != nil {
		handler.writeStoreError(writer, err)
		return
	}
	writer.Header().Set("Location", ServicesPath+"/"+string(serviceID)+"/identity-profiles/"+string(record.Profile.ID))
	writer.Header().Set("ETag", record.ETag)
	writeJSON(writer, http.StatusCreated, record.Profile)
}

func (handler *Handler) listIdentityProfiles(writer http.ResponseWriter, request *http.Request, profiles storage.IdentityProfileStore, serviceID contract.ServiceID) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		writeError(writer, http.StatusBadRequest, "invalid_query", "query is invalid")
		return
	}
	options := storage.IdentityProfileListOptions{}
	for name, values := range query {
		if len(values) != 1 || (name != "limit" && name != "cursor") {
			writeError(writer, http.StatusBadRequest, "invalid_query", "only single limit and cursor values are accepted")
			return
		}
	}
	if values, exists := query["limit"]; exists {
		limit, err := strconv.Atoi(values[0])
		if err != nil || limit < 1 || limit > 200 {
			writeError(writer, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 200")
			return
		}
		options.Limit = limit
	}
	options.Cursor = query.Get("cursor")
	if len(options.Cursor) > 512 {
		writeError(writer, http.StatusBadRequest, "invalid_cursor", "cursor is too long")
		return
	}
	page, err := profiles.ListIdentityProfiles(request.Context(), serviceID, options)
	if err != nil {
		handler.writeStoreError(writer, err)
		return
	}
	response := identityProfilePageResponse{Items: make([]contract.IdentityProfile, 0, len(page.Items))}
	for _, record := range page.Items {
		response.Items = append(response.Items, record.Profile)
	}
	if page.NextCursor != "" {
		response.NextCursor = &page.NextCursor
	}
	writeJSON(writer, http.StatusOK, response)
}
