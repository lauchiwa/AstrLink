package forkcheckin

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

func (api *apiHandler) jobsResource(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		api.listJobs(writer, request)
	case http.MethodPost:
		api.createJobs(writer, request)
	default:
		methodNotAllowed(writer, "GET, POST")
	}
}

// jobResource serves /jobs/{id} (read) and /jobs/{id}/cancel (write).
func (api *apiHandler) jobResource(writer http.ResponseWriter, request *http.Request) {
	rest := strings.TrimPrefix(request.URL.Path, APIPrefix+"/jobs/")
	if id, found := strings.CutSuffix(rest, "/cancel"); found {
		if request.Method != http.MethodPost {
			methodNotAllowed(writer, http.MethodPost)
			return
		}
		api.cancelJob(writer, request, id)
		return
	}
	if strings.Contains(rest, "/") {
		writeAPIError(writer, http.StatusNotFound, "not_found", "check-in path not found")
		return
	}
	if request.Method != http.MethodGet {
		methodNotAllowed(writer, http.MethodGet)
		return
	}
	api.getJob(writer, request)
}

// createJobs answers 202 as soon as the jobs and their receipt are stored.
// Execution starts afterwards in the background; this request never waits
// for a site or a person.
func (api *apiHandler) createJobs(writer http.ResponseWriter, request *http.Request) {
	var body JobCreateRequest
	if !noQuery(writer, request) || !decodeWriteBody(writer, request, &body) {
		return
	}
	if ValidateRequestID(body.RequestID) != nil {
		writeAPIValidation(writer, "request_id", "request_id is required and must be 8-128 URL-safe characters")
		return
	}
	if body.Action.Validate() != nil {
		writeAPIValidation(writer, "action", "action must be check_in or status_refresh")
		return
	}
	if len(body.Accounts) == 0 || len(body.Accounts) > MaxBatchAccounts {
		writeAPIValidation(writer, "accounts", "accounts must name 1 to 25 accounts")
		return
	}
	seen := make(map[AccountID]bool, len(body.Accounts))
	for _, id := range body.Accounts {
		if !accountPathID.MatchString(string(id)) || seen[id] {
			writeAPIValidation(writer, "accounts", "accounts must be distinct account ids")
			return
		}
		seen[id] = true
	}
	if body.ExpectedRevision < 0 || body.ExpectedRevision > 0 && len(body.Accounts) > 1 {
		writeAPIValidation(writer, "expected_revision", "expected_revision is a positive revision, for one account only")
		return
	}
	api.runJobWrite(writer, request, func(ctx context.Context, jobs JobWriter) (JobWriteResult, error) {
		return jobs.CreateJobs(ctx, body)
	})
}

func (api *apiHandler) cancelJob(writer http.ResponseWriter, request *http.Request, id string) {
	if !jobPathID.MatchString(id) {
		writeAPIError(writer, http.StatusNotFound, "not_found", "no such check-in resource")
		return
	}
	var body JobCancelRequest
	if !noQuery(writer, request) || !decodeWriteBody(writer, request, &body) {
		return
	}
	if ValidateRequestID(body.RequestID) != nil {
		writeAPIValidation(writer, "request_id", "request_id is required and must be 8-128 URL-safe characters")
		return
	}
	api.runJobWrite(writer, request, func(ctx context.Context, jobs JobWriter) (JobWriteResult, error) {
		return jobs.CancelJob(ctx, JobID(id), body)
	})
}

func (api *apiHandler) runJobWrite(writer http.ResponseWriter, request *http.Request, run func(context.Context, JobWriter) (JobWriteResult, error)) {
	result, err := api.facade.jobs(request.Context(), run)
	if errors.Is(err, ErrJobNotCancellable) {
		writeAPIError(writer, http.StatusConflict, "job_not_cancellable", "the check-in job already finished")
		return
	}
	if writeWriteError(writer, err) {
		return
	}
	writer.WriteHeader(result.Status)
	_, _ = writer.Write(result.Body)
	_, _ = writer.Write([]byte("\n"))
}
