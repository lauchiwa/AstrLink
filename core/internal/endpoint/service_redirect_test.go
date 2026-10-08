package endpoint

import (
	"context"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func redirectTestSubscription(id contract.ServiceID, kind contract.ServiceKind, models ...string) contract.Service {
	return contract.Service{
		ID: id, Name: string(id), Kind: kind, Enabled: true, Models: models,
		Capabilities: kind.SubscriptionProvider().Capabilities(),
		Subscription: &contract.SubscriptionConnection{
			Provider: kind.SubscriptionProvider(), Status: contract.SubscriptionStatusConnected,
			CredentialRef: "local://subscription/" + string(id),
		},
	}
}

func resolveServed(t *testing.T, services []contract.Service, model string) map[contract.ServiceID]string {
	t.Helper()
	resolver, err := NewStoreResolver(serviceReaderFunc(func(context.Context, storage.ServiceListOptions) (storage.ServicePage, error) {
		page := storage.ServicePage{}
		for _, service := range services {
			page.Items = append(page.Items, storage.ServiceRecord{Service: service})
		}
		return page, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	candidates, _, err := resolver.ResolveRankedCandidates(context.Background(), ResolveRequest{
		Protocol: contract.ProtocolAnthropicMessages, Model: model, Streaming: true, AllCandidates: true,
	})
	served := map[contract.ServiceID]string{}
	if err != nil {
		return served
	}
	for _, candidate := range candidates {
		served[candidate.Service.ID] = candidate.UpstreamModel
	}
	return served
}

func TestServiceRedirectsServeAModelOnlyThroughTheirOwnProvider(t *testing.T) {
	claude := redirectTestSubscription("service_claude", contract.ServiceKindClaudeSubscription, "claude-sonnet-4-6", "claude-haiku-4-5-20251001")
	copilot := redirectTestSubscription("service_copilot", contract.ServiceKindCopilotSubscription, "claude-sonnet-4.6", "claude-haiku-4.5", "claude-opus-4.6", "claude-fable-6.0")

	// Copilot's built-in rules serve Claude Code's ids under its own names; the
	// Claude subscription keeps serving them as they are, so either can fail over.
	served := resolveServed(t, []contract.Service{claude, copilot}, "claude-sonnet-4-6")
	if served["service_claude"] != "claude-sonnet-4-6" || served["service_copilot"] != "claude-sonnet-4.6" {
		t.Fatalf("served = %v", served)
	}
	if served := resolveServed(t, []contract.Service{claude, copilot}, "claude-haiku-4-5-20251001"); served["service_claude"] != "claude-haiku-4-5-20251001" || served["service_copilot"] != "claude-haiku-4.5" {
		t.Fatalf("dated id served = %v", served)
	}

	// A stored rule replaces the built-in one with the same source: turned off
	// it stops the mapping, retargeted it sends another model. Rules for newer
	// models sit beside the built-in ones.
	copilot.ModelRedirects = []contract.ModelRedirect{
		{From: "claude-opus-4-6", To: "claude-opus-4.6", Enabled: false},
		{From: "claude-sonnet-4-6", To: "claude-haiku-4.5", Enabled: true},
		{From: "claude-fable-6-0", To: "claude-fable-6.0", Enabled: true},
	}
	for model, want := range map[string]string{"claude-sonnet-4-6": "claude-haiku-4.5", "claude-fable-6-0": "claude-fable-6.0", "claude-haiku-4-5": "claude-haiku-4.5"} {
		if served := resolveServed(t, []contract.Service{copilot}, model); served["service_copilot"] != want {
			t.Fatalf("%s served = %v", model, served)
		}
	}
	// A disabled rule, or a built-in one whose target the provider does not
	// list, never applies.
	for _, model := range []string{"claude-opus-4-6", "claude-opus-5-5"} {
		if served := resolveServed(t, []contract.Service{copilot}, model); len(served) != 0 {
			t.Fatalf("%s served = %v", model, served)
		}
	}
	// Other kinds have no built-in rules.
	if served := resolveServed(t, []contract.Service{claude}, "claude-sonnet-4.6"); len(served) != 0 {
		t.Fatalf("Claude subscription served a dotted id: %v", served)
	}
}
