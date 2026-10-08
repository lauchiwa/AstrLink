package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// The check-in extension keeps its own tables and its own migration ledger.
// Nothing here is created by Open: a build with the extension disabled holds
// no extension table, so a database written by it stays readable by a core
// that does not know the extension at all.
//
// The ledger is deliberately not migrate.Runner: that runner owns
// schema_migrations and the main history reconciliation, and pointing it at a
// second ledger would entangle the two histories. An extension schema failure
// must disable the extension, never the main store.

const (
	// forkCheckinLedger is the extension's private migration ledger. The main
	// schema_migrations table is never written by this file.
	forkCheckinLedger = "fork_checkin_schema_migrations"

	forkCheckinAccounts    = "fork_checkin_accounts"
	forkCheckinBindings    = "fork_checkin_account_services"
	forkCheckinCredentials = "fork_checkin_credentials"
	forkCheckinJobs        = "fork_checkin_jobs"
	forkCheckinAttempts    = "fork_checkin_job_attempts"
	forkCheckinReceipts    = "fork_checkin_request_receipts"
	forkCheckinBatches     = "fork_checkin_batches"
	forkCheckinBatchJobs   = "fork_checkin_batch_jobs"
)

var (
	// ErrForkCheckinSchemaNewer reports an extension ledger written by a newer
	// build. Only the extension is refused; the main store stays usable.
	ErrForkCheckinSchemaNewer = errors.New("fork check-in schema is newer than this core")
	// ErrForkCheckinSchemaHistory reports an extension ledger whose recorded
	// steps differ from this build's.
	ErrForkCheckinSchemaHistory = errors.New("fork check-in migration history differs from this core")
)

// forkCheckinMigration mirrors migrate.Migration without borrowing its runner,
// so the extension history cannot be appended to the main one by mistake.
type forkCheckinMigration struct {
	version    int64
	name       string
	statements []string
}

// forkCheckinMigrations returns this build's full extension history. Steps are
// append-only: an existing version is never renamed or rewritten, because
// EnsureForkCheckinSchema verifies recorded names before applying anything.
//
// Every foreign key points at another fork_checkin_* table. The upstream
// services table gains no foreign key and no trigger, so removing a service
// never cascades into extension rows and an extension table can be dropped
// without touching the main schema.
func forkCheckinMigrations() []forkCheckinMigration {
	return []forkCheckinMigration{
		{
			version: 1,
			name:    "accounts",
			statements: []string{
				// The columns mirror forkcheckin.Account exactly. A column the
				// model cannot fill would either be dead or invite a second
				// source of truth, so there is none.
				fmt.Sprintf(`CREATE TABLE %s (
    id TEXT PRIMARY KEY,
    dashboard_base_url TEXT NOT NULL,
    remote_user_id TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL,
    time_zone TEXT NOT NULL,
    automatic INTEGER NOT NULL DEFAULT 0 CHECK(automatic IN (0, 1)),
    network_mode TEXT NOT NULL,
    network_proxy_url TEXT NOT NULL DEFAULT '',
    config_fingerprint TEXT NOT NULL DEFAULT '',
    revision INTEGER NOT NULL DEFAULT 1 CHECK(revision >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    -- A draft has no verified remote user, and automatic check-in
    -- needs a session the site has accepted. Both mirror
    -- forkcheckin.Account.Validate.
    CHECK (state <> 'draft' OR remote_user_id = ''),
    CHECK (state = 'draft' OR remote_user_id <> ''),
    CHECK (automatic = 0 OR state = 'connected')
)`, forkCheckinAccounts),
				// One site plus one remote user is one account, however many
				// services are bound to it. This is forkcheckin.AccountIdentity.
				// The remote user is unknown until the first successful
				// sign-in, so draft rows are excluded rather than colliding on
				// the empty string.
				fmt.Sprintf(`CREATE UNIQUE INDEX fork_checkin_accounts_identity
    ON %s(dashboard_base_url, remote_user_id)
    WHERE remote_user_id <> ''`, forkCheckinAccounts),
			},
		},
		{
			version: 2,
			name:    "account_services",
			statements: []string{
				// service_id is stored as plain text on purpose: a foreign key
				// into the upstream services table would make extension rows
				// part of the main schema's delete path.
				fmt.Sprintf(`CREATE TABLE %s (
    account_id TEXT NOT NULL REFERENCES %s(id) ON DELETE CASCADE,
    service_id TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (account_id, service_id)
)`, forkCheckinBindings, forkCheckinAccounts),
				fmt.Sprintf(`CREATE INDEX fork_checkin_account_services_service
    ON %s(service_id)`, forkCheckinBindings),
			},
		},
		{
			version: 3,
			name:    "credentials",
			statements: []string{
				// Only ciphertext is stored. The column holds a sealed
				// envelope bound to this table and account id, so a blob
				// copied into another row fails to open.
				fmt.Sprintf(`CREATE TABLE %s (
    account_id TEXT PRIMARY KEY REFERENCES %s(id) ON DELETE CASCADE,
    sealed_value BLOB NOT NULL CHECK(length(sealed_value) > 0),
    updated_at TEXT NOT NULL
)`, forkCheckinCredentials, forkCheckinAccounts),
			},
		},
		{
			version: 4,
			name:    "jobs",
			statements: []string{
				// dispatched and proof_source are columns rather than derived
				// values: once a submission has left the machine that fact
				// must survive a restart, and a success must record where its
				// proof came from.
				fmt.Sprintf(`CREATE TABLE %s (
    id TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES %s(id) ON DELETE CASCADE,
    parent_id TEXT REFERENCES %s(id) ON DELETE CASCADE,
    action TEXT NOT NULL,
    trigger TEXT NOT NULL DEFAULT 'manual',
    expected_revision INTEGER NOT NULL CHECK(expected_revision >= 1),
    status TEXT NOT NULL,
    dispatched INTEGER NOT NULL DEFAULT 0 CHECK(dispatched IN (0, 1)),
    proof_source TEXT NOT NULL DEFAULT 'none',
    request_id TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    site_date TEXT NOT NULL DEFAULT '',
    reward_known INTEGER NOT NULL DEFAULT 0 CHECK(reward_known IN (0, 1)),
    reward_quota INTEGER,
    reward_unit TEXT NOT NULL DEFAULT '',
    failure_code TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    finished_at TEXT NOT NULL DEFAULT '',
    CHECK (reward_known = 1 OR reward_quota IS NULL),
    CHECK (status <> 'success' OR proof_source <> 'none'),
    CHECK (proof_source <> 'submit_response' OR dispatched = 1)
)`, forkCheckinJobs, forkCheckinAccounts, forkCheckinJobs),
				// A retried write carries the same request_id, so the stored
				// job is found again instead of a second one being created.
				fmt.Sprintf(`CREATE UNIQUE INDEX fork_checkin_jobs_request
    ON %s(request_id)`, forkCheckinJobs),
				fmt.Sprintf(`CREATE INDEX fork_checkin_jobs_account
    ON %s(account_id, created_at)`, forkCheckinJobs),
				fmt.Sprintf(`CREATE INDEX fork_checkin_jobs_parent
    ON %s(parent_id)`, forkCheckinJobs),
			},
		},
		{
			version: 5,
			name:    "job_attempts",
			statements: []string{
				fmt.Sprintf(`CREATE TABLE %s (
    job_id TEXT NOT NULL REFERENCES %s(id) ON DELETE CASCADE,
    attempt INTEGER NOT NULL CHECK(attempt >= 1),
    phase TEXT NOT NULL,
    dispatched INTEGER NOT NULL DEFAULT 0 CHECK(dispatched IN (0, 1)),
    outcome TEXT NOT NULL,
    failure_code TEXT NOT NULL DEFAULT '',
    lease_owner TEXT NOT NULL DEFAULT '',
    lease_expires_at TEXT NOT NULL DEFAULT '',
    started_at TEXT NOT NULL,
    ended_at TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (job_id, attempt)
)`, forkCheckinAttempts, forkCheckinJobs),
			},
		},
		{
			version: 6,
			name:    "request_receipts",
			statements: []string{
				// The receipt is what makes a retried write idempotent across
				// restarts: the same request_id replays this stored response,
				// and the same id with different content is a conflict.
				fmt.Sprintf(`CREATE TABLE %s (
    request_id TEXT PRIMARY KEY,
    route TEXT NOT NULL,
    request_fingerprint TEXT NOT NULL,
    response_status INTEGER NOT NULL CHECK(response_status BETWEEN 100 AND 599),
    response_body TEXT NOT NULL,
    created_at TEXT NOT NULL
)`, forkCheckinReceipts),
				fmt.Sprintf(`CREATE INDEX fork_checkin_request_receipts_created
    ON %s(created_at)`, forkCheckinReceipts),
			},
		},
		{
			version: 7,
			name:    "job_receipt_fences_and_batches",
			statements: []string{
				`ALTER TABLE fork_checkin_request_receipts ADD COLUMN expected_revision INTEGER NOT NULL DEFAULT 0`,
				`ALTER TABLE fork_checkin_request_receipts ADD COLUMN job_snapshot TEXT NOT NULL DEFAULT ''`,
				`UPDATE fork_checkin_request_receipts SET job_snapshot = response_body WHERE route = 'jobs'`,
				`UPDATE fork_checkin_request_receipts SET expected_revision = COALESCE(
    (SELECT expected_revision FROM fork_checkin_jobs WHERE request_id = fork_checkin_request_receipts.request_id), 0)`,
				// Earlier DAO builds used RFC3339Nano, which drops fractional
				// zeroes and is not lexically sortable at second boundaries.
				`UPDATE fork_checkin_job_attempts SET lease_expires_at =
    substr(lease_expires_at, 1, 19) || '.' ||
    substr(CASE WHEN length(lease_expires_at) = 20 THEN '' ELSE substr(lease_expires_at, 21, length(lease_expires_at) - 21) END || '000000000', 1, 9) || 'Z'
WHERE lease_expires_at <> '' AND substr(lease_expires_at, -1) = 'Z'`,
				// A batch is not an executable account job. Deleting one account
				// must not cascade through its parent into other accounts' jobs.
				`CREATE TABLE fork_checkin_batches (
    id TEXT PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE REFERENCES fork_checkin_request_receipts(request_id)
)`,
				`CREATE TABLE fork_checkin_batch_jobs (
    batch_id TEXT NOT NULL REFERENCES fork_checkin_batches(id),
    job_id TEXT NOT NULL UNIQUE REFERENCES fork_checkin_jobs(id) ON DELETE CASCADE,
    PRIMARY KEY (batch_id, job_id)
)`,
				`CREATE INDEX fork_checkin_jobs_day ON fork_checkin_jobs(account_id, site_date, status)`,
				`CREATE INDEX fork_checkin_attempts_open ON fork_checkin_job_attempts(job_id, lease_expires_at) WHERE ended_at = ''`,
			},
		},
		{
			version: 8,
			name:    "daily_schedule_and_retry_deadlines",
			statements: []string{
				`ALTER TABLE fork_checkin_jobs ADD COLUMN schedule_day TEXT NOT NULL DEFAULT ''`,
				`ALTER TABLE fork_checkin_jobs ADD COLUMN retry_not_before TEXT NOT NULL DEFAULT ''`,
				`CREATE INDEX fork_checkin_jobs_schedule ON fork_checkin_jobs(account_id, schedule_day, action)`,
				`CREATE INDEX fork_checkin_jobs_unfinished ON fork_checkin_jobs(account_id, dispatched) WHERE status IN ('queued', 'running')`,
			},
		},
	}
}

// ForkCheckinSchemaVersion reports the extension schema version this build
// owns.
func ForkCheckinSchemaVersion() int64 {
	migrations := forkCheckinMigrations()
	if len(migrations) == 0 {
		return 0
	}
	return migrations[len(migrations)-1].version
}

// EnsureForkCheckinSchema creates or migrates the extension's tables. It is
// called only when the extension is being enabled; Open never calls it, so a
// disabled extension leaves no table, no index and no ledger behind.
//
// The whole history runs in one transaction: a step that fails leaves the
// database exactly as it was, including the ledger itself.
func (store *Store) EnsureForkCheckinSchema(ctx context.Context) error {
	// A read-only store is opened with query_only(1) rather than a Go field,
	// so ask SQLite instead of guessing from the Store value.
	readOnly, err := store.forkCheckinQueryOnly(ctx)
	if err != nil {
		return err
	}
	if readOnly {
		return fmt.Errorf("%w: read-only store cannot create the fork check-in schema", storagecontract.ErrInvalidArgument)
	}
	return store.applyForkCheckinMigrations(ctx, forkCheckinMigrations())
}

// forkCheckinQueryOnly reports whether this connection refuses writes.
func (store *Store) forkCheckinQueryOnly(ctx context.Context) (bool, error) {
	var queryOnly int
	if err := store.forkCheckinDB().QueryRowContext(ctx, `PRAGMA query_only`).Scan(&queryOnly); err != nil {
		return false, fmt.Errorf("read query_only pragma: %w", err)
	}
	return queryOnly != 0, nil
}

// RequireForkCheckinSchema reports whether the stored extension schema matches
// this build, without writing. A read-only reader and the disabled-extension
// status route use it instead of EnsureForkCheckinSchema.
func (store *Store) RequireForkCheckinSchema(ctx context.Context) error {
	present, err := store.forkCheckinLedgerExists(ctx)
	if err != nil {
		return err
	}
	if !present {
		return fmt.Errorf("%w: fork check-in schema is absent", storagecontract.ErrNotFound)
	}
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin fork check-in schema check: %w", err)
	}
	defer func() { _ = transaction.Rollback() }()
	migrations := forkCheckinMigrations()
	current, err := forkCheckinCurrentVersion(ctx, transaction)
	if err != nil {
		return err
	}
	if current > ForkCheckinSchemaVersion() {
		return fmt.Errorf("%w: database=%d core=%d", ErrForkCheckinSchemaNewer, current, ForkCheckinSchemaVersion())
	}
	if current < ForkCheckinSchemaVersion() {
		return fmt.Errorf("%w: database=%d core=%d", ErrForkCheckinSchemaHistory, current, ForkCheckinSchemaVersion())
	}
	return verifyForkCheckinHistory(ctx, transaction, migrations, current)
}

// ForkCheckinSchemaPresent reports whether any extension table exists. The
// status route answers from this while the extension is disabled, so it must
// not create anything.
func (store *Store) ForkCheckinSchemaPresent(ctx context.Context) (bool, error) {
	return store.forkCheckinLedgerExists(ctx)
}

func (store *Store) forkCheckinLedgerExists(ctx context.Context) (bool, error) {
	var exists bool
	err := store.forkCheckinDB().QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`,
		forkCheckinLedger,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("read fork check-in schema presence: %w", err)
	}
	return exists, nil
}

// applyForkCheckinMigrations runs one transactional pass over the given
// history. Tests pass a deliberately broken history to prove that a partial
// failure rolls back; production always passes forkCheckinMigrations().
func (store *Store) applyForkCheckinMigrations(ctx context.Context, migrations []forkCheckinMigration) (err error) {
	if err := validateForkCheckinMigrations(migrations); err != nil {
		return err
	}
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin fork check-in migration: %w", err)
	}
	defer func() {
		if err != nil {
			_ = transaction.Rollback()
		}
	}()

	if _, err = transaction.ExecContext(ctx, fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s (
    version INTEGER PRIMARY KEY,
    name TEXT NOT NULL,
    applied_at TEXT NOT NULL
)`, forkCheckinLedger)); err != nil {
		return fmt.Errorf("create fork check-in ledger: %w", err)
	}

	var current int64
	if current, err = forkCheckinCurrentVersion(ctx, transaction); err != nil {
		return err
	}
	var latest int64
	if len(migrations) > 0 {
		latest = migrations[len(migrations)-1].version
	}
	if current > latest {
		return fmt.Errorf("%w: database=%d core=%d", ErrForkCheckinSchemaNewer, current, latest)
	}
	// Names are verified before anything is applied: a version recorded under
	// another name means the step this build owns there never ran, and
	// skipping it by version alone would leave a half-built schema.
	if err = verifyForkCheckinHistory(ctx, transaction, migrations, current); err != nil {
		return err
	}

	now := store.now().UTC().Format(time.RFC3339Nano)
	for _, migration := range migrations {
		if migration.version <= current {
			continue
		}
		for _, statement := range migration.statements {
			if _, err = transaction.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply fork check-in migration %d (%s): %w", migration.version, migration.name, err)
			}
		}
		if _, err = transaction.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s (version, name, applied_at) VALUES (?, ?, ?)`, forkCheckinLedger),
			migration.version, migration.name, now,
		); err != nil {
			return fmt.Errorf("record fork check-in migration %d (%s): %w", migration.version, migration.name, err)
		}
	}
	if err = transaction.Commit(); err != nil {
		return fmt.Errorf("commit fork check-in migration: %w", err)
	}
	return nil
}

func validateForkCheckinMigrations(migrations []forkCheckinMigration) error {
	var previous int64
	for index, migration := range migrations {
		if migration.version <= 0 {
			return fmt.Errorf("%w: fork check-in migrations[%d] version must be positive", storagecontract.ErrInvalidArgument, index)
		}
		if migration.version <= previous {
			return fmt.Errorf("%w: fork check-in migrations[%d] versions must increase", storagecontract.ErrInvalidArgument, index)
		}
		if strings.TrimSpace(migration.name) == "" {
			return fmt.Errorf("%w: fork check-in migrations[%d] name is required", storagecontract.ErrInvalidArgument, index)
		}
		for statementIndex, statement := range migration.statements {
			if strings.TrimSpace(statement) == "" {
				return fmt.Errorf("%w: fork check-in migrations[%d].statements[%d] is empty", storagecontract.ErrInvalidArgument, index, statementIndex)
			}
		}
		previous = migration.version
	}
	return nil
}

func forkCheckinCurrentVersion(ctx context.Context, transaction *sql.Tx) (int64, error) {
	var current int64
	if err := transaction.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COALESCE(MAX(version), 0) FROM %s`, forkCheckinLedger),
	).Scan(&current); err != nil {
		return 0, fmt.Errorf("read fork check-in schema version: %w", err)
	}
	return current, nil
}

// verifyForkCheckinHistory requires the ledger to record exactly this build's
// steps through current, and nothing else.
func verifyForkCheckinHistory(ctx context.Context, transaction *sql.Tx, migrations []forkCheckinMigration, current int64) error {
	var expected int64
	for _, migration := range migrations {
		if migration.version > current {
			break
		}
		expected++
		var name string
		err := transaction.QueryRowContext(ctx,
			fmt.Sprintf(`SELECT name FROM %s WHERE version = ?`, forkCheckinLedger), migration.version,
		).Scan(&name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("%w: migration %d (%s) is not recorded", ErrForkCheckinSchemaHistory, migration.version, migration.name)
		case err != nil:
			return fmt.Errorf("read recorded fork check-in migration %d: %w", migration.version, err)
		case name != migration.name:
			return fmt.Errorf("%w: migration %d is recorded as %q, want %q", ErrForkCheckinSchemaHistory, migration.version, name, migration.name)
		}
	}
	var recorded int64
	if err := transaction.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT COUNT(*) FROM %s`, forkCheckinLedger),
	).Scan(&recorded); err != nil {
		return fmt.Errorf("count recorded fork check-in migrations: %w", err)
	}
	if recorded != expected {
		return fmt.Errorf("%w: %d migrations are recorded through version %d, want %d", ErrForkCheckinSchemaHistory, recorded, current, expected)
	}
	return nil
}
