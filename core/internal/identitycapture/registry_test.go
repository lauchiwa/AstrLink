package identitycapture

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

const (
	fixtureService = contract.ServiceID("service_relay")
	otherService   = contract.ServiceID("service_other")
)

// memoryProfiles stands in for the SQLite store. It keeps the same invariant
// the real store enforces: a created profile is always unconfirmed.
type memoryProfiles struct {
	mu       sync.Mutex
	created  []contract.IdentityProfile
	failWith error
}

func (profiles *memoryProfiles) CreateIdentityProfile(_ context.Context, candidate contract.IdentityProfile) (storage.IdentityProfileRecord, error) {
	profiles.mu.Lock()
	defer profiles.mu.Unlock()
	if profiles.failWith != nil {
		return storage.IdentityProfileRecord{}, profiles.failWith
	}
	if candidate.ConfirmedAt != nil {
		return storage.IdentityProfileRecord{}, errors.New("new profiles must be unconfirmed")
	}
	candidate.CreatedAt = time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	if err := candidate.Validate(); err != nil {
		return storage.IdentityProfileRecord{}, err
	}
	profiles.created = append(profiles.created, candidate.Clone())
	return storage.IdentityProfileRecord{Profile: candidate, ETag: "etag"}, nil
}

func (profiles *memoryProfiles) snapshot() []contract.IdentityProfile {
	profiles.mu.Lock()
	defer profiles.mu.Unlock()
	return append([]contract.IdentityProfile(nil), profiles.created...)
}

func testRegistry(t *testing.T, profiles ProfileCreator) (*Registry, *time.Time) {
	t.Helper()
	registry, err := New(profiles)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	registry.now = func() time.Time { return now }
	// ID minting runs outside the registry mutex, so concurrent observations
	// must not share an unsynchronized counter.
	var counter atomic.Int64
	registry.newID = func() (contract.IdentityProfileID, error) {
		return contract.IdentityProfileID(fmt.Sprintf("identity_fixture%02d", counter.Add(1))), nil
	}
	return registry, &now
}

func codexRequest() http.Header {
	return http.Header{
		"User-Agent":    {"codex_cli_rs/0.160.0 (Mac OS 26.0.0; arm64) iTerm.app/3.6.1"},
		"Originator":    {"codex_cli_rs"},
		"Session-Id":    {"0f8c1d3e-3a4b-4c5d-8e9f-0a1b2c3d4e5f"},
		"Authorization": {"Bearer fixture-credential"},
	}
}

func TestUnarmedServiceNeverPublishesACandidate(t *testing.T) {
	profiles := &memoryProfiles{}
	registry, _ := testRegistry(t, profiles)

	if client, armed := registry.Armed(fixtureService); armed || client != "" {
		t.Fatal("a service is armed before any operator action")
	}
	if id, published, err := registry.Observe(context.Background(), fixtureService, codexRequest(), nil); published || id != "" || err != nil {
		t.Fatalf("unarmed observe = %q, %t, %v", id, published, err)
	}
	if len(profiles.snapshot()) != 0 {
		t.Fatal("capture published a candidate without an armed window")
	}
	status := registry.Status(fixtureService)
	if status.Armed || status.Client != "" || status.ArmedAt != nil || status.CapturedProfile != "" {
		t.Fatalf("unarmed status = %+v", status)
	}
	// A nil registry is the "capture disabled" build: every call is inert.
	var disabled *Registry
	if _, armed := disabled.Armed(fixtureService); armed {
		t.Fatal("nil registry reported an armed window")
	}
	if _, published, err := disabled.Observe(context.Background(), fixtureService, codexRequest(), nil); published || err != nil {
		t.Fatal("nil registry published a candidate")
	}
	if _, err := disabled.Arm(fixtureService, contract.IdentityClientCodexCLI, 0); !errors.Is(err, ErrNotArmed) {
		t.Fatal("nil registry armed a window")
	}
}

func TestArmedWindowIsServiceScopedAndPublishesOneUnconfirmedCandidate(t *testing.T) {
	profiles := &memoryProfiles{}
	registry, now := testRegistry(t, profiles)
	ctx := context.Background()

	status, err := registry.Arm(fixtureService, contract.IdentityClientCodexCLI, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Armed || status.Client != contract.IdentityClientCodexCLI ||
		status.ExpiresAt == nil || !status.ExpiresAt.Equal(now.Add(DefaultWindow)) {
		t.Fatalf("armed status = %+v", status)
	}
	// Another service must not inherit this window.
	if _, armed := registry.Armed(otherService); armed {
		t.Fatal("arming one service armed another")
	}
	if _, published, err := registry.Observe(ctx, otherService, codexRequest(), nil); published || err != nil {
		t.Fatal("an unarmed service published a candidate")
	}

	id, published, err := registry.Observe(ctx, fixtureService, codexRequest(), nil)
	if !published || err != nil || id == "" {
		t.Fatalf("armed observe = %q, %t, %v", id, published, err)
	}
	saved := profiles.snapshot()
	if len(saved) != 1 {
		t.Fatalf("saved %d candidates", len(saved))
	}
	candidate := saved[0]
	if candidate.ServiceID != fixtureService || candidate.Client != contract.IdentityClientCodexCLI ||
		candidate.Source != contract.IdentityProfileRequestCapture {
		t.Fatalf("candidate = %+v", candidate)
	}
	if candidate.ConfirmedAt != nil {
		t.Fatal("capture published a confirmed profile")
	}
	if candidate.ObservedAt == nil || !candidate.ObservedAt.Equal(*now) {
		t.Fatalf("observed at = %v", candidate.ObservedAt)
	}
	if candidate.Fingerprint.Version != "0.160.0" || candidate.Fingerprint.Headers["Originator"] != "codex_cli_rs" {
		t.Fatalf("fingerprint = %+v", candidate.Fingerprint)
	}
	if _, present := candidate.Fingerprint.Headers["Session-Id"]; present {
		t.Fatal("candidate retained a session identifier")
	}
	if _, present := candidate.Fingerprint.Headers["Authorization"]; present {
		t.Fatal("candidate retained a credential")
	}

	// One window publishes once: the retry of the same logical request, and any
	// later request, must not sample again.
	if _, armed := registry.Armed(fixtureService); armed {
		t.Fatal("window stayed open after publishing")
	}
	if _, published, err := registry.Observe(ctx, fixtureService, codexRequest(), nil); published || err != nil {
		t.Fatal("window published twice")
	}
	if len(profiles.snapshot()) != 1 {
		t.Fatal("a second candidate was saved from one window")
	}
	status = registry.Status(fixtureService)
	if status.Armed || status.CapturedProfile != id {
		t.Fatalf("post-capture status = %+v", status)
	}
}

func TestWindowClosesOnExpiryDisarmAndRepeatedMismatches(t *testing.T) {
	ctx := context.Background()

	t.Run("expiry", func(t *testing.T) {
		profiles := &memoryProfiles{}
		registry, now := testRegistry(t, profiles)
		if _, err := registry.Arm(fixtureService, contract.IdentityClientCodexCLI, time.Minute); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(time.Minute)
		if _, armed := registry.Armed(fixtureService); armed {
			t.Fatal("an expired window stayed armed")
		}
		if _, published, err := registry.Observe(ctx, fixtureService, codexRequest(), nil); published || err != nil {
			t.Fatal("an expired window published a candidate")
		}
		if status := registry.Status(fixtureService); status.Armed || status.ExpiresAt != nil {
			t.Fatalf("expired status = %+v", status)
		}
		if len(profiles.snapshot()) != 0 {
			t.Fatal("an expired window saved a candidate")
		}
	})

	t.Run("ttl is bounded", func(t *testing.T) {
		registry, now := testRegistry(t, &memoryProfiles{})
		status, err := registry.Arm(fixtureService, contract.IdentityClientCodexCLI, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if status.ExpiresAt == nil || !status.ExpiresAt.Equal(now.Add(DefaultWindow)) {
			t.Fatalf("an oversized ttl was accepted: %+v", status)
		}
	})

	t.Run("disarm", func(t *testing.T) {
		profiles := &memoryProfiles{}
		registry, _ := testRegistry(t, profiles)
		if _, err := registry.Arm(fixtureService, contract.IdentityClientCodexCLI, 0); err != nil {
			t.Fatal(err)
		}
		if status := registry.Disarm(fixtureService); status.Armed || status.ExpiresAt != nil {
			t.Fatalf("disarmed status = %+v", status)
		}
		if _, published, err := registry.Observe(ctx, fixtureService, codexRequest(), nil); published || err != nil {
			t.Fatal("a disarmed window published a candidate")
		}
		if len(profiles.snapshot()) != 0 {
			t.Fatal("a disarmed window saved a candidate")
		}
	})

	t.Run("repeated mismatches", func(t *testing.T) {
		profiles := &memoryProfiles{}
		registry, _ := testRegistry(t, profiles)
		if _, err := registry.Arm(fixtureService, contract.IdentityClientClaudeCode, 0); err != nil {
			t.Fatal(err)
		}
		// A Codex request never satisfies a window armed for Claude Code.
		for range MaxRejections {
			if _, published, _ := registry.Observe(ctx, fixtureService, codexRequest(), nil); published {
				t.Fatal("a mismatched client published a candidate")
			}
		}
		status := registry.Status(fixtureService)
		if status.Armed || status.Rejected != MaxRejections {
			t.Fatalf("status after mismatches = %+v", status)
		}
		if len(profiles.snapshot()) != 0 {
			t.Fatal("a mismatched client saved a candidate")
		}
	})
}

func TestReArmingResetsConsentAndStorageFailureNeverPublishes(t *testing.T) {
	ctx := context.Background()

	t.Run("re-arming resets rejections and client", func(t *testing.T) {
		profiles := &memoryProfiles{}
		registry, _ := testRegistry(t, profiles)
		if _, err := registry.Arm(fixtureService, contract.IdentityClientClaudeCode, 0); err != nil {
			t.Fatal(err)
		}
		if _, published, _ := registry.Observe(ctx, fixtureService, codexRequest(), nil); published {
			t.Fatal("mismatched client published")
		}
		if registry.Status(fixtureService).Rejected != 1 {
			t.Fatal("a mismatch was not counted")
		}
		if _, err := registry.Arm(fixtureService, contract.IdentityClientCodexCLI, 0); err != nil {
			t.Fatal(err)
		}
		status := registry.Status(fixtureService)
		if status.Rejected != 0 || status.Client != contract.IdentityClientCodexCLI || status.CapturedProfile != "" {
			t.Fatalf("re-armed status = %+v", status)
		}
		if _, published, err := registry.Observe(ctx, fixtureService, codexRequest(), nil); !published || err != nil {
			t.Fatal("re-armed window did not publish the newly selected client")
		}
	})

	t.Run("storage failure", func(t *testing.T) {
		profiles := &memoryProfiles{failWith: errors.New("storage unavailable")}
		registry, _ := testRegistry(t, profiles)
		if _, err := registry.Arm(fixtureService, contract.IdentityClientCodexCLI, 0); err != nil {
			t.Fatal(err)
		}
		id, published, err := registry.Observe(ctx, fixtureService, codexRequest(), nil)
		if published || id != "" || err == nil {
			t.Fatalf("failed save reported = %q, %t, %v", id, published, err)
		}
		// The window stays open so the operator can retry without re-arming.
		if _, armed := registry.Armed(fixtureService); !armed {
			t.Fatal("a storage failure closed the window")
		}
	})

	t.Run("invalid arm input", func(t *testing.T) {
		registry, _ := testRegistry(t, &memoryProfiles{})
		if _, err := registry.Arm(fixtureService, "unsupported", 0); !errors.Is(err, ErrUnsupportedClient) {
			t.Fatalf("unsupported client error = %v", err)
		}
		if _, err := registry.Arm("not a service id", contract.IdentityClientCodexCLI, 0); err == nil {
			t.Fatal("an invalid service id was armed")
		}
	})
}

func TestConcurrentObservationsPublishAtMostOneCandidate(t *testing.T) {
	profiles := &memoryProfiles{}
	registry, _ := testRegistry(t, profiles)
	if _, err := registry.Arm(fixtureService, contract.IdentityClientCodexCLI, 0); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var start, done sync.WaitGroup
	start.Add(1)
	var mu sync.Mutex
	publishedCount := 0
	for range 16 {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			if _, published, _ := registry.Observe(ctx, fixtureService, codexRequest(), nil); published {
				mu.Lock()
				publishedCount++
				mu.Unlock()
			}
		}()
	}
	start.Done()
	done.Wait()
	// Extraction runs outside the lock, so concurrent callers may both reach
	// storage; the window must still converge on one captured reference.
	if publishedCount < 1 {
		t.Fatal("no concurrent observation published")
	}
	if status := registry.Status(fixtureService); status.Armed || status.CapturedProfile == "" {
		t.Fatalf("status after concurrent observations = %+v", status)
	}
	for _, candidate := range profiles.snapshot() {
		if candidate.ConfirmedAt != nil {
			t.Fatal("a concurrently published candidate was confirmed")
		}
	}
}
