package sqlite

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
)

// Access-token creation counts existing tokens before inserting, so a
// concurrent writer can make the read-to-write upgrade fail. Retry must
// recount and still commit exactly one token with its sealed secret.
func TestCreateAccessTokenRetriesRealReadUpgradeContention(t *testing.T) {
	for _, conflict := range []string{"writer_held", "snapshot_changed"} {
		t.Run(conflict, func(t *testing.T) {
			store, actor, barrier, _ := configContentionFixture(t)
			manager, err := accesstoken.NewManager(store)
			if err != nil {
				t.Fatal(err)
			}
			var before int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM local_access_tokens`).Scan(&before); err != nil {
				t.Fatal(err)
			}
			barrier.readCount.Store(0)
			var injected atomic.Bool
			var releaseOnce sync.Once
			release := func() {
				releaseOnce.Do(func() {
					if conflict == "writer_held" {
						if _, err := actor.Exec(`ROLLBACK`); err != nil {
							t.Error(err)
						}
					}
				})
			}
			barrier.afterRead = func() {
				if !injected.CompareAndSwap(false, true) {
					return
				}
				if conflict == "writer_held" {
					if _, err := actor.Exec(`BEGIN IMMEDIATE`); err != nil {
						t.Error(err)
						return
					}
				}
				if _, err := actor.Exec(`UPDATE config_write_noise SET value = value + 1`); err != nil {
					t.Error(err)
				}
			}
			barrier.afterRollback = release
			t.Cleanup(release)

			created, err := manager.Create(context.Background(), "contention client")
			if err != nil {
				t.Fatalf("access token create lost a retryable read-upgrade conflict: %v", err)
			}
			if barrier.readCount.Load() != 2 {
				t.Fatalf("expected exactly two attempts, reads=%d", barrier.readCount.Load())
			}
			barrier.afterRead = nil
			var after, secrets int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM local_access_tokens`).Scan(&after); err != nil {
				t.Fatal(err)
			}
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM local_access_token_secrets WHERE token_id = ?`, created.Token.ID).Scan(&secrets); err != nil {
				t.Fatal(err)
			}
			if after != before+1 || secrets != 1 {
				t.Fatalf("retry committed tokens=%d (was %d) secrets=%d", after, before, secrets)
			}
			if revealed, err := manager.Reveal(context.Background(), created.Token.ID); err != nil || revealed != created.Value {
				t.Fatal("retried token secret does not reveal to its value")
			}
		})
	}
}

// A conflict on the retried attempt must still be reported, not hidden.
func TestCreateAccessTokenRetryKeepsNameConflict(t *testing.T) {
	store, actor, barrier, _ := configContentionFixture(t)
	manager, err := accesstoken.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Create(context.Background(), "winner"); err != nil {
		t.Fatal(err)
	}
	var injected atomic.Bool
	barrier.afterRead = func() {
		if injected.CompareAndSwap(false, true) {
			if _, err := actor.Exec(`UPDATE config_write_noise SET value = value + 1`); err != nil {
				t.Error(err)
			}
		}
	}
	barrier.readCount.Store(0)
	if _, err := manager.Create(context.Background(), "WINNER"); !errors.Is(err, accesstoken.ErrConflict) {
		t.Fatalf("retried duplicate must remain a conflict, got %v", err)
	}
	if barrier.readCount.Load() != 2 {
		t.Fatalf("conflict retry reads=%d", barrier.readCount.Load())
	}
}
