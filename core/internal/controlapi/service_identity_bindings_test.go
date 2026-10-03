package controlapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	"github.com/QuantumNous/astrlink/core/internal/storage"
)

func bindingProfile(t *testing.T, handler *Handler, serviceID contract.ServiceID, client contract.IdentityClient, confirm bool) contract.IdentityProfile {
	t.Helper()
	created := serviceRequestForTest(t, handler, http.MethodPost, ServicesPath+"/"+string(serviceID)+"/identity-profiles", "application/json",
		fmt.Sprintf(`{"client":%q,"source":"builtin"}`, client), "")
	if created.Code != http.StatusCreated {
		t.Fatalf("create profile: %d %s", created.Code, created.Body.String())
	}
	response := created
	if confirm {
		response = serviceRequestForTest(t, handler, http.MethodPost, created.Header().Get("Location")+"/confirm", "application/json", "{}", created.Header().Get("ETag"))
		if response.Code != http.StatusOK {
			t.Fatalf("confirm profile: %d %s", response.Code, response.Body.String())
		}
	}
	var profile contract.IdentityProfile
	decode(t, response, &profile)
	return profile
}

func TestServiceIdentityBindingsRejectUnusableSnapshotsWithoutWriting(t *testing.T) {
	store, handler := newServiceHandler(t, "service_a", "service_b")
	service := createServiceForTest(t, handler, profileServiceBody)
	other := createServiceForTest(t, handler, profileServiceBody)
	candidate := bindingProfile(t, handler, service.ID, contract.IdentityClientCodexCLI, false)
	foreign := bindingProfile(t, handler, other.ID, contract.IdentityClientCodexCLI, true)
	path := ServicesPath + "/" + string(service.ID)
	before, err := store.GetService(context.Background(), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		id   contract.IdentityProfileID
	}{
		{"missing", "identity_missing"},
		{"unconfirmed", candidate.ID},
		{"other service", foreign.ID},
	} {
		for _, binding := range []struct{ name, field, configuration string }{
			{"default", "http.identity_profile_id", `"identity_profile_id":%q`},
			{"rule", "http.model_rules[0].identity_profile", `"model_rules":[{"match":"*","identity_profile":%q}]`},
			{"disabled rule", "http.model_rules[0].identity_profile", `"model_rules":[{"match":"*","enabled":false,"identity_profile":%q}]`},
		} {
			t.Run(test.name+"/"+binding.name, func(t *testing.T) {
				body := `{"http":{"credential":{"secret":"fake-rejected-key"},` + fmt.Sprintf(binding.configuration, test.id) + `}}`
				response := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", body, before.ETag)
				if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), binding.field) {
					t.Fatalf("invalid binding: %d %s", response.Code, response.Body.String())
				}
				after, err := store.GetService(context.Background(), service.ID)
				if err != nil || after.ETag != before.ETag || after.Service.HTTP.CredentialRef != "" {
					t.Fatalf("rejected patch changed service: %+v, %v", after, err)
				}
				secret, err := store.Get(context.Background(), secretstore.Ref("local://service/"+string(service.ID)))
				defer clear(secret)
				if err == nil || len(secret) != 0 {
					t.Fatal("rejected patch wrote a credential")
				}
			})
		}
	}
}

func TestServiceCreateCannotBindAnotherServicesProfile(t *testing.T) {
	store, handler := newServiceHandler(t, "service_owner", "service_default", "service_rule")
	owner := createServiceForTest(t, handler, profileServiceBody)
	profile := bindingProfile(t, handler, owner.ID, contract.IdentityClientCodexCLI, true)
	for _, binding := range []string{
		fmt.Sprintf(`"identity_profile_id":%q,`, profile.ID),
		fmt.Sprintf(`"model_rules":[{"match":"*","identity_profile":%q}],`, profile.ID),
	} {
		body := strings.Replace(profileServiceBody, `"base_url":`, binding+`"credential":{"secret":"fake-rejected-key"},"base_url":`, 1)
		response := serviceRequestForTest(t, handler, http.MethodPost, ServicesPath, "application/json", body, "")
		if response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("create with foreign binding: %d %s", response.Code, response.Body.String())
		}
	}
	page, err := store.ListServices(context.Background(), storage.ServiceListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].Service.ID != owner.ID {
		t.Fatalf("rejected create wrote services: %+v, %v", page, err)
	}
}

func TestServiceIdentityBindingsRoundTripAndClear(t *testing.T) {
	_, handler := newServiceHandler(t, "service_bound")
	service := createServiceForTest(t, handler, profileServiceBody)
	path := ServicesPath + "/" + string(service.ID)
	read := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
	for _, client := range []contract.IdentityClient{contract.IdentityClientCodexCLI, contract.IdentityClientClaudeCode, contract.IdentityClientGrokCLI} {
		profile := bindingProfile(t, handler, service.ID, client, true)
		body := fmt.Sprintf(`{"http":{"identity_profile_id":%q,"model_rules":[{"match":"*","identity_profile":%q,"enabled":false}]}}`, profile.ID, profile.ID)
		patched := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", body, read.Header().Get("ETag"))
		if patched.Code != http.StatusOK {
			t.Fatalf("bind %s: %d %s", client, patched.Code, patched.Body.String())
		}
		read = serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", `{"name":"renamed"}`, patched.Header().Get("ETag"))
		var saved contract.Service
		decode(t, read, &saved)
		if read.Code != http.StatusOK || saved.HTTP.IdentityProfileID != profile.ID || len(saved.HTTP.ModelRules) != 1 || saved.HTTP.ModelRules[0].IdentityProfile != profile.ID || saved.HTTP.ModelRules[0].Active() {
			t.Fatalf("omitted bindings changed: %d %s", read.Code, read.Body.String())
		}
	}
	cleared := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", `{"http":{"identity_profile_id":null,"model_rules":[]}}`, read.Header().Get("ETag"))
	var saved contract.Service
	decode(t, cleared, &saved)
	if cleared.Code != http.StatusOK || saved.HTTP.IdentityProfileID != "" || len(saved.HTTP.ModelRules) != 0 {
		t.Fatalf("clear bindings: %d %s", cleared.Code, cleared.Body.String())
	}
	stale := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", `{"http":{"identity_profile_id":"identity_missing"}}`, read.Header().Get("ETag"))
	if stale.Code != http.StatusPreconditionFailed {
		t.Fatalf("binding validation bypassed service ETag: %d %s", stale.Code, stale.Body.String())
	}
}

func TestServiceIdentityBindingRejectsAuthenticationConflicts(t *testing.T) {
	_, handler := newServiceHandler(t, "service_auth")
	service := createServiceForTest(t, handler, profileServiceBody)
	profile := bindingProfile(t, handler, service.ID, contract.IdentityClientCodexCLI, true)
	path := ServicesPath + "/" + string(service.ID)
	read := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
	bound := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", fmt.Sprintf(`{"http":{"identity_profile_id":%q}}`, profile.ID), read.Header().Get("ETag"))
	if bound.Code != http.StatusOK {
		t.Fatal(bound.Body.String())
	}
	for _, name := range []string{"User-Agent", "originator", "version"} {
		body := fmt.Sprintf(`{"http":{"auth":{"scheme":"custom_header","header_name":%q},"credential":{"secret":"fake-conflicting-key"}}}`, name)
		response := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", body, bound.Header().Get("ETag"))
		if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "http.identity_profile_id") || strings.Contains(response.Body.String(), "fake-conflicting-key") {
			t.Fatalf("auth conflict: %d %s", response.Code, response.Body.String())
		}
	}
	// Removing the binding and changing auth in the same patch is legitimate.
	response := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json",
		`{"http":{"identity_profile_id":null,"auth":{"scheme":"custom_header","header_name":"Version"},"credential":{"secret":"fake-new-key"}}}`, bound.Header().Get("ETag"))
	if response.Code != http.StatusOK {
		t.Fatalf("remove conflicting binding: %d %s", response.Code, response.Body.String())
	}
}

type serviceStoreWithoutIdentityProfiles struct{ storage.ServiceStore }

type serviceStoreWithUnavailableIdentityProfiles struct{ storage.ServiceStore }

func (serviceStoreWithUnavailableIdentityProfiles) GetIdentityProfile(context.Context, contract.ServiceID, contract.IdentityProfileID) (storage.IdentityProfileRecord, error) {
	return storage.IdentityProfileRecord{}, errors.New("fake-private-storage-error")
}

func TestServiceIdentityBindingStorageFailureDoesNotWriteOrLeak(t *testing.T) {
	store, handler := newServiceHandler(t, "service_unavailable")
	service := createServiceForTest(t, handler, profileServiceBody)
	profile := bindingProfile(t, handler, service.ID, contract.IdentityClientCodexCLI, true)
	path := ServicesPath + "/" + string(service.ID)
	for _, serviceStore := range []storage.ServiceStore{
		serviceStoreWithoutIdentityProfiles{store}, serviceStoreWithUnavailableIdentityProfiles{store},
	} {
		handler.serviceStore = serviceStore
		read := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
		body := fmt.Sprintf(`{"http":{"identity_profile_id":%q}}`, profile.ID)
		response := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", body, read.Header().Get("ETag"))
		if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "fake-private-storage-error") {
			t.Fatalf("storage failure: %d %s", response.Code, response.Body.String())
		}
		after := serviceRequestForTest(t, handler, http.MethodGet, path, "", "", "")
		if after.Header().Get("ETag") != read.Header().Get("ETag") {
			t.Fatal("unavailable profile storage changed service")
		}
		// Services without a profile do not require the optional storage feature.
		renamed := serviceRequestForTest(t, handler, http.MethodPatch, path, "application/merge-patch+json", `{"name":"no identity needed"}`, read.Header().Get("ETag"))
		if renamed.Code != http.StatusOK {
			t.Fatalf("unconfigured service depends on profile storage: %d %s", renamed.Code, renamed.Body.String())
		}
	}
}
