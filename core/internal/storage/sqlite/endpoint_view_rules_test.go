package sqlite

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/endpoint"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

// ruleConnection is the compatibility configuration an operator saves: service
// defaults, one per-model rule, and a pinned identity profile.
func ruleConnection() contract.HTTPConnection {
	return contract.HTTPConnection{
		ExtraHeaders: map[string]string{"originator": "codex_exec"},
		ModelRules: []contract.ModelRule{{
			Match:   "upstream-model",
			Headers: map[string]string{"user-agent": "codex-tui/0.156.0"},
		}},
		IdentityProfileID: "identity_pinned",
	}
}

func sameRules(connection *contract.HTTPConnection, want contract.HTTPConnection) bool {
	return connection != nil &&
		reflect.DeepEqual(connection.ExtraHeaders, want.ExtraHeaders) &&
		reflect.DeepEqual(connection.ModelRules, want.ModelRules) &&
		connection.IdentityProfileID == want.IdentityProfileID
}

// The legacy endpoint view cannot express request rules. Reading through it and
// writing back must not silently clear a provider's compatibility
// configuration: that would restore the client identity the upstream rejects
// while reporting a successful save.
func TestLegacyEndpointViewPreservesRequestRules(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "rules.db")
	store := openTestStore(t, path)
	// Capture the variable, not the initial receiver: the restart below replaces
	// the store, and Windows cannot remove its database until it is closed.
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})

	want := ruleConnection()
	service := contract.ServiceFromEndpoint(testEndpoint("service_rules"))
	service.HTTP.ExtraHeaders = want.ExtraHeaders
	service.HTTP.ModelRules = want.ModelRules
	service.HTTP.IdentityProfileID = want.IdentityProfileID
	record, err := store.CreateService(ctx, service, storage.CredentialMutation{})
	if err != nil {
		t.Fatal(err)
	}

	// Every legacy read path drops the configuration from its view, by design.
	legacyReads := map[string]func() (contract.Endpoint, error){
		"GetEndpoint": func() (contract.Endpoint, error) {
			view, err := store.GetEndpoint(ctx, service.ID)
			return view.Endpoint, err
		},
		"ListEndpoints": func() (contract.Endpoint, error) {
			page, err := store.ListEndpoints(ctx, storage.EndpointListOptions{})
			if err != nil {
				return contract.Endpoint{}, err
			}
			if len(page.Items) != 1 {
				t.Fatalf("ListEndpoints items = %d, want 1", len(page.Items))
			}
			return page.Items[0].Endpoint, nil
		},
		"EndpointView": func() (contract.Endpoint, error) {
			return record.Service.EndpointView()
		},
	}
	for name, read := range legacyReads {
		view, err := read()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if view.ID != service.ID || view.BaseURL != service.HTTP.BaseURL {
			t.Fatalf("%s returned a different service: %+v", name, view)
		}
	}

	// A legacy write carrying only the fields the view can express keeps the
	// stored rules, and the canonical read still returns them verbatim.
	legacy, err := record.Service.EndpointView()
	if err != nil {
		t.Fatal(err)
	}
	legacy.Name = "renamed by compatibility API"
	updated, err := store.UpdateEndpoint(ctx, legacy, storage.CredentialMutation{}, record.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Endpoint.Name != legacy.Name {
		t.Fatalf("legacy rename did not apply: %q", updated.Endpoint.Name)
	}
	current, err := store.GetService(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRules(current.Service.HTTP, want) {
		t.Fatalf("legacy write cleared request rules: %+v", current.Service.HTTP)
	}

	// Restarting proves the configuration is persisted, not just cached.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = openTestStore(t, path)
	reopened, err := store.GetService(ctx, service.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !sameRules(reopened.Service.HTTP, want) {
		t.Fatalf("request rules did not survive restart: %+v", reopened.Service.HTTP)
	}
}

// Routing must reach forwarding with the rules intact. The resolver accepts
// either a canonical service reader or a legacy endpoint reader, and the legacy
// adapter rebuilds each candidate through the lossy view. The production store
// therefore has to be picked up as the canonical reader, or every candidate
// would arrive at forwarding as "no configuration".
func TestResolvedCandidateFromProductionStoreCarriesRequestRules(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "resolver.db"))
	defer store.Close()

	want := ruleConnection()
	service := contract.ServiceFromEndpoint(testEndpoint("service_rules"))
	service.HTTP.ExtraHeaders = want.ExtraHeaders
	service.HTTP.ModelRules = want.ModelRules
	service.HTTP.IdentityProfileID = want.IdentityProfileID
	if _, err := store.CreateService(ctx, service, storage.CredentialMutation{}); err != nil {
		t.Fatal(err)
	}

	resolver, err := endpoint.NewStoreResolver(store)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := resolver.ResolveCandidates(ctx, endpoint.ResolveRequest{
		Protocol: contract.ProtocolOpenAIResponses, Model: "upstream-model", Streaming: true,
	})
	if err != nil || len(candidates) != 1 {
		t.Fatalf("ResolveCandidates = %d candidates, %v", len(candidates), err)
	}
	if !sameRules(candidates[0].CanonicalService().HTTP, want) {
		t.Fatalf("routing dropped request rules: %+v", candidates[0].CanonicalService().HTTP)
	}

	// The legacy adapter is what would lose them, so the conversion it relies on
	// must stay demonstrably lossy rather than quietly appearing to work.
	view, err := candidates[0].CanonicalService().EndpointView()
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt := contract.ServiceFromEndpoint(view); sameRules(rebuilt.HTTP, want) {
		t.Fatal("the legacy view now carries request rules; drop the preservation workaround")
	}
}
