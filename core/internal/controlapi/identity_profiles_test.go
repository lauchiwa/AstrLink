package controlapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
)

const profileServiceBody = `{"name":"fixture","kind":"openai_compatible","http":{"base_url":"https://upstream.example.test/v1","auth":{"scheme":"none"}},"capabilities":[{"protocol":"openai.responses","mode":"native","streaming":true}]}`

func TestIdentityProfileAPICandidateConfirmationAndIsolation(t *testing.T) {
	store, handler := newServiceHandler(t, "service_a", "service_b", "service_sub")
	service := createServiceForTest(t, handler, profileServiceBody)
	other := createServiceForTest(t, handler, profileServiceBody)
	subscription := createServiceForTest(t, handler, `{"name":"subscription","kind":"codex_subscription"}`)
	before, err := store.GetService(context.Background(), service.ID)
	if err != nil {
		t.Fatal(err)
	}
	path := ServicesPath + "/" + string(service.ID) + "/identity-profiles"
	for _, client := range []string{"codex_cli", "claude_code", "grok_cli"} {
		response := serviceRequestForTest(t, handler, http.MethodPost, path, "application/json", `{"client":"`+client+`","source":"builtin"}`, "")
		if response.Code != http.StatusCreated || response.Header().Get("ETag") == "" || response.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("create: %d %s", response.Code, response.Body.String())
		}
		var candidate contract.IdentityProfile
		decode(t, response, &candidate)
		if candidate.ServiceID != service.ID || candidate.ConfirmedAt != nil || candidate.ObservedAt != nil || candidate.Client != contract.IdentityClient(client) {
			t.Fatal("server created an active, unscoped or mislabelled candidate")
		}
		location := response.Header().Get("Location")
		etag := response.Header().Get("ETag")
		get := serviceRequestForTest(t, handler, http.MethodGet, location, "", "", "")
		if get.Code != http.StatusOK || get.Header().Get("ETag") != etag {
			t.Fatalf("get: %d %s", get.Code, get.Body.String())
		}
		for _, match := range []string{"", "stale"} {
			bad := serviceRequestForTest(t, handler, http.MethodPost, location+"/confirm", "application/json", "{}", match)
			want := http.StatusPreconditionFailed
			if match == "" {
				want = http.StatusBadRequest
			}
			if bad.Code != want {
				t.Fatalf("confirmation precondition: %d %s", bad.Code, bad.Body.String())
			}
		}
		confirmed := serviceRequestForTest(t, handler, http.MethodPost, location+"/confirm", "application/json", "{}", etag)
		if confirmed.Code != http.StatusOK || confirmed.Header().Get("ETag") == etag {
			t.Fatalf("confirm: %d %s", confirmed.Code, confirmed.Body.String())
		}
		var profile contract.IdentityProfile
		decode(t, confirmed, &profile)
		if profile.ConfirmedAt == nil || profile.Fingerprint.UserAgent != candidate.Fingerprint.UserAgent {
			t.Fatal("confirmation did not freeze the reviewed candidate")
		}
		repeat := serviceRequestForTest(t, handler, http.MethodPost, location+"/confirm", "application/json", "{}", confirmed.Header().Get("ETag"))
		if repeat.Code != http.StatusOK || repeat.Header().Get("ETag") != confirmed.Header().Get("ETag") {
			t.Fatal("idempotent confirmation changed the resource")
		}
		deleted := serviceRequestForTest(t, handler, http.MethodDelete, location, "", "", confirmed.Header().Get("ETag"))
		if deleted.Code != http.StatusConflict {
			t.Fatalf("confirmed snapshot can be discarded: %d", deleted.Code)
		}
		wrongScope := ServicesPath + "/" + string(other.ID) + "/identity-profiles/" + string(candidate.ID)
		for _, operation := range []struct{ method, suffix, body string }{
			{http.MethodGet, "", ""}, {http.MethodDelete, "", ""}, {http.MethodPost, "/confirm", "{}"},
		} {
			wrong := serviceRequestForTest(t, handler, operation.method, wrongScope+operation.suffix, "application/json", operation.body, confirmed.Header().Get("ETag"))
			if wrong.Code != http.StatusNotFound {
				t.Fatalf("cross-service operation: %d", wrong.Code)
			}
		}
	}
	list := serviceRequestForTest(t, handler, http.MethodGet, path+"?limit=2", "", "", "")
	var page identityProfilePageResponse
	decode(t, list, &page)
	if list.Code != http.StatusOK || len(page.Items) != 2 || page.NextCursor == nil {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	last := serviceRequestForTest(t, handler, http.MethodGet, path+"?limit=2&cursor="+*page.NextCursor, "", "", "")
	decode(t, last, &page)
	if last.Code != http.StatusOK || len(page.Items) != 1 || page.NextCursor != nil {
		t.Fatalf("last page: %d %s", last.Code, last.Body.String())
	}
	empty := serviceRequestForTest(t, handler, http.MethodGet, ServicesPath+"/"+string(other.ID)+"/identity-profiles", "", "", "")
	if empty.Code != http.StatusOK || !strings.Contains(empty.Body.String(), `"items":[]`) {
		t.Fatal("empty page is not an empty array")
	}
	notHTTP := serviceRequestForTest(t, handler, http.MethodPost, ServicesPath+"/"+string(subscription.ID)+"/identity-profiles", "application/json", `{"client":"codex_cli","source":"builtin"}`, "")
	if notHTTP.Code != http.StatusUnprocessableEntity {
		t.Fatalf("subscription identity was exposed to API profile writes: %d", notHTTP.Code)
	}
	after, err := store.GetService(context.Background(), service.ID)
	if err != nil || after.ETag != before.ETag {
		t.Fatal("profile confirmation mutated or activated service configuration")
	}
}

func TestIdentityProfileAPIImportsAFrozenLearnedIdentity(t *testing.T) {
	store, handler := newServiceHandler(t, "service_import")
	service := createServiceForTest(t, handler, profileServiceBody)
	path := ServicesPath + "/" + string(service.ID) + "/identity-profiles"
	input := `{"client":"codex_cli","source":"subscription_import"}`
	unavailable := serviceRequestForTest(t, handler, http.MethodPost, path, "application/json", input, "")
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable importer silently fell back: %d", unavailable.Code)
	}
	registry := accountauth.NewIdentityRegistry(store, store)
	handler.clientIdentities = registry
	missing := serviceRequestForTest(t, handler, http.MethodPost, path, "application/json", input, "")
	if missing.Code != http.StatusConflict {
		t.Fatalf("missing learned identity silently fell back: %d", missing.Code)
	}
	learn := func(version string) {
		t.Helper()
		changed, err := registry.LearnCodex(context.Background(), http.Header{"User-Agent": {"codex-tui/" + version}, "Originator": {"codex-tui"}})
		if err != nil || !changed {
			t.Fatalf("learn fixture: %v", err)
		}
	}
	learn("0.160.0")
	candidate := serviceRequestForTest(t, handler, http.MethodPost, path, "application/json", input, "")
	if candidate.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", candidate.Code, candidate.Body.String())
	}
	learn("0.161.0")
	confirmed := serviceRequestForTest(t, handler, http.MethodPost, candidate.Header().Get("Location")+"/confirm", "application/json", "{}", candidate.Header().Get("ETag"))
	var profile contract.IdentityProfile
	decode(t, confirmed, &profile)
	if confirmed.Code != http.StatusOK || profile.Fingerprint.Version != "0.160.0" || profile.Source != contract.IdentityProfileSubscriptionImport || profile.ObservedAt != nil {
		t.Fatal("confirmation used latest registry data or invented observation metadata")
	}
	if registry.ClientIdentities().Codex.LearnedVersion != "0.161.0" {
		t.Fatal("API confirmation changed subscription learning")
	}
}

func TestIdentityProfileAPIRejectsUntrustedWritesAndObserverReads(t *testing.T) {
	_, handler := newServiceHandler(t, "service_guard")
	service := createServiceForTest(t, handler, profileServiceBody)
	path := ServicesPath + "/" + string(service.ID) + "/identity-profiles"
	for _, body := range []string{
		`{"client":"codex_cli","source":"builtin","fingerprint":{}}`,
		`{"client":"codex_cli","source":"builtin","confirmed_at":"2026-01-01T00:00:00Z"}`,
		`{"client":"codex_cli","source":"builtin","service_id":"service_other"}`,
		`{"client":"codex_cli","source":"request_capture"}`,
		`{"client":"unsupported","source":"builtin"}`, `{}`, `null`,
		`{"client":"codex_cli","source":"builtin"} {}`,
	} {
		response := serviceRequestForTest(t, handler, http.MethodPost, path, "application/json", body, "")
		if response.Code != http.StatusBadRequest && response.Code != http.StatusUnprocessableEntity {
			t.Fatalf("unsafe input accepted: %d %s", response.Code, response.Body.String())
		}
	}
	created := serviceRequestForTest(t, handler, http.MethodPost, path, "application/json", `{"client":"codex_cli","source":"builtin"}`, "")
	if created.Code != http.StatusCreated {
		t.Fatal(created.Body.String())
	}
	location, etag := created.Header().Get("Location"), created.Header().Get("ETag")
	for _, operation := range []struct{ method, path, body string }{
		{http.MethodGet, path, ""}, {http.MethodPost, path, `{"client":"codex_cli","source":"builtin"}`},
		{http.MethodGet, location, ""}, {http.MethodDelete, location, ""}, {http.MethodPost, location + "/confirm", "{}"},
	} {
		request := httptest.NewRequest(operation.method, operation.path, strings.NewReader(operation.body))
		request.Header.Set("Authorization", "Bearer "+testObserverToken)
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("If-Match", etag)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusForbidden || strings.Contains(response.Body.String(), "user_agent") {
			t.Fatalf("observer accessed fingerprint operation: %d", response.Code)
		}
		request.Header.Del("Authorization")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated operation: %d", response.Code)
		}
	}
	for _, body := range []string{`{"fingerprint":{}}`, `{"confirmed_at":"2026-01-01T00:00:00Z"}`, `null`, `{} {}`} {
		response := serviceRequestForTest(t, handler, http.MethodPost, location+"/confirm", "application/json", body, etag)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("confirmation accepted write fields: %d", response.Code)
		}
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=1&limit=2", "unknown=1", "cursor=!", "cursor=" + strings.Repeat("a", 513), "bad=%zz"} {
		response := serviceRequestForTest(t, handler, http.MethodGet, path+"?"+query, "", "", "")
		if response.Code != http.StatusBadRequest {
			t.Fatalf("bad query accepted: %s, %d", query, response.Code)
		}
	}
	deleteResponse := serviceRequestForTest(t, handler, http.MethodDelete, location, "", "", etag)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("discard candidate: %d %s", deleteResponse.Code, deleteResponse.Body.String())
	}
	get := serviceRequestForTest(t, handler, http.MethodGet, location, "", "", "")
	if get.Code != http.StatusNotFound {
		t.Fatal("discarded candidate remains readable")
	}
}
