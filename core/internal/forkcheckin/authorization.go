package forkcheckin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"sync"
	"time"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

const (
	// AuthorizationTTL bounds one login window. It covers a person typing a
	// password and a second factor, and nothing longer: an unfinished session
	// is not a standing permission to store a session later.
	AuthorizationTTL = 15 * time.Minute
	// maxAuthorizations bounds the live sessions held in memory. It is well
	// above the desktop's concurrent login windows; past it the oldest idle
	// session is dropped rather than refusing the operator.
	maxAuthorizations = 16
	// authorizationVerifyBudget bounds the site read that establishes whose
	// session was captured. The adapter applies its own shorter read limit.
	authorizationVerifyBudget = 30 * time.Second
)

// Write routes for authorization. Begin keeps its ledger in memory; only a
// completion leaves a durable receipt.
const (
	WriteRouteAuthorizationBegin    = "authorizations.begin"
	WriteRouteAuthorizationComplete = "authorizations.complete"
)

var (
	// ErrAuthorizationNotFound: no such session, or it expired, or Core
	// restarted since it began. Unfinished sessions never survive a restart.
	ErrAuthorizationNotFound = errors.New("check-in authorization session is unknown or expired")
	// ErrAuthorizationCompleted: the session already stored a session under
	// another request id. A session completes once.
	ErrAuthorizationCompleted = errors.New("check-in authorization session was already completed")
	// ErrCredentialInvalid: the captured session is not a valid envelope for
	// this account's site and network. Nothing was sent anywhere.
	ErrCredentialInvalid = errors.New("captured check-in session is not valid for this account")
	// ErrSiteUnavailable: the site could not be asked whose session this is.
	// Nothing was stored; the same request may be retried.
	ErrSiteUnavailable = errors.New("check-in site could not verify the session")
)

// AuthorizationRequest is POST /authorizations.
type AuthorizationRequest struct {
	RequestID        string    `json:"request_id"`
	AccountID        AccountID `json:"account_id"`
	ExpectedRevision int64     `json:"expected_revision"`
}

// Fingerprint covers everything but the request id.
func (request AuthorizationRequest) Fingerprint() string {
	request.RequestID = ""
	return writeFingerprint(WriteRouteAuthorizationBegin, request.AccountID, request)
}

// Authorization is the 201 body. It names a pending login, never a session.
type Authorization struct {
	SessionID         string    `json:"session_id"`
	AccountID         AccountID `json:"account_id"`
	ExpiresAt         time.Time `json:"expires_at"`
	ConfigFingerprint string    `json:"config_fingerprint"`
	LoginURL          string    `json:"login_url"`
}

// AuthorizationCompletion is POST /authorizations/{id}/complete. Credential is
// the NetworkCredential envelope, base64 on the wire and write-only: no
// response, receipt or fingerprint carries it or anything derived from it.
type AuthorizationCompletion struct {
	RequestID        string  `json:"request_id"`
	Credential       []byte  `json:"credential"`
	ExpectedRevision *int64  `json:"expected_revision,omitempty"`
	ClaimedUserID    *string `json:"claimed_user_id,omitempty"`
}

// MarshalJSON refuses: the completion holds a captured session.
func (AuthorizationCompletion) MarshalJSON() ([]byte, error) {
	return nil, ErrCredentialUnavailable
}

func (AuthorizationCompletion) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[private check-in authorization completion]")
}

// Fingerprint identifies the request content for its receipt. It binds the
// session, so a request id reused for another session is a conflict. The
// captured session is excluded on purpose: a session is single-use, and no
// digest of it is stored outside its sealed row.
func (completion AuthorizationCompletion) Fingerprint(sessionID string) string {
	body := struct {
		SessionID        string  `json:"session_id"`
		ExpectedRevision *int64  `json:"expected_revision,omitempty"`
		ClaimedUserID    *string `json:"claimed_user_id,omitempty"`
	}{sessionID, completion.ExpectedRevision, completion.ClaimedUserID}
	return writeFingerprint(WriteRouteAuthorizationComplete, "", body)
}

// ValidateClaimedUserID checks the shape of an untrusted login-window hint.
func ValidateClaimedUserID(value string) error {
	if value == "" {
		return fmt.Errorf("claimed user id must not be empty")
	}
	return validateRemoteUserID(value)
}

// AuthorizationReceipt is the stored response for a completion request id.
type AuthorizationReceipt struct {
	Route       string
	Fingerprint string
	Status      int
	Body        []byte
}

// AuthorizationCommit is one verified session to store. Revision and
// ConfigFingerprint are what the session was bound to; the commit is refused
// if the account no longer has them.
type AuthorizationCommit struct {
	RequestID         string
	Fingerprint       string
	AccountID         AccountID
	Revision          int64
	ConfigFingerprint string
	RemoteUserID      string
	Credential        []byte
}

func (AuthorizationCommit) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "[private check-in authorization commit]")
}

// AuthorizationStore is the storage behind authorization. Reads are local;
// Complete stores the account change, the sealed session and the receipt in
// one transaction, so a failure leaves none of them.
type AuthorizationStore interface {
	// ReadForkCheckinAuthorizationAccount returns the account and whether a
	// job holds a live lease on it.
	ReadForkCheckinAuthorizationAccount(context.Context, AccountID) (Account, bool, error)
	// LookupForkCheckinAuthorizationReceipt returns a stored receipt under
	// any route, or storage ErrNotFound.
	LookupForkCheckinAuthorizationReceipt(context.Context, string) (AuthorizationReceipt, error)
	// CompleteForkCheckinAuthorization stores a verified session. A replay of
	// the same request returns the original receipt with Replayed set.
	CompleteForkCheckinAuthorization(context.Context, AuthorizationCommit) (AccountWriteResult, error)
}

type authorizationSession struct {
	id                string
	requestID         string
	digest            string
	accountID         AccountID
	revision          int64
	configFingerprint string
	loginURL          string
	expiresAt         time.Time
	completed         bool
	holders           int
	// turn serializes completions of one session, so a retry waits for the
	// attempt still in flight instead of racing it.
	turn chan struct{}
}

func (session *authorizationSession) public() Authorization {
	return Authorization{
		SessionID: session.id, AccountID: session.accountID, ExpiresAt: session.expiresAt,
		ConfigFingerprint: session.configFingerprint, LoginURL: session.loginURL,
	}
}

// authorizationRegistry holds live sessions in memory, so none outlives the
// module. Disabling the extension or restarting Core expires every unfinished
// session; completed receipts are durable in storage instead.
type authorizationRegistry struct {
	now       func() time.Time
	mu        sync.Mutex
	sessions  map[string]*authorizationSession
	byRequest map[string]*authorizationSession
}

func newAuthorizationRegistry(now func() time.Time) *authorizationRegistry {
	if now == nil {
		now = time.Now
	}
	return &authorizationRegistry{now: now, sessions: make(map[string]*authorizationSession), byRequest: make(map[string]*authorizationSession)}
}

// purgeLocked drops expired sessions nobody is completing. It requires mu.
func (registry *authorizationRegistry) purgeLocked(now time.Time) {
	for id, session := range registry.sessions {
		if session.holders == 0 && !now.Before(session.expiresAt) {
			registry.dropLocked(id, session)
		}
	}
}

func (registry *authorizationRegistry) dropLocked(id string, session *authorizationSession) {
	delete(registry.sessions, id)
	if registry.byRequest[session.requestID] == session {
		delete(registry.byRequest, session.requestID)
	}
}

// replay returns the session an earlier begin with this request id created.
func (registry *authorizationRegistry) replay(requestID, digest string) (Authorization, bool, error) {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	registry.purgeLocked(registry.now())
	session := registry.byRequest[requestID]
	if session == nil {
		return Authorization{}, false, nil
	}
	if session.digest != digest {
		return Authorization{}, false, ErrRequestIDReused
	}
	return session.public(), true, nil
}

// begun reports whether a live session was begun under this request id.
func (registry *authorizationRegistry) begun(requestID string) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.byRequest[requestID] != nil
}

// begin stores a new session unless the request id began one meanwhile.
func (registry *authorizationRegistry) begin(requestID, digest string, account Account) (Authorization, error) {
	id, err := newAuthorizationSessionID()
	if err != nil {
		return Authorization{}, err
	}
	login, err := NormalizeDashboardURL(account.DashboardBaseURL)
	if err != nil {
		return Authorization{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidRecord, err)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := registry.now()
	registry.purgeLocked(now)
	if existing := registry.byRequest[requestID]; existing != nil {
		if existing.digest != digest {
			return Authorization{}, ErrRequestIDReused
		}
		return existing.public(), nil
	}
	for len(registry.sessions) >= maxAuthorizations {
		var victim *authorizationSession
		for _, candidate := range registry.sessions {
			if victim == nil || evictsBefore(candidate, victim) {
				victim = candidate
			}
		}
		registry.dropLocked(victim.id, victim)
	}
	session := &authorizationSession{
		id: id, requestID: requestID, digest: digest, accountID: account.ID, revision: account.Revision,
		configFingerprint: account.ConfigFingerprint(), loginURL: login, expiresAt: now.Add(AuthorizationTTL),
		turn: make(chan struct{}, 1),
	}
	registry.sessions[id] = session
	registry.byRequest[requestID] = session
	return session.public(), nil
}

// evictsBefore prefers sessions that are idle, then already completed, then
// closest to expiry. A session in mid-completion is detached, not cancelled:
// its commit is still fenced by the account revision.
func evictsBefore(candidate, current *authorizationSession) bool {
	if (candidate.holders == 0) != (current.holders == 0) {
		return candidate.holders == 0
	}
	if candidate.completed != current.completed {
		return candidate.completed
	}
	return candidate.expiresAt.Before(current.expiresAt)
}

// authorizationTurn is one completion's exclusive hold on its session.
type authorizationTurn struct {
	registry *authorizationRegistry
	session  *authorizationSession
}

// acquire waits for the session's turn. A session that expired before its
// turn began, or that already completed, is refused.
func (registry *authorizationRegistry) acquire(ctx context.Context, id string) (*authorizationTurn, error) {
	registry.mu.Lock()
	registry.purgeLocked(registry.now())
	session := registry.sessions[id]
	if session == nil {
		registry.mu.Unlock()
		return nil, ErrAuthorizationNotFound
	}
	session.holders++
	registry.mu.Unlock()
	turn := &authorizationTurn{registry: registry, session: session}
	select {
	case session.turn <- struct{}{}:
	case <-ctx.Done():
		registry.mu.Lock()
		session.holders--
		registry.mu.Unlock()
		return nil, ctx.Err()
	}
	registry.mu.Lock()
	completed, expired := session.completed, !registry.now().Before(session.expiresAt)
	registry.mu.Unlock()
	switch {
	case completed:
		turn.release(false)
		return nil, ErrAuthorizationCompleted
	case expired:
		turn.release(false)
		return nil, ErrAuthorizationNotFound
	}
	return turn, nil
}

func (turn *authorizationTurn) release(completed bool) {
	turn.registry.mu.Lock()
	turn.session.holders--
	if completed {
		turn.session.completed = true
	}
	turn.registry.mu.Unlock()
	<-turn.session.turn
}

func newAuthorizationSessionID() (string, error) {
	var value [24]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("create check-in authorization session id: %w", err)
	}
	return "auth_" + hex.EncodeToString(value[:]), nil
}

// authorizer runs authorization against one live module.
type authorizer struct {
	registry       *authorizationRegistry
	store          AuthorizationStore
	verifyIdentity func(context.Context, AccountSnapshot) (SiteIdentity, error)
}

func (a *authorizer) begin(ctx context.Context, request AuthorizationRequest) (Authorization, error) {
	digest := request.Fingerprint()
	if existing, replayed, err := a.registry.replay(request.RequestID, digest); err != nil || replayed {
		return existing, err
	}
	// A request id names one action across every route, including a
	// completion that was stored before a restart.
	if _, err := a.store.LookupForkCheckinAuthorizationReceipt(ctx, request.RequestID); err == nil {
		return Authorization{}, ErrRequestIDReused
	} else if !errors.Is(err, storagecontract.ErrNotFound) {
		return Authorization{}, err
	}
	account, _, err := a.store.ReadForkCheckinAuthorizationAccount(ctx, request.AccountID)
	if err != nil {
		return Authorization{}, err
	}
	if account.Revision != request.ExpectedRevision {
		return Authorization{}, ErrRevisionChanged
	}
	return a.registry.begin(request.RequestID, digest, account)
}

func (a *authorizer) complete(ctx context.Context, sessionID string, completion AuthorizationCompletion) (AccountWriteResult, error) {
	digest := completion.Fingerprint(sessionID)
	// The receipt answers first: a completion whose response was lost is
	// replayed even after a restart dropped the session itself.
	if result, ok, err := a.replay(ctx, completion.RequestID, digest); ok || err != nil {
		return result, err
	}
	if a.registry.begun(completion.RequestID) {
		return AccountWriteResult{}, ErrRequestIDReused
	}
	turn, err := a.registry.acquire(ctx, sessionID)
	if err != nil {
		// The first attempt can commit while this retry waits for its turn
		// (or between the initial receipt lookup and session acquisition).
		// Prefer that durable receipt, including its content-conflict check,
		// over a session that has since completed or expired.
		if errors.Is(err, ErrAuthorizationCompleted) || errors.Is(err, ErrAuthorizationNotFound) {
			if result, ok, replayErr := a.replay(ctx, completion.RequestID, digest); ok || replayErr != nil {
				return result, replayErr
			}
		}
		return AccountWriteResult{}, err
	}
	// An attempt that held the turn may have just stored this very request.
	if result, ok, err := a.replay(ctx, completion.RequestID, digest); ok || err != nil {
		turn.release(false)
		return result, err
	}
	result, err := a.verifyAndCommit(ctx, turn.session, completion, digest)
	turn.release(err == nil)
	return result, err
}

func (a *authorizer) replay(ctx context.Context, requestID, digest string) (AccountWriteResult, bool, error) {
	stored, err := a.store.LookupForkCheckinAuthorizationReceipt(ctx, requestID)
	switch {
	case errors.Is(err, storagecontract.ErrNotFound):
		return AccountWriteResult{}, false, nil
	case err != nil:
		return AccountWriteResult{}, false, err
	case stored.Route != WriteRouteAuthorizationComplete || stored.Fingerprint != digest:
		return AccountWriteResult{}, false, ErrRequestIDReused
	}
	return AccountWriteResult{Status: stored.Status, Body: stored.Body, Replayed: true}, true, nil
}

// verifyAndCommit asks the site whose session was captured, outside any
// transaction, then stores it under the session's fences. Only the account's
// own dashboard origin and network are ever contacted.
func (a *authorizer) verifyAndCommit(ctx context.Context, session *authorizationSession, completion AuthorizationCompletion, digest string) (AccountWriteResult, error) {
	account, busy, err := a.store.ReadForkCheckinAuthorizationAccount(ctx, session.accountID)
	if err != nil {
		return AccountWriteResult{}, err
	}
	if account.Revision != session.revision || account.ConfigFingerprint() != session.configFingerprint {
		return AccountWriteResult{}, ErrRevisionChanged
	}
	if completion.ExpectedRevision != nil && *completion.ExpectedRevision != session.revision {
		return AccountWriteResult{}, ErrRevisionChanged
	}
	if busy {
		// Verifying would replace the running job's network client.
		return AccountWriteResult{}, ErrAccountBusy
	}
	expected := account.RemoteUserID
	if completion.ClaimedUserID != nil {
		// A login window's hint is never trusted: it only tells the site
		// which user to confirm, and an identified account keeps its user.
		if expected != "" && expected != *completion.ClaimedUserID {
			return AccountWriteResult{}, ErrIdentityMismatch
		}
		expected = *completion.ClaimedUserID
	}
	candidate := verificationAccount(account, expected)
	if err := validateCapturedCredential(candidate, completion.Credential); err != nil {
		return AccountWriteResult{}, err
	}
	verify, cancel := context.WithTimeout(ctx, authorizationVerifyBudget)
	identity, err := a.verifyIdentity(verify, AccountSnapshot{Account: candidate, Credential: completion.Credential})
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return AccountWriteResult{}, ctx.Err()
		}
		return AccountWriteResult{}, classifyAuthorizationFailure(err)
	}
	if identity.RemoteUserID == "" || validateRemoteUserID(identity.RemoteUserID) != nil {
		return AccountWriteResult{}, ErrUnsupported
	}
	if expected != "" && identity.RemoteUserID != expected {
		return AccountWriteResult{}, ErrIdentityMismatch
	}
	return a.store.CompleteForkCheckinAuthorization(ctx, AuthorizationCommit{
		RequestID: completion.RequestID, Fingerprint: digest, AccountID: account.ID,
		Revision: session.revision, ConfigFingerprint: session.configFingerprint,
		RemoteUserID: identity.RemoteUserID, Credential: completion.Credential,
	})
}

// verificationAccount is the account as it would be stored once the session
// is confirmed. It is only handed to the adapter, never persisted.
func verificationAccount(account Account, remoteUserID string) Account {
	candidate := account
	candidate.RemoteUserID = remoteUserID
	candidate.Automatic = false
	if remoteUserID != "" {
		candidate.State = AccountStateConnected
	} else {
		candidate.State = AccountStateDraft
	}
	return candidate
}

// validateCapturedCredential checks the envelope against the account's own
// site and network before anything leaves the machine.
func validateCapturedCredential(account Account, credential []byte) error {
	if len(credential) == 0 || len(credential) > MaxCredentialBytes {
		return ErrCredentialInvalid
	}
	normalized, err := NormalizeDashboardURL(account.DashboardBaseURL)
	if err != nil {
		return fmt.Errorf("%w: %v", storagecontract.ErrInvalidRecord, err)
	}
	base, _ := url.Parse(normalized)
	if _, err := decodeNetworkCredential(credential, base, account.Network.Mode); err != nil {
		return ErrCredentialInvalid
	}
	return nil
}

// classifyAuthorizationFailure keeps only the typed reason. No site text,
// address or session detail survives into the API error.
func classifyAuthorizationFailure(err error) error {
	switch {
	case errors.Is(err, ErrIdentityMismatch):
		return ErrIdentityMismatch
	case errors.Is(err, ErrAuthRequired), errors.Is(err, ErrPermissionDenied):
		return ErrAuthRequired
	case errors.Is(err, ErrManualRequired):
		return ErrManualRequired
	case errors.Is(err, ErrUnsupported):
		return ErrUnsupported
	case errors.Is(err, ErrCredentialUnavailable):
		return ErrCredentialInvalid
	case errors.Is(err, ErrRateLimited):
		return ErrRateLimited
	case errors.Is(err, ErrRevisionChanged):
		return ErrRevisionChanged
	default:
		return ErrSiteUnavailable
	}
}
