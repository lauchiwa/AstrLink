package forkcheckin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"

	"github.com/QuantumNous/astrlink/core/contract"
)

var (
	// ErrRequestIDReused: the request_id already has a receipt for other
	// content. The stored receipt is left as it is.
	ErrRequestIDReused = errors.New("check-in request id was used for different content")
	// ErrAccountConflict: another account already holds this site and user.
	ErrAccountConflict = errors.New("check-in account conflicts with an existing account")
	// ErrAccountBusy: a job holds a live lease on the account, so deleting it
	// now would erase the record of a submission that may be in flight.
	ErrAccountBusy = errors.New("check-in account has a running job")
	// ErrUnknownService: a binding names a service that does not exist.
	ErrUnknownService = errors.New("check-in binding names an unknown service")
)

// Write routes. A request_id belongs to exactly one route.
const (
	WriteRouteAccountCreate = "accounts.create"
	WriteRouteAccountUpdate = "accounts.update"
	WriteRouteAccountDelete = "accounts.delete"
)

// AccountDraftRequest is POST /accounts. A new account is always a draft
// with automation off; the server chooses its id.
type AccountDraftRequest struct {
	RequestID        string   `json:"request_id"`
	DashboardBaseURL string   `json:"dashboard_base_url"`
	TimeZone         string   `json:"time_zone"`
	Network          *Network `json:"network,omitempty"`
}

// AccountUpdateRequest is PATCH /accounts/{id}. Absent fields keep their
// stored value; the patch is applied inside the compare-and-set.
type AccountUpdateRequest struct {
	RequestID        string                `json:"request_id"`
	ExpectedRevision int64                 `json:"expected_revision"`
	DashboardBaseURL *string               `json:"dashboard_base_url,omitempty"`
	TimeZone         *string               `json:"time_zone,omitempty"`
	Network          *Network              `json:"network,omitempty"`
	Automatic        *bool                 `json:"automatic,omitempty"`
	BoundServices    *[]contract.ServiceID `json:"bound_services,omitempty"`
}

// AccountDeleteRequest is DELETE /accounts/{id}?request_id=&expected_revision=.
type AccountDeleteRequest struct {
	RequestID        string `json:"request_id"`
	ExpectedRevision int64  `json:"expected_revision"`
}

// AccountWriteResult is the stored public receipt. Replayed is true when an
// earlier request with the same id and content already produced it.
type AccountWriteResult struct {
	Status   int
	Body     []byte
	Replayed bool
}

// AccountWriter performs operator account writes. Each call commits the
// change and its receipt in one transaction; it never contacts a site and
// never writes a service.
type AccountWriter interface {
	CreateAccount(context.Context, AccountDraftRequest) (AccountWriteResult, error)
	UpdateAccount(context.Context, AccountID, AccountUpdateRequest) (AccountWriteResult, error)
	DeleteAccount(context.Context, AccountID, AccountDeleteRequest) (AccountWriteResult, error)
}

// Fingerprint covers everything but the request id, so a retry with the same
// content replays and any difference is a conflict.
func (request AccountDraftRequest) Fingerprint() string {
	request.RequestID = ""
	return writeFingerprint(WriteRouteAccountCreate, "", request)
}

func (request AccountUpdateRequest) Fingerprint(id AccountID) string {
	request.RequestID = ""
	return writeFingerprint(WriteRouteAccountUpdate, id, request)
}

func (request AccountDeleteRequest) Fingerprint(id AccountID) string {
	request.RequestID = ""
	return writeFingerprint(WriteRouteAccountDelete, id, request)
}

func writeFingerprint(route string, id AccountID, body any) string {
	encoded, err := json.Marshal(body)
	if err != nil {
		// Every request type above is plain data; marshalling cannot fail.
		panic(err)
	}
	digest := sha256.New()
	for _, part := range [][]byte{[]byte(route), []byte(id), encoded} {
		digest.Write(part)
		digest.Write([]byte{0})
	}
	return hex.EncodeToString(digest.Sum(nil))
}

// Apply returns the account a patch produces. Identity, state and revision
// stay server-owned.
func (request AccountUpdateRequest) Apply(current Account) Account {
	next := current
	if request.DashboardBaseURL != nil {
		next.DashboardBaseURL = *request.DashboardBaseURL
	}
	if request.TimeZone != nil {
		next.TimeZone = *request.TimeZone
	}
	if request.Network != nil {
		next.Network = *request.Network
	}
	if request.Automatic != nil {
		next.Automatic = *request.Automatic
	}
	if request.BoundServices != nil {
		next.BoundServices = append([]contract.ServiceID(nil), (*request.BoundServices)...)
	}
	return next
}

// Empty reports a patch that names no field.
func (request AccountUpdateRequest) Empty() bool {
	return request.DashboardBaseURL == nil && request.TimeZone == nil && request.Network == nil &&
		request.Automatic == nil && request.BoundServices == nil
}
