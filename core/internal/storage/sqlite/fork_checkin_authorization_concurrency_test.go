package sqlite

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

func TestForkCheckinAuthorizationDoesNotInvalidateOtherRequests(t *testing.T) {
	for _, firstKind := range []string{"job", "another authorization"} {
		t.Run(firstKind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/user/self" {
					http.NotFound(w, r)
					return
				}
				if r.Header.Get("Authorization") == "Bearer earlier-test-session" {
					enteredOnce.Do(func() { close(entered) })
					select {
					case <-release:
					case <-r.Context().Done():
						return
					}
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"success":true,"data":{"id":7}}`))
			}))
			defer server.Close()
			defer unblock()
			store := openTestStore(t, filepath.Join(t.TempDir(), "isolation.db"))
			defer store.Close()
			parts, err := store.ForkCheckinModuleFactory(newAPIModuleBuilder)(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = parts.Close(context.Background()) }()
			if parts.VerifyAuthorization == nil {
				t.Fatal("module has no isolated authorization verifier")
			}
			account := forkCheckinDraft("acct_auth_isolation", server.URL)
			account.State, account.RemoteUserID, account.Revision = forkcheckin.AccountStateConnected, "7", 1
			credential := func(value string) []byte {
				t.Helper()
				encoded, err := forkcheckin.EncodeNetworkCredential(forkcheckin.NetworkCredential{Version: 1, Bearer: value})
				if err != nil {
					t.Fatal(err)
				}
				return encoded
			}
			earlier := forkcheckin.AccountSnapshot{Account: account, Credential: credential("earlier-test-session")}
			later := forkcheckin.AccountSnapshot{Account: account, Credential: credential("later-test-session")}
			first := parts.Adapter.ValidateIdentity
			if firstKind == "another authorization" {
				first = parts.VerifyAuthorization
			}
			done := make(chan error, 1)
			go func() {
				_, err := first(ctx, earlier)
				done <- err
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("earlier request did not reach the site")
			}
			identity, err := parts.VerifyAuthorization(ctx, later)
			if err != nil || identity.RemoteUserID != "7" {
				t.Fatalf("later verification: identity=%+v err=%v", identity, err)
			}
			unblock()
			if err := <-done; err != nil {
				t.Fatalf("authorization invalidated %s: %v", firstKind, err)
			}
			// It also must not poison the job adapter's cached client after
			// verification finishes and destroys its own private factory.
			if _, err := parts.Adapter.ValidateIdentity(ctx, earlier); err != nil {
				t.Fatalf("job adapter unusable after authorization: %v", err)
			}
		})
	}
}
