package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/storage/migrate"
)

func newStoredIdentityCandidate(t *testing.T, id contract.IdentityProfileID, serviceID contract.ServiceID) contract.IdentityProfile {
	t.Helper()
	fingerprint, err := accountauth.BuiltinIdentityFingerprint(contract.IdentityClientCodexCLI)
	if err != nil {
		t.Fatal(err)
	}
	return contract.IdentityProfile{ID: id, ServiceID: serviceID, Client: contract.IdentityClientCodexCLI, Source: contract.IdentityProfileBuiltin, Fingerprint: fingerprint}
}

func TestIdentityProfileUpgradeRestartAndServiceCascade(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "profiles.db")
	db, err := sql.Open(driverName, sqliteFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	all := migrate.DefaultMigrations()
	previous := all[:len(all)-1]
	old, err := migrate.New(migrate.SQLDatabase{DB: db}, previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Up(ctx); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	store := openTestStore(t, path)
	service, err := store.CreateService(ctx, pathTestService("service_profiles"), storage.CredentialMutation{})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := store.CreateIdentityProfile(ctx, newStoredIdentityCandidate(t, "identity_one", service.Service.ID))
	if err != nil || candidate.Profile.ConfirmedAt != nil {
		t.Fatalf("create candidate: %v", err)
	}
	confirmed, err := store.ConfirmIdentityProfile(ctx, service.Service.ID, candidate.Profile.ID, candidate.ETag)
	if err != nil || confirmed.Profile.ConfirmedAt == nil || confirmed.ETag == candidate.ETag {
		t.Fatalf("confirm candidate: %v", err)
	}
	if !reflect.DeepEqual(candidate.Profile.Fingerprint, confirmed.Profile.Fingerprint) {
		t.Fatal("confirmation rewrote the fingerprint")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	defer store.Close()
	got, err := store.GetIdentityProfile(ctx, service.Service.ID, candidate.Profile.ID)
	if err != nil || !reflect.DeepEqual(got, confirmed) {
		t.Fatalf("restart changed the pinned snapshot: %v", err)
	}
	legacyReader, err := migrate.New(migrate.SQLDatabase{DB: store.db}, previous)
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyReader.RequireCurrent(ctx); !errors.Is(err, migrate.ErrDatabaseNewer) {
		t.Fatalf("older core accepted profile database: %v", err)
	}
	if err := store.DeleteService(ctx, service.Service.ID, service.ETag); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetIdentityProfile(ctx, service.Service.ID, candidate.Profile.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("deleted service retained profile: %v", err)
	}
}

func TestIdentityProfileCASIsolationAndDiscard(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "profiles.db"))
	defer store.Close()
	for _, id := range []contract.ServiceID{"service_a", "service_b"} {
		if _, err := store.CreateService(ctx, pathTestService(id), storage.CredentialMutation{}); err != nil {
			t.Fatal(err)
		}
	}
	candidate := newStoredIdentityCandidate(t, "identity_one", "service_a")
	record, err := store.CreateIdentityProfile(ctx, candidate)
	if err != nil {
		t.Fatal(err)
	}
	candidate.Fingerprint.Headers["Originator"] = "changed"
	if _, err := store.CreateIdentityProfile(ctx, record.Profile); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("candidate ID can overwrite an existing fingerprint: %v", err)
	}
	if _, err := store.GetIdentityProfile(ctx, "service_b", record.Profile.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-service profile read: %v", err)
	}
	if _, err := store.ConfirmIdentityProfile(ctx, "service_b", record.Profile.ID, record.ETag); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-service profile confirm: %v", err)
	}
	if err := store.DiscardIdentityProfile(ctx, "service_b", record.Profile.ID, record.ETag); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-service profile discard: %v", err)
	}
	for _, etag := range []string{"", "stale"} {
		want := storage.ErrPrecondition
		if etag == "" {
			want = storage.ErrInvalidArgument
		}
		if _, err := store.ConfirmIdentityProfile(ctx, "service_a", record.Profile.ID, etag); !errors.Is(err, want) {
			t.Fatalf("invalid confirmation precondition: %v", err)
		}
	}
	const concurrent = 12
	results := make(chan error, concurrent)
	var workers sync.WaitGroup
	for range concurrent {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, err := store.ConfirmIdentityProfile(ctx, "service_a", record.Profile.ID, record.ETag)
			results <- err
		}()
	}
	workers.Wait()
	close(results)
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, storage.ErrPrecondition) {
			t.Fatalf("CAS returned an unexpected error: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("confirmation winners = %d, want 1", wins)
	}
	confirmed, err := store.GetIdentityProfile(ctx, "service_a", record.Profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	if confirmed.Profile.Fingerprint.Headers["Originator"] == "changed" {
		t.Fatal("store retained caller-owned mutable map")
	}
	repeat, err := store.ConfirmIdentityProfile(ctx, "service_a", record.Profile.ID, confirmed.ETag)
	if err != nil || !reflect.DeepEqual(repeat, confirmed) {
		t.Fatalf("idempotent confirmation changed the snapshot: %v", err)
	}
	if err := store.DiscardIdentityProfile(ctx, "service_a", record.Profile.ID, confirmed.ETag); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("confirmed snapshot can be discarded: %v", err)
	}
	other, err := store.CreateIdentityProfile(ctx, newStoredIdentityCandidate(t, "identity_discard", "service_a"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DiscardIdentityProfile(ctx, "service_a", other.Profile.ID, "stale"); !errors.Is(err, storage.ErrPrecondition) {
		t.Fatalf("stale discard: %v", err)
	}
	if err := store.DiscardIdentityProfile(ctx, "service_a", other.Profile.ID, other.ETag); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetIdentityProfile(ctx, "service_a", other.Profile.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("discard did not delete candidate: %v", err)
	}
}

func TestIdentityProfileListAndValidation(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "profiles.db"))
	defer store.Close()
	if _, err := store.CreateService(ctx, pathTestService("service_a"), storage.CredentialMutation{}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []contract.IdentityProfileID{"identity_c", "identity_a", "identity_b"} {
		if _, err := store.CreateIdentityProfile(ctx, newStoredIdentityCandidate(t, id, "service_a")); err != nil {
			t.Fatal(err)
		}
	}
	page, err := store.ListIdentityProfiles(ctx, "service_a", storage.IdentityProfileListOptions{Limit: 2})
	if err != nil || len(page.Items) != 2 || page.NextCursor == "" || page.Items[0].Profile.ID != "identity_a" {
		t.Fatalf("first page: %+v %v", page, err)
	}
	last, err := store.ListIdentityProfiles(ctx, "service_a", storage.IdentityProfileListOptions{Limit: 2, Cursor: page.NextCursor})
	if err != nil || len(last.Items) != 1 || last.NextCursor != "" || last.Items[0].Profile.ID != "identity_c" {
		t.Fatalf("last page: %+v %v", last, err)
	}
	for _, options := range []storage.IdentityProfileListOptions{{Limit: -1}, {Limit: 201}, {Cursor: "!"}} {
		if _, err := store.ListIdentityProfiles(ctx, "service_a", options); err == nil {
			t.Fatal("invalid list options accepted")
		}
	}
	missing := newStoredIdentityCandidate(t, "identity_missing", "service_missing")
	if _, err := store.CreateIdentityProfile(ctx, missing); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing parent accepted: %v", err)
	}
	candidate := newStoredIdentityCandidate(t, "identity_invalid", "service_a")
	now := time.Now().UTC()
	candidate.ConfirmedAt = &now
	if _, err := store.CreateIdentityProfile(ctx, candidate); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("pre-confirmed candidate accepted: %v", err)
	}
	candidate.ConfirmedAt = nil
	candidate.Fingerprint.Headers["Authorization"] = "fixture-only"
	if _, err := store.CreateIdentityProfile(ctx, candidate); !errors.Is(err, storage.ErrInvalidArgument) {
		t.Fatalf("credential-bearing fingerprint accepted: %v", err)
	}
	// A damaged document must not be returned merely because its SQL scope fits.
	for _, mutation := range []func(map[string]any){
		func(v map[string]any) { v["service_id"] = "service_other" },
		func(v map[string]any) { v["unknown"] = "fixture" },
		func(v map[string]any) {
			v["fingerprint"].(map[string]any)["headers"] = map[string]string{"Authorization": "fixture-only"}
		},
	} {
		encoded, _ := json.Marshal(page.Items[0].Profile)
		var document map[string]any
		if err := json.Unmarshal(encoded, &document); err != nil {
			t.Fatal(err)
		}
		mutation(document)
		encoded, _ = json.Marshal(document)
		if _, err := store.db.Exec(`UPDATE service_identity_profiles SET document_json = ? WHERE id = 'identity_a'`, string(encoded)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.GetIdentityProfile(ctx, "service_a", "identity_a"); !errors.Is(err, storage.ErrInvalidRecord) {
			t.Fatalf("corrupted document escaped validation: %v", err)
		}
	}
}
