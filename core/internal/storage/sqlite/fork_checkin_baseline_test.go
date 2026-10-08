package sqlite

// This test-only probe is also copied into a clean git archive of the baseline
// Core. Keep it independent of forkcheckin and its production storage helpers.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/secretstore"
	storagecontract "github.com/QuantumNous/astrlink/core/internal/storage"
	"github.com/QuantumNous/astrlink/core/internal/storage/migrate"
)

const (
	forkCheckinCompatService = "service_checkin_compat"
	forkCheckinCompatSecret  = "CHECKIN-COMPAT-SERVICE-SECRET-CANARY"
	forkCheckinCompatKeyByte = 0x53
)

type forkCheckinTableDigest struct {
	Columns []string
	Rows    []string
}

type forkCheckinDatabaseDigest struct {
	Schema []string
	Tables map[string]forkCheckinTableDigest
}

type forkCheckinBaselineReport struct {
	Main    forkCheckinDatabaseDigest
	ETag    string
	Version int64
}

// Hash each typed row before comparing unordered multisets. The report must
// not print credentials or captured bodies on a test failure. Schema SQL and
// all table columns/rows are covered, including key envelopes and bootstrap.
func snapshotForkCheckinDatabase(t *testing.T, store *Store, mainOnly bool) forkCheckinDatabaseDigest {
	t.Helper()
	result := forkCheckinDatabaseDigest{Tables: make(map[string]forkCheckinTableDigest)}
	rows, err := store.db.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_schema ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var kind, name, table, definition string
		if err := rows.Scan(&kind, &name, &table, &definition); err != nil {
			t.Fatal(err)
		}
		if mainOnly && strings.HasPrefix(table, "fork_checkin_") {
			continue
		}
		result.Schema = append(result.Schema, forkCheckinDigestJSON(t, []string{kind, name, table, definition}))
		if kind == "table" {
			tables = append(tables, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	for _, table := range tables {
		quoted := `"` + strings.ReplaceAll(table, `"`, `""`) + `"`
		rows, err := store.db.Query(`SELECT * FROM ` + quoted)
		if err != nil {
			t.Fatalf("snapshot table %s: %v", table, err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		digest := forkCheckinTableDigest{Columns: columns, Rows: make([]string, 0)}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for index := range values {
				pointers[index] = &values[index]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			// Retain SQL value types: a BLOB's base64 JSON must not collide
			// with a TEXT value that happens to contain that same base64.
			typed := make([]any, len(values))
			for index, value := range values {
				typed[index] = []any{fmt.Sprintf("%T", value), value}
			}
			digest.Rows = append(digest.Rows, forkCheckinDigestJSON(t, typed))
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		sort.Strings(digest.Rows)
		result.Tables[table] = digest
	}
	return result
}

func forkCheckinDigestJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func assertForkCheckinDatabaseDigest(t *testing.T, want, got forkCheckinDatabaseDigest) {
	t.Helper()
	if !reflect.DeepEqual(want.Schema, got.Schema) {
		t.Error("database schema changed")
	}
	if len(want.Tables) != len(got.Tables) {
		t.Errorf("table count changed: %d -> %d", len(want.Tables), len(got.Tables))
	}
	for table, before := range want.Tables {
		if !reflect.DeepEqual(before, got.Tables[table]) {
			t.Errorf("table %s changed (columns or typed row bytes)", table)
		}
	}
}

func seedForkCheckinBaseline(t *testing.T, path string) (*Store, forkCheckinBaselineReport) {
	t.Helper()
	ctx := context.Background()
	store := openWithKey(t, path, testLocalKey(t, forkCheckinCompatKeyByte), nil)
	service := contract.ServiceFromEndpoint(testEndpoint(forkCheckinCompatService))
	record, err := store.CreateService(ctx, service, storagecontract.CredentialMutation{Present: true, Secret: []byte(forkCheckinCompatSecret)})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.InsertRequestRecord(ctx, contract.RequestRecord{
		ID: bodyRequestID, StartedAt: time.Now().UTC(), Status: contract.RequestStatusSucceeded,
		InputProtocol: contract.ProtocolOpenAIChat,
	}); err != nil {
		t.Fatal(err)
	}
	auditKey, err := store.GetAuditKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(auditKey)
	if err := store.InsertAuditBlob(ctx, sealedPayload(t, auditKey, bodyRequestID, storagecontract.AuditDirectionRequest, knownPrompt)); err != nil {
		t.Fatal(err)
	}
	report := forkCheckinBaselineReport{ETag: record.ETag}
	if err := store.db.QueryRow(`SELECT MAX(version) FROM schema_migrations`).Scan(&report.Version); err != nil {
		t.Fatal(err)
	}
	report.Main = snapshotForkCheckinDatabase(t, store, true)
	return store, report
}

func assertForkCheckinBaselineReadable(t *testing.T, store *Store, report forkCheckinBaselineReport) {
	t.Helper()
	ctx := context.Background()
	record, err := store.GetService(ctx, forkCheckinCompatService)
	if err != nil || record.ETag != report.ETag {
		t.Fatalf("baseline service or ETag changed: %v", err)
	}
	secret, err := store.Get(ctx, secretstore.Ref("local://service/"+forkCheckinCompatService))
	defer clear(secret)
	if err != nil || !bytes.Equal(secret, []byte(forkCheckinCompatSecret)) {
		t.Fatalf("baseline service secret no longer decrypts: %v", err)
	}
	auditKey, err := store.GetAuditKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(auditKey)
	assertBodyReadable(t, store, auditKey)
}

func TestForkCheckinBaselineProcess(t *testing.T) {
	path := os.Getenv("ASTRLINK_CHECKIN_BASELINE_DB")
	if path == "" {
		t.Skip("baseline subprocess helper")
	}
	mode := os.Getenv("ASTRLINK_CHECKIN_BASELINE_MODE")
	reportPath := os.Getenv("ASTRLINK_CHECKIN_BASELINE_REPORT")
	if mode == "create" {
		store, report := seedForkCheckinBaseline(t, path)
		defer store.Close()
		body, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(reportPath, body, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	body, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatal(err)
	}
	var report forkCheckinBaselineReport
	if err := json.Unmarshal(body, &report); err != nil {
		t.Fatal(err)
	}
	options := []Option{WithLocalKey(testLocalKey(t, forkCheckinCompatKeyByte))}
	switch mode {
	case "readonly":
		options = append(options, WithReadOnly())
	case "offline":
		options = append(options, WithExistingDatabase())
	case "normal":
	default:
		t.Fatalf("unknown baseline mode %q", mode)
	}
	store, err := Open(context.Background(), path, options...)
	// An upstream sync can add main migrations, so this older Core may be
	// behind the database. Refusing it is the designed outcome; the parent
	// decides which outcome to expect by comparing schema versions.
	if os.Getenv("ASTRLINK_CHECKIN_BASELINE_EXPECT") == "schema_newer" {
		if !errors.Is(err, migrate.ErrDatabaseNewer) {
			if err == nil {
				_ = store.Close()
			}
			t.Fatalf("baseline %s opened a newer database: %v", mode, err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	assertForkCheckinBaselineReadable(t, store, report)
	assertForkCheckinDatabaseDigest(t, report.Main, snapshotForkCheckinDatabase(t, store, true))
}
