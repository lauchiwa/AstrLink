package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
	sqlitedriver "modernc.org/sqlite"
)

// The wrapper does not fake SQL results. It provides a barrier after a real
// SQLite SELECT has closed its rows but before the caller can upgrade its read
// transaction to a writer. A second real connection creates the conflict.
// Every test owns its connector; no global driver hooks or sleeps are needed.
type configBarrierDriver struct {
	base            driver.Driver
	dsn             string
	afterRead       func()
	beforeTokenLock func()
	tokenLockCount  atomic.Int32
	afterRollback   func()
	commitError     error
	rollbackError   error
	readCount       atomic.Int32
}

func (d *configBarrierDriver) Open(name string) (driver.Conn, error) {
	c, err := d.base.Open(name)
	if err != nil {
		return nil, err
	}
	return &configBarrierConn{Conn: c, owner: d}, nil
}
func (d *configBarrierDriver) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d.Open(d.dsn)
}
func (d *configBarrierDriver) Driver() driver.Driver { return d }

type configBarrierConn struct {
	driver.Conn
	owner *configBarrierDriver
}

func (c *configBarrierConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &configBarrierTx{Tx: tx, owner: c.owner}, nil
}
func (c *configBarrierConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if query == "UPDATE local_access_tokens SET id = id WHERE 0" {
		c.owner.tokenLockCount.Add(1)
		if c.owner.beforeTokenLock != nil {
			c.owner.beforeTokenLock()
		}
	}
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}
func (c *configBarrierConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	normalized := strings.Join(strings.Fields(query), " ")
	if strings.HasPrefix(normalized, "SELECT document_json FROM services WHERE id = ?") || strings.HasPrefix(normalized, "SELECT document_json FROM policies WHERE id = ?") || normalized == "SELECT COUNT(*) FROM local_access_tokens" {
		return &configBarrierRows{Rows: rows, owner: c.owner}, nil
	}
	return rows, nil
}

type configBarrierRows struct {
	driver.Rows
	owner *configBarrierDriver
	once  sync.Once
}

func (r *configBarrierRows) Close() error {
	err := r.Rows.Close()
	r.once.Do(func() {
		r.owner.readCount.Add(1)
		if r.owner.afterRead != nil {
			r.owner.afterRead()
		}
	})
	return err
}

type configBarrierTx struct {
	driver.Tx
	owner *configBarrierDriver
}

func (tx *configBarrierTx) Rollback() error {
	err := tx.Tx.Rollback()
	if tx.owner.afterRollback != nil {
		tx.owner.afterRollback()
	}
	return errors.Join(err, tx.owner.rollbackError)
}
func (tx *configBarrierTx) Commit() error {
	return errors.Join(tx.Tx.Commit(), tx.owner.commitError)
}

func configContentionFixture(t *testing.T) (*Store, *sql.DB, *configBarrierDriver, storagecontract.ServiceRecord) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "configuration.db")
	store := openTestStore(t, path)
	t.Cleanup(func() { _ = store.Close() })
	service, err := store.CreateService(context.Background(), contract.ServiceFromEndpoint(testEndpoint("service_contention")), storagecontract.CredentialMutation{Present: true, Secret: []byte("config-test-original-secret")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`CREATE TABLE config_write_noise (value INTEGER); INSERT INTO config_write_noise VALUES (0)`); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Close(); err != nil {
		t.Fatal(err)
	}
	barrier := &configBarrierDriver{base: &sqlitedriver.Driver{}, dsn: sqliteFileDSN(path)}
	store.db = sql.OpenDB(barrier)
	store.db.SetMaxOpenConns(1)
	actor, err := sql.Open(driverName, sqliteFileDSN(path))
	if err != nil {
		t.Fatal(err)
	}
	actor.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = actor.Close() })
	return store, actor, barrier, service
}

func TestConfigWritesRetryRealReadUpgradeContention(t *testing.T) {
	for _, operation := range []string{"delete", "update", "policy"} {
		for _, conflict := range []string{"writer_held", "snapshot_changed"} {
			t.Run(operation+"/"+conflict, func(t *testing.T) {
				store, actor, barrier, original := configContentionFixture(t)
				policy, err := store.GetPolicy(context.Background(), contract.DefaultPrivacyPolicyID)
				if err != nil {
					t.Fatal(err)
				}
				barrier.readCount.Store(0)
				var injected atomic.Bool
				var releaseOnce sync.Once
				release := func() {
					releaseOnce.Do(func() {
						if conflict == "writer_held" {
							if _, err := actor.Exec(`ROLLBACK`); err != nil {
								t.Error(err)
							}
						}
					})
				}
				barrier.afterRead = func() {
					if !injected.CompareAndSwap(false, true) {
						return
					}
					if conflict == "writer_held" {
						if _, err := actor.Exec(`BEGIN IMMEDIATE`); err != nil {
							t.Error(err)
							return
						}
					}
					if _, err := actor.Exec(`UPDATE config_write_noise SET value = value + 1`); err != nil {
						t.Error(err)
					}
				}
				barrier.afterRollback = release
				t.Cleanup(release)
				switch operation {
				case "delete":
					err = store.DeleteService(context.Background(), original.Service.ID, original.ETag)
				case "update":
					next := original.Service
					next.Name = "updated"
					_, err = store.UpdateService(context.Background(), next, storagecontract.CredentialMutation{Present: true, Secret: []byte("config-test-replacement-secret")}, original.ETag)
				case "policy":
					next := policy.Policy
					next.Enabled = true
					_, err = store.UpdatePolicy(context.Background(), next, policy.ETag)
				}
				if err != nil {
					t.Fatalf("configuration %s lost a retryable read-upgrade conflict: %v", operation, err)
				}
				if barrier.readCount.Load() != 2 {
					t.Fatalf("expected exactly two attempts, reads=%d", barrier.readCount.Load())
				}
				barrier.afterRead = nil
				if operation == "delete" {
					if _, err := store.GetService(context.Background(), original.Service.ID); !errors.Is(err, storagecontract.ErrNotFound) {
						t.Fatalf("delete not committed: %v", err)
					}
					var count int
					if err := store.db.QueryRow(`SELECT count(*) FROM service_credentials WHERE service_id = ?`, original.Service.ID).Scan(&count); err != nil || count != 0 {
						t.Fatal("deleted service retained its credential")
					}
				}
				if operation == "policy" {
					got, err := store.GetPolicy(context.Background(), policy.Policy.ID)
					if err != nil || !got.Policy.Enabled || got.ETag == policy.ETag {
						t.Fatal("policy update not committed")
					}
				}
				if operation == "update" {
					got, err := store.GetService(context.Background(), original.Service.ID)
					if err != nil || got.Service.Name != "updated" || got.ETag == original.ETag {
						t.Fatalf("update not committed: %v", err)
					}
					secret, err := store.Get(context.Background(), secretstore.Ref(localServiceRef(original.Service.ID)))
					defer clear(secret)
					if err != nil || string(secret) != "config-test-replacement-secret" {
						t.Fatal("credential mutation was not committed atomically")
					}
				}
			})
		}
	}
}

func TestConfigWriteRetryRechecksCAS(t *testing.T) {
	for _, operation := range []string{"delete", "update", "policy"} {
		t.Run(operation, func(t *testing.T) {
			store, actor, barrier, original := configContentionFixture(t)
			policy, err := store.GetPolicy(context.Background(), contract.DefaultPrivacyPolicyID)
			if err != nil {
				t.Fatal(err)
			}
			barrier.readCount.Store(0)
			var injected atomic.Bool
			barrier.afterRead = func() {
				if !injected.CompareAndSwap(false, true) {
					return
				}
				var err error
				if operation == "policy" {
					_, err = actor.Exec(`UPDATE policies SET document_json = json_set(document_json, '$.min_confidence', 0.9)`)
				} else {
					_, err = actor.Exec(`UPDATE services SET document_json = json_set(document_json, '$.name', 'concurrent winner')`)
				}
				if err != nil {
					t.Error(err)
				}
			}
			switch operation {
			case "delete":
				err = store.DeleteService(context.Background(), original.Service.ID, original.ETag)
			case "update":
				next := original.Service
				next.Name = "stale writer"
				_, err = store.UpdateService(context.Background(), next, storagecontract.CredentialMutation{Present: true, Secret: []byte("must-not-persist")}, original.ETag)
			case "policy":
				next := policy.Policy
				next.Enabled = true
				_, err = store.UpdatePolicy(context.Background(), next, policy.ETag)
			}
			if !errors.Is(err, storagecontract.ErrPrecondition) {
				t.Fatalf("concurrent write must become a CAS conflict, got %v", err)
			}
			if barrier.readCount.Load() != 2 {
				t.Fatalf("CAS retry reads=%d", barrier.readCount.Load())
			}
			barrier.afterRead = nil
			if operation == "policy" {
				got, err := store.GetPolicy(context.Background(), policy.Policy.ID)
				if err != nil || got.Policy.Enabled || got.Policy.MinConfidence != 0.9 {
					t.Fatal("stale policy writer replaced concurrent winner")
				}
			}
			if operation != "policy" {
				got, err := store.GetService(context.Background(), original.Service.ID)
				if err != nil || got.Service.Name != "concurrent winner" {
					t.Fatal("stale operation replaced or deleted concurrent winner")
				}
				secret, err := store.Get(context.Background(), secretstore.Ref(localServiceRef(original.Service.ID)))
				defer clear(secret)
				if err != nil || string(secret) != "config-test-original-secret" {
					t.Fatal("stale operation changed the credential")
				}
			}
		})
	}
}
