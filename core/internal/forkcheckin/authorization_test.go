package forkcheckin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type authClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *authClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *authClock) advance(delay time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(delay)
	c.mu.Unlock()
}

func authAccount(id AccountID) Account {
	return Account{ID: id, DashboardBaseURL: "https://relay.example/", State: AccountStateDraft, Revision: 1,
		Network: Network{Mode: NetworkModeDirect}, TimeZone: "UTC"}
}

func (registry *authorizationRegistry) has(id string) bool {
	registry.mu.Lock()
	defer registry.mu.Unlock()
	return registry.sessions[id] != nil
}

func TestAuthorizationRegistryReplaysExpiresAndStaysBounded(t *testing.T) {
	clock := &authClock{now: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
	registry := newAuthorizationRegistry(clock.Now)
	request := AuthorizationRequest{RequestID: "request_auth_01", AccountID: "acct_auth", ExpectedRevision: 1}
	first, err := registry.begin(request.RequestID, request.Fingerprint(), authAccount("acct_auth"))
	if err != nil || first.LoginURL != "https://relay.example" || first.AccountID != "acct_auth" ||
		!first.ExpiresAt.Equal(clock.Now().Add(AuthorizationTTL)) || len(first.ConfigFingerprint) != 64 {
		t.Fatalf("begin=%+v err=%v", first, err)
	}
	if !sessionPathID.MatchString(first.SessionID) {
		t.Fatalf("session id %q does not match the contract pattern", first.SessionID)
	}
	if again, replayed, err := registry.replay(request.RequestID, request.Fingerprint()); err != nil || !replayed || again != first {
		t.Fatalf("replay=%+v replayed=%v err=%v", again, replayed, err)
	}
	if again, err := registry.begin(request.RequestID, request.Fingerprint(), authAccount("acct_auth")); err != nil || again != first {
		t.Fatalf("racing begin=%+v err=%v", again, err)
	}
	other := request
	other.ExpectedRevision = 2
	if _, _, err := registry.replay(request.RequestID, other.Fingerprint()); !errors.Is(err, ErrRequestIDReused) {
		t.Fatalf("different content replay=%v", err)
	}
	if _, err := registry.begin(request.RequestID, other.Fingerprint(), authAccount("acct_auth")); !errors.Is(err, ErrRequestIDReused) {
		t.Fatalf("different content begin=%v", err)
	}
	if !registry.begun(request.RequestID) {
		t.Fatal("a live begin was not reported")
	}
	clock.advance(AuthorizationTTL)
	if _, err := registry.acquire(context.Background(), first.SessionID); !errors.Is(err, ErrAuthorizationNotFound) {
		t.Fatalf("expired acquire=%v", err)
	}
	if _, replayed, _ := registry.replay(request.RequestID, request.Fingerprint()); replayed || registry.begun(request.RequestID) {
		t.Fatal("an expired session was still answered")
	}

	// A session in mid-completion outlives idle ones when the bound is hit.
	held, err := registry.begin("request_held_01", "digest", authAccount("acct_auth"))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := registry.acquire(context.Background(), held.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for index := range maxAuthorizations + 2 {
		clock.advance(time.Second)
		session, err := registry.begin(fmt.Sprintf("request_bound_%02d", index), "digest", authAccount("acct_auth"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, session.SessionID)
	}
	registry.mu.Lock()
	count := len(registry.sessions)
	registry.mu.Unlock()
	if count != maxAuthorizations || !registry.has(held.SessionID) || registry.has(ids[0]) || !registry.has(ids[len(ids)-1]) {
		t.Fatalf("bounded registry count=%d held=%v oldest=%v newest=%v", count,
			registry.has(held.SessionID), registry.has(ids[0]), registry.has(ids[len(ids)-1]))
	}
	turn.release(false)
}

func TestAuthorizationTurnsSerializeAndCompleteOnce(t *testing.T) {
	registry := newAuthorizationRegistry(nil)
	session, err := registry.begin("request_turn_01", "digest", authAccount("acct_turn"))
	if err != nil {
		t.Fatal(err)
	}
	first, err := registry.acquire(context.Background(), session.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan error, 1)
	go func() {
		turn, err := registry.acquire(context.Background(), session.SessionID)
		if turn != nil {
			turn.release(false)
		}
		waiting <- err
	}()
	select {
	case err := <-waiting:
		t.Fatalf("a second completion did not wait for the first: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	first.release(true)
	if err := <-waiting; !errors.Is(err, ErrAuthorizationCompleted) {
		t.Fatalf("after completion=%v", err)
	}

	// A failed attempt leaves the session usable, and a cancelled waiter
	// gives its hold back.
	other, err := registry.begin("request_turn_02", "digest", authAccount("acct_turn"))
	if err != nil {
		t.Fatal(err)
	}
	turn, err := registry.acquire(context.Background(), other.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	turn.release(false)
	turn, err = registry.acquire(context.Background(), other.SessionID)
	if err != nil {
		t.Fatalf("session unusable after a failed attempt: %v", err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := registry.acquire(cancelled, other.SessionID); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter=%v", err)
	}
	turn.release(false)
	registry.mu.Lock()
	holders := registry.sessions[other.SessionID].holders
	registry.mu.Unlock()
	if holders != 0 {
		t.Fatalf("holders=%d after every turn ended", holders)
	}
}

func TestAuthorizationCompletionIsNeverSerialized(t *testing.T) {
	completion := AuthorizationCompletion{RequestID: "request_print_01", Credential: []byte("PRINT-SECRET-MARKER")}
	if _, err := completion.MarshalJSON(); err == nil {
		t.Fatal("a completion marshalled")
	}
	commit := AuthorizationCommit{Credential: []byte("PRINT-SECRET-MARKER")}
	for _, printed := range []string{fmt.Sprintf("%v %+v %s", completion, completion, completion), fmt.Sprintf("%v %+v", commit, commit)} {
		if printed == "" || strings.Contains(printed, "PRINT-SECRET-MARKER") {
			t.Fatalf("printed a captured session: %q", printed)
		}
	}
	claimed := "7"
	base := completion.Fingerprint("auth_session_one")
	if completion.Fingerprint("auth_session_two") == base {
		t.Fatal("fingerprint does not bind the session")
	}
	withClaim := completion
	withClaim.ClaimedUserID = &claimed
	if withClaim.Fingerprint("auth_session_one") == base {
		t.Fatal("fingerprint ignores the claimed user")
	}
	otherCredential := completion
	otherCredential.Credential = []byte("another session")
	if otherCredential.Fingerprint("auth_session_one") != base {
		t.Fatal("fingerprint is derived from the captured session")
	}
}
