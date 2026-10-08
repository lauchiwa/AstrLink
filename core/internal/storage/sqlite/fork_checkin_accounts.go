package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// Account and binding persistence for the check-in extension.
//
// Services are read-only here. Binding an account to a service stores the
// service id in an extension table and nothing else: no service document,
// ETag, credential or enabled flag is read for writing, and the upstream
// services table gains no foreign key and no trigger. Deleting a service
// therefore cannot cascade into extension rows; it only leaves a binding with
// nothing to point at, which is reported as stale.

// ForkCheckinAccountPage is one page of accounts. NextCursor is empty on the
// last page.
type ForkCheckinAccountPage struct {
	Items      []forkcheckin.Account
	NextCursor string
}

// ForkCheckinBinding is one stored account-to-service binding.
//
// BoundAt exists to survive id reuse. A service id is just text here, so a
// service that is deleted and recreated under the same id would otherwise be
// silently adopted by an old binding. A service created after its binding is
// reported stale instead.
type ForkCheckinBinding struct {
	ServiceID contract.ServiceID
	BoundAt   time.Time
	// Live is false when no service currently answers to this id, or when the
	// service answering to it was created after the binding was made.
	Live bool
}

// CreateForkCheckinAccount stores a new account at revision 1.
//
// A draft has no verified remote user; a state that requires one must already
// carry it, and that identity must be unique. Both rules are checked here and
// again by the schema, so neither this DAO nor a future writer can introduce a
// second account for one site and user.
func (store *Store) CreateForkCheckinAccount(ctx context.Context, account forkcheckin.Account) (stored forkcheckin.Account, err error) {
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return stored, fmt.Errorf("begin fork check-in account create: %w", err)
	}
	defer rollbackOnError(transaction, &err)
	if stored, err = store.createForkCheckinAccountTx(ctx, transaction, account); err != nil {
		return stored, err
	}
	if err = transaction.Commit(); err != nil {
		return forkcheckin.Account{}, fmt.Errorf("commit fork check-in account create: %w", err)
	}
	return stored, nil
}

func (store *Store) createForkCheckinAccountTx(ctx context.Context, transaction *sql.Tx, account forkcheckin.Account) (stored forkcheckin.Account, err error) {
	account.Revision = 1
	normalized, err := normalizeForkCheckinAccount(account)
	if err != nil {
		return stored, err
	}
	var exists bool
	if err = transaction.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT EXISTS(SELECT 1 FROM %s WHERE id = ?)`, forkCheckinAccounts), normalized.ID,
	).Scan(&exists); err != nil {
		return stored, fmt.Errorf("read fork check-in account: %w", err)
	}
	if exists {
		return stored, fmt.Errorf("%w: fork check-in account %q", storagecontract.ErrConflict, normalized.ID)
	}
	if err = requireUniqueForkCheckinIdentity(ctx, transaction, normalized); err != nil {
		return stored, err
	}

	now := store.now().UTC()
	if err = insertForkCheckinAccount(ctx, transaction, normalized, now); err != nil {
		return stored, err
	}
	if err = replaceForkCheckinBindings(ctx, transaction, normalized, now); err != nil {
		return stored, err
	}
	return normalized, nil
}

// GetForkCheckinAccount reads one account with its bindings.
func (store *Store) GetForkCheckinAccount(ctx context.Context, id forkcheckin.AccountID) (forkcheckin.Account, error) {
	if err := id.Validate(); err != nil {
		return forkcheckin.Account{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	account, err := scanForkCheckinAccount(store.forkCheckinDB().QueryRowContext(ctx,
		fmt.Sprintf(`%s WHERE id = ?`, forkCheckinAccountSelect), id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return forkcheckin.Account{}, fmt.Errorf("%w: fork check-in account %q", storagecontract.ErrNotFound, id)
	}
	if err != nil {
		return forkcheckin.Account{}, err
	}
	services, err := store.readForkCheckinBoundServices(ctx, id)
	if err != nil {
		return forkcheckin.Account{}, err
	}
	account.BoundServices = services
	return account, nil
}

// ListForkCheckinAccounts pages accounts by id. A limit above the shared
// maximum is rejected rather than clamped, so a caller is never handed a
// different page than it asked for.
func (store *Store) ListForkCheckinAccounts(ctx context.Context, options forkcheckin.ListOptions) (ForkCheckinAccountPage, error) {
	if err := options.Validate(); err != nil {
		return ForkCheckinAccountPage{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	limit := options.EffectiveLimit()
	after, err := decodeForkCheckinAccountCursor(options.Cursor)
	if err != nil {
		return ForkCheckinAccountPage{}, err
	}
	rows, err := store.forkCheckinDB().QueryContext(ctx,
		fmt.Sprintf(`%s WHERE id > ? ORDER BY id LIMIT ?`, forkCheckinAccountSelect), after, limit+1)
	if err != nil {
		return ForkCheckinAccountPage{}, fmt.Errorf("list fork check-in accounts: %w", err)
	}
	defer rows.Close()
	matched := make([]forkcheckin.Account, 0, limit+1)
	for rows.Next() {
		account, scanErr := scanForkCheckinAccount(rows)
		if scanErr != nil {
			return ForkCheckinAccountPage{}, scanErr
		}
		matched = append(matched, account)
	}
	if err := rows.Err(); err != nil {
		return ForkCheckinAccountPage{}, fmt.Errorf("iterate fork check-in accounts: %w", err)
	}
	page := ForkCheckinAccountPage{Items: matched}
	if len(matched) > limit {
		page.Items = matched[:limit]
		page.NextCursor = encodeForkCheckinAccountCursor(page.Items[len(page.Items)-1].ID)
	}
	for index := range page.Items {
		services, err := store.readForkCheckinBoundServices(ctx, page.Items[index].ID)
		if err != nil {
			return ForkCheckinAccountPage{}, err
		}
		page.Items[index].BoundServices = services
	}
	return page, nil
}

// UpdateForkCheckinAccount applies an operator edit under compare-and-set on
// the revision, and returns the stored account at its new revision.
//
// Editing anything that decides where a stored session is sent, or whose
// session it is, invalidates that session: the sealed row is removed and the
// account returns to draft. A session captured for one address and egress must
// not be silently reused after either changes.
func (store *Store) UpdateForkCheckinAccount(
	ctx context.Context,
	account forkcheckin.Account,
	expectedRevision int64,
) (stored forkcheckin.Account, err error) {
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return stored, fmt.Errorf("begin fork check-in account update: %w", err)
	}
	defer rollbackOnError(transaction, &err)
	if stored, err = store.updateForkCheckinAccountTx(ctx, transaction, account, expectedRevision); err != nil {
		return stored, err
	}
	if err = transaction.Commit(); err != nil {
		return forkcheckin.Account{}, fmt.Errorf("commit fork check-in account update: %w", err)
	}
	return stored, nil
}

// readForkCheckinAccountTx reads one account with its bindings inside tx.
func readForkCheckinAccountTx(ctx context.Context, transaction *sql.Tx, id forkcheckin.AccountID) (forkcheckin.Account, error) {
	account, err := scanForkCheckinAccount(transaction.QueryRowContext(ctx,
		fmt.Sprintf(`%s WHERE id = ?`, forkCheckinAccountSelect), id,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return forkcheckin.Account{}, fmt.Errorf("%w: fork check-in account %q", storagecontract.ErrNotFound, id)
	}
	if err != nil {
		return forkcheckin.Account{}, err
	}
	account.BoundServices, err = readForkCheckinBoundServicesTx(ctx, transaction, id)
	return account, err
}

func (store *Store) updateForkCheckinAccountTx(
	ctx context.Context,
	transaction *sql.Tx,
	account forkcheckin.Account,
	expectedRevision int64,
) (stored forkcheckin.Account, err error) {
	if expectedRevision < 1 {
		return stored, fmt.Errorf("%w: expected revision is required", storagecontract.ErrInvalidArgument)
	}
	current, err := scanForkCheckinAccount(transaction.QueryRowContext(ctx,
		fmt.Sprintf(`%s WHERE id = ?`, forkCheckinAccountSelect), account.ID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return stored, fmt.Errorf("%w: fork check-in account %q", storagecontract.ErrNotFound, account.ID)
	}
	if err != nil {
		return stored, err
	}
	if current.Revision != expectedRevision {
		return stored, fmt.Errorf(
			"%w: fork check-in account %q is at revision %d, not %d",
			storagecontract.ErrPrecondition, account.ID, current.Revision, expectedRevision)
	}
	currentServices, err := readForkCheckinBoundServicesTx(ctx, transaction, account.ID)
	if err != nil {
		return stored, err
	}
	current.BoundServices = currentServices

	// Revision always advances, so work prepared against the old revision is
	// refused rather than applied to a changed account.
	account.Revision = current.Revision + 1
	redirected := forkCheckinSessionRedirected(current, account)
	if redirected {
		account.State = forkcheckin.AccountStateDraft
		account.RemoteUserID = ""
		account.Automatic = false
	}
	normalized, err := normalizeForkCheckinAccount(account)
	if err != nil {
		return stored, err
	}
	if err = requireUniqueForkCheckinIdentity(ctx, transaction, normalized); err != nil {
		return stored, err
	}

	now := store.now().UTC()
	result, err := transaction.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET
    dashboard_base_url = ?, remote_user_id = ?, state = ?, time_zone = ?,
    automatic = ?, network_mode = ?, network_proxy_url = ?,
    config_fingerprint = ?, revision = ?, updated_at = ?
WHERE id = ? AND revision = ?`, forkCheckinAccounts),
		normalized.DashboardBaseURL, normalized.RemoteUserID, string(normalized.State),
		normalized.TimeZone, boolToInt(normalized.Automatic), string(normalized.Network.Mode),
		normalized.Network.ProxyURL, normalized.ConfigFingerprint(), normalized.Revision,
		now.Format(time.RFC3339Nano), normalized.ID, expectedRevision,
	)
	if err != nil {
		return stored, mapForkCheckinWriteError(err, normalized.ID)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return stored, fmt.Errorf("confirm fork check-in account update: %w", err)
	}
	// A concurrent writer that advanced the revision between the read and the
	// write loses here rather than overwriting.
	if affected == 0 {
		return stored, fmt.Errorf(
			"%w: fork check-in account %q changed concurrently", storagecontract.ErrPrecondition, normalized.ID)
	}
	if err = replaceForkCheckinBindings(ctx, transaction, normalized, now); err != nil {
		return stored, err
	}
	if redirected {
		if _, err = transaction.ExecContext(ctx,
			fmt.Sprintf(`DELETE FROM %s WHERE account_id = ?`, forkCheckinCredentials), normalized.ID,
		); err != nil {
			return stored, fmt.Errorf("invalidate fork check-in session: %w", err)
		}
	}
	return normalized, nil
}

// ConnectForkCheckinAccount records the remote user the site reported, under
// compare-and-set on the revision.
//
// The identity comes from the adapter's own read of the site. A site and user
// pair that already belongs to another account is refused, because one day
// would otherwise produce two check-ins for the same upstream account.
func (store *Store) ConnectForkCheckinAccount(
	ctx context.Context,
	id forkcheckin.AccountID,
	remoteUserID string,
	expectedRevision int64,
) (forkcheckin.Account, error) {
	current, err := store.GetForkCheckinAccount(ctx, id)
	if err != nil {
		return forkcheckin.Account{}, err
	}
	next := current
	next.State = forkcheckin.AccountStateConnected
	next.RemoteUserID = remoteUserID
	// A connect only establishes the identity; it never silently redirects the
	// session, so the fields that would invalidate it are carried over.
	return store.UpdateForkCheckinAccount(ctx, next, expectedRevision)
}

// DeleteForkCheckinAccount removes an account under compare-and-set. Its
// bindings and sealed session go with it through the extension's own cascade.
//
// A missing account is not an error: a retried delete must not report a
// conflict for work it already completed.
func (store *Store) DeleteForkCheckinAccount(
	ctx context.Context,
	id forkcheckin.AccountID,
	expectedRevision int64,
) (err error) {
	transaction, ctx, err := store.forkCheckinDB().BeginTx(ctx)
	if err != nil {
		return fmt.Errorf("begin fork check-in account delete: %w", err)
	}
	defer rollbackOnError(transaction, &err)
	if err = deleteForkCheckinAccountTx(ctx, transaction, id, expectedRevision); err != nil {
		return err
	}
	if err = transaction.Commit(); err != nil {
		return fmt.Errorf("commit fork check-in account delete: %w", err)
	}
	return nil
}

func deleteForkCheckinAccountTx(ctx context.Context, transaction *sql.Tx, id forkcheckin.AccountID, expectedRevision int64) error {
	if err := id.Validate(); err != nil {
		return fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	if expectedRevision < 1 {
		return fmt.Errorf("%w: expected revision is required", storagecontract.ErrInvalidArgument)
	}
	var revision int64
	err := transaction.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT revision FROM %s WHERE id = ?`, forkCheckinAccounts), id,
	).Scan(&revision)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("read fork check-in account for delete: %w", err)
	case revision != expectedRevision:
		return fmt.Errorf(
			"%w: fork check-in account %q is at revision %d, not %d",
			storagecontract.ErrPrecondition, id, revision, expectedRevision)
	}
	if _, err = transaction.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE id = ?`, forkCheckinAccounts), id,
	); err != nil {
		return fmt.Errorf("delete fork check-in account: %w", err)
	}
	return nil
}

// ListForkCheckinBindableServices projects the services an account may be
// bound to. It reads service documents and returns no credential, proxy, model
// list or ETag: this extension never writes a service, so it has no use for a
// service's concurrency token.
func (store *Store) ListForkCheckinBindableServices(ctx context.Context) ([]forkcheckin.BindableService, error) {
	rows, err := store.forkCheckinDB().QueryContext(ctx, `SELECT id, document_json FROM services ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list bindable services: %w", err)
	}
	defer rows.Close()
	var services []forkcheckin.BindableService
	for rows.Next() {
		var id, document string
		if err := rows.Scan(&id, &document); err != nil {
			return nil, fmt.Errorf("scan bindable service: %w", err)
		}
		record, err := decodeServiceRecord(id, []byte(document))
		if err != nil {
			return nil, err
		}
		service := forkcheckin.BindableService{
			ID:      record.Service.ID,
			Name:    record.Service.Name,
			Kind:    record.Service.Kind,
			Enabled: record.Service.Enabled,
		}
		if record.Service.HTTP != nil {
			service.BaseURL = record.Service.HTTP.BaseURL
		}
		services = append(services, service)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate bindable services: %w", err)
	}
	return services, nil
}

// ResolveForkCheckinBindings reports each stored binding and whether a service
// still answers to it.
//
// A binding is stale when its service is gone, and also when the service
// holding that id was created after the binding: the id was reused by a
// different service, and adopting it would quietly fund something the operator
// never chose.
func (store *Store) ResolveForkCheckinBindings(
	ctx context.Context,
	id forkcheckin.AccountID,
) ([]ForkCheckinBinding, error) {
	if err := id.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	rows, err := store.forkCheckinDB().QueryContext(ctx,
		fmt.Sprintf(`SELECT service_id, created_at FROM %s WHERE account_id = ? ORDER BY service_id`, forkCheckinBindings), id)
	if err != nil {
		return nil, fmt.Errorf("read fork check-in bindings: %w", err)
	}
	defer rows.Close()
	var bindings []ForkCheckinBinding
	for rows.Next() {
		var serviceID, boundAt string
		if err := rows.Scan(&serviceID, &boundAt); err != nil {
			return nil, fmt.Errorf("scan fork check-in binding: %w", err)
		}
		parsed, err := time.Parse(time.RFC3339Nano, boundAt)
		if err != nil {
			return nil, fmt.Errorf("%w: fork check-in binding timestamp %q", storagecontract.ErrInvalidRecord, boundAt)
		}
		bindings = append(bindings, ForkCheckinBinding{
			ServiceID: contract.ServiceID(serviceID),
			BoundAt:   parsed.UTC(),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fork check-in bindings: %w", err)
	}
	for index, binding := range bindings {
		var document string
		err := store.forkCheckinDB().QueryRowContext(ctx,
			`SELECT document_json FROM services WHERE id = ?`, string(binding.ServiceID)).Scan(&document)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read bound service: %w", err)
		}
		record, err := decodeServiceRecord(string(binding.ServiceID), []byte(document))
		if err != nil {
			return nil, err
		}
		created := record.Service.CreatedAt.UTC()
		bindings[index].Live = created.IsZero() || !created.After(binding.BoundAt)
	}
	return bindings, nil
}

const forkCheckinAccountSelect = `SELECT id, dashboard_base_url, remote_user_id, state, time_zone,
    automatic, network_mode, network_proxy_url, revision
FROM fork_checkin_accounts`

type forkCheckinScanner interface {
	Scan(...any) error
}

func scanForkCheckinAccount(scanner forkCheckinScanner) (forkcheckin.Account, error) {
	var (
		account   forkcheckin.Account
		id        string
		state     string
		mode      string
		automatic int
	)
	if err := scanner.Scan(
		&id, &account.DashboardBaseURL, &account.RemoteUserID, &state, &account.TimeZone,
		&automatic, &mode, &account.Network.ProxyURL, &account.Revision,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return account, err
		}
		return account, fmt.Errorf("scan fork check-in account: %w", err)
	}
	account.ID = forkcheckin.AccountID(id)
	account.State = forkcheckin.AccountState(state)
	account.Network.Mode = forkcheckin.NetworkMode(mode)
	account.Automatic = automatic != 0
	return account, nil
}

// normalizeForkCheckinAccount stores the canonical address rather than what
// the caller typed, so two spellings of one site cannot become two accounts.
func normalizeForkCheckinAccount(account forkcheckin.Account) (forkcheckin.Account, error) {
	normalized, err := forkcheckin.NormalizeDashboardURL(account.DashboardBaseURL)
	if err != nil {
		return forkcheckin.Account{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidRecord, err)
	}
	account.DashboardBaseURL = normalized
	account.BoundServices = dedupeServiceIDs(account.BoundServices)
	if err := account.Validate(); err != nil {
		return forkcheckin.Account{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidRecord, err)
	}
	return account, nil
}

func dedupeServiceIDs(services []contract.ServiceID) []contract.ServiceID {
	if len(services) == 0 {
		return nil
	}
	seen := make(map[contract.ServiceID]struct{}, len(services))
	unique := make([]contract.ServiceID, 0, len(services))
	for _, id := range services {
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	sort.Slice(unique, func(left, right int) bool { return unique[left] < unique[right] })
	return unique
}

// forkCheckinSessionRedirected reports whether an edit changes where a stored
// session would be sent, or whose session it is.
//
// Bound services are not included: funding another service does not move the
// session, and treating it as a redirect would force the operator to sign in
// again for an unrelated change.
func forkCheckinSessionRedirected(current, next forkcheckin.Account) bool {
	nextURL, err := forkcheckin.NormalizeDashboardURL(next.DashboardBaseURL)
	if err != nil {
		// An unusable address is reported by validation; treat it as a change
		// so no stored session survives it.
		return true
	}
	if current.DashboardBaseURL != nextURL {
		return true
	}
	return current.Network != next.Network
}

func requireUniqueForkCheckinIdentity(ctx context.Context, transaction *sql.Tx, account forkcheckin.Account) error {
	identity, ok := account.Identity()
	if !ok {
		return nil
	}
	var owner string
	err := transaction.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT id FROM %s WHERE dashboard_base_url = ? AND remote_user_id = ? AND id <> ?`, forkCheckinAccounts),
		identity.DashboardBaseURL, identity.RemoteUserID, account.ID,
	).Scan(&owner)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("check fork check-in account identity: %w", err)
	default:
		return fmt.Errorf(
			"%w: fork check-in account %q already holds this site and user", storagecontract.ErrConflict, owner)
	}
}

func insertForkCheckinAccount(
	ctx context.Context,
	transaction *sql.Tx,
	account forkcheckin.Account,
	now time.Time,
) error {
	timestamp := now.Format(time.RFC3339Nano)
	_, err := transaction.ExecContext(ctx, fmt.Sprintf(`INSERT INTO %s (
    id, dashboard_base_url, remote_user_id, state, time_zone, automatic,
    network_mode, network_proxy_url, config_fingerprint, revision,
    created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, forkCheckinAccounts),
		account.ID, account.DashboardBaseURL, account.RemoteUserID, string(account.State),
		account.TimeZone, boolToInt(account.Automatic), string(account.Network.Mode),
		account.Network.ProxyURL, account.ConfigFingerprint(), account.Revision,
		timestamp, timestamp,
	)
	if err != nil {
		return mapForkCheckinWriteError(err, account.ID)
	}
	return nil
}

// replaceForkCheckinBindings makes the stored bindings match the account. Only
// extension rows are touched; the services themselves are never written.
// Unchanged bindings keep their BoundAt, so an unrelated edit cannot make a
// binding to a since-recreated service id look live again.
func replaceForkCheckinBindings(
	ctx context.Context,
	transaction *sql.Tx,
	account forkcheckin.Account,
	now time.Time,
) error {
	existing, err := readForkCheckinBoundServicesTx(ctx, transaction, account.ID)
	if err != nil {
		return err
	}
	keep := make(map[contract.ServiceID]bool, len(account.BoundServices))
	for _, serviceID := range account.BoundServices {
		keep[serviceID] = true
	}
	for _, serviceID := range existing {
		if keep[serviceID] {
			delete(keep, serviceID)
			continue
		}
		if _, err := transaction.ExecContext(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE account_id = ? AND service_id = ?`, forkCheckinBindings), account.ID, string(serviceID),
		); err != nil {
			return fmt.Errorf("unbind fork check-in service %q: %w", serviceID, err)
		}
	}
	timestamp := now.Format(time.RFC3339Nano)
	for _, serviceID := range account.BoundServices {
		if !keep[serviceID] {
			continue
		}
		if _, err := transaction.ExecContext(ctx, fmt.Sprintf(
			`INSERT INTO %s (account_id, service_id, created_at) VALUES (?, ?, ?)`, forkCheckinBindings),
			account.ID, string(serviceID), timestamp,
		); err != nil {
			return fmt.Errorf("bind fork check-in service %q: %w", serviceID, err)
		}
	}
	return nil
}

func (store *Store) readForkCheckinBoundServices(
	ctx context.Context,
	id forkcheckin.AccountID,
) ([]contract.ServiceID, error) {
	rows, err := store.forkCheckinDB().QueryContext(ctx,
		fmt.Sprintf(`SELECT service_id FROM %s WHERE account_id = ? ORDER BY service_id`, forkCheckinBindings), id)
	if err != nil {
		return nil, fmt.Errorf("read fork check-in bindings: %w", err)
	}
	defer rows.Close()
	return collectForkCheckinServiceIDs(rows)
}

func readForkCheckinBoundServicesTx(
	ctx context.Context,
	transaction *sql.Tx,
	id forkcheckin.AccountID,
) ([]contract.ServiceID, error) {
	rows, err := transaction.QueryContext(ctx,
		fmt.Sprintf(`SELECT service_id FROM %s WHERE account_id = ? ORDER BY service_id`, forkCheckinBindings), id)
	if err != nil {
		return nil, fmt.Errorf("read fork check-in bindings: %w", err)
	}
	defer rows.Close()
	return collectForkCheckinServiceIDs(rows)
}

func collectForkCheckinServiceIDs(rows *sql.Rows) ([]contract.ServiceID, error) {
	var services []contract.ServiceID
	for rows.Next() {
		var serviceID string
		if err := rows.Scan(&serviceID); err != nil {
			return nil, fmt.Errorf("scan fork check-in binding: %w", err)
		}
		services = append(services, contract.ServiceID(serviceID))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate fork check-in bindings: %w", err)
	}
	return services, nil
}

// mapForkCheckinWriteError reports a unique-index violation as a conflict. The
// explicit identity check above is the primary guard; this keeps the index as a
// backstop from surfacing as an opaque driver error.
func mapForkCheckinWriteError(err error, id forkcheckin.AccountID) error {
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return fmt.Errorf("%w: fork check-in account %q", storagecontract.ErrConflict, id)
	}
	if strings.Contains(err.Error(), "CHECK constraint failed") {
		return fmt.Errorf("%w: fork check-in account %q", storagecontract.ErrInvalidRecord, id)
	}
	return fmt.Errorf("write fork check-in account %q: %w", id, err)
}

func encodeForkCheckinAccountCursor(id forkcheckin.AccountID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(id))
}

// decodeForkCheckinAccountCursor validates the cursor as an account id. The
// shared service cursor helper validates as a service id, which an account id
// is not, so the extension owns this one.
func decodeForkCheckinAccountCursor(cursor string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != cursor {
		return "", fmt.Errorf("%w: fork check-in account cursor encoding", storagecontract.ErrInvalidCursor)
	}
	id := forkcheckin.AccountID(decoded)
	if err := id.Validate(); err != nil {
		return "", fmt.Errorf("%w: fork check-in account cursor id", storagecontract.ErrInvalidCursor)
	}
	return string(id), nil
}
