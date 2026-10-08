package sqlite

import (
	"context"
	"fmt"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
)

// ForkCheckinAdapterBuilder creates the site protocol on the module's own
// network factory. It runs only while the extension is being enabled.
type ForkCheckinAdapterBuilder func(*forkcheckin.TransportFactory) (forkcheckin.SiteAdapter, error)

// ForkCheckinModuleFactory adapts the shared Store to the extension lifecycle.
// Tables are created only here, when enabling; the Store is never closed by
// the module, and Close drains only the module's own network clients.
func (store *Store) ForkCheckinModuleFactory(build ForkCheckinAdapterBuilder) forkcheckin.ModuleFactory {
	return func(ctx context.Context) (forkcheckin.ModuleParts, error) {
		if build == nil {
			return forkcheckin.ModuleParts{}, fmt.Errorf("check-in adapter builder unavailable")
		}
		if err := store.EnsureForkCheckinSchema(ctx); err != nil {
			return forkcheckin.ModuleParts{}, err
		}
		factory := forkcheckin.NewTransportFactory()
		adapter, err := build(factory)
		if err != nil || adapter == nil {
			_ = factory.Close(context.WithoutCancel(ctx))
			return forkcheckin.ModuleParts{}, fmt.Errorf("build check-in adapter: %w", err)
		}
		return forkcheckin.ModuleParts{
			Store:               store,
			Vault:               store.ForkCheckinSessions(),
			Adapter:             adapter,
			Close:               factory.Close,
			CountAccounts:       store.CountForkCheckinAccounts,
			Reader:              forkCheckinReader{store: store},
			Writer:              forkCheckinAccountWriter{store: store},
			Jobs:                forkCheckinJobWriter{store: store},
			Authorizations:      store,
			VerifyAuthorization: forkcheckin.NewAuthorizationVerifier(build),
			Invalidate:          factory.Invalidate,
		}, nil
	}
}

// CountForkCheckinAccounts is the status count. It reads only the extension
// table and is called only while the module is initialized.
func (store *Store) CountForkCheckinAccounts(ctx context.Context) (int, error) {
	var count int
	if err := store.forkCheckinDB().QueryRowContext(ctx, `SELECT COUNT(*) FROM fork_checkin_accounts`).Scan(&count); err != nil {
		return 0, fmt.Errorf("count fork check-in accounts: %w", err)
	}
	return count, nil
}

// ListForkCheckinAccountViews pages public account views for the read API.
func (store *Store) ListForkCheckinAccountViews(ctx context.Context, options forkcheckin.ListOptions) (forkcheckin.AccountViewPage, error) {
	page, err := store.ListForkCheckinAccounts(ctx, options)
	if err != nil {
		return forkcheckin.AccountViewPage{}, err
	}
	views := make([]forkcheckin.AccountView, 0, len(page.Items))
	for _, account := range page.Items {
		views = append(views, account.View())
	}
	return forkcheckin.AccountViewPage{Items: views, NextCursor: page.NextCursor}, nil
}

// ListForkCheckinJobs pages job receipts in stable id order, optionally for
// one account. Batch parents are not rows here; their children are listed
// as ordinary jobs. It reads only local state.
func (store *Store) ListForkCheckinJobs(ctx context.Context, account forkcheckin.AccountID, options forkcheckin.ListOptions) (forkcheckin.JobPage, error) {
	if err := options.Validate(); err != nil {
		return forkcheckin.JobPage{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
	}
	if account != "" {
		if err := account.Validate(); err != nil {
			return forkcheckin.JobPage{}, fmt.Errorf("%w: %v", storagecontract.ErrInvalidArgument, err)
		}
	}
	after, err := decodeForkCheckinJobCursor(options.Cursor)
	if err != nil {
		return forkcheckin.JobPage{}, err
	}
	limit := options.EffectiveLimit()
	rows, err := store.forkCheckinDB().QueryContext(ctx, fmt.Sprintf(`%s
WHERE (? = '' OR j.account_id = ?) AND j.id > ? ORDER BY j.id LIMIT ?`, forkCheckinJobSelect), account, account, after, limit+1)
	if err != nil {
		return forkcheckin.JobPage{}, fmt.Errorf("list fork check-in jobs: %w", err)
	}
	defer rows.Close()
	items := make([]forkcheckin.JobReceipt, 0, limit+1)
	for rows.Next() {
		job, scanErr := scanForkCheckinJob(rows)
		if scanErr != nil {
			return forkcheckin.JobPage{}, scanErr
		}
		items = append(items, job.Receipt())
	}
	if err := rows.Err(); err != nil {
		return forkcheckin.JobPage{}, fmt.Errorf("iterate fork check-in jobs: %w", err)
	}
	page := forkcheckin.JobPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		page.NextCursor = encodeForkCheckinJobCursor(page.Items[len(page.Items)-1].ID)
	}
	return page, nil
}

// forkCheckinReader is the cache-only read surface for the control API.
type forkCheckinReader struct{ store *Store }

func (reader forkCheckinReader) GetAccount(ctx context.Context, id forkcheckin.AccountID) (forkcheckin.AccountView, error) {
	account, err := reader.store.GetForkCheckinAccount(ctx, id)
	if err != nil {
		return forkcheckin.AccountView{}, err
	}
	return account.View(), nil
}

func (reader forkCheckinReader) ListAccounts(ctx context.Context, options forkcheckin.ListOptions) (forkcheckin.AccountViewPage, error) {
	return reader.store.ListForkCheckinAccountViews(ctx, options)
}

func (reader forkCheckinReader) GetJob(ctx context.Context, id forkcheckin.JobID) (forkcheckin.JobReceipt, error) {
	job, err := reader.store.GetForkCheckinJob(ctx, id)
	if err != nil {
		return forkcheckin.JobReceipt{}, err
	}
	return job.Receipt(), nil
}

func (reader forkCheckinReader) GetBatch(ctx context.Context, id forkcheckin.JobID) ([]forkcheckin.JobReceipt, error) {
	return reader.store.GetForkCheckinBatch(ctx, id)
}

func (reader forkCheckinReader) ListJobs(ctx context.Context, account forkcheckin.AccountID, options forkcheckin.ListOptions) (forkcheckin.JobPage, error) {
	return reader.store.ListForkCheckinJobs(ctx, account, options)
}
