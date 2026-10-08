package forkcheckin

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRewardStaysUnknownRatherThanGuessingAUnit(t *testing.T) {
	if err := (Reward{}).Validate(); err != nil {
		t.Fatalf("unknown reward: %v", err)
	}
	if err := (Reward{Known: true, Quota: 500000, Unit: "quota"}).Validate(); err != nil {
		t.Fatalf("known reward: %v", err)
	}
	// A zero award the site actually reported is still known.
	if err := (Reward{Known: true, Unit: "quota"}).Validate(); err != nil {
		t.Fatalf("known zero reward: %v", err)
	}
	for name, reward := range map[string]Reward{
		"unknown with a value": {Quota: 1},
		"unknown with a unit":  {Unit: "quota"},
		"known without a unit": {Known: true, Quota: 1},
		"negative quota":       {Known: true, Quota: -1, Unit: "quota"},
	} {
		if err := reward.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestSubmitOutcomeSeparatesDispatchFromResult(t *testing.T) {
	for name, outcome := range map[string]SubmitOutcome{
		"never dispatched":    {},
		"dispatched, no read": {Dispatched: true},
		"already checked in": {
			Dispatched: true, ResponseRead: true, AlreadyCheckedIn: true, SiteDate: "2026-01-01",
		},
		"succeeded with a reward": {
			Dispatched: true, ResponseRead: true, Succeeded: true,
			Reward: Reward{Known: true, Quota: 500000, Unit: "quota"}, SiteDate: "2026-01-01",
		},
		"succeeded with an unknown reward": {
			Dispatched: true, ResponseRead: true, Succeeded: true,
		},
		"read but refused": {Dispatched: true, ResponseRead: true},
	} {
		if err := outcome.Validate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	// A success that was never sent, or never read, would let the scheduler
	// record a check-in that no site ever confirmed.
	for name, outcome := range map[string]SubmitOutcome{
		"success without dispatch":   {ResponseRead: true, Succeeded: true},
		"repeat without dispatch":    {ResponseRead: true, AlreadyCheckedIn: true},
		"response without dispatch":  {ResponseRead: true},
		"success without a response": {Dispatched: true, Succeeded: true},
		"repeat without a response":  {Dispatched: true, AlreadyCheckedIn: true},
		"both success and repeat": {
			Dispatched: true, ResponseRead: true, Succeeded: true, AlreadyCheckedIn: true,
		},
		"reward without success": {
			Dispatched: true, ResponseRead: true,
			Reward: Reward{Known: true, Quota: 1, Unit: "quota"},
		},
		"invalid reward": {
			Dispatched: true, ResponseRead: true, Succeeded: true,
			Reward: Reward{Known: true, Quota: -1, Unit: "quota"},
		},
	} {
		if err := outcome.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

// countingAdapter records which methods ran, so the read-only split can be
// asserted instead of assumed.
type countingAdapter struct {
	submits int
	reads   int
}

func (adapter *countingAdapter) Dialect() string { return "test" }

func (adapter *countingAdapter) Inspect(context.Context, AccountSnapshot) (SiteCapability, error) {
	adapter.reads++
	return SiteCapability{Supported: true, Dialect: "test"}, nil
}

func (adapter *countingAdapter) ValidateIdentity(context.Context, AccountSnapshot) (SiteIdentity, error) {
	adapter.reads++
	return SiteIdentity{RemoteUserID: "7", DisplayHint: "u***r"}, nil
}

func (adapter *countingAdapter) ReadStatus(context.Context, AccountSnapshot) (CheckInStatus, error) {
	adapter.reads++
	return CheckInStatus{CheckedInToday: true, SiteDate: "2026-01-01"}, nil
}

func (adapter *countingAdapter) Submit(context.Context, AccountSnapshot) (SubmitOutcome, error) {
	adapter.submits++
	return SubmitOutcome{Dispatched: true, ResponseRead: true, Succeeded: true}, nil
}

func TestAdapterDiscoveryAndStatusNeverSubmit(t *testing.T) {
	ctx := context.Background()
	adapter := &countingAdapter{}
	var site SiteAdapter = adapter
	snapshot := AccountSnapshot{Account: connectedAccount(), Credential: []byte("session")}
	if err := snapshot.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := site.Inspect(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := site.ValidateIdentity(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	status, err := site.ReadStatus(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if !status.CheckedInToday || adapter.reads != 3 {
		t.Fatalf("status = %+v, reads = %d", status, adapter.reads)
	}
	if adapter.submits != 0 {
		t.Fatalf("read-only inspection submitted %d times", adapter.submits)
	}
	outcome, err := site.Submit(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := outcome.Validate(); err != nil || adapter.submits != 1 {
		t.Fatalf("outcome = %+v, submits = %d, err = %v", outcome, adapter.submits, err)
	}
}

func TestTypedFailuresAreDistinctSentinels(t *testing.T) {
	sentinels := map[string]error{
		"credential":  ErrCredentialUnavailable,
		"auth":        ErrAuthRequired,
		"manual":      ErrManualRequired,
		"unsupported": ErrUnsupported,
		"rate":        ErrRateLimited,
		"uncertain":   ErrUncertain,
		"revision":    ErrRevisionChanged,
		"identity":    ErrIdentityMismatch,
	}
	for name, sentinel := range sentinels {
		wrapped := errors.New("site said something")
		if errors.Is(wrapped, sentinel) {
			t.Fatalf("%s matches an unrelated error", name)
		}
		for otherName, other := range sentinels {
			if otherName == name {
				continue
			}
			if errors.Is(sentinel, other) {
				t.Fatalf("%s and %s are not distinct", name, otherName)
			}
		}
	}
}

func TestBindableServiceCarriesNoCredentialOrConcurrencyToken(t *testing.T) {
	encoded, err := json.Marshal(BindableService{
		ID: "service_one", Name: "Relay", Kind: "new_api",
		BaseURL: "https://relay.example/v1", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := []string{"id", "name", "kind", "base_url", "enabled"}
	if len(fields) != len(want) {
		t.Fatalf("published fields = %v, want exactly %v", fields, want)
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			t.Fatalf("missing published field %q", key)
		}
	}
	// No ETag: this extension never writes a service, so it has no use for
	// its concurrency token.
	for _, forbidden := range []string{"etag", "credential", "proxy", "models"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("published JSON mentions %q: %s", forbidden, encoded)
		}
	}
}

// vaultRecorder is the in-memory stand-in used to pin the vault contract:
// opaque bytes in, opaque bytes out, idempotent delete.
type vaultRecorder struct {
	stored map[AccountID][]byte
}

func (vault *vaultRecorder) Get(_ context.Context, id AccountID) ([]byte, error) {
	credential, ok := vault.stored[id]
	if !ok {
		return nil, ErrCredentialUnavailable
	}
	return credential, nil
}

func (vault *vaultRecorder) Put(_ context.Context, id AccountID, credential []byte) error {
	if vault.stored == nil {
		vault.stored = map[AccountID][]byte{}
	}
	vault.stored[id] = credential
	return nil
}

func (vault *vaultRecorder) Delete(_ context.Context, id AccountID) error {
	delete(vault.stored, id)
	return nil
}

func TestVaultContractIsOpaqueAndDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	var vault Vault = &vaultRecorder{}
	if _, err := vault.Get(ctx, "account_one"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("missing session error = %v, want ErrCredentialUnavailable", err)
	}
	session := []byte(`{"cookie":"session=abc"}`)
	if err := vault.Put(ctx, "account_one", session); err != nil {
		t.Fatal(err)
	}
	got, err := vault.Get(ctx, "account_one")
	if err != nil || string(got) != string(session) {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if err := vault.Delete(ctx, "account_one"); err != nil {
		t.Fatal(err)
	}
	// Account removal must not fail because the session was already gone.
	if err := vault.Delete(ctx, "account_one"); err != nil {
		t.Fatalf("second Delete = %v", err)
	}
	if _, err := vault.Get(ctx, "account_one"); !errors.Is(err, ErrCredentialUnavailable) {
		t.Fatalf("Get after Delete = %v", err)
	}
}
