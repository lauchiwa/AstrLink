package sqlite

import (
	"context"
	"errors"
	"testing"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

func TestForkCheckinClaimJobIDDoesNotClaimQueuedSibling(t *testing.T) {
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_exact")
	first := forkCheckinJobDraft("acct_exact", "request_exact_first", 'a')
	first.ID = "job_exact_first"
	second := forkCheckinJobDraft("acct_exact", "request_exact_second", 'b')
	second.ID = "job_exact_second"
	createForkCheckinJob(t, store, first)
	createForkCheckinJob(t, store, second)
	lease, err := store.ClaimForkCheckinJobID(context.Background(), second.ID, 0)
	if err != nil || lease.JobID != second.ID {
		t.Fatalf("lease=%+v err=%v", lease, err)
	}
	untouched, err := store.GetForkCheckinJob(context.Background(), first.ID)
	if err != nil || untouched.Status != forkcheckin.JobStatusQueued {
		t.Fatalf("sibling changed: %+v %v", untouched, err)
	}
	if _, err = store.ClaimForkCheckinJobID(context.Background(), first.ID, 0); !errors.Is(err, ErrForkCheckinLeaseHeld) {
		t.Fatalf("account lease bypassed: %v", err)
	}
}

func TestForkCheckinClaimMissingJobIDDoesNotClaimOtherJob(t *testing.T) {
	store := openForkCheckinStore(t)
	createForkCheckinJobAccount(t, store, "acct_missing")
	job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_missing", "request_missing", 'a')).Job
	if _, err := store.ClaimForkCheckinJobID(context.Background(), "job_missing", 0); !errors.Is(err, storagecontract.ErrNotFound) {
		t.Fatalf("missing job: %v", err)
	}
	got, err := store.GetForkCheckinJob(context.Background(), job.ID)
	if err != nil || got.Status != forkcheckin.JobStatusQueued {
		t.Fatalf("unrelated job claimed: %+v %v", got, err)
	}
}
