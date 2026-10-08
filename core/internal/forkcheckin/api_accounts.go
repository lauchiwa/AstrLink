package forkcheckin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

const (
	// maxWriteBodyBytes bounds every extension write body. The largest valid
	// body (an update with two 2 KiB URLs and 32 bindings) is well below it.
	maxWriteBodyBytes = 16 << 10
	// settingsStopBudget bounds how long a disable waits for workers before
	// answering. The switch is already saved; Status reports stop_incomplete.
	settingsStopBudget = 10 * time.Second
	// settingsInitBudget bounds enabling: it prepares extension tables only.
	settingsInitBudget = 30 * time.Second
	// settingsLedgerSize bounds the in-memory settings receipts. The switch
	// file holds no request ids, and a settings write is state-idempotent,
	// so a ledger lost on restart replays as the same state.
	settingsLedgerSize = 256
)

// SettingsUpdateRequest is PUT /settings.
type SettingsUpdateRequest struct {
	RequestID string `json:"request_id"`
	Enabled   *bool  `json:"enabled"`
}

type settingsLedger struct {
	mu      sync.Mutex
	order   []string
	entries map[string]bool
}

func (ledger *settingsLedger) lookup(id string) (bool, bool) {
	enabled, ok := ledger.entries[id]
	return enabled, ok
}

func (ledger *settingsLedger) record(id string, enabled bool) {
	if ledger.entries == nil {
		ledger.entries = make(map[string]bool, settingsLedgerSize)
	}
	if _, ok := ledger.entries[id]; ok {
		return
	}
	if len(ledger.order) == settingsLedgerSize {
		delete(ledger.entries, ledger.order[0])
		ledger.order = ledger.order[1:]
	}
	ledger.order = append(ledger.order, id)
	ledger.entries[id] = enabled
}

// settingsResource answers while disabled; PUT is how the extension is turned
// on. Enabling an already running module and disabling an already stopped one
// are no-ops beyond rewriting the switch, so nothing is rebuilt.
func (api *apiHandler) settingsResource(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		api.settings(writer, request)
	case http.MethodPut:
		api.updateSettings(writer, request)
	default:
		methodNotAllowed(writer, "GET, PUT")
	}
}

func (api *apiHandler) updateSettings(writer http.ResponseWriter, request *http.Request) {
	var body SettingsUpdateRequest
	if !decodeWriteBody(writer, request, &body) {
		return
	}
	if ValidateRequestID(body.RequestID) != nil {
		writeAPIValidation(writer, "request_id", "request_id is required and must be 8-128 URL-safe characters")
		return
	}
	if body.Enabled == nil {
		writeAPIValidation(writer, "enabled", "enabled is required")
		return
	}
	enabled := *body.Enabled
	api.ledger.mu.Lock()
	defer api.ledger.mu.Unlock()
	if previous, ok := api.ledger.lookup(body.RequestID); ok {
		if previous != enabled {
			writeAPIError(writer, http.StatusConflict, "request_id_reused", "request_id was used for different content")
			return
		}
		writeAPIJSON(writer, http.StatusOK, apiSettings{Enabled: previous})
		return
	}
	// A dropped connection must not abandon initialization or a stop half
	// way, so neither follows the request's cancellation.
	var err error
	if enabled {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), settingsInitBudget)
		_, err = api.facade.Enable(ctx)
		cancel()
	} else {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(request.Context()), settingsStopBudget)
		_, err = api.facade.Disable(ctx)
		cancel()
		if errors.Is(err, context.DeadlineExceeded) {
			// Saved and stopping; the status route reports stop_incomplete
			// and a later disable resumes the wait.
			err = nil
		}
	}
	switch {
	case errors.Is(err, ErrExtensionStopping):
		writeAPIRetryable(writer, http.StatusServiceUnavailable, "checkin_stopping", "the check-in extension is stopping")
		return
	case err != nil:
		writeAPIError(writer, http.StatusInternalServerError, "checkin_settings_failed", "check-in settings could not be saved")
		return
	}
	api.ledger.record(body.RequestID, enabled)
	writeAPIJSON(writer, http.StatusOK, apiSettings{Enabled: enabled})
}

func (api *apiHandler) accountsResource(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		api.listAccounts(writer, request)
	case http.MethodPost:
		api.createAccount(writer, request)
	default:
		methodNotAllowed(writer, "GET, POST")
	}
}

func (api *apiHandler) accountResource(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		api.getAccount(writer, request)
	case http.MethodPatch:
		api.updateAccount(writer, request)
	case http.MethodDelete:
		api.deleteAccount(writer, request)
	default:
		methodNotAllowed(writer, "GET, PATCH, DELETE")
	}
}

func (api *apiHandler) createAccount(writer http.ResponseWriter, request *http.Request) {
	var body AccountDraftRequest
	if !noQuery(writer, request) || !decodeWriteBody(writer, request, &body) {
		return
	}
	if ValidateRequestID(body.RequestID) != nil {
		writeAPIValidation(writer, "request_id", "request_id is required and must be 8-128 URL-safe characters")
		return
	}
	if body.DashboardBaseURL == "" || body.TimeZone == "" {
		writeAPIValidation(writer, "", "dashboard_base_url and time_zone are required")
		return
	}
	api.runWrite(writer, request, func(ctx context.Context, accounts AccountWriter) (AccountWriteResult, error) {
		return accounts.CreateAccount(ctx, body)
	})
}

func (api *apiHandler) updateAccount(writer http.ResponseWriter, request *http.Request) {
	id, ok := pathID(writer, request, APIPrefix+"/accounts/", accountPathID)
	if !ok || !noQuery(writer, request) {
		return
	}
	var body AccountUpdateRequest
	if !decodeWriteBody(writer, request, &body) {
		return
	}
	if ValidateRequestID(body.RequestID) != nil {
		writeAPIValidation(writer, "request_id", "request_id is required and must be 8-128 URL-safe characters")
		return
	}
	if body.ExpectedRevision < 1 {
		writeAPIValidation(writer, "expected_revision", "expected_revision is required")
		return
	}
	if body.Empty() {
		writeAPIValidation(writer, "", "the update names no field")
		return
	}
	api.runWrite(writer, request, func(ctx context.Context, accounts AccountWriter) (AccountWriteResult, error) {
		return accounts.UpdateAccount(ctx, AccountID(id), body)
	})
}

func (api *apiHandler) deleteAccount(writer http.ResponseWriter, request *http.Request) {
	id, ok := pathID(writer, request, APIPrefix+"/accounts/", accountPathID)
	if !ok {
		return
	}
	query := request.URL.Query()
	for name, values := range query {
		if name != "request_id" && name != "expected_revision" || len(values) != 1 {
			writeAPIValidation(writer, name, "only request_id and expected_revision are accepted, once each")
			return
		}
	}
	body := AccountDeleteRequest{RequestID: query.Get("request_id")}
	if ValidateRequestID(body.RequestID) != nil {
		writeAPIValidation(writer, "request_id", "request_id is required and must be 8-128 URL-safe characters")
		return
	}
	raw := query.Get("expected_revision")
	revision, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || revision < 1 || strconv.FormatInt(revision, 10) != raw {
		writeAPIValidation(writer, "expected_revision", "expected_revision must be a positive integer")
		return
	}
	body.ExpectedRevision = revision
	if request.ContentLength > 0 {
		writeAPIValidation(writer, "body", "delete takes no request body")
		return
	}
	api.runWrite(writer, request, func(ctx context.Context, accounts AccountWriter) (AccountWriteResult, error) {
		return accounts.DeleteAccount(ctx, AccountID(id), body)
	})
}

func (api *apiHandler) runWrite(writer http.ResponseWriter, request *http.Request, run func(context.Context, AccountWriter) (AccountWriteResult, error)) {
	var result AccountWriteResult
	err := api.facade.write(request.Context(), func(ctx context.Context, accounts AccountWriter) (err error) {
		result, err = run(ctx, accounts)
		return err
	})
	if writeWriteError(writer, err) {
		return
	}
	if result.Status == http.StatusNoContent {
		writer.Header().Del("Content-Type")
		writer.WriteHeader(http.StatusNoContent)
		return
	}
	writer.WriteHeader(result.Status)
	_, _ = writer.Write(result.Body)
	_, _ = writer.Write([]byte("\n"))
}

func writeWriteError(writer http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrRequestIDReused):
		writeAPIError(writer, http.StatusConflict, "request_id_reused", "request_id was used for different content")
	case errors.Is(err, ErrAccountConflict):
		writeAPIError(writer, http.StatusConflict, "account_conflict", "another check-in account already holds this site and user")
	case errors.Is(err, ErrAccountBusy):
		writeAPIRetryable(writer, http.StatusConflict, "account_busy", "a check-in for this account is running")
	case errors.Is(err, ErrUnknownService):
		writeAPIValidation(writer, "bound_services", "a bound service does not exist")
	case errors.Is(err, storagecontract.ErrPrecondition), errors.Is(err, ErrRevisionChanged):
		writeAPIError(writer, http.StatusPreconditionFailed, "revision_conflict", "the account changed; reload it and retry")
	case errors.Is(err, storagecontract.ErrInvalidRecord):
		writeAPIValidation(writer, "", "the account is not valid")
	default:
		return writeReadError(writer, err)
	}
	return true
}

// decodeWriteBody reads one strict JSON object no larger than the write
// limit. Unknown fields, trailing data and other media types are refused.
func decodeWriteBody(writer http.ResponseWriter, request *http.Request, target any) bool {
	return decodeBody(writer, request, target, maxWriteBodyBytes)
}

// decodeBody is decodeWriteBody with an explicit limit. The raw body is
// cleared once decoded, since one route carries a captured session.
func decodeBody(writer http.ResponseWriter, request *http.Request, target any, limit int64) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeAPIError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "request Content-Type must be application/json")
		return false
	}
	data, err := io.ReadAll(http.MaxBytesReader(writer, request.Body, limit))
	defer clear(data)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeAPIError(writer, http.StatusRequestEntityTooLarge, "payload_too_large", "request body exceeds the check-in limit")
		return false
	}
	if err != nil {
		writeAPIError(writer, http.StatusBadRequest, "invalid_json", "request body could not be read")
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var extra any
	if decoder.Decode(target) != nil || !errors.Is(decoder.Decode(&extra), io.EOF) {
		writeAPIError(writer, http.StatusBadRequest, "invalid_json", "request body must be exactly one JSON object with known fields")
		return false
	}
	return true
}

func methodNotAllowed(writer http.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	writeAPIError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method is not allowed on this path")
}
