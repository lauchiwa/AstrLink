package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// openForkCheckinStore returns a store whose extension schema already exists.
// EnsureForkCheckinSchema is explicit in production too, so calling it here
// mirrors enabling the extension rather than working around Open.
func openForkCheckinStore(t *testing.T) *Store {
	t.Helper()
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	t.Cleanup(func() { _ = store.Close() })
	if err := store.EnsureForkCheckinSchema(context.Background()); err != nil {
		t.Fatalf("ensure fork check-in schema: %v", err)
	}
	return store
}

func forkCheckinDraft(id forkcheckin.AccountID, dashboard string) forkcheckin.Account {
	return forkcheckin.Account{
		ID:               id,
		DashboardBaseURL: dashboard,
		State:            forkcheckin.AccountStateDraft,
		Revision:         1,
		Network:          forkcheckin.Network{Mode: forkcheckin.NetworkModeDirect},
		TimeZone:         "Asia/Shanghai",
	}
}

// createForkCheckinService adds a real service so binding is exercised against
// stored service documents rather than a stub.
func createForkCheckinService(t *testing.T, store *Store, id contract.ServiceID) storagecontract.ServiceRecord {
	t.Helper()
	service := contract.ServiceFromEndpoint(testEndpoint(id))
	record, err := store.CreateService(context.Background(), service, storagecontract.CredentialMutation{
		Present: true,
		Secret:  []byte("service-credential-" + string(id)),
	})
	if err != nil {
		t.Fatalf("create service %q: %v", id, err)
	}
	return record
}

func TestForkCheckinAccountsAllowManySitesAndManyServicesPerAccount(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinService(t, store, "service_alpha")
	createForkCheckinService(t, store, "service_beta")

	// Two accounts on the same site are allowed while neither is identified.
	first, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_one", "https://relay.example/"))
	if err != nil {
		t.Fatalf("create first account: %v", err)
	}
	if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_two", "https://relay.example/")); err != nil {
		t.Fatalf("second account on the same site was refused: %v", err)
	}

	// Distinct users on one site stay distinct accounts.
	if _, err := store.ConnectForkCheckinAccount(ctx, "acct_one", "user-1", first.Revision); err != nil {
		t.Fatalf("connect first account: %v", err)
	}
	if _, err := store.ConnectForkCheckinAccount(ctx, "acct_two", "user-2", 1); err != nil {
		t.Fatalf("second user on the same site was refused: %v", err)
	}

	// One account funds several services.
	connected, err := store.GetForkCheckinAccount(ctx, "acct_one")
	if err != nil {
		t.Fatal(err)
	}
	connected.BoundServices = []contract.ServiceID{"service_beta", "service_alpha", "service_beta"}
	bound, err := store.UpdateForkCheckinAccount(ctx, connected, connected.Revision)
	if err != nil {
		t.Fatalf("bind two services: %v", err)
	}
	if len(bound.BoundServices) != 2 ||
		bound.BoundServices[0] != "service_alpha" || bound.BoundServices[1] != "service_beta" {
		t.Fatalf("bound services = %v, want deduplicated and sorted", bound.BoundServices)
	}
	reloaded, err := store.GetForkCheckinAccount(ctx, "acct_one")
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.BoundServices) != 2 {
		t.Fatalf("stored bindings = %v", reloaded.BoundServices)
	}

	// The same service may be funded by more than one account.
	other, err := store.GetForkCheckinAccount(ctx, "acct_two")
	if err != nil {
		t.Fatal(err)
	}
	other.BoundServices = []contract.ServiceID{"service_alpha"}
	if _, err := store.UpdateForkCheckinAccount(ctx, other, other.Revision); err != nil {
		t.Fatalf("second account binding a shared service was refused: %v", err)
	}
}

func TestForkCheckinAccountRefusesADuplicateSiteAndUser(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_one", "https://relay.example/")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_two", "https://relay.example/")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConnectForkCheckinAccount(ctx, "acct_one", "user-1", 1); err != nil {
		t.Fatal(err)
	}

	// The same site and user twice would produce two check-ins for one
	// upstream account, so it is refused rather than merged.
	_, err := store.ConnectForkCheckinAccount(ctx, "acct_two", "user-1", 1)
	if !errors.Is(err, storagecontract.ErrConflict) {
		t.Fatalf("duplicate identity error = %v, want ErrConflict", err)
	}
	second, err := store.GetForkCheckinAccount(ctx, "acct_two")
	if err != nil {
		t.Fatal(err)
	}
	if second.State != forkcheckin.AccountStateDraft || second.RemoteUserID != "" || second.Revision != 1 {
		t.Fatalf("refused connect changed the account: %#v", second)
	}

	// A differently spelled address is the same site, so it collides too.
	if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_three", "https://relay.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ConnectForkCheckinAccount(ctx, "acct_three", "user-1", 1); !errors.Is(err, storagecontract.ErrConflict) {
		t.Fatalf("duplicate identity through another spelling = %v, want ErrConflict", err)
	}
}

func TestForkCheckinAccountUpdateRequiresTheCurrentRevision(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	created, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_cas", "https://relay.example/"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Revision != 1 {
		t.Fatalf("created revision = %d, want 1", created.Revision)
	}

	first := created
	first.TimeZone = "UTC"
	updated, err := store.UpdateForkCheckinAccount(ctx, first, created.Revision)
	if err != nil {
		t.Fatalf("first update: %v", err)
	}
	if updated.Revision != 2 {
		t.Fatalf("revision after update = %d, want 2", updated.Revision)
	}

	// A second writer holding the old revision loses instead of overwriting.
	stale := created
	stale.TimeZone = "Europe/Berlin"
	_, err = store.UpdateForkCheckinAccount(ctx, stale, created.Revision)
	if !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("stale update error = %v, want ErrPrecondition", err)
	}
	current, err := store.GetForkCheckinAccount(ctx, "acct_cas")
	if err != nil {
		t.Fatal(err)
	}
	if current.TimeZone != "UTC" || current.Revision != 2 {
		t.Fatalf("stale update was applied: %#v", current)
	}

	if _, err := store.UpdateForkCheckinAccount(ctx, current, 0); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("missing expected revision error = %v, want ErrInvalidArgument", err)
	}
	missing := forkCheckinDraft("acct_absent", "https://relay.example/")
	if _, err := store.UpdateForkCheckinAccount(ctx, missing, 1); !errors.Is(err, storagecontract.ErrNotFound) {
		t.Fatalf("update of a missing account = %v, want ErrNotFound", err)
	}
}

func TestForkCheckinRedirectingEditInvalidatesTheStoredSession(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_session", "https://relay.example/")); err != nil {
		t.Fatal(err)
	}
	connected, err := store.ConnectForkCheckinAccount(ctx, "acct_session", "user-9", 1)
	if err != nil {
		t.Fatal(err)
	}
	// Stand in for the sealed session D03 will write.
	sealed, err := store.keys.sealColumn(forkCheckinCredentials, "acct_session", []byte(`{"session":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, fmt.Sprintf(
		`INSERT INTO %s (account_id, sealed_value, updated_at) VALUES (?, ?, ?)`, forkCheckinCredentials),
		"acct_session", sealed, time.Now().UTC().Format(time.RFC3339Nano))

	// Binding another service does not move the session, so it is kept.
	createForkCheckinService(t, store, "service_alpha")
	withService := connected
	withService.BoundServices = []contract.ServiceID{"service_alpha"}
	kept, err := store.UpdateForkCheckinAccount(ctx, withService, connected.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if kept.State != forkcheckin.AccountStateConnected || kept.RemoteUserID != "user-9" {
		t.Fatalf("binding a service dropped the session: %#v", kept)
	}
	if countRows(t, store, forkCheckinCredentials) != 1 {
		t.Fatal("binding a service removed the stored session")
	}

	// Changing the egress sends that session somewhere else, so it is dropped.
	redirected := kept
	redirected.Network = forkcheckin.Network{
		Mode: forkcheckin.NetworkModeCustom, ProxyURL: "socks5://127.0.0.1:1080",
	}
	after, err := store.UpdateForkCheckinAccount(ctx, redirected, kept.Revision)
	if err != nil {
		t.Fatalf("redirecting update: %v", err)
	}
	if after.State != forkcheckin.AccountStateDraft || after.RemoteUserID != "" || after.Automatic {
		t.Fatalf("redirected account kept its identity: %#v", after)
	}
	if countRows(t, store, forkCheckinCredentials) != 0 {
		t.Fatal("redirected account kept its stored session")
	}
}

func TestForkCheckinAccountDeleteIsConditionalAndIdempotent(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinService(t, store, "service_alpha")
	account := forkCheckinDraft("acct_delete", "https://relay.example/")
	account.BoundServices = []contract.ServiceID{"service_alpha"}
	created, err := store.CreateForkCheckinAccount(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := store.keys.sealColumn(forkCheckinCredentials, "acct_delete", []byte(`{"session":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, store, fmt.Sprintf(
		`INSERT INTO %s (account_id, sealed_value, updated_at) VALUES (?, ?, ?)`, forkCheckinCredentials),
		"acct_delete", sealed, time.Now().UTC().Format(time.RFC3339Nano))

	if err := store.DeleteForkCheckinAccount(ctx, "acct_delete", created.Revision+5); !errors.Is(err, storagecontract.ErrPrecondition) {
		t.Fatalf("stale delete error = %v, want ErrPrecondition", err)
	}
	if countRows(t, store, forkCheckinAccounts) != 1 {
		t.Fatal("refused delete removed the account")
	}

	if err := store.DeleteForkCheckinAccount(ctx, "acct_delete", created.Revision); err != nil {
		t.Fatalf("delete: %v", err)
	}
	// Bindings and the sealed session go with the account through the
	// extension's own cascade.
	if countRows(t, store, forkCheckinBindings) != 0 || countRows(t, store, forkCheckinCredentials) != 0 {
		t.Fatal("delete left bindings or a session behind")
	}
	// A retried delete must not report a conflict for completed work.
	if err := store.DeleteForkCheckinAccount(ctx, "acct_delete", created.Revision); err != nil {
		t.Fatalf("repeated delete: %v", err)
	}
}

func TestForkCheckinBindingsNeverWriteTheBoundService(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	before := createForkCheckinService(t, store, "service_alpha")

	account := forkCheckinDraft("acct_bind", "https://relay.example/")
	account.BoundServices = []contract.ServiceID{"service_alpha"}
	if _, err := store.CreateForkCheckinAccount(ctx, account); err != nil {
		t.Fatal(err)
	}

	after, err := store.GetService(ctx, "service_alpha")
	if err != nil {
		t.Fatal(err)
	}
	if after.ETag != before.ETag {
		t.Fatalf("binding changed the service ETag: %q -> %q", before.ETag, after.ETag)
	}
	if after.Service.Enabled != before.Service.Enabled {
		t.Fatal("binding changed whether the service is enabled")
	}
	credential, err := store.Get(ctx, "local://service/service_alpha")
	if err != nil || string(credential) != "service-credential-service_alpha" {
		t.Fatalf("binding changed the service credential: %q, %v", credential, err)
	}
	clear(credential)

	// The projection offered for binding carries no concurrency token and no
	// credential, because this extension never writes a service.
	services, err := store.ListForkCheckinBindableServices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0].ID != "service_alpha" {
		t.Fatalf("bindable services = %#v", services)
	}
	if services[0].Name != before.Service.Name || services[0].Kind != before.Service.Kind {
		t.Fatalf("bindable projection lost identifying fields: %#v", services[0])
	}
}

func TestForkCheckinBindingGoesStaleWhenItsServiceIsRemovedOrReused(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	record := createForkCheckinService(t, store, "service_alpha")

	account := forkCheckinDraft("acct_stale", "https://relay.example/")
	account.BoundServices = []contract.ServiceID{"service_alpha"}
	if _, err := store.CreateForkCheckinAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	bindings, err := store.ResolveForkCheckinBindings(ctx, "acct_stale")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || !bindings[0].Live {
		t.Fatalf("binding to an existing service = %#v", bindings)
	}

	// Removing the service must not remove the binding: the operator's choice
	// is preserved and reported as stale.
	if err := store.DeleteService(ctx, "service_alpha", record.ETag); err != nil {
		t.Fatal(err)
	}
	bindings, err = store.ResolveForkCheckinBindings(ctx, "acct_stale")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Live {
		t.Fatalf("binding after the service was deleted = %#v", bindings)
	}
	stored, err := store.GetForkCheckinAccount(ctx, "acct_stale")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.BoundServices) != 1 {
		t.Fatalf("deleting a service changed stored bindings: %v", stored.BoundServices)
	}

	// Recreating a service under the reused id is a different service. Adopting
	// it would quietly fund something the operator never chose.
	createForkCheckinService(t, store, "service_alpha")
	bindings, err = store.ResolveForkCheckinBindings(ctx, "acct_stale")
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != 1 || bindings[0].Live {
		t.Fatalf("binding adopted a service created after it: %#v", bindings)
	}
}

func TestForkCheckinAccountPagingIsStableAndBounded(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	for index := range 5 {
		id := forkcheckin.AccountID(fmt.Sprintf("acct_page_%d", index))
		if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft(id, "https://relay.example/")); err != nil {
			t.Fatal(err)
		}
	}

	var seen []forkcheckin.AccountID
	cursor := ""
	for range 10 {
		page, err := store.ListForkCheckinAccounts(ctx, forkcheckin.ListOptions{Limit: 2, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, account := range page.Items {
			seen = append(seen, account.ID)
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("paged accounts = %v, want 5 distinct ids", seen)
	}
	for index := 1; index < len(seen); index++ {
		if seen[index-1] >= seen[index] {
			t.Fatalf("paging repeated or reordered ids: %v", seen)
		}
	}

	// A limit above the shared maximum is rejected, not clamped: a caller must
	// not believe it received everything.
	if _, err := store.ListForkCheckinAccounts(ctx, forkcheckin.ListOptions{
		Limit: forkcheckin.MaxPageSize + 1,
	}); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("oversized limit error = %v, want ErrInvalidArgument", err)
	}
	if _, err := store.ListForkCheckinAccounts(ctx, forkcheckin.ListOptions{
		Cursor: "not-base64!",
	}); !errors.Is(err, storagecontract.ErrInvalidCursor) {
		t.Fatalf("malformed cursor error = %v, want ErrInvalidCursor", err)
	}
}

func TestForkCheckinAccountWriteRollsBackCompletely(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	createForkCheckinService(t, store, "service_alpha")

	// A bound service list that exceeds the model's bound is refused, and the
	// account row must not survive the refusal.
	tooMany := forkCheckinDraft("acct_rollback", "https://relay.example/")
	for index := range forkcheckin.MaxBoundServices + 1 {
		tooMany.BoundServices = append(tooMany.BoundServices, contract.ServiceID(fmt.Sprintf("service_%02d", index)))
	}
	if _, err := store.CreateForkCheckinAccount(ctx, tooMany); !errors.Is(err, storagecontract.ErrInvalidRecord) {
		t.Fatalf("oversized binding list error = %v, want ErrInvalidRecord", err)
	}
	if countRows(t, store, forkCheckinAccounts) != 0 || countRows(t, store, forkCheckinBindings) != 0 {
		t.Fatal("refused create left rows behind")
	}

	// An update refused by validation leaves the stored account untouched,
	// including its bindings.
	account := forkCheckinDraft("acct_rollback", "https://relay.example/")
	account.BoundServices = []contract.ServiceID{"service_alpha"}
	created, err := store.CreateForkCheckinAccount(ctx, account)
	if err != nil {
		t.Fatal(err)
	}
	invalid := created
	invalid.TimeZone = "Local"
	invalid.BoundServices = nil
	if _, err := store.UpdateForkCheckinAccount(ctx, invalid, created.Revision); !errors.Is(err, storagecontract.ErrInvalidRecord) {
		t.Fatalf("invalid time zone error = %v, want ErrInvalidRecord", err)
	}
	reloaded, err := store.GetForkCheckinAccount(ctx, "acct_rollback")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Revision != created.Revision || reloaded.TimeZone != created.TimeZone {
		t.Fatalf("refused update changed the account: %#v", reloaded)
	}
	if len(reloaded.BoundServices) != 1 {
		t.Fatalf("refused update changed bindings: %v", reloaded.BoundServices)
	}
}

func TestForkCheckinAccountRejectsInvalidIdentifiers(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	if _, err := store.GetForkCheckinAccount(ctx, "No"); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("invalid id error = %v, want ErrInvalidArgument", err)
	}
	if _, err := store.GetForkCheckinAccount(ctx, "acct_missing"); !errors.Is(err, storagecontract.ErrNotFound) {
		t.Fatalf("missing account error = %v, want ErrNotFound", err)
	}
	// http to a remote host would send a session in the clear.
	plaintext := forkCheckinDraft("acct_plain", "http://relay.example/")
	if _, err := store.CreateForkCheckinAccount(ctx, plaintext); !errors.Is(err, storagecontract.ErrInvalidRecord) {
		t.Fatalf("plaintext remote error = %v, want ErrInvalidRecord", err)
	}
}
