package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
	sqlitedriver "modernc.org/sqlite"
)

func TestConfigWriteRetryStopsAtAttemptLimit(t *testing.T) {
	store, actor, barrier, original := configContentionFixture(t)
	barrier.afterRead = func() {
		if _, err := actor.Exec(`UPDATE config_write_noise SET value = value + 1`); err != nil {
			t.Error(err)
		}
	}
	err := store.DeleteService(context.Background(), original.Service.ID, original.ETag)
	var busy *sqlitedriver.Error
	if !errors.As(err, &busy) || busy.Code() != 517 {
		t.Fatalf("expected exhausted snapshot conflicts, got %v", err)
	}
	if barrier.readCount.Load() != configWriteMaxAttempts {
		t.Fatalf("attempts=%d", barrier.readCount.Load())
	}
	barrier.afterRead = nil
	if err := store.DeleteService(context.Background(), original.Service.ID, original.ETag); err != nil {
		t.Fatalf("exhausted attempts stranded a transaction: %v", err)
	}
}

func TestConfigWriteCancellationAndDeadline(t *testing.T) {
	for _, mode := range []string{"cancel_before", "deadline_before", "cancel_after_rollback", "deadline_after_rollback"} {
		t.Run(mode, func(t *testing.T) {
			store, actor, barrier, original := configContentionFixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			want := context.Canceled
			switch mode {
			case "cancel_before":
				cancel()
			case "deadline_before":
				cancel()
				ctx, cancel = context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				want = context.DeadlineExceeded
			case "deadline_after_rollback":
				cancel()
				ctx, cancel = context.WithTimeout(context.Background(), 200*time.Millisecond)
				want = context.DeadlineExceeded
			}
			defer cancel()
			barrier.afterRead = func() {
				if _, err := actor.Exec(`UPDATE config_write_noise SET value = value + 1`); err != nil {
					t.Error(err)
				}
			}
			barrier.afterRollback = func() {
				if mode == "deadline_after_rollback" {
					<-ctx.Done()
				} else {
					cancel()
				}
			}
			err := store.DeleteService(ctx, original.Service.ID, original.ETag)
			if !errors.Is(err, want) {
				t.Fatalf("expected %v, got %v", want, err)
			}
			reads := int32(0)
			if mode == "cancel_after_rollback" || mode == "deadline_after_rollback" {
				reads = 1
			}
			if barrier.readCount.Load() != reads {
				t.Fatalf("cancelled operation kept retrying: reads=%d", barrier.readCount.Load())
			}
			// A fresh operation still succeeds after cancellation and rollback.
			barrier.afterRead, barrier.afterRollback = nil, nil
			if err := store.DeleteService(context.Background(), original.Service.ID, original.ETag); err != nil {
				t.Fatalf("cancelled operation stranded transaction: %v", err)
			}
		})
	}
}

func TestConfigWriteNeverRetriesNonBusyOrUncertainCommit(t *testing.T) {
	for _, mode := range []string{"non_sql_error", "constraint", "commit_response_lost", "cancel_before_commit", "panic"} {
		t.Run(mode, func(t *testing.T) {
			store, _, barrier, _ := configContentionFixture(t)
			sentinel := errors.New("database is locked: synthetic non-SQL error")
			if mode == "commit_response_lost" {
				barrier.commitError = sentinel
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			var result int
			var err error
			panicked := false
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				result, err = retryConfigWrite(ctx, store.db, func(ctx context.Context, tx *sql.Tx) (int, error) {
					calls++
					deadline, ok := ctx.Deadline()
					if !ok || time.Until(deadline) > configWriteRetryBudget {
						t.Error("configuration transaction lost its time budget")
					}
					if _, err := tx.ExecContext(ctx, `UPDATE config_write_noise SET value = value + 1`); err != nil {
						return 0, err
					}
					switch mode {
					case "non_sql_error":
						return 42, sentinel
					case "constraint":
						_, err := tx.ExecContext(ctx, `INSERT INTO services (id) VALUES ('invalid_without_document')`)
						return 42, err
					case "cancel_before_commit":
						cancel()
					case "panic":
						panic("synthetic apply panic")
					}
					return 42, nil
				})
			}()
			if calls != 1 || result != 0 {
				t.Fatalf("replayed or published an unsuccessful transaction: calls=%d result=%d", calls, result)
			}
			if mode == "panic" {
				if !panicked {
					t.Error("apply panic disappeared")
				}
			} else if err == nil {
				t.Error("failed transaction was reported successful")
			}
			if mode == "non_sql_error" || mode == "commit_response_lost" {
				if !errors.Is(err, sentinel) {
					t.Fatalf("original error lost: %v", err)
				}
			}
			if mode == "cancel_before_commit" && !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation lost: %v", err)
			}
			var value int
			if err := store.db.QueryRow(`SELECT value FROM config_write_noise`).Scan(&value); err != nil {
				t.Fatal(err)
			}
			want := 0
			if mode == "commit_response_lost" {
				want = 1
			}
			if value != want {
				t.Fatalf("rollback/replay violated atomicity: value=%d want=%d", value, want)
			}
		})
	}
}

func TestConfigWriteDoesNotRetryFailedRollback(t *testing.T) {
	store, actor, barrier, original := configContentionFixture(t)
	sentinel := errors.New("rollback outcome unknown")
	barrier.rollbackError = sentinel
	barrier.afterRead = func() {
		if _, err := actor.Exec(`UPDATE config_write_noise SET value = value + 1`); err != nil {
			t.Error(err)
		}
	}
	err := store.DeleteService(context.Background(), original.Service.ID, original.ETag)
	var busy *sqlitedriver.Error
	if !errors.Is(err, sentinel) || !errors.As(err, &busy) || barrier.readCount.Load() != 1 {
		t.Fatalf("failed rollback was discarded or replayed: %v", err)
	}
}

func TestConfigWriteRollsBackCredentialFailureWithoutMutatingCaller(t *testing.T) {
	store, _, barrier, original := configContentionFixture(t)
	if _, err := store.db.Exec(`CREATE TRIGGER reject_config_credential BEFORE UPDATE ON service_credentials BEGIN SELECT RAISE(ABORT, 'fixture credential rejected'); END`); err != nil {
		t.Fatal(err)
	}
	input := original.Service
	input.Name = "must roll back"
	connection := *input.HTTP
	connection.CredentialRef = ""
	input.HTTP = &connection
	_, err := store.UpdateService(context.Background(), input, storagecontract.CredentialMutation{Present: true, Secret: []byte("must-not-persist")}, original.ETag)
	if err == nil || barrier.readCount.Load() != 1 {
		t.Fatal("non-busy credential failure retried or disappeared")
	}
	if input.HTTP.CredentialRef != "" {
		t.Fatal("attempt mutated caller-owned HTTP connection")
	}
	got, err := store.GetService(context.Background(), original.Service.ID)
	if err != nil || got.ETag != original.ETag || got.Service.Name != original.Service.Name {
		t.Fatal("credential error committed a partial service update")
	}
	value, err := store.Get(context.Background(), secretstore.Ref(localServiceRef(original.Service.ID)))
	defer clear(value)
	if err != nil || string(value) != "config-test-original-secret" {
		t.Fatal("credential failure changed the original secret")
	}
}
