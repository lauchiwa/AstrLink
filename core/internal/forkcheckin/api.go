package forkcheckin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// APIPrefix is the extension's private control namespace. A Core built
// without the extension answers every path under it with 404.
const APIPrefix = "/control/v1/extensions/checkin"

var (
	accountPathID = regexp.MustCompile(`^[a-z][a-z0-9_]{2,63}$`)
	jobPathID     = regexp.MustCompile(`^[a-z0-9_]{3,64}$`)
)

// APIStatus is the wire shape of GET /status in checkin.openapi.yaml.
type APIStatus struct {
	ProtocolVersion  int    `json:"protocol_version"`
	Present          bool   `json:"present"`
	Enabled          bool   `json:"enabled"`
	StorageReady     bool   `json:"storage_ready"`
	SchedulerRunning bool   `json:"scheduler_running"`
	LastErrorCode    string `json:"last_error_code,omitempty"`
}

type apiSettings struct {
	Enabled bool `json:"enabled"`
}

type apiJobPage struct {
	Items      []PublicJob `json:"items"`
	NextCursor string      `json:"next_cursor,omitempty"`
}

// NewAPIHandler serves the extension's control routes. It performs no
// authentication itself: the Core mounts it behind the operator guard. No
// route contacts a site or opens the vault. Only status and settings answer
// while the extension is disabled, and only a settings PUT creates storage.
func NewAPIHandler(facade *Facade) http.Handler {
	api := &apiHandler{facade: facade, ledger: &settingsLedger{}}
	mux := http.NewServeMux()
	mux.HandleFunc(APIPrefix+"/status", api.getOnly(api.status))
	mux.HandleFunc(APIPrefix+"/settings", api.settingsResource)
	mux.HandleFunc(APIPrefix+"/accounts", api.accountsResource)
	mux.HandleFunc(APIPrefix+"/accounts/", api.accountResource)
	mux.HandleFunc(APIPrefix+"/jobs", api.jobsResource)
	mux.HandleFunc(APIPrefix+"/jobs/", api.jobResource)
	mux.HandleFunc(APIPrefix+"/authorizations", api.authorizationsResource)
	mux.HandleFunc(APIPrefix+"/authorizations/", api.authorizationResource)
	mux.HandleFunc("/", func(writer http.ResponseWriter, _ *http.Request) {
		writeAPIError(writer, http.StatusNotFound, "not_found", "check-in path not found")
	})
	return api.withHeaders(mux)
}

type apiHandler struct {
	facade *Facade
	ledger *settingsLedger
}

func (api *apiHandler) withHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		header := writer.Header()
		header.Set("Cache-Control", "no-store")
		header.Set("Content-Type", "application/json")
		header.Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(writer, request)
	})
}

// getOnly guards read-only routes.
func (api *apiHandler) getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			writer.Header().Set("Allow", http.MethodGet)
			writeAPIError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "only GET is available")
			return
		}
		next(writer, request)
	}
}

func (api *apiHandler) status(writer http.ResponseWriter, request *http.Request) {
	status := api.facade.Status(request.Context())
	writeAPIJSON(writer, http.StatusOK, APIStatus{
		ProtocolVersion: ProtocolVersion, Present: true, Enabled: status.Enabled,
		StorageReady: status.Initialized, SchedulerRunning: status.Initialized,
		LastErrorCode: status.LastInitError,
	})
}

func (api *apiHandler) settings(writer http.ResponseWriter, request *http.Request) {
	writeAPIJSON(writer, http.StatusOK, apiSettings{Enabled: api.facade.Status(request.Context()).Enabled})
}

func (api *apiHandler) listAccounts(writer http.ResponseWriter, request *http.Request) {
	options, ok := pageOptions(writer, request, "limit", "cursor")
	if !ok {
		return
	}
	var page AccountViewPage
	err := api.facade.read(request.Context(), func(ctx context.Context, reader Reader) (err error) {
		page, err = reader.ListAccounts(ctx, options)
		return err
	})
	if writeReadError(writer, err) {
		return
	}
	if page.Items == nil {
		page.Items = []AccountView{}
	}
	writeAPIJSON(writer, http.StatusOK, page)
}

func (api *apiHandler) getAccount(writer http.ResponseWriter, request *http.Request) {
	id, ok := pathID(writer, request, APIPrefix+"/accounts/", accountPathID)
	if !ok || !noQuery(writer, request) {
		return
	}
	var view AccountView
	err := api.facade.read(request.Context(), func(ctx context.Context, reader Reader) (err error) {
		view, err = reader.GetAccount(ctx, AccountID(id))
		return err
	})
	if writeReadError(writer, err) {
		return
	}
	writeAPIJSON(writer, http.StatusOK, view)
}

func (api *apiHandler) listJobs(writer http.ResponseWriter, request *http.Request) {
	options, ok := pageOptions(writer, request, "limit", "cursor", "account_id")
	if !ok {
		return
	}
	account := request.URL.Query().Get("account_id")
	if account != "" && !accountPathID.MatchString(account) {
		writeAPIValidation(writer, "account_id", "account_id is not a check-in account id")
		return
	}
	var page JobPage
	err := api.facade.read(request.Context(), func(ctx context.Context, reader Reader) (err error) {
		page, err = reader.ListJobs(ctx, AccountID(account), options)
		return err
	})
	if writeReadError(writer, err) {
		return
	}
	result := apiJobPage{Items: make([]PublicJob, 0, len(page.Items)), NextCursor: page.NextCursor}
	for _, receipt := range page.Items {
		result.Items = append(result.Items, receipt.Public())
	}
	writeAPIJSON(writer, http.StatusOK, result)
}

func (api *apiHandler) getJob(writer http.ResponseWriter, request *http.Request) {
	id, ok := pathID(writer, request, APIPrefix+"/jobs/", jobPathID)
	if !ok || !noQuery(writer, request) {
		return
	}
	var public PublicJob
	err := api.facade.read(request.Context(), func(ctx context.Context, reader Reader) error {
		// A batch parent is not a job row; it is projected from its
		// children's current state.
		if strings.HasPrefix(id, BatchIDPrefix) {
			children, err := reader.GetBatch(ctx, JobID(id))
			public = AggregateBatch(JobID(id), children)
			return err
		}
		receipt, err := reader.GetJob(ctx, JobID(id))
		public = receipt.Public()
		return err
	})
	if writeReadError(writer, err) {
		return
	}
	writeAPIJSON(writer, http.StatusOK, public)
}

// pageOptions parses the shared paging query strictly: unknown or repeated
// parameters and out-of-range limits are rejected, never clamped.
func pageOptions(writer http.ResponseWriter, request *http.Request, allowed ...string) (ListOptions, bool) {
	query := request.URL.Query()
	for name, values := range query {
		known := false
		for _, candidate := range allowed {
			known = known || candidate == name
		}
		if !known {
			writeAPIValidation(writer, name, "unknown query parameter")
			return ListOptions{}, false
		}
		if len(values) != 1 {
			writeAPIValidation(writer, name, "query parameter must appear once")
			return ListOptions{}, false
		}
	}
	var options ListOptions
	if raw, present := query["limit"]; present {
		limit, err := strconv.Atoi(raw[0])
		if err != nil || limit < 1 || limit > MaxPageSize || strconv.Itoa(limit) != raw[0] {
			writeAPIValidation(writer, "limit", "limit must be an integer from 1 to 100")
			return ListOptions{}, false
		}
		options.Limit = limit
	}
	options.Cursor = query.Get("cursor")
	if _, present := query["cursor"]; present && options.Cursor == "" || len(options.Cursor) > 256 {
		writeAPIValidation(writer, "cursor", "cursor is invalid")
		return ListOptions{}, false
	}
	return options, true
}

func noQuery(writer http.ResponseWriter, request *http.Request) bool {
	if request.URL.RawQuery != "" {
		writeAPIValidation(writer, "query", "this path takes no query parameters")
		return false
	}
	return true
}

func pathID(writer http.ResponseWriter, request *http.Request, prefix string, pattern *regexp.Regexp) (string, bool) {
	id := strings.TrimPrefix(request.URL.Path, prefix)
	if strings.Contains(id, "/") {
		// Sub-resources such as /jobs/{id}/cancel have their own handler.
		writeAPIError(writer, http.StatusNotFound, "not_found", "check-in path not found")
		return "", false
	}
	if !pattern.MatchString(id) {
		writeAPIError(writer, http.StatusNotFound, "not_found", "no such check-in resource")
		return "", false
	}
	return id, true
}

// writeReadError maps local outcomes to stable codes. Storage error text is
// never written, so no SQL, path or identifier detail reaches the caller.
func writeReadError(writer http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrExtensionDisabled):
		writeAPIError(writer, http.StatusConflict, "checkin_disabled", "the check-in extension is off")
	case errors.Is(err, ErrExtensionStopping):
		writeAPIRetryable(writer, http.StatusServiceUnavailable, "checkin_stopping", "the check-in extension is stopping")
	case errors.Is(err, storagecontract.ErrNotFound):
		writeAPIError(writer, http.StatusNotFound, "not_found", "no such check-in resource")
	case errors.Is(err, storagecontract.ErrInvalidCursor):
		writeAPIValidation(writer, "cursor", "cursor is invalid")
	case errors.Is(err, storagecontract.ErrInvalidArgument):
		writeAPIValidation(writer, "", "the request was rejected")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeAPIRetryable(writer, http.StatusServiceUnavailable, "checkin_unavailable", "the check-in read did not finish")
	default:
		writeAPIError(writer, http.StatusInternalServerError, "checkin_storage_failed", "check-in storage failed")
	}
	return true
}

type apiErrorEnvelope struct {
	Error     apiError `json:"error"`
	RequestID string   `json:"request_id"`
}

type apiError struct {
	Code      string           `json:"code"`
	Message   string           `json:"message"`
	Retryable bool             `json:"retryable"`
	Details   []apiErrorDetail `json:"details"`
}

type apiErrorDetail struct {
	Field  string `json:"field,omitempty"`
	Reason string `json:"reason,omitempty"`
}

func writeAPIValidation(writer http.ResponseWriter, field, reason string) {
	writeAPIEnvelope(writer, http.StatusBadRequest, apiError{Code: "validation_failed",
		Message: "the request was rejected", Details: []apiErrorDetail{{Field: field, Reason: reason}}})
}

func writeAPIError(writer http.ResponseWriter, status int, code, message string) {
	writeAPIEnvelope(writer, status, apiError{Code: code, Message: message})
}

func writeAPIRetryable(writer http.ResponseWriter, status int, code, message string) {
	writeAPIEnvelope(writer, status, apiError{Code: code, Message: message, Retryable: true})
}

// writeAPIEnvelope matches the control API's error envelope so the desktop's
// existing error handling applies unchanged.
func writeAPIEnvelope(writer http.ResponseWriter, status int, body apiError) {
	if body.Details == nil {
		body.Details = []apiErrorDetail{}
	}
	writeAPIJSON(writer, status, apiErrorEnvelope{Error: body, RequestID: apiRequestID()})
}

func writeAPIJSON(writer http.ResponseWriter, status int, value any) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func apiRequestID() string {
	var value [12]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "req_unavailable"
	}
	return "req_" + hex.EncodeToString(value[:])
}
