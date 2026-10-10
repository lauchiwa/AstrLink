package sqlite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestCreateAccessTokenCountsConcurrentLimitAfterLock(t *testing.T) {
	store, actor, barrier, _ := configContentionFixture(t)
	manager, err := accesstoken.NewManager(store)
	if err != nil {
		t.Fatal(err)
	}
	// This view borrows the fixture's key ring and second connection; their
	// owners close them. It writes real, correctly sealed competing tokens.
	competitor, err := accesstoken.NewManager(&Store{db: actor, keys: store.keys, now: store.now})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	existing, err := manager.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for index := len(existing); index < storagecontract.AccessTokenLimit-1; index++ {
		if _, err := manager.Create(ctx, fmt.Sprintf("limit fixture %d", index)); err != nil {
			t.Fatal(err)
		}
	}
	var injected atomic.Bool
	barrier.readCount.Store(0)
	barrier.beforeTokenLock = func() {
		if injected.CompareAndSwap(false, true) {
			if _, err := competitor.Create(ctx, "concurrent last slot"); err != nil {
				t.Error(err)
			}
		}
	}
	created, err := manager.Create(ctx, "must not exceed the limit")
	if !errors.Is(err, accesstoken.ErrTokenLimit) || created.Value != "" || created.Token.ID != "" {
		t.Fatalf("write-first count must refuse the full limit: %v", err)
	}
	if barrier.readCount.Load() != 1 {
		t.Fatalf("limit reads=%d", barrier.readCount.Load())
	}
	barrier.beforeTokenLock = nil
	got, err := manager.List(ctx)
	if err != nil || len(got) != storagecontract.AccessTokenLimit {
		t.Fatalf("limit changed after concurrent creation: count=%d err=%v", len(got), err)
	}
	for _, token := range got {
		if token.Name == "must not exceed the limit" {
			t.Fatal("rejected token persisted")
		}
	}
}

func TestCreateAccessTokenUsesCurrentNameAfterLock(t *testing.T) {
	for _, change := range []string{"insert", "delete"} {
		t.Run(change, func(t *testing.T) {
			store, actor, barrier, _ := configContentionFixture(t)
			manager, err := accesstoken.NewManager(store)
			if err != nil {
				t.Fatal(err)
			}
			competitor, err := accesstoken.NewManager(&Store{db: actor, keys: store.keys, now: store.now})
			if err != nil {
				t.Fatal(err)
			}
			ctx := context.Background()
			var old accesstoken.CreatedToken
			if change == "delete" {
				old, err = competitor.Create(ctx, "contended name")
				if err != nil {
					t.Fatal(err)
				}
			}
			var injected atomic.Bool
			barrier.readCount.Store(0)
			barrier.beforeTokenLock = func() {
				if !injected.CompareAndSwap(false, true) {
					return
				}
				if change == "delete" {
					if err := competitor.Delete(ctx, old.Token.ID); err != nil {
						t.Error(err)
					}
				} else if _, err := competitor.Create(ctx, "contended name"); err != nil {
					t.Error(err)
				}
			}
			created, err := manager.Create(ctx, "CONTENDED NAME")
			if change == "insert" {
				if !errors.Is(err, accesstoken.ErrConflict) || created.Value != "" {
					t.Fatalf("concurrent winner was not preserved: %v", err)
				}
			} else if err != nil || created.Token.ID == "" {
				t.Fatalf("stale snapshot reported a conflict after concurrent deletion: %v", err)
			}
			if barrier.readCount.Load() != 1 {
				t.Fatalf("name reads=%d", barrier.readCount.Load())
			}
		})
	}
}

func TestCreateAccessTokenRetryFailureDoesNotPublishOrReplay(t *testing.T) {
	for _, mode := range []string{"secret_rejected", "commit_response_lost", "cancel_after_rollback", "attempt_limit"} {
		t.Run(mode, func(t *testing.T) {
			store, actor, barrier, _ := configContentionFixture(t)
			manager, err := accesstoken.NewManager(store)
			if err != nil {
				t.Fatal(err)
			}
			before, err := manager.List(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			sentinel := errors.New("synthetic lost token commit response")
			switch mode {
			case "secret_rejected":
				if _, err := actor.Exec(`CREATE TRIGGER reject_token_secret BEFORE INSERT ON local_access_token_secrets BEGIN SELECT RAISE(ABORT, 'fixture secret rejected'); END`); err != nil {
					t.Fatal(err)
				}
			case "commit_response_lost":
				barrier.commitError = sentinel
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			// Establish a real competing writer before any count is read.
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
				if mode != "attempt_limit" {
					release()
				}
				if mode == "cancel_after_rollback" {
					cancel()
				}
			}
			barrier.readCount.Store(0)
			created, err := manager.Create(ctx, "unsuccessful token")
			if err == nil || created.Value != "" || created.Token.ID != "" {
				t.Fatal("failed create published a token or swallowed its error")
			}
			wantReads, wantLocks, wantRows := int32(1), int32(2), len(before)
			switch mode {
			case "commit_response_lost":
				wantRows++ // The commit happened, but its response was lost.
				if !errors.Is(err, sentinel) {
					t.Fatalf("lost commit response changed its error: %v", err)
				}
			case "cancel_after_rollback":
				wantReads, wantLocks = 0, 1
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation was lost: %v", err)
				}
			case "attempt_limit":
				wantReads, wantLocks = 0, configWriteMaxAttempts
			}
			if barrier.readCount.Load() != wantReads || barrier.tokenLockCount.Load() != wantLocks {
				t.Fatalf("unsafe replay or wrong retry bound: reads=%d/%d locks=%d/%d", barrier.readCount.Load(), wantReads, barrier.tokenLockCount.Load(), wantLocks)
			}
			release()
			after, err := manager.List(context.Background())
			if err != nil || len(after) != wantRows {
				t.Fatalf("token metadata committed partially or more than once: rows=%d want=%d err=%v", len(after), wantRows, err)
			}
			var secretCount int
			if err := store.db.QueryRow(`SELECT COUNT(*) FROM local_access_token_secrets`).Scan(&secretCount); err != nil || secretCount != wantRows {
				t.Fatalf("token secret atomicity: rows=%d want=%d err=%v", secretCount, wantRows, err)
			}
		})
	}
}
