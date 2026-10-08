package forkcheckin

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// maxAuthorizationBodyBytes admits one base64 session of MaxCredentialBytes
// plus the small fields around it. Every other write keeps maxWriteBodyBytes.
const maxAuthorizationBodyBytes = 96 << 10

var sessionPathID = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)

// authorizationsResource serves POST /authorizations.
func (api *apiHandler) authorizationsResource(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	api.beginAuthorization(writer, request)
}

// authorizationResource serves POST /authorizations/{id}/complete. A session
// has no other sub-resource and is never readable: it names a pending login.
func (api *apiHandler) authorizationResource(writer http.ResponseWriter, request *http.Request) {
	rest := strings.TrimPrefix(request.URL.Path, APIPrefix+"/authorizations/")
	id, found := strings.CutSuffix(rest, "/complete")
	if !found || strings.Contains(id, "/") {
		writeAPIError(writer, http.StatusNotFound, "not_found", "check-in path not found")
		return
	}
	if request.Method != http.MethodPost {
		methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !sessionPathID.MatchString(id) {
		writeAPIError(writer, http.StatusNotFound, "authorization_not_found", "the authorization session is unknown or expired")
		return
	}
	api.completeAuthorization(writer, request, id)
}

// beginAuthorization answers 201 with a short-lived session. It opens no
// window, contacts no site and stores nothing durable.
func (api *apiHandler) beginAuthorization(writer http.ResponseWriter, request *http.Request) {
	var body AuthorizationRequest
	if !noQuery(writer, request) || !decodeWriteBody(writer, request, &body) {
		return
	}
	if ValidateRequestID(body.RequestID) != nil {
		writeAPIValidation(writer, "request_id", "request_id is required and must be 8-128 URL-safe characters")
		return
	}
	if body.AccountID.Validate() != nil {
		writeAPIValidation(writer, "account_id", "account_id is not a check-in account id")
		return
	}
	if body.ExpectedRevision < 1 {
		writeAPIValidation(writer, "expected_revision", "expected_revision is required")
		return
	}
	var session Authorization
	err := api.facade.authorize(request.Context(), func(ctx context.Context, authorization *authorizer) (err error) {
		session, err = authorization.begin(ctx, body)
		return err
	})
	if writeAuthorizationError(writer, err) {
		return
	}
	writeAPIJSON(writer, http.StatusCreated, session)
}

// completeAuthorization stores a captured session once the account's own
// site confirms whose it is. The response is the account receipt; the
// session is never echoed and never logged.
func (api *apiHandler) completeAuthorization(writer http.ResponseWriter, request *http.Request, id string) {
	var body AuthorizationCompletion
	if !noQuery(writer, request) || !decodeBody(writer, request, &body, maxAuthorizationBodyBytes) {
		clear(body.Credential)
		return
	}
	defer clear(body.Credential)
	if ValidateRequestID(body.RequestID) != nil {
		writeAPIValidation(writer, "request_id", "request_id is required and must be 8-128 URL-safe characters")
		return
	}
	if len(body.Credential) == 0 || len(body.Credential) > MaxCredentialBytes {
		writeAPIValidation(writer, "credential", "credential is required and must be at most 64 KiB decoded")
		return
	}
	if body.ExpectedRevision != nil && *body.ExpectedRevision < 1 {
		writeAPIValidation(writer, "expected_revision", "expected_revision must be positive")
		return
	}
	if body.ClaimedUserID != nil && ValidateClaimedUserID(*body.ClaimedUserID) != nil {
		writeAPIValidation(writer, "claimed_user_id", "claimed_user_id must be 1-128 printable characters")
		return
	}
	var result AccountWriteResult
	err := api.facade.authorize(request.Context(), func(ctx context.Context, authorization *authorizer) (err error) {
		result, err = authorization.complete(ctx, id, body)
		return err
	})
	if writeAuthorizationError(writer, err) {
		return
	}
	writer.WriteHeader(result.Status)
	_, _ = writer.Write(result.Body)
	_, _ = writer.Write([]byte("\n"))
}

// writeAuthorizationError maps typed outcomes to stable codes. No site text,
// address or session detail is ever written.
func writeAuthorizationError(writer http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, ErrAuthorizationNotFound):
		writeAPIError(writer, http.StatusNotFound, "authorization_not_found", "the authorization session is unknown or expired")
	case errors.Is(err, ErrAuthorizationCompleted):
		writeAPIError(writer, http.StatusConflict, "authorization_completed", "the authorization session was already completed")
	case errors.Is(err, ErrIdentityMismatch):
		writeAPIError(writer, http.StatusConflict, "identity_mismatch", "the site reported a different user than this account")
	case errors.Is(err, ErrCredentialInvalid):
		writeAPIValidation(writer, "credential", "the captured session is not valid for this account's site and network")
	case errors.Is(err, ErrAuthRequired):
		writeAPIError(writer, http.StatusConflict, "auth_required", "the site did not accept the captured session")
	case errors.Is(err, ErrManualRequired):
		writeAPIError(writer, http.StatusConflict, "manual_required", "the site needs manual action before this session can be verified")
	case errors.Is(err, ErrUnsupported):
		writeAPIError(writer, http.StatusConflict, "unsupported", "this site cannot verify a session in a supported way")
	case errors.Is(err, ErrRateLimited):
		writeAPIRetryable(writer, http.StatusServiceUnavailable, "rate_limited", "the site asked to slow down; retry later")
	case errors.Is(err, ErrSiteUnavailable):
		writeAPIRetryable(writer, http.StatusServiceUnavailable, "site_unavailable", "the site could not verify the session; retry later")
	case errors.Is(err, ErrRevisionChanged):
		writeAPIError(writer, http.StatusPreconditionFailed, "revision_conflict", "the account changed; reload it and retry")
	default:
		return writeWriteError(writer, err)
	}
	return true
}
