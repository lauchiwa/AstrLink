package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

var _ forkcheckin.SchedulerStore = (*Store)(nil)

func (store *Store) ListForkCheckinScheduledAccounts(ctx context.Context, after forkcheckin.AccountID, limit int) ([]forkcheckin.ScheduledAccount, error) {
	if limit < 1 || limit > forkcheckin.MaxPageSize || after != "" && after.Validate() != nil {
		return nil, storagecontract.ErrInvalidArgument
	}
	rows, err := store.forkCheckinDB().QueryContext(ctx, `SELECT a.id, a.dashboard_base_url FROM fork_checkin_accounts a
WHERE a.id > ? AND (a.automatic = 1 OR EXISTS (
 SELECT 1 FROM fork_checkin_jobs j WHERE j.account_id = a.id AND j.status IN ('queued', 'running')))
ORDER BY a.id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []forkcheckin.ScheduledAccount
	for rows.Next() {
		var account forkcheckin.ScheduledAccount
		if err := rows.Scan(&account.ID, &account.DashboardBaseURL); err != nil {
			return nil, err
		}
		result = append(result, account)
	}
	return result, rows.Err()
}

// Prepare does local bookkeeping only. Its transaction both checks the current
// consent and inserts a caller-fixed request receipt, so two processes cannot
// allocate different jobs for the same daily slot. No network runs here.
func (store *Store) PrepareForkCheckinScheduledJob(ctx context.Context, id forkcheckin.AccountID, now time.Time) (jobID forkcheckin.JobID, err error) {
	if id.Validate() != nil || now.IsZero() {
		return "", storagecontract.ErrInvalidArgument
	}
	tx, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return "", err
	}
	defer rollbackOnError(tx, &err)
	if _, err = lockForkCheckinAccount(ctx, tx, id); err != nil {
		return "", err
	}
	account, err := scanForkCheckinAccount(tx.QueryRowContext(ctx, forkCheckinAccountSelect+` WHERE id = ?`, id))
	if err != nil {
		return "", err
	}
	location, err := time.LoadLocation(account.TimeZone)
	if err != nil {
		return "", storagecontract.ErrInvalidRecord
	}
	today := now.In(location).Format(time.DateOnly)
	now = now.UTC()
	if err = expireForkCheckinAttempts(ctx, tx, id, formatForkCheckinTime(now)); err != nil {
		return "", err
	}
	var leased bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fork_checkin_job_attempts a JOIN fork_checkin_jobs j ON j.id = a.job_id
WHERE j.account_id = ? AND a.ended_at = '' AND a.lease_owner <> '' AND a.lease_expires_at > ?)`, id, formatForkCheckinTime(now)).Scan(&leased)
	if err != nil {
		return "", err
	}
	if leased {
		return "", tx.Commit()
	}

	// Dispatched work always recovers first, including old dates and revoked
	// automatic consent. Recovery cannot submit. Bound local cancellations per
	// wakeup, rather than loading all historical jobs into memory.
	for range forkcheckin.MaxPageSize {
		job, readErr := scanForkCheckinJob(tx.QueryRowContext(ctx, forkCheckinJobSelect+`
WHERE j.account_id = ? AND j.status IN ('queued', 'running') ORDER BY j.dispatched DESC, j.rowid LIMIT 1`, id))
		if errors.Is(readErr, sql.ErrNoRows) {
			break
		}
		if readErr != nil {
			return "", readErr
		}
		if job.Dispatched {
			return job.ID, tx.Commit()
		}
		bucket := job.ScheduleDay
		if bucket == "" {
			bucket = job.CreatedAt.In(location).Format(time.DateOnly)
		}
		code := ""
		switch {
		case job.ExpectedRevision != account.Revision:
			code = "account_changed"
		case job.Trigger == forkcheckin.JobTriggerAutomatic && !account.Automatic:
			code = "cancelled"
		case bucket != today:
			code = "day_expired"
		}
		if code == "" && job.Action == forkcheckin.JobActionCheckIn {
			blocked, checkErr := forkCheckinDayBlocked(ctx, tx, id, job.SiteDay, job.ScheduleDay, job.ID)
			if checkErr != nil {
				return "", checkErr
			}
			if blocked {
				code = "day_resolved"
			}
		}
		if code == "" && job.ScheduleDay != "" && job.Action == forkcheckin.JobActionCheckIn {
			attempts, countErr := forkCheckinScheduledAttempts(ctx, tx, id, today)
			if countErr != nil {
				return "", countErr
			}
			if attempts >= forkcheckin.AutomaticAttemptLimit {
				code = "retry_exhausted"
			}
		}
		if code == "" && job.ScheduleDay != "" && job.Attempts > 0 {
			if job.Action == forkcheckin.JobActionStatusRefresh && job.Attempts >= forkcheckin.AutomaticAttemptLimit {
				code = "retry_exhausted"
			} else {
				// An expired pre-dispatch attempt is a safe failure too. Its
				// persisted end time anchors backoff across repeated restarts.
				var ended string
				if err = tx.QueryRowContext(ctx, `SELECT ended_at FROM fork_checkin_job_attempts WHERE job_id = ? ORDER BY attempt DESC LIMIT 1`, job.ID).Scan(&ended); err != nil {
					return "", err
				}
				finished, parseErr := parseForkCheckinTime(ended)
				if parseErr != nil {
					return "", parseErr
				}
				if now.Before(finished.Add(forkcheckin.AutomaticRetryDelay(job.Attempts))) {
					return "", tx.Commit()
				}
			}
		}
		if code == "" {
			return job.ID, tx.Commit()
		}
		if err = cancelForkCheckinScheduledJob(ctx, tx, job, now, code); err != nil {
			return "", err
		}
	}
	// If a large backlog remains, drain it on another local wakeup first.
	var unfinished bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fork_checkin_jobs WHERE account_id = ? AND status IN ('queued','running'))`, id).Scan(&unfinished); err != nil {
		return "", err
	}
	if unfinished || !account.Automatic || account.State != forkcheckin.AccountStateConnected {
		return "", tx.Commit()
	}

	// Authentication and human-only failures are not daily retries. An
	// explicit account revision change (for example reconnecting) is required.
	var needsOperator bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fork_checkin_jobs WHERE account_id = ? AND expected_revision = ? AND status IN ('auth_required','manual_required','unsupported'))`, id, account.Revision).Scan(&needsOperator); err != nil {
		return "", err
	}
	if needsOperator {
		return "", tx.Commit()
	}
	// A server deadline survives midnight as well as a restart. A new local
	// daily bucket is not permission to ignore Retry-After from the last one.
	var delayed bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fork_checkin_jobs WHERE account_id = ? AND retry_not_before > ?)`, id, formatForkCheckinTime(now)).Scan(&delayed); err != nil {
		return "", err
	}
	if delayed {
		return "", tx.Commit()
	}
	var done bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fork_checkin_jobs WHERE account_id = ?
AND (site_date = ? OR schedule_day = ?) AND status IN ('success','already_checked'))`, id, today, today).Scan(&done); err != nil {
		return "", err
	}
	if done {
		return "", tx.Commit()
	}
	var dispatchedDay string
	err = tx.QueryRowContext(ctx, `SELECT site_date FROM fork_checkin_jobs WHERE account_id = ?
AND (site_date = ? OR schedule_day = ?) AND action = 'check_in' AND dispatched = 1 ORDER BY rowid DESC LIMIT 1`, id, today, today).Scan(&dispatchedDay)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	action, day := forkcheckin.JobActionCheckIn, today
	if dispatchedDay != "" {
		action, day = forkcheckin.JobActionStatusRefresh, dispatchedDay
	}
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM fork_checkin_jobs WHERE account_id = ? AND schedule_day = ? AND action = ?`, id, today, action).Scan(&count); err != nil {
		return "", err
	}
	// One read-only supplement per day; it does not mutate the uncertain job
	// or publish its historical award. Safe submission retries get three slots.
	if action == forkcheckin.JobActionStatusRefresh && count > 0 || count >= forkcheckin.AutomaticAttemptLimit {
		return "", tx.Commit()
	}
	if action == forkcheckin.JobActionCheckIn && count > 0 {
		last, readErr := scanForkCheckinJob(tx.QueryRowContext(ctx, forkCheckinJobSelect+`
WHERE j.account_id = ? AND j.schedule_day = ? AND j.action = 'check_in' ORDER BY j.rowid DESC LIMIT 1`, id, today))
		if readErr != nil {
			return "", readErr
		}
		if last.Dispatched || last.Status != forkcheckin.JobStatusRetryableFailure && last.Status != forkcheckin.JobStatusRateLimited {
			return "", tx.Commit()
		}
		attempts, countErr := forkCheckinScheduledAttempts(ctx, tx, id, today)
		if countErr != nil {
			return "", countErr
		}
		if attempts >= forkcheckin.AutomaticAttemptLimit {
			return "", tx.Commit()
		}
		due := last.CompletedAt.Add(forkcheckin.AutomaticRetryDelay(attempts))
		if last.RetryNotBefore != nil && last.RetryNotBefore.After(due) {
			due = *last.RetryNotBefore
		}
		if now.Before(due) {
			return "", tx.Commit()
		}
	}
	// No configuration revision is in this key: toggling auto, editing bindings
	// or moving the clock backwards must not recreate a consumed daily slot.
	hash := sha256.Sum256([]byte(fmt.Sprintf("%s\n%s\n%s\n%d", id, today, action, count)))
	request := forkcheckin.JobRequest{RequestID: "auto_" + hex.EncodeToString(hash[:]), AccountIDs: []forkcheckin.AccountID{id}, Action: action, Trigger: forkcheckin.JobTriggerAutomatic, ExpectedRevision: account.Revision}
	job, err := prepareForkCheckinJob(store, forkcheckin.Job{AccountID: id, Action: action, Trigger: request.Trigger,
		RequestID: request.RequestID, InputDigest: request.Digest(), ExpectedRevision: account.Revision,
		SiteDay: day, ScheduleDay: today, Status: forkcheckin.JobStatusQueued, ProofSource: forkcheckin.ProofSourceNone, CreatedAt: now})
	if err != nil {
		return "", err
	}
	if err = insertForkCheckinJob(ctx, tx, job); err != nil {
		return "", err
	}
	body, err := json.Marshal(job.Receipt().Public())
	if err != nil {
		return "", err
	}
	snapshot, err := json.Marshal(job.Receipt())
	if err != nil {
		return "", err
	}
	if err = insertForkCheckinReceipt(ctx, tx, ForkCheckinStoredReceipt{RequestID: job.RequestID, Route: "jobs", Status: 202, Body: body, CreatedAt: now, snapshot: snapshot}, job.InputDigest, account.Revision); err != nil {
		return "", err
	}
	return job.ID, tx.Commit()
}

func cancelForkCheckinScheduledJob(ctx context.Context, tx *sql.Tx, job forkcheckin.Job, now time.Time, code string) error {
	if now.Before(job.CreatedAt) {
		now = job.CreatedAt
	}
	_, err := tx.ExecContext(ctx, `UPDATE fork_checkin_jobs SET status = 'cancelled', proof_source = 'none', failure_code = ?, finished_at = ?
WHERE id = ? AND dispatched = 0 AND status IN ('queued','running')`, code, formatForkCheckinTime(now), job.ID)
	return err
}

func forkCheckinScheduledAttempts(ctx context.Context, tx *sql.Tx, id forkcheckin.AccountID, day string) (int, error) {
	var attempts int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM fork_checkin_job_attempts a JOIN fork_checkin_jobs j ON j.id = a.job_id
WHERE j.account_id = ? AND j.schedule_day = ? AND j.action = 'check_in'`, id, day).Scan(&attempts)
	return attempts, err
}

func checkForkCheckinScheduledClaim(ctx context.Context, tx *sql.Tx, id forkcheckin.JobID, now time.Time) error {
	var accountID forkcheckin.AccountID
	var day, zone, action string
	var automatic bool
	err := tx.QueryRowContext(ctx, `SELECT j.account_id, j.schedule_day, a.time_zone, a.automatic, j.action
FROM fork_checkin_jobs j JOIN fork_checkin_accounts a ON a.id = j.account_id WHERE j.id = ?`, id).Scan(&accountID, &day, &zone, &automatic, &action)
	if err != nil || day == "" {
		return err
	}
	location, err := time.LoadLocation(zone)
	if err != nil {
		return storagecontract.ErrInvalidRecord
	}
	if !automatic || now.In(location).Format(time.DateOnly) != day {
		return forkcheckin.ErrRevisionChanged
	}
	if action == string(forkcheckin.JobActionStatusRefresh) {
		return nil
	}
	attempts, err := forkCheckinScheduledAttempts(ctx, tx, accountID, day)
	if err != nil {
		return err
	}
	if attempts >= forkcheckin.AutomaticAttemptLimit {
		return storagecontract.ErrPrecondition
	}
	return nil
}

func forkCheckinDayBlocked(ctx context.Context, tx *sql.Tx, account forkcheckin.AccountID, day, schedule string, exclude forkcheckin.JobID) (bool, error) {
	var blocked bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM fork_checkin_jobs WHERE account_id = ? AND id <> ?
AND (site_date = ? OR (? <> '' AND schedule_day = ?))
AND (status IN ('success','already_checked') OR (action = 'check_in' AND dispatched = 1)))`, account, exclude, day, schedule, schedule).Scan(&blocked)
	return blocked, err
}

func checkForkCheckinDispatchDay(ctx context.Context, tx *sql.Tx, id forkcheckin.JobID) error {
	var account forkcheckin.AccountID
	var day, schedule string
	if err := tx.QueryRowContext(ctx, `SELECT account_id, site_date, schedule_day FROM fork_checkin_jobs WHERE id = ?`, id).Scan(&account, &day, &schedule); err != nil {
		return err
	}
	blocked, err := forkCheckinDayBlocked(ctx, tx, account, day, schedule, id)
	if err != nil {
		return err
	}
	if blocked {
		return forkcheckin.ErrDayResolved
	}
	return nil
}

// BindForkCheckinJobDay accepts an authoritative current site date only while
// a live claim is still read-only. ScheduleDay and its retry budget stay fixed.
func (store *Store) BindForkCheckinJobDay(ctx context.Context, lease forkcheckin.ExecutionLease, day string) (job forkcheckin.Job, err error) {
	parsed, parseErr := time.Parse(time.DateOnly, day)
	if validateForkCheckinLease(lease) != nil || parseErr != nil || parsed.Year() < 1 || parsed.Format(time.DateOnly) != day {
		return job, storagecontract.ErrInvalidArgument
	}
	tx, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return job, err
	}
	defer rollbackOnError(tx, &err)
	revision, err := lockForkCheckinAccount(ctx, tx, lease.AccountID)
	if err != nil {
		return job, err
	}
	if revision != lease.Revision {
		return job, forkcheckin.ErrRevisionChanged
	}
	result, err := tx.ExecContext(ctx, `UPDATE fork_checkin_jobs SET site_date = ? WHERE id = ? AND account_id = ? AND expected_revision = ?
AND status = 'running' AND dispatched = 0 AND EXISTS(SELECT 1 FROM fork_checkin_job_attempts a
WHERE a.job_id = fork_checkin_jobs.id AND a.attempt = ? AND a.lease_owner = ? AND a.ended_at = '' AND a.phase = 'claim' AND a.lease_expires_at > ?)`, day, lease.JobID, lease.AccountID, lease.Revision, lease.Attempt, lease.Token, formatForkCheckinTime(store.now()))
	if err != nil {
		return job, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return job, err
	}
	if changed != 1 {
		return job, storagecontract.ErrPrecondition
	}
	job, err = scanForkCheckinJob(tx.QueryRowContext(ctx, forkCheckinJobSelect+` WHERE j.id = ?`, lease.JobID))
	if err != nil {
		return job, err
	}
	return job, tx.Commit()
}

func optionalForkCheckinTime(value *time.Time) string {
	if value == nil {
		return ""
	}
	return formatForkCheckinTime(*value)
}
