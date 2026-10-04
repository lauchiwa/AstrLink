// Package identitycapture owns the explicitly armed, service-scoped windows
// that turn one observed client request into an unconfirmed identity candidate.
//
// Arming is in-memory on purpose: consent does not survive a restart, a window
// expires on its own, and it closes as soon as it publishes one candidate. A
// window never changes what is forwarded, and recognizing a request's shape is
// a format check, never authentication of a client binary. Nothing here reads
// credentials, prompts, or request bodies beyond the bounded recognition check,
// and nothing is written to the published candidate except reusable identity
// fields.
package identitycapture

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

const (
	// DefaultWindow bounds one armed window. It is short because an operator
	// arms it immediately before running the client they want to copy.
	DefaultWindow = 10 * time.Minute
	// MaxRejections closes a window that keeps seeing requests which do not
	// match the selected client, instead of sampling until the window expires.
	MaxRejections = 64
)

var (
	// ErrNotArmed reports that no window is open for the service.
	ErrNotArmed = errors.New("identity capture is not armed")
	// ErrUnsupportedClient rejects a client this build cannot recognize.
	ErrUnsupportedClient = errors.New("identity capture client is unsupported")
)

// ProfileCreator is satisfied by the SQLite store. Creation always yields an
// unconfirmed candidate, so publishing cannot enable forwarding on its own.
type ProfileCreator interface {
	CreateIdentityProfile(context.Context, contract.IdentityProfile) (storage.IdentityProfileRecord, error)
}

type window struct {
	client     contract.IdentityClient
	armedAt    time.Time
	expiresAt  time.Time
	captured   contract.IdentityProfileID
	publishing bool
	rejections int
}

// Registry is safe for concurrent use. A nil *Registry answers "not armed" for
// every service so the inference path can call it unconditionally.
type Registry struct {
	profiles ProfileCreator
	now      func() time.Time
	newID    func() (contract.IdentityProfileID, error)

	mu      sync.Mutex
	windows map[contract.ServiceID]*window
}

func New(profiles ProfileCreator) (*Registry, error) {
	if profiles == nil {
		return nil, fmt.Errorf("identity profile storage is required")
	}
	return &Registry{
		profiles: profiles,
		now:      time.Now,
		newID:    randomProfileID,
		windows:  make(map[contract.ServiceID]*window),
	}, nil
}

// Arm opens or replaces the window for one service. Replacing discards an
// earlier window's published candidate reference, never the saved candidate.
func (registry *Registry) Arm(serviceID contract.ServiceID, client contract.IdentityClient, ttl time.Duration) (contract.IdentityCaptureStatus, error) {
	if registry == nil {
		return contract.IdentityCaptureStatus{}, ErrNotArmed
	}
	if err := serviceID.Validate(); err != nil {
		return contract.IdentityCaptureStatus{}, err
	}
	if !client.Valid() {
		return contract.IdentityCaptureStatus{}, ErrUnsupportedClient
	}
	if ttl <= 0 || ttl > DefaultWindow {
		ttl = DefaultWindow
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	now := registry.now().UTC()
	registry.windows[serviceID] = &window{client: client, armedAt: now, expiresAt: now.Add(ttl)}
	return registry.statusLocked(serviceID), nil
}

// Disarm closes the window and reports its final state, including a candidate
// it already published so the caller can still link to the pending review.
func (registry *Registry) Disarm(serviceID contract.ServiceID) contract.IdentityCaptureStatus {
	if registry == nil {
		return contract.IdentityCaptureStatus{ServiceID: serviceID}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	status := registry.statusLocked(serviceID)
	delete(registry.windows, serviceID)
	status.Armed = false
	status.ExpiresAt = nil
	return status
}

func (registry *Registry) Status(serviceID contract.ServiceID) contract.IdentityCaptureStatus {
	if registry == nil {
		return contract.IdentityCaptureStatus{ServiceID: serviceID}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.statusLocked(serviceID)
}

// Armed is the cheap gate for the inference path, so an unarmed service never
// pays for header cloning or a bounded body read.
func (registry *Registry) Armed(serviceID contract.ServiceID) (contract.IdentityClient, bool) {
	if registry == nil {
		return "", false
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	open := registry.openLocked(serviceID)
	if open == nil || open.publishing {
		return "", false
	}
	return open.client, true
}

// Observe extracts a candidate from ORIGINAL inbound data and publishes it.
// The caller must pass unmodified inbound headers, must already have passed
// local authentication and privacy admission, and must only call this when the
// request is actually being sent to serviceID. body is used solely for the
// bounded recognition check and is never stored.
//
// Observe never fails the request: it reports whether it published and returns
// a storage error only so the caller can log it.
func (registry *Registry) Observe(
	ctx context.Context,
	serviceID contract.ServiceID,
	original http.Header,
	body []byte,
) (contract.IdentityProfileID, bool, error) {
	if registry == nil {
		return "", false, nil
	}
	registry.mu.Lock()
	open := registry.openLocked(serviceID)
	if open == nil || open.publishing {
		registry.mu.Unlock()
		return "", false, nil
	}
	client := open.client
	registry.mu.Unlock()

	fingerprint, recognized := accountauth.CaptureIdentityFingerprint(client, original, body)
	if !recognized {
		registry.mu.Lock()
		defer registry.mu.Unlock()
		// Re-read the window: it may have been disarmed or replaced while the
		// extraction ran, and a replaced window must not inherit rejections.
		if current := registry.openLocked(serviceID); current == open && !current.publishing {
			current.rejections++
			if current.rejections >= MaxRejections {
				current.expiresAt = registry.now().UTC()
			}
		}
		return "", false, nil
	}

	id, err := registry.newID()
	if err != nil {
		return "", false, err
	}
	// Reserve this exact consent window before storage I/O. Extraction can run
	// concurrently, but only one observation may publish. A replaced, expired
	// or disarmed window cannot authorize a write that has not started yet.
	registry.mu.Lock()
	if current := registry.openLocked(serviceID); current != open || open.publishing {
		registry.mu.Unlock()
		return "", false, nil
	}
	open.publishing = true
	observed := registry.now().UTC()
	registry.mu.Unlock()

	record, err := registry.profiles.CreateIdentityProfile(ctx, contract.IdentityProfile{
		ID: id, ServiceID: serviceID, Client: client,
		Source: contract.IdentityProfileRequestCapture, Fingerprint: fingerprint,
		ObservedAt: &observed,
	})
	registry.mu.Lock()
	defer registry.mu.Unlock()
	open.publishing = false
	if err != nil {
		// Release the reservation so a still-open window can retry. A newer
		// window is independent and must not inherit this observation's state.
		return "", false, err
	}
	// Once storage starts, this observation may finish its reserved write, but
	// it must never close or overwrite consent granted to a newer window.
	open.captured = record.Profile.ID
	open.expiresAt = registry.now().UTC()
	return record.Profile.ID, true, nil
}

func (registry *Registry) openLocked(serviceID contract.ServiceID) *window {
	current, exists := registry.windows[serviceID]
	if !exists || current.captured != "" || !registry.now().UTC().Before(current.expiresAt) {
		return nil
	}
	return current
}

func (registry *Registry) statusLocked(serviceID contract.ServiceID) contract.IdentityCaptureStatus {
	status := contract.IdentityCaptureStatus{ServiceID: serviceID}
	current, exists := registry.windows[serviceID]
	if !exists {
		return status
	}
	armedAt, expiresAt := current.armedAt, current.expiresAt
	status.Client = current.client
	status.ArmedAt = &armedAt
	status.CapturedProfile = current.captured
	status.Rejected = current.rejections
	if registry.openLocked(serviceID) != nil {
		status.Armed = true
		status.ExpiresAt = &expiresAt
	}
	return status
}

func randomProfileID() (contract.IdentityProfileID, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return contract.IdentityProfileID("identity_" + hex.EncodeToString(random[:])), nil
}
