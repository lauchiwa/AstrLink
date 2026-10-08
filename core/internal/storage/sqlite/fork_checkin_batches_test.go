package sqlite

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestForkCheckinBatchReplaySurvivesReorderingAndDeletingOneAccount(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	ids := []forkcheckin.AccountID{"acct_batch_one", "acct_batch_two"}
	days := make(map[forkcheckin.AccountID]string)
	for _, id := range ids {
		createForkCheckinJobAccount(t, store, id)
		days[id] = time.Now().UTC().Format(time.DateOnly)
	}
	request := forkcheckin.JobRequest{RequestID: "request_batch_replay_01", AccountIDs: ids,
		Action: forkcheckin.JobActionCheckIn, Trigger: forkcheckin.JobTriggerManual}
	created, err := store.CreateForkCheckinBatchOnce(ctx, request, days)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteForkCheckinAccount(ctx, ids[0], 1); err != nil {
		t.Fatal(err)
	}
	request.AccountIDs = []forkcheckin.AccountID{ids[1], ids[0]}
	replayed, err := store.CreateForkCheckinBatchOnce(ctx, request, nil)
	if err != nil || replayed.Outcome != forkcheckin.IdempotencyReplayed || !bytes.Equal(replayed.Receipt.Body, created.Receipt.Body) {
		t.Fatalf("batch replay: %+v, %v", replayed, err)
	}
	page, err := store.ListForkCheckinChildren(ctx, created.Batch.ParentID, forkcheckin.ListOptions{})
	if err != nil || len(page.Items) != 1 || page.Items[0].AccountID != ids[1] {
		t.Fatalf("deleting an account affected unrelated batch children: %+v, %v", page, err)
	}
	child := page.Items[0]
	if _, err := store.ClaimForkCheckin(ctx, child.AccountID, child.Action, child.SiteDay, 1, time.Minute); err != nil {
		t.Fatal(err)
	}
	request.Action = forkcheckin.JobActionStatusRefresh
	if _, err := store.CreateForkCheckinBatchOnce(ctx, request, nil); !errors.Is(err, storagecontract.ErrConflict) {
		t.Fatalf("batch ID reused for another action: %v", err)
	}
}

func TestForkCheckinBatchFailureIsAtomic(t *testing.T) {
	ctx := context.Background()
	store := openForkCheckinStore(t)
	ids := []forkcheckin.AccountID{"acct_atomic_a", "acct_atomic_b"}
	days := make(map[forkcheckin.AccountID]string)
	for _, id := range ids {
		createForkCheckinJobAccount(t, store, id)
		days[id] = time.Now().UTC().Format(time.DateOnly)
	}
	request := forkcheckin.JobRequest{RequestID: "request_atomic_batch_01", AccountIDs: ids,
		Action: forkcheckin.JobActionCheckIn, Trigger: forkcheckin.JobTriggerManual}
	mustExec(t, store, `CREATE TRIGGER reject_second_child BEFORE INSERT ON fork_checkin_jobs WHEN NEW.account_id = 'acct_atomic_b' BEGIN SELECT RAISE(ABORT, 'injected second child failure'); END`)
	if _, err := store.CreateForkCheckinBatchOnce(ctx, request, days); err == nil {
		t.Fatal("injected batch failure was ignored")
	}
	for _, table := range []string{forkCheckinBatches, forkCheckinBatchJobs, forkCheckinJobs, forkCheckinReceipts} {
		if countRows(t, store, table) != 0 {
			t.Fatalf("failed batch left rows in %s", table)
		}
	}
	mustExec(t, store, `DROP TRIGGER reject_second_child`)
	if _, err := store.CreateForkCheckinBatchOnce(ctx, request, days); err != nil {
		t.Fatal(err)
	}
	// Nested or separately added children would bypass the atomic input set.
	child := forkCheckinJobDraft(ids[0], "request_nested_child_01", 'n')
	child.ParentID = "batch_existing"
	if _, err := store.CreateForkCheckinJobOnce(ctx, child); !errors.Is(err, storagecontract.ErrInvalidArgument) {
		t.Fatalf("accepted a separately appended child: %v", err)
	}
	if _, err := store.ListForkCheckinChildren(ctx, "batch_missing", forkcheckin.ListOptions{}); !errors.Is(err, storagecontract.ErrNotFound) {
		t.Fatalf("missing parent: %v", err)
	}
}
