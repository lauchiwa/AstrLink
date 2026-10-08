package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// A sign-in rewrites the subscription service; the provider's own redirect
// settings must survive it.
func TestSubscriptionUpdatesKeepServiceRedirects(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "redirects.db"))
	defer store.Close()
	ctx := context.Background()
	now := time.Now().UTC()
	account := contract.SubscriptionAccount{
		ID: "service_copilot", DisplayName: "Copilot", Provider: contract.SubscriptionProviderGitHubCopilot,
		Status: contract.SubscriptionStatusDisconnected, Capabilities: contract.DefaultGitHubCopilotCapabilities(),
		CreatedAt: now, UpdatedAt: now,
	}
	if err := store.PutSubscriptionAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	record, err := store.GetService(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	service := record.Service
	service.ModelRedirects = []contract.ModelRedirect{{From: "claude-opus-4-6", To: "claude-opus-4.6", Enabled: true}}
	if _, err := store.UpdateService(ctx, service, storage.CredentialMutation{}, record.ETag); err != nil {
		t.Fatal(err)
	}
	account.Status = contract.SubscriptionStatusConnected
	account.CredentialRef = "local://subscription/service_copilot"
	if err := store.PutSubscriptionAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	saved, err := store.GetService(ctx, account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved.Service.ModelRedirects) != 1 || saved.Service.Subscription.Status != contract.SubscriptionStatusConnected {
		t.Fatalf("saved = %#v", saved.Service)
	}
}
