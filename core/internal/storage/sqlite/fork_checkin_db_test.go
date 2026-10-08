package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

// doneSpyContext counts calls to Done. A detached statement context never
// consults its parent, so any call means a cancellable context reached the
// pool or the driver, where modernc.org/sqlite v1.38.2 can strand a lock.
type doneSpyContext struct {
	context.Context
	calls atomic.Int32
}

func (spy *doneSpyContext) Done() <-chan struct{} {
	spy.calls.Add(1)
	return spy.Context.Done()
}

func TestForkCheckinStatementsNeverSeeCancellableContext(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &doneSpyContext{Context: parent}
	store := openTestStore(t, filepath.Join(t.TempDir(), "detached.db"))
	t.Cleanup(func() { _ = store.Close() })
	must := func(step string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}

	must("ensure schema", store.EnsureForkCheckinSchema(ctx))
	must("require schema", store.RequireForkCheckinSchema(ctx))
	if _, err := store.ForkCheckinSchemaPresent(ctx); err != nil {
		t.Fatal(err)
	}
	_, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_detached", "https://relay.example"))
	must("create account", err)
	account, err := store.ConnectForkCheckinAccount(ctx, "acct_detached", "7", 1)
	must("connect account", err)
	credential, err := forkcheckin.EncodeNetworkCredential(forkcheckin.NetworkCredential{Version: 1, Bearer: forkCheckinSessionMarker})
	must("encode credential", err)
	sessions := store.ForkCheckinSessions()
	must("put session", sessions.Put(ctx, account.ID, credential))
	if _, err := sessions.Get(ctx, account.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.HasForkCheckinSession(ctx, account.ID); err != nil {
		t.Fatal(err)
	}

	draft := forkCheckinJobDraft(account.ID, "request_detached_job", 'd')
	draft.ExpectedRevision = account.Revision
	created, err := store.CreateForkCheckinJobOnce(ctx, draft)
	must("create job", err)
	lease, err := store.ClaimForkCheckinJobID(ctx, created.Job.ID, time.Minute)
	must("claim job", err)
	_, err = store.BindForkCheckinJobDay(ctx, lease, created.Job.SiteDay)
	must("bind day", err)
	must("mark dispatched", store.MarkForkCheckinDispatched(ctx, lease))
	// The live lease makes recovery refuse; it still runs its statements.
	_, _ = store.RecoverForkCheckinDispatch(ctx, created.Job.ID, account.ID, account.Revision, time.Minute)
	_, err = store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceSubmitResponse,
	})
	must("complete job", err)
	_, err = store.GetForkCheckinJob(ctx, created.Job.ID)
	must("get job", err)
	_, err = store.LookupForkCheckinReceipt(ctx, draft.RequestID)
	must("lookup receipt", err)
	_, err = store.LookupForkCheckinJobReceipt(ctx, draft.RequestID)
	must("lookup job receipt", err)
	_, err = store.ListForkCheckinJobs(ctx, account.ID, forkcheckin.ListOptions{})
	must("list jobs", err)
	current, _, err := store.ReadForkCheckinAuthorizationAccount(ctx, account.ID)
	must("read authorization account", err)
	_, err = store.CompleteForkCheckinAuthorization(ctx, forkcheckin.AuthorizationCommit{
		RequestID: "request_detached_authorization", Fingerprint: digestText("detached authorization"),
		AccountID: account.ID, Revision: current.Revision, ConfigFingerprint: current.ConfigFingerprint(),
		RemoteUserID: "7", Credential: credential,
	})
	must("complete authorization", err)
	_, err = store.LookupForkCheckinAuthorizationReceipt(ctx, "request_detached_authorization")
	must("lookup authorization receipt", err)

	_, err = store.ListForkCheckinScheduledAccounts(ctx, "", forkcheckin.MaxPageSize)
	must("list scheduled", err)
	_, err = store.PrepareForkCheckinScheduledJob(ctx, account.ID, time.Now())
	must("prepare scheduled", err)
	_, err = store.ListForkCheckinAccounts(ctx, forkcheckin.ListOptions{})
	must("list accounts", err)
	_, err = store.ListForkCheckinAccountViews(ctx, forkcheckin.ListOptions{})
	must("list views", err)
	_, err = store.CountForkCheckinAccounts(ctx)
	must("count accounts", err)
	_, err = store.ListForkCheckinBindableServices(ctx)
	must("list bindable", err)
	_, err = store.ResolveForkCheckinBindings(ctx, account.ID)
	must("resolve bindings", err)

	jobs := forkCheckinJobWriter{store: store}
	_, err = store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_detached_b", "https://relay-b.example"))
	must("create second account", err)
	_, err = store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_detached_c", "https://relay-c.example"))
	must("create third account", err)
	batch, err := jobs.CreateJobs(ctx, forkcheckin.JobCreateRequest{
		RequestID: "request_detached_batch", Action: forkcheckin.JobActionCheckIn,
		Accounts: []forkcheckin.AccountID{"acct_detached_b", "acct_detached_c"},
	})
	must("create batch", err)
	var receipt struct {
		ID forkcheckin.JobID `json:"id"`
	}
	must("decode batch", json.Unmarshal(batch.Body, &receipt))
	_, err = store.GetForkCheckinBatch(ctx, receipt.ID)
	must("get batch", err)
	_, err = store.ListForkCheckinChildren(ctx, receipt.ID, forkcheckin.ListOptions{})
	must("list children", err)
	_, err = jobs.CancelJob(ctx, receipt.ID, forkcheckin.JobCancelRequest{RequestID: "request_detached_batch_cancel"})
	must("cancel batch", err)
	single, err := jobs.CreateJobs(ctx, forkcheckin.JobCreateRequest{
		RequestID: "request_detached_single", Action: forkcheckin.JobActionCheckIn,
		Accounts: []forkcheckin.AccountID{"acct_detached_b"},
	})
	must("create single", err)
	_, err = jobs.CancelJob(ctx, single.Launch[0], forkcheckin.JobCancelRequest{RequestID: "request_detached_cancel"})
	must("cancel single", err)

	accounts := forkCheckinAccountWriter{store: store}
	_, err = accounts.CreateAccount(ctx, forkcheckin.AccountDraftRequest{
		RequestID: "request_detached_create", DashboardBaseURL: "https://relay-d.example", TimeZone: "UTC",
	})
	must("writer create", err)
	zone := "UTC"
	_, err = accounts.UpdateAccount(ctx, "acct_detached_c", forkcheckin.AccountUpdateRequest{
		RequestID: "request_detached_update", ExpectedRevision: 1, TimeZone: &zone,
	})
	must("writer update", err)
	_, err = accounts.DeleteAccount(ctx, "acct_detached_c", forkcheckin.AccountDeleteRequest{
		RequestID: "request_detached_delete", ExpectedRevision: 2,
	})
	must("writer delete", err)
	must("delete session", sessions.Delete(ctx, account.ID))
	must("delete account", store.DeleteForkCheckinAccount(ctx, "acct_detached_b", 1))

	if calls := ctx.calls.Load(); calls != 0 {
		t.Fatalf("a cancellable context reached the shared pool %d time(s)", calls)
	}
	// The spy does see a statement that is not detached.
	var one int
	if err := store.db.QueryRowContext(ctx, `SELECT 1`).Scan(&one); err != nil || ctx.calls.Load() == 0 {
		t.Fatalf("spy missed a cancellable statement: calls=%d err=%v", ctx.calls.Load(), err)
	}
}

// Extension code reaches the shared pool only through forkCheckinDB. This
// keeps a later edit from reintroducing a cancellable statement.
func TestForkCheckinCodeUsesOnlyDetachedPool(t *testing.T) {
	files, err := filepath.Glob("fork_checkin_*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("list extension files: %v", err)
	}
	fileSet := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") || name == "fork_checkin_db.go" {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			if selector, ok := node.(*ast.SelectorExpr); ok && selector.Sel.Name == "db" {
				t.Errorf("%s: extension code uses the shared pool directly; use forkCheckinDB", fileSet.Position(selector.Pos()))
			}
			return true
		})
	}
}

// Before forkCheckinDB, a scheduler transaction cancelled at a random point
// left a zombie connection holding the writer lock within a few iterations.
func TestForkCheckinCancelledTransactionsNeverStrandWriterLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cancelled.db")
	store := openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_cancelled", "https://relay.example")); err != nil {
		t.Fatal(err)
	}
	probe, err := sql.Open(driverName, fmt.Sprintf("file:%s?_pragma=busy_timeout(50)", filepath.ToSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	defer probe.Close()
	for iteration := range 300 {
		cancelled, cancel := context.WithCancel(ctx)
		delay := time.Duration(rand.IntN(600)) * time.Microsecond
		go func() { time.Sleep(delay); cancel() }()
		_, _ = store.PrepareForkCheckinScheduledJob(cancelled, "acct_cancelled", time.Now())
		cancel()
		if _, err := probe.ExecContext(ctx, `BEGIN IMMEDIATE; ROLLBACK`); err != nil {
			t.Fatalf("writer lock stranded after iteration %d: %v", iteration, err)
		}
	}
}
