package forkcheckin_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
	"github.com/QuantumNous/astrlink/core/internal/subscription"
)

const acceptanceSubscriptionAccess = "CHECKIN-ACCEPTANCE-SUBSCRIPTION-ACCESS-"
const acceptanceSubscriptionRefresh = "CHECKIN-ACCEPTANCE-SUBSCRIPTION-REFRESH-"

func acceptanceSubscriptionID(index int) contract.ServiceID {
	return contract.ServiceID(fmt.Sprintf("service_acceptance_subscription_%d", index))
}

func seedAcceptanceSubscriptions(t *testing.T, directory string) {
	t.Helper()
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(directory, "astrlink.db"), sqlite.WithExistingDatabase(), sqlite.WithLocalKey(acceptanceKey()))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	credentials := subscription.NewStorageAccountCredentialStore(store)
	now, expires := time.Now().UTC(), time.Now().UTC().Add(time.Hour)
	for index := 0; index < 2; index++ {
		id := acceptanceSubscriptionID(index)
		accountID := fmt.Sprintf("acceptance_subscription_remote_%d", index)
		account := contract.SubscriptionAccount{
			ID: id, Provider: contract.SubscriptionProviderOpenAICodex,
			Status: contract.SubscriptionStatusConnected, DisplayName: "Acceptance subscription",
			ProviderAccountID: accountID, CredentialRef: accountauth.CredentialRefFor(id),
			Capabilities: contract.DefaultOpenAICodexCapabilities(), TokenExpiresAt: &expires,
			CreatedAt: now, UpdatedAt: now,
		}
		if err := store.PutSubscriptionAccount(ctx, account); err != nil {
			t.Fatal(err)
		}
		if err := credentials.Put(ctx, id, accountauth.AccountTokens{
			AccessToken:  fmt.Sprintf("%s%d", acceptanceSubscriptionAccess, index),
			RefreshToken: fmt.Sprintf("%s%d", acceptanceSubscriptionRefresh, index),
			AccountID:    accountID, ExpiresAt: expires,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

type acceptanceQuotaSite struct {
	server     *httptest.Server
	calls      [2]atomic.Int32
	failFirst  atomic.Bool
	violations atomic.Int32
}

func newAcceptanceQuotaSite(t *testing.T) *acceptanceQuotaSite {
	t.Helper()
	site := &acceptanceQuotaSite{}
	site.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 1024))
		if r.Method != http.MethodGet || r.URL.Path != "/backend-api/wham/usage" || r.URL.RawQuery != "" || len(body) != 0 {
			site.violations.Add(1)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		for key, values := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-astrlink-") || strings.Contains(strings.ToLower(strings.Join(values, " ")), "astrlink") {
				site.violations.Add(1)
			}
		}
		for _, marker := range []string{acceptanceOperator, acceptanceObserver, acceptanceProviderSecret, acceptanceSessionSecret, acceptancePrivatePrompt, acceptanceSubscriptionRefresh} {
			if strings.Contains(fmt.Sprint(r.Header), marker) {
				site.violations.Add(1)
			}
		}
		index := -1
		for candidate := 0; candidate < 2; candidate++ {
			if r.Header.Get("Authorization") == fmt.Sprintf("Bearer %s%d", acceptanceSubscriptionAccess, candidate) {
				index = candidate
			}
		}
		if index < 0 || r.Header.Get("ChatGPT-Account-ID") != fmt.Sprintf("acceptance_subscription_remote_%d", index) {
			site.violations.Add(1)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		call := site.calls[index].Add(1)
		if index == 0 && site.failFirst.Load() {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":%d,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`, 10+index*50+int(call))
	}))
	t.Cleanup(site.server.Close)
	return site
}

func acceptanceUsage(t *testing.T, core *acceptanceCore, index int, refresh bool, wantStatus int) acceptanceResponse {
	t.Helper()
	path := "/control/v1/services/" + string(acceptanceSubscriptionID(index)) + "/usage"
	if refresh {
		path += "?refresh=1"
	}
	response := core.request(t, http.MethodGet, path, "", acceptanceOperator, "")
	requireAcceptanceStatus(t, response, wantStatus)
	for _, marker := range []string{acceptanceSubscriptionAccess, acceptanceSubscriptionRefresh, acceptanceSessionSecret, acceptanceProviderSecret} {
		if bytes.Contains(response.body, []byte(marker)) {
			t.Fatal("subscription usage response exposed seeded credentials")
		}
	}
	if response.header.Get("Cache-Control") != "no-store" {
		t.Fatal("subscription usage response is browser-cacheable")
	}
	return response
}

// The real startup monitor must fetch both accounts without a UI request.
// Thereafter repeated reads, extension work and account-A refreshes must not
// cause cross-account invalidation. The fixture only uses synthetic local://
// credentials, so startup never needs to migrate an OS keychain entry.
func primeAcceptanceQuota(t *testing.T, core *acceptanceCore, site *acceptanceQuotaSite) [2][]byte {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for site.calls[0].Load() == 0 || site.calls[1].Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("subscription monitor did not independently fetch both quota windows")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var snapshots [2][]byte
	for index := range snapshots {
		snapshots[index] = acceptanceUsage(t, core, index, false, http.StatusOK).body
		var usage contract.SubscriptionUsage
		if err := json.Unmarshal(snapshots[index], &usage); err != nil || usage.Validate() != nil || usage.ServiceID != acceptanceSubscriptionID(index) || usage.Primary == nil || usage.Primary.UsedPercent != float64(11+index*50) {
			t.Fatal("invalid or cross-account subscription quota snapshot")
		}
		for repeat := 0; repeat < 3; repeat++ {
			if !bytes.Equal(acceptanceUsage(t, core, index, false, http.StatusOK).body, snapshots[index]) {
				t.Fatal("cached quota read changed its snapshot")
			}
		}
		if site.calls[index].Load() != 1 {
			t.Fatal("cached quota reads triggered provider calls after startup")
		}
	}
	return snapshots
}

func verifyAcceptanceQuota(t *testing.T, core *acceptanceCore, site *acceptanceQuotaSite, snapshots [2][]byte) {
	t.Helper()
	for index := range snapshots {
		if !bytes.Equal(acceptanceUsage(t, core, index, false, http.StatusOK).body, snapshots[index]) || site.calls[index].Load() != 1 {
			t.Fatal("extension workload invalidated or changed a live subscription cache")
		}
	}
	refreshed := acceptanceUsage(t, core, 0, true, http.StatusOK).body
	if bytes.Equal(refreshed, snapshots[0]) || site.calls[0].Load() != 2 {
		t.Fatal("explicit subscription refresh did not fetch exactly once")
	}
	if !bytes.Equal(acceptanceUsage(t, core, 0, false, http.StatusOK).body, refreshed) || site.calls[0].Load() != 2 {
		t.Fatal("refreshed subscription snapshot was not cached")
	}
	// A failed explicit refresh must not be cached as success or poison B.
	site.failFirst.Store(true)
	acceptanceUsage(t, core, 0, true, http.StatusBadGateway)
	site.failFirst.Store(false)
	recovered := acceptanceUsage(t, core, 0, false, http.StatusOK).body
	if bytes.Equal(recovered, refreshed) || site.calls[0].Load() != 4 {
		t.Fatal("subscription failed-refresh recovery did not fetch once")
	}
	if !bytes.Equal(acceptanceUsage(t, core, 1, false, http.StatusOK).body, snapshots[1]) || site.calls[1].Load() != 1 {
		t.Fatal("account-A refresh or failure evicted account-B quota")
	}
	if site.violations.Load() != 0 {
		t.Fatalf("subscription outgoing identity/secret violations=%d", site.violations.Load())
	}
	t.Log("subscription monitor/cache: startup=1+1, cached reads=0, A refresh=1, failed A refresh/recovery=1+1, B unchanged")
}
