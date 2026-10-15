package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/controlapi"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
)

// Fork: identity profiles are device-local and are not part of a config sync
// package, but a service document that names one is. A peer that receives such
// a document has no matching profile, so every consumer must refuse to forward
// instead of falling back to a default identity.
func TestSyncedServiceWithUnknownIdentityProfileFailsClosed(t *testing.T) {
	upstreamURL, received := identityUpstream(t)
	core, _ := openIdentityCore(t, t.TempDir())
	operator, inference, _ := identityCoreClients(t, core)
	call := operator.control

	policyPath := controlapi.PoliciesPath + "/" + string(contract.DefaultPrivacyPolicyID)
	policy := call("GET", policyPath, "", "")
	identityStatus(t, policy, 200)
	identityStatus(t, call("PATCH", policyPath, `{"enabled":false}`, policy.Header().Get("ETag")), 200)
	created := call("POST", controlapi.ServicesPath, fmt.Sprintf(`{"name":"synced fixture","kind":"openai_compatible","enabled":true,"models":["fixture-model"],"http":{"base_url":%q,"auth":{"scheme":"bearer"},"credential":{"secret":"fixture-provider-secret"}},"capabilities":[{"protocol":"openai.responses","mode":"native","streaming":true},{"protocol":"openai.models","mode":"native","streaming":false}]}`, upstreamURL+"/v1"), "")
	identityStatus(t, created, 201)
	var service contract.Service
	identityDecode(t, created, &service)
	tokenResponse := call("POST", controlapi.AccessTokensPath, `{"name":"synced fixture client"}`, "")
	identityStatus(t, tokenResponse, 201)
	var token struct {
		Value string `json:"access_token"`
	}
	identityDecode(t, tokenResponse, &token)
	inference.token = token.Value

	// Replay what a merge writes: the peer's service document, naming a
	// profile id that exists only on the peer, followed by the local
	// credential rebind.
	store, ok := core.gateway.IdentityProfiles.(*sqlite.Store)
	if !ok {
		t.Fatalf("identity profiles reader is %T, want the persistent store", core.gateway.IdentityProfiles)
	}
	const peerProfile = contract.IdentityProfileID("identity_peer_only_fixture")
	if err := peerProfile.Validate(); err != nil {
		t.Fatal(err)
	}
	synced := service
	connection := *service.HTTP
	connection.IdentityProfileID = peerProfile
	synced.HTTP = &connection
	document, err := json.Marshal(synced)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.SyncWriteEntity(ctx, "service", string(service.ID), document); err != nil {
		t.Fatalf("sync write refused the peer document: %v", err)
	}
	if err := store.SyncBindCredentialRefs(ctx); err != nil {
		t.Fatal(err)
	}
	path := controlapi.ServicesPath + "/" + string(service.ID)
	stored := call("GET", path, "", "")
	identityStatus(t, stored, 200)
	var reread contract.Service
	identityDecode(t, stored, &reread)
	if reread.HTTP == nil || reread.HTTP.IdentityProfileID != peerProfile || reread.HTTP.CredentialRef == "" {
		t.Fatalf("synced service did not keep the peer profile with a local credential: %+v", reread.HTTP)
	}

	header := http.Header{"Content-Type": {"application/json"}, "User-Agent": {"ordinary-fixture-client/1.0"}}
	if response := inference.send("POST", "/v1/responses", identityInferenceBody, header); response.Code == 200 {
		t.Fatalf("inference forwarded without the named profile: %s", response.Body.String())
	}
	tested := call("POST", path+"/test", `{"protocol":"openai.responses","model":"fixture-model"}`, "")
	identityStatus(t, tested, 200)
	var result contract.ServiceTestResult
	identityDecode(t, tested, &result)
	// The tester reports the shared configuration error, the same one an
	// unusable request rule produces, rather than a provider failure.
	if result.OK || result.StatusCode != 0 || result.ErrorCode != "invalid_configuration" {
		t.Fatalf("connection test did not fail closed on the missing profile: %+v", result)
	}
	if probe := call("POST", path+"/probe-models", `{"protocol":"openai.models"}`, ""); probe.Code == 200 {
		t.Fatalf("model discovery succeeded without the named profile: %s", probe.Body.String())
	}
	if listing := inference.send("GET", "/v1/models", "", header); strings.Contains(listing.Body.String(), "fixture-model") {
		t.Fatalf("aggregate discovery listed a service whose profile is missing: %s", listing.Body.String())
	}

	// Every call above returned before this point, so any upstream request it
	// made is already buffered.
	select {
	case request := <-received:
		t.Fatalf("a request reached the upstream without its identity profile: %v", request.header)
	default:
	}
}
