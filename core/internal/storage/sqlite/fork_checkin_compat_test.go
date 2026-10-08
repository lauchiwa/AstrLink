package sqlite

import (
	"bytes"
	"context"
	"database/sql/driver"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	"github.com/QuantumNous/astrlink/core/internal/localkey"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
	sqlitedriver "modernc.org/sqlite"
)

// This is the no-check-in HEAD used for the compatibility gate, not upstream
// migration 48. Pin it so committing the extension cannot silently turn the
// "old reader" into the new implementation under test.
const forkCheckinBaselineCommit = "e7ebad25b30e6fc78bfb0145cddfaac5fad662a2"
const forkCheckinCompatSession = "CHECKIN-COMPAT-SESSION-CANARY"

//go:embed fork_checkin_baseline_test.go
var forkCheckinBaselineProbe []byte

func seedForkCheckinExtension(t *testing.T, store *Store) forkcheckin.Job {
	t.Helper()
	ctx := context.Background()
	if err := store.EnsureForkCheckinSchema(ctx); err != nil {
		t.Fatal(err)
	}
	account := forkCheckinDraft("acct_compat", "https://relay.example/")
	account.State, account.RemoteUserID, account.Automatic = forkcheckin.AccountStateConnected, "7", true
	account.BoundServices = []contract.ServiceID{forkCheckinCompatService}
	if _, err := store.CreateForkCheckinAccount(ctx, account); err != nil {
		t.Fatal(err)
	}
	if err := store.ForkCheckinSessions().Put(ctx, account.ID, []byte(forkCheckinCompatSession)); err != nil {
		t.Fatal(err)
	}
	job := createForkCheckinJob(t, store, forkCheckinJobDraft(account.ID, "request_compat_01", 'c')).Job
	lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkForkCheckinDispatched(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CompleteForkCheckinJob(ctx, lease, ForkCheckinCompletion{
		Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceSubmitResponse,
		Reward: forkcheckin.Reward{Known: true, Quota: 3, Unit: "quota"},
	}); err != nil {
		t.Fatal(err)
	}
	return job
}

func assertForkCheckinSessionReadable(t *testing.T, store *Store) {
	t.Helper()
	value, err := store.ForkCheckinSessions().Get(context.Background(), "acct_compat")
	defer clear(value)
	if err != nil || !bytes.Equal(value, []byte(forkCheckinCompatSession)) {
		t.Fatalf("original check-in session no longer decrypts: %v", err)
	}
}

func buildForkCheckinBaseline(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("baseline source build is an integration gate; run without -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rootBytes, err := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("baseline gate requires a git checkout: %v", err)
	}
	root := strings.TrimSpace(string(rootBytes))
	run := func(directory string, name string, args ...string) []byte {
		t.Helper()
		command := exec.CommandContext(ctx, name, args...)
		command.Dir = directory
		command.Env = append(os.Environ(), "GOWORK=off")
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("baseline command %s %v: %v\n%s", name, args, err, output)
		}
		return output
	}
	if err := exec.CommandContext(ctx, "git", "cat-file", "-e", forkCheckinBaselineCommit+"^{commit}").Run(); err != nil {
		t.Skipf("baseline commit unavailable (shallow archive): %v", err)
	}
	paths := run(root, "git", "ls-tree", "-r", "--name-only", forkCheckinBaselineCommit, "core/internal/storage/sqlite", "core/internal/forkcheckin")
	if bytes.Contains(paths, []byte("fork_checkin_")) || bytes.Contains(paths, []byte("/forkcheckin/")) {
		t.Fatal("pinned baseline already contains the check-in extension")
	}
	directory := t.TempDir()
	archive := filepath.Join(directory, "baseline.tar")
	run(root, "git", "archive", "--format=tar", "--output="+archive, forkCheckinBaselineCommit, "core", "convo")
	run(directory, "tar", "-xf", archive)
	probe := filepath.Join(directory, "core", "internal", "storage", "sqlite", "fork_checkin_baseline_test.go")
	if err := os.WriteFile(probe, forkCheckinBaselineProbe, 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(directory, "baseline.test")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	run(filepath.Join(directory, "core"), "go", "test", "-mod=readonly", "-c", "-o", binary, "./internal/storage/sqlite")
	t.Logf("built independent no-extension storage binary from %s", forkCheckinBaselineCommit)
	return binary
}

func runForkCheckinBaseline(t *testing.T, binary, path, report, mode string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "-test.run=^TestForkCheckinBaselineProcess$")
	command.Env = append(os.Environ(), "ASTRLINK_CHECKIN_BASELINE_DB="+path,
		"ASTRLINK_CHECKIN_BASELINE_REPORT="+report, "ASTRLINK_CHECKIN_BASELINE_MODE="+mode)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("baseline %s: %v\n%s", mode, err, output)
	}
}

func TestForkCheckinCompatBaselineUpgradeAndDisabledReopen(t *testing.T) {
	ctx := context.Background()
	binary := buildForkCheckinBaseline(t)
	path := filepath.Join(t.TempDir(), "baseline.db")
	reportPath := path + ".report.json"
	runForkCheckinBaseline(t, binary, path, reportPath, "create")
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report forkCheckinBaselineReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	t.Logf("baseline main schema=%d, tables=%d", report.Version, len(report.Main.Tables))
	store := openWithKey(t, path, testLocalKey(t, forkCheckinCompatKeyByte), nil)
	assertForkCheckinDatabaseDigest(t, report.Main, snapshotForkCheckinDatabase(t, store, true))
	if present, err := store.ForkCheckinSchemaPresent(ctx); err != nil || present {
		t.Fatalf("plain Open initialized the extension: %v %v", present, err)
	}
	seedForkCheckinExtension(t, store)
	account, err := store.GetForkCheckinAccount(ctx, "acct_compat")
	if err != nil {
		t.Fatal(err)
	}
	account.Automatic = false
	if _, err := store.UpdateForkCheckinAccount(ctx, account, 1); err != nil {
		t.Fatal(err)
	}
	assertForkCheckinDatabaseDigest(t, report.Main, snapshotForkCheckinDatabase(t, store, true))
	assertForkCheckinBaselineReadable(t, store, report)
	beforeDisabled := snapshotForkCheckinDatabase(t, store, false)
	backup := filepath.Join(t.TempDir(), "baseline-backup.db")
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// There is no runtime switch in this storage task. A disabled startup is
	// represented by plain Open with no Ensure call and no extension workers.
	store = openWithKey(t, path, testLocalKey(t, forkCheckinCompatKeyByte), nil)
	assertForkCheckinDatabaseDigest(t, beforeDisabled, snapshotForkCheckinDatabase(t, store, false))
	assertForkCheckinSessionReadable(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{path, backup} {
		for _, mode := range []string{"readonly", "offline", "normal"} {
			runForkCheckinBaseline(t, binary, candidate, reportPath, mode)
		}
	}
	// Unknown extension history must also be irrelevant to the baseline.
	store = openWithKey(t, path, testLocalKey(t, forkCheckinCompatKeyByte), nil)
	mustExec(t, store, `INSERT INTO fork_checkin_schema_migrations VALUES (?, 'future', 'test')`, ForkCheckinSchemaVersion()+1)
	if err := store.EnsureForkCheckinSchema(ctx); !errors.Is(err, ErrForkCheckinSchemaNewer) {
		t.Fatalf("new extension history was accepted: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	runForkCheckinBaseline(t, binary, path, reportPath, "normal")
}

func TestForkCheckinCompatReadOnlyAndOfflineReaders(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "readers.db")
	store, report := seedForkCheckinBaseline(t, path)
	defer store.Close()
	job := seedForkCheckinExtension(t, store)
	before := snapshotForkCheckinDatabase(t, store, false)
	files := databaseFiles(t, path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := Open(ctx, path, WithLocalKey(testLocalKey(t, forkCheckinCompatKeyByte)), WithReadOnly())
	if err != nil {
		t.Fatal(err)
	}
	assertForkCheckinBaselineReadable(t, reader, report)
	assertForkCheckinSessionReadable(t, reader)
	if _, err := reader.LookupForkCheckinReceipt(ctx, job.RequestID); err != nil {
		t.Fatal(err)
	}
	writes := []func() error{
		func() error { return reader.EnsureForkCheckinSchema(ctx) },
		func() error { return reader.ForkCheckinSessions().Put(ctx, "acct_compat", []byte("replacement")) },
		func() error { return reader.ForkCheckinSessions().Delete(ctx, "acct_compat") },
		func() error {
			_, err := reader.CreateForkCheckinAccount(ctx, forkCheckinDraft("acct_readonly", "https://relay.example/"))
			return err
		},
		func() error {
			_, err := reader.CreateForkCheckinJobOnce(ctx, forkCheckinJobDraft("acct_compat", "request_readonly_write", 'w'))
			return err
		},
		func() error { return reader.DeleteForkCheckinAccount(ctx, "acct_compat", 1) },
	}
	for index, write := range writes {
		if err := write(); err == nil {
			t.Fatalf("read-only extension write %d succeeded", index)
		}
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(files, databaseFiles(t, path)) {
		t.Fatal("read-only access changed database/WAL bytes (SHM lock bookkeeping excluded)")
	}
	afterInfo, err := os.Stat(path)
	if err != nil || info.Mode() != afterInfo.Mode() {
		t.Fatalf("read-only open changed database permissions: %v", err)
	}
	assertForkCheckinDatabaseDigest(t, before, snapshotForkCheckinDatabase(t, store, false))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	offline, err := Open(ctx, path, WithLocalKey(testLocalKey(t, forkCheckinCompatKeyByte)), WithExistingDatabase())
	if err != nil {
		t.Fatal(err)
	}
	defer offline.Close()
	assertForkCheckinDatabaseDigest(t, before, snapshotForkCheckinDatabase(t, offline, false))
	assertForkCheckinBaselineReadable(t, offline, report)
	assertForkCheckinSessionReadable(t, offline)
}

func TestForkCheckinCompatBackupAndExtensionDamageStayIsolated(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "source.db")
	store, report := seedForkCheckinBaseline(t, path)
	defer store.Close()
	job := seedForkCheckinExtension(t, store)
	before := snapshotForkCheckinDatabase(t, store, false)
	originalReceipt, err := store.LookupForkCheckinReceipt(ctx, job.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	backup := filepath.Join(t.TempDir(), "backup.db")
	// A live WAL database cannot be backed up by copying only its .db file.
	// VACUUM INTO gives this test a consistent SQLite-owned snapshot.
	if _, err := store.db.ExecContext(ctx, `VACUUM INTO ?`, backup); err != nil {
		t.Fatal(err)
	}
	copyStore, err := Open(ctx, backup, WithLocalKey(testLocalKey(t, forkCheckinCompatKeyByte)), WithExistingDatabase())
	if err != nil {
		t.Fatal(err)
	}
	defer copyStore.Close()
	assertForkCheckinDatabaseDigest(t, before, snapshotForkCheckinDatabase(t, copyStore, false))
	assertForkCheckinBaselineReadable(t, copyStore, report)
	assertForkCheckinSessionReadable(t, copyStore)
	receipt, err := copyStore.LookupForkCheckinReceipt(ctx, job.RequestID)
	if err != nil || !bytes.Equal(receipt.Body, originalReceipt.Body) {
		t.Fatalf("backup changed accepted receipt: %v", err)
	}
	mustExec(t, copyStore, `UPDATE fork_checkin_credentials SET sealed_value = ?`, []byte("damaged envelope"))
	if _, err := copyStore.ForkCheckinSessions().Get(ctx, "acct_compat"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("corrupt extension credential: %v", err)
	}
	mustExec(t, copyStore, `UPDATE fork_checkin_request_receipts SET response_body = '{' WHERE request_id = ?`, job.RequestID)
	if _, err := copyStore.LookupForkCheckinReceipt(ctx, job.RequestID); !errors.Is(err, storagecontract.ErrInvalidRecord) {
		t.Fatalf("corrupt extension receipt: %v", err)
	}
	mustExec(t, copyStore, `UPDATE fork_checkin_schema_migrations SET name = 'diverged' WHERE version = 1`)
	if err := copyStore.RequireForkCheckinSchema(ctx); !errors.Is(err, ErrForkCheckinSchemaHistory) {
		t.Fatalf("diverged extension history: %v", err)
	}
	mustExec(t, copyStore, `DROP TABLE fork_checkin_credentials`)
	if _, err := copyStore.ForkCheckinSessions().Get(ctx, "acct_compat"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("missing extension table: %v", err)
	}
	assertForkCheckinBaselineReadable(t, copyStore, report)
	assertForkCheckinDatabaseDigest(t, report.Main, snapshotForkCheckinDatabase(t, copyStore, true))
	assertForkCheckinDatabaseDigest(t, before, snapshotForkCheckinDatabase(t, store, false))
	assertForkCheckinSessionReadable(t, store)
	var integrity string
	if err := copyStore.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("extension row damage affected SQLite integrity: %q %v", integrity, err)
	}
}

func TestForkCheckinCompatMissingKeyDoesNotReplaceSession(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "missing-key.db")
	store, report := seedForkCheckinBaseline(t, path)
	defer store.Close()
	seedForkCheckinExtension(t, store)
	before := snapshotForkCheckinDatabase(t, store, false)
	// Simulate an unavailable in-memory keystore without invoking the main
	// repair policy. There are no workers in this storage-only test.
	keys := store.keys
	store.keys = nil
	defer func() { store.keys = keys }()
	if _, err := store.ForkCheckinSessions().Get(ctx, "acct_compat"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("missing-key read: %v", err)
	}
	if err := store.ForkCheckinSessions().Put(ctx, "acct_compat", []byte("must-not-replace")); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Errorf("missing-key write must use the extension error domain: %v", err)
	}
	store.keys = keys
	assertForkCheckinDatabaseDigest(t, before, snapshotForkCheckinDatabase(t, store, false))
	assertForkCheckinSessionReadable(t, store)
	assertForkCheckinBaselineReadable(t, store, report)
	files := databaseFiles(t, path)
	for _, option := range []Option{WithReadOnly(), WithExistingDatabase()} {
		if _, err := Open(ctx, path, option); !errors.Is(err, storagecontract.ErrInvalidArgument) {
			t.Fatalf("offline open without supplied key: %v", err)
		}
		if _, err := Open(ctx, path, WithLocalKey(testLocalKey(t, 0x54)), option); !errors.Is(err, ErrLocalKeyMismatch) {
			t.Fatalf("offline open with wrong key: %v", err)
		}
	}
	if !reflect.DeepEqual(files, databaseFiles(t, path)) {
		t.Fatal("strict key refusal changed database/WAL bytes")
	}
}

func TestForkCheckinCompatOriginalKeyRestoresOriginalSession(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	path := filepath.Join(directory, "recovery.db")
	store, report := seedForkCheckinBaseline(t, path)
	seedForkCheckinExtension(t, store)
	envelopes := readEnvelopes(t, store)
	before := snapshotForkCheckinDatabase(t, store, false)
	keyPath := filepath.Join(directory, localkey.FileName)
	key := testLocalKey(t, forkCheckinCompatKeyByte)
	defer clear(key)
	if err := os.WriteFile(keyPath, localkey.Encode(key), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(keyPath, keyPath+".saved"); err != nil {
		t.Fatal(err)
	}
	var logs []string
	logf := func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }
	store, err := Open(ctx, path, WithLogger(logf))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ForkCheckinSessions().Get(ctx, "acct_compat"); !errors.Is(err, forkcheckin.ErrCredentialUnavailable) {
		t.Fatalf("session readable after key loss: %v", err)
	}
	if _, err := store.Get(ctx, secretstore.Ref("local://service/"+forkCheckinCompatService)); !errors.Is(err, secretstore.ErrUnavailable) {
		t.Fatalf("main secret key-loss policy changed: %v", err)
	}
	if _, err := store.GetService(ctx, forkCheckinCompatService); err != nil {
		t.Fatalf("key loss blocked non-secret service reads: %v", err)
	}
	afterLoss := snapshotForkCheckinDatabase(t, store, false)
	// The existing main repair policy creates/reassigns envelope rows. All
	// other rows, including both sets of ciphertext, must remain identical.
	afterLoss.Tables["key_envelopes"] = before.Tables["key_envelopes"]
	assertForkCheckinDatabaseDigest(t, before, afterLoss)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(keyPath+".saved", keyPath); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path, WithLogger(logf))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertForkCheckinSessionReadable(t, store)
	assertForkCheckinBaselineReadable(t, store, report)
	recoveredEnvelopes := readEnvelopes(t, store)
	if len(envelopes) != 2 || len(recoveredEnvelopes) != 4 {
		t.Fatalf("unexpected repair envelope history: before=%d after=%d", len(envelopes), len(recoveredEnvelopes))
	}
	for kind, original := range envelopes {
		if !bytes.Equal(original.wrapped, recoveredEnvelopes[kind].wrapped) || !bytes.Equal(original.nonce, recoveredEnvelopes[kind].nonce) {
			t.Fatalf("original %s envelope was not restored", kind)
		}
	}
	afterRecovery := snapshotForkCheckinDatabase(t, store, false)
	afterRecovery.Tables["key_envelopes"] = before.Tables["key_envelopes"]
	assertForkCheckinDatabaseDigest(t, before, afterRecovery)
	for _, line := range logs {
		if strings.Contains(line, forkCheckinCompatSession) || strings.Contains(line, forkCheckinCompatSecret) {
			t.Fatal("key-loss log contained credential plaintext")
		}
	}
}

func TestForkCheckinCompatCrashProcess(t *testing.T) {
	path, mode := os.Getenv("ASTRLINK_CHECKIN_CRASH_DB"), os.Getenv("ASTRLINK_CHECKIN_CRASH_MODE")
	if path == "" {
		t.Skip("crash subprocess helper")
	}
	if err := sqlitedriver.RegisterScalarFunction("checkin_test_crash", 0, func(*sqlitedriver.FunctionContext, []driver.Value) (driver.Value, error) {
		os.Exit(86) // No deferred rollback, Close or Go unwinding.
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	store := openWithKey(t, path, testLocalKey(t, forkCheckinCompatKeyByte), nil)
	store.db.SetMaxOpenConns(1)
	store.db.SetMaxIdleConns(1)
	ctx := context.Background()
	switch mode {
	case "account":
		mustExec(t, store, `CREATE TEMP TRIGGER checkin_crash BEFORE INSERT ON fork_checkin_account_services BEGIN SELECT checkin_test_crash(); END`)
		account := forkCheckinDraft("acct_interrupted", "https://relay.example/")
		account.BoundServices = []contract.ServiceID{forkCheckinCompatService}
		_, err := store.CreateForkCheckinAccount(ctx, account)
		t.Fatalf("account write did not reach crash injection: %v", err)
	case "receipt":
		mustExec(t, store, `CREATE TEMP TRIGGER checkin_crash BEFORE INSERT ON fork_checkin_request_receipts BEGIN SELECT checkin_test_crash(); END`)
		_, err := store.CreateForkCheckinJobOnce(ctx, forkCheckinJobDraft("acct_crash", "request_crash_01", 'c'))
		t.Fatalf("receipt write did not reach crash injection: %v", err)
	case "dispatch":
		job := createForkCheckinJob(t, store, forkCheckinJobDraft("acct_crash", "request_crash_01", 'c')).Job
		lease, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := store.MarkForkCheckinDispatched(ctx, lease); err != nil {
			t.Fatal(err)
		}
		os.Exit(86)
	default:
		t.Fatalf("unknown crash mode %q", mode)
	}
}

func TestForkCheckinCompatProcessCrashRollsBackOrRetainsDispatch(t *testing.T) {
	for _, mode := range []string{"account", "receipt", "dispatch"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "crash.db")
			store, report := seedForkCheckinBaseline(t, path)
			if err := store.EnsureForkCheckinSchema(ctx); err != nil {
				t.Fatal(err)
			}
			createForkCheckinJobAccount(t, store, "acct_crash")
			before := snapshotForkCheckinDatabase(t, store, false)
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			command := exec.CommandContext(childCtx, executable, "-test.run=^TestForkCheckinCompatCrashProcess$")
			command.Env = append(os.Environ(), "ASTRLINK_CHECKIN_CRASH_DB="+path, "ASTRLINK_CHECKIN_CRASH_MODE="+mode)
			output, err := command.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 86 {
				t.Fatalf("child did not terminate at the injected crash: %v\n%s", err, output)
			}
			store = openWithKey(t, path, testLocalKey(t, forkCheckinCompatKeyByte), nil)
			defer store.Close()
			assertForkCheckinBaselineReadable(t, store, report)
			assertForkCheckinDatabaseDigest(t, report.Main, snapshotForkCheckinDatabase(t, store, true))
			if mode != "dispatch" {
				assertForkCheckinDatabaseDigest(t, before, snapshotForkCheckinDatabase(t, store, false))
				return
			}
			receipt, err := store.LookupForkCheckinJobReceipt(ctx, "request_crash_01")
			if err != nil {
				t.Fatal(err)
			}
			job, err := store.GetForkCheckinJob(ctx, receipt.ID)
			if err != nil || !job.Dispatched || job.Status != forkcheckin.JobStatusRunning {
				t.Fatalf("committed dispatch was lost: %+v %v", job, err)
			}
			store.now = func() time.Time { return time.Now().UTC().Add(time.Hour) }
			if _, err := store.ClaimForkCheckin(ctx, job.AccountID, job.Action, job.SiteDay, 1, time.Minute); !errors.Is(err, ErrForkCheckinLeaseHeld) {
				t.Fatalf("crashed dispatch was resubmitted: %v", err)
			}
			recovery, err := store.RecoverForkCheckinDispatch(ctx, job.ID, job.AccountID, 1, time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.CompleteForkCheckinJob(ctx, recovery, ForkCheckinCompletion{
				Status: forkcheckin.JobStatusSuccess, ProofSource: forkcheckin.ProofSourceStatusRecheck, ReadOnly: true,
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
