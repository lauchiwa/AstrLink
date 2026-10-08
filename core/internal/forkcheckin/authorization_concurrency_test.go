package forkcheckin

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type blockingAuthorizationAdapter struct {
	ReadOnlySiteAdapter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *blockingAuthorizationAdapter) ValidateIdentity(ctx context.Context, _ AccountSnapshot) (SiteIdentity, error) {
	a.once.Do(func() { close(a.entered) })
	select {
	case <-a.release:
		return SiteIdentity{RemoteUserID: "7"}, nil
	case <-ctx.Done():
		return SiteIdentity{}, ctx.Err()
	}
}

func TestAuthorizationConcurrentCompletionReplaysAfterWaiting(t *testing.T) {
	for _, test := range []struct {
		name      string
		requestID string
		claim     *string
		wantError error
	}{
		{name: "same request", requestID: "request_complete_concurrent"},
		{name: "different request", requestID: "request_complete_other", wantError: ErrAuthorizationCompleted},
		{name: "different content", requestID: "request_complete_concurrent", claim: new("7"), wantError: ErrRequestIDReused},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			store := &authStore{account: authAccount("acct_concurrent"), receipts: make(map[string]AuthorizationReceipt)}
			adapter := &blockingAuthorizationAdapter{entered: make(chan struct{}), release: make(chan struct{})}
			a := &authorizer{registry: newAuthorizationRegistry(nil), store: store, verifyIdentity: adapter.ValidateIdentity}
			session, err := a.begin(ctx, AuthorizationRequest{RequestID: "request_begin_concurrent", AccountID: store.account.ID, ExpectedRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			credential, err := EncodeNetworkCredential(NetworkCredential{Version: 1, Bearer: "test-session"})
			if err != nil {
				t.Fatal(err)
			}
			defer clear(credential)
			type answer struct {
				result AccountWriteResult
				err    error
			}
			first, second := make(chan answer, 1), make(chan answer, 1)
			go func() {
				result, err := a.complete(ctx, session.SessionID, AuthorizationCompletion{RequestID: "request_complete_concurrent", Credential: credential})
				first <- answer{result, err}
			}()
			select {
			case <-adapter.entered:
			case <-ctx.Done():
				t.Fatal("first verification did not start")
			}
			go func() {
				result, err := a.complete(ctx, session.SessionID, AuthorizationCompletion{RequestID: test.requestID, Credential: credential, ClaimedUserID: test.claim})
				second <- answer{result, err}
			}()
			// Ensure the retry looked for the receipt before the first request
			// committed and is now waiting for the same session's turn.
			for {
				a.registry.mu.Lock()
				holders := a.registry.sessions[session.SessionID].holders
				a.registry.mu.Unlock()
				if holders == 2 {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("second completion did not wait")
				case <-time.After(time.Millisecond):
				}
			}
			close(adapter.release)
			original, replay := <-first, <-second
			if original.err != nil {
				t.Fatalf("original completion: %v", original.err)
			}
			if !errors.Is(replay.err, test.wantError) {
				t.Fatalf("waiting completion: got %v, want %v", replay.err, test.wantError)
			}
			if test.wantError == nil && (!replay.result.Replayed || string(replay.result.Body) != string(original.result.Body)) {
				t.Fatal("waiting completion did not return the original receipt")
			}
			store.mu.Lock()
			commits := store.commits
			store.mu.Unlock()
			if commits != 1 {
				t.Fatalf("commits = %d, want 1", commits)
			}
		})
	}
}
