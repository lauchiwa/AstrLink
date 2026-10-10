package sqlite

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
)

// The writer must lock before counting; a busy lock retries a whole transaction
// without reading a stale snapshot. The fixture alone disables busy_timeout to
// expose retry deterministically rather than waiting for a real-time timeout.
func TestCreateAccessTokenRetriesBeforeCounting(t *testing.T) {
	store, actor, barrier, _ := configContentionFixture(t)
	manager, err := accesstoken.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	before, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA busy_timeout = 0`); err != nil {
		t.Fatal(err)
	}
	if _, err := actor.Exec(`BEGIN IMMEDIATE`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if _, err := actor.Exec(`ROLLBACK`); err != nil {
				t.Error(err)
			}
		})
	}
	t.Cleanup(release)
	barrier.afterRollback = func() {
		if barrier.readCount.Load() != 0 {
			t.Error("token count was read before acquiring the writer lock")
		}
		release()
	}
	created, err := manager.Create(context.Background(), "contention client")
	if err != nil {
		t.Fatalf("access token create lost a retryable lock conflict: %v", err)
	}
	if barrier.tokenLockCount.Load() != 2 || barrier.readCount.Load() != 1 {
		t.Fatalf("locks=%d reads=%d, want two lock attempts and one count", barrier.tokenLockCount.Load(), barrier.readCount.Load())
	}
	after, err := manager.List(context.Background())
	if err != nil || len(after) != len(before)+1 {
		t.Fatalf("create did not commit exactly one token: count=%d err=%v", len(after), err)
	}
	var secrets int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM local_access_token_secrets WHERE token_id = ?`, created.Token.ID).Scan(&secrets); err != nil || secrets != 1 {
		t.Fatalf("secret count=%d err=%v", secrets, err)
	}
	if revealed, err := manager.Reveal(context.Background(), created.Token.ID); err != nil || revealed != created.Value {
		t.Fatal("retried token secret does not reveal to its value")
	}
}

func TestCreateAccessTokenKeepsNameConflictAfterLock(t *testing.T) {
	store, _, barrier, _ := configContentionFixture(t)
	manager, err := accesstoken.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), "winner"); err != nil {
		t.Fatal(err)
	}
	barrier.readCount.Store(0)
	barrier.tokenLockCount.Store(0)
	if _, err := manager.Create(context.Background(), "WINNER"); !errors.Is(err, accesstoken.ErrConflict) {
		t.Fatalf("duplicate must remain a conflict, got %v", err)
	}
	if barrier.readCount.Load() != 1 || barrier.tokenLockCount.Load() != 1 {
		t.Fatal("a name conflict must not replay")
	}
}
