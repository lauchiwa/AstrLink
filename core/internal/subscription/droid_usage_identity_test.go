package subscription

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/accountauth"
)

// The quota read goes straight to Factory without the forwarder, so the
// gateway-local EU endpoint overlay must be removed here. Assert on what the
// server actually received, for both regions.
func TestDroidUsageSendsNoGatewayHeaders(t *testing.T) {
	for _, region := range []string{"", accountauth.DroidRegionEU} {
		name := region
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			received := make(chan http.Header, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/api/billing/limits" {
					t.Errorf("path = %q", request.URL.Path)
				}
				received <- request.Header.Clone()
				writer.Header().Set("Content-Type", "application/json")
				_, _ = writer.Write([]byte(`{"planType":"pro","limits":{"standard":{"fiveHour":{"usedPercent":1,"windowEnd":"2026-10-10T11:30:00Z"}}}}`))
			}))
			defer upstream.Close()
			manager := &Manager{
				droidConfig: accountauth.OAuthConfig{APIBaseURL: upstream.URL, HTTPClient: upstream.Client()},
				now:         func() time.Time { return time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC) },
			}
			tokens := accountauth.AccountTokens{AccessToken: "droid-test-token", AccountID: "org_test", Region: region}
			if _, err := manager.droidUsage(context.Background(), tokens); err != nil {
				t.Fatalf("droidUsage() = %v", err)
			}
			var header http.Header
			select {
			case header = <-received:
			default:
				t.Fatal("usage request never reached the upstream stub")
			}
			for name := range header {
				if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Astrlink-") {
					t.Fatalf("gateway header %s reached Factory: %#v", name, header)
				}
			}
			identity := accountauth.DefaultDroidIdentity()
			if got := header.Get("Authorization"); got != "Bearer droid-test-token" {
				t.Fatalf("Authorization = %q", got)
			}
			if got := header.Get("User-Agent"); got != identity.UserAgent || strings.Contains(strings.ToLower(got), "astrlink") {
				t.Fatalf("User-Agent = %q, want %q", got, identity.UserAgent)
			}
		})
	}
}
