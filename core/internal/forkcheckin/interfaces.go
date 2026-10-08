package forkcheckin

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
)

// Typed failures every layer of the extension shares. They are sentinels so
// the scheduler can decide what to do without matching on message text, and
// so a site's own wording never reaches a caller as a classification.
var (
	// ErrCredentialUnavailable means no session is stored, or it could not
	// be opened because the local key is gone. It is not a site rejection.
	ErrCredentialUnavailable = errors.New("check-in session is unavailable")
	// ErrAuthRequired means the site rejected the stored session.
	ErrAuthRequired = errors.New("check-in session was rejected by the site")
	// ErrManualRequired means a human has to act: a challenge page, or a
	// login dialect this build does not support. Nothing is retried.
	ErrManualRequired = errors.New("check-in requires manual action")
	// ErrUnsupported means the site does not offer check-in, or not in a
	// shape this adapter speaks.
	ErrUnsupported = errors.New("check-in is unsupported for this site")
	// ErrCheckInDisabled is a recognized site with check-in explicitly off.
	ErrCheckInDisabled = fmt.Errorf("%w: feature is disabled", ErrUnsupported)
	// ErrPermissionDenied is a valid protocol's refusal of this operation.
	ErrPermissionDenied = errors.New("check-in permission was denied")
	// ErrRateLimited means the site asked the caller to slow down.
	ErrRateLimited = errors.New("check-in was rate limited")
	// ErrUncertain means a request was dispatched and its result could not
	// be established. The caller re-reads status; it never submits again.
	ErrUncertain = errors.New("check-in result is uncertain")
	// ErrRevisionChanged means the account changed after the work was
	// prepared, so the prepared work no longer applies.
	ErrRevisionChanged = errors.New("account revision changed")
	// ErrIdentityMismatch means the site reported a different user than the
	// account expects. Nothing is overwritten.
	ErrIdentityMismatch = errors.New("site reported a different account")
)

// Vault stores one opaque session per account. It is internal to the
// extension: it is not a secretstore.SecretStore, it introduces no global
// credential reference, and it has no method that returns a key.
//
// Implementations seal the value bound to its account, so a row copied from
// another account fails to open rather than decrypting.
type Vault interface {
	// Get returns the stored session. It returns a value wrapping
	// [ErrCredentialUnavailable] when nothing is stored or the value
	// cannot be opened.
	Get(ctx context.Context, id AccountID) ([]byte, error)
	// Put replaces the stored session. Callers bound the input to
	// [MaxCredentialBytes].
	Put(ctx context.Context, id AccountID, credential []byte) error
	// Delete removes the stored session. Deleting a missing session is not
	// an error, so account removal is idempotent.
	Delete(ctx context.Context, id AccountID) error
}

// ServiceReader is the extension's read-only view of services. Binding an
// account to a service must not be able to edit, enable, disable or
// re-credential it, so this interface has no write method and returns only
// what the binding UI needs.
type ServiceReader interface {
	// ListBindableServices returns the services an account may be bound to.
	ListBindableServices(ctx context.Context) ([]BindableService, error)
}

// BindableService is the projection of a service used for binding. It
// deliberately omits credentials, proxies, models and the ETag: this
// extension never writes a service, so it has no use for its concurrency
// token.
type BindableService struct {
	ID      contract.ServiceID   `json:"id"`
	Name    string               `json:"name"`
	Kind    contract.ServiceKind `json:"kind"`
	BaseURL string               `json:"base_url"`
	Enabled bool                 `json:"enabled"`
}

// SiteCapability is what a read-only inspection of a site established.
type SiteCapability struct {
	// Supported is false when the site has no check-in this adapter can
	// drive; the reason is carried by a typed error, not by site text.
	Supported bool
	// RequiresManual is true when check-in exists but needs a human, for
	// example behind a challenge page.
	RequiresManual bool
	// Dialect names the protocol variant that was recognized, for the
	// operator's record. It is a fixed set of adapter-owned labels.
	Dialect string
}

// SiteIdentity is the site's answer to "whose session is this". The adapter
// must read it from the site itself; a value supplied by a window or a page
// is never trusted.
type SiteIdentity struct {
	RemoteUserID string
	// DisplayHint is a short, non-secret label such as a masked username,
	// shown so the operator can tell two accounts apart.
	DisplayHint string
}

// CheckInStatus is today's state as the site reports it.
type CheckInStatus struct {
	// CheckedInToday is the site's own answer. The extension never infers
	// it from a balance change.
	CheckedInToday bool
	// SiteDate is the date the site considers current, when it reports one.
	// It wins over the account's time zone for de-duplication.
	SiteDate string
	// NextAvailableAt is when the site says the next check-in opens.
	NextAvailableAt *time.Time
	// Reward is the award the site reported for today, when it reported
	// one and in a unit this adapter recognized.
	Reward *Reward
	// Records preserve the dates and awards the site explicitly reports. A
	// current-month record is not necessarily today's record; without a
	// site-reported current date it must not populate SiteDate or Reward.
	Records []CheckInRecord
}

// CheckInRecord is a historical site award, not proof of today's submission.
type CheckInRecord struct {
	SiteDate string
	Reward   Reward
}

// Reward is an awarded amount with the unit the site expressed it in.
// A missing or unrecognized unit leaves Known false rather than guessing,
// so the UI shows "unknown" instead of an invented number.
type Reward struct {
	Known bool
	// Quota is the raw site quota value.
	Quota int64
	// Unit labels the quota, for example "quota" or "usd".
	Unit string
}

func (reward Reward) Validate() error {
	if !reward.Known {
		if reward.Quota != 0 || reward.Unit != "" {
			return fmt.Errorf("an unknown reward must not carry a value")
		}
		return nil
	}
	if reward.Quota < 0 {
		return fmt.Errorf("reward quota must not be negative")
	}
	if reward.Unit == "" {
		return fmt.Errorf("a known reward requires a unit")
	}
	return nil
}

// SubmitOutcome is the result of one check-in submission. Dispatched is
// recorded separately from the response so a connection that dropped after
// the request left the machine is never retried as if nothing happened.
type SubmitOutcome struct {
	// Dispatched is true once the request was written to the connection.
	Dispatched bool
	// ResponseRead is true once a complete response was parsed.
	ResponseRead bool
	// AlreadyCheckedIn is the site reporting that today is already done.
	// It is a success for the day, not a failure.
	AlreadyCheckedIn bool
	// Succeeded is true only when the site affirmatively reported success.
	// An HTTP 200 alone never sets it.
	Succeeded bool
	Reward    Reward
	// SiteDate is the date the site attributed the check-in to.
	SiteDate string
}

// Validate refuses outcomes that could not have happened, so a buggy
// adapter cannot report a success that was never read.
func (outcome SubmitOutcome) Validate() error {
	if err := outcome.Reward.Validate(); err != nil {
		return err
	}
	if !outcome.Dispatched && (outcome.ResponseRead || outcome.Succeeded || outcome.AlreadyCheckedIn) {
		return fmt.Errorf("an undispatched submission has no result")
	}
	if !outcome.ResponseRead && (outcome.Succeeded || outcome.AlreadyCheckedIn) {
		return fmt.Errorf("a result requires a response that was read")
	}
	if outcome.Succeeded && outcome.AlreadyCheckedIn {
		return fmt.Errorf("a submission cannot both succeed and be a repeat")
	}
	if !outcome.Succeeded && outcome.Reward.Known {
		return fmt.Errorf("only a successful submission carries a reward")
	}
	return nil
}

// ReadOnlySiteAdapter cannot submit a check-in. Each method takes the snapshot
// explicitly; adapters never read the Vault or retain account state.
type ReadOnlySiteAdapter interface {
	// Dialect names the protocol this adapter implements.
	Dialect() string
	// Inspect establishes, read-only, whether the site offers check-in.
	Inspect(ctx context.Context, snapshot AccountSnapshot) (SiteCapability, error)
	// ValidateIdentity asks the site whose session this is. It is the only
	// accepted source of a remote user ID.
	ValidateIdentity(ctx context.Context, snapshot AccountSnapshot) (SiteIdentity, error)
	// ReadStatus reads today's state without submitting anything.
	ReadStatus(ctx context.Context, snapshot AccountSnapshot) (CheckInStatus, error)
}

// SiteAdapter adds the single write protocol to the read-only surface.
type SiteAdapter interface {
	ReadOnlySiteAdapter
	// Submit performs the single write this extension makes. It reports
	// what it dispatched even when it fails, so the caller can tell
	// "never sent" from "sent, result unknown".
	Submit(ctx context.Context, snapshot AccountSnapshot) (SubmitOutcome, error)
}
