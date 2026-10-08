package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	storage "github.com/QuantumNous/astrlink/core/internal/storage"
)

// agentMessage is one message of a simulated agent conversation: about 3 KiB
// of text that does not repeat.
func agentMessage(index int) string {
	random := rand.New(rand.NewPCG(uint64(index), 0x9e3779b97f4a7c15))
	const letters = "abcdefghijklmnopqrstuvwxyz      .,"
	text := make([]byte, 3<<10)
	for position := range text {
		text[position] = letters[random.IntN(len(letters))]
	}
	return string(text)
}

// agentBody is what an agent sends on its calls-th call: every message so
// far, then extra appended to the last one.
func agentBody(calls int, extra string) []byte {
	var body bytes.Buffer
	body.WriteString(`{"model":"gpt-5","input":[`)
	for index := range calls {
		if index > 0 {
			body.WriteByte(',')
		}
		message := agentMessage(index)
		if index == calls-1 {
			message += extra
		}
		fmt.Fprintf(&body, `{"role":"user","content":%q}`, message)
	}
	body.WriteString(`]}`)
	return body.Bytes()
}

func insertSessionRecord(t *testing.T, store *Store, id contract.RequestID, session contract.SessionID, status contract.RequestStatus) {
	t.Helper()
	audit := contract.NotCapturedAuditSummary()
	audit.RequestBodyCaptured, audit.UpstreamRequestBodyCaptured = true, true
	if err := store.InsertRequestRecord(context.Background(), contract.RequestRecord{
		ID: id, StartedAt: store.now(), Status: status, SessionID: &session,
		InputProtocol: contract.ProtocolOpenAIResponses, Audit: audit,
	}); err != nil {
		t.Fatal(err)
	}
}

func sealedBody(t *testing.T, key []byte, id contract.RequestID, direction storage.AuditDirection, body []byte, exposure storage.AuditExposure) storage.AuditBlob {
	t.Helper()
	nonce, ciphertext, err := storage.SealAuditBlob(key, body)
	if err != nil {
		t.Fatal(err)
	}
	return storage.AuditBlob{RequestID: id, Direction: direction, MediaType: "application/json", Nonce: nonce,
		Ciphertext: ciphertext, CapturedBytes: len(body), CreatedAt: time.Now().UTC(), Exposure: exposure}
}

// storeAgentCall records one finished call of the session with the same
// shareable body on both request directions, then chunks it.
func storeAgentCall(t *testing.T, store *Store, key []byte, session contract.SessionID, call int) contract.RequestID {
	t.Helper()
	return storeAgentCallAs(t, store, key, session, contract.RequestID(fmt.Sprintf("request_%s_%02d", session, call)), call)
}

func storeAgentCallAs(t *testing.T, store *Store, key []byte, session contract.SessionID, id contract.RequestID, call int) contract.RequestID {
	t.Helper()
	insertSessionRecord(t, store, id, session, contract.RequestStatusSucceeded)
	body := agentBody(call, "")
	for _, direction := range []storage.AuditDirection{storage.AuditDirectionRequest, storage.AuditDirectionUpstreamRequest} {
		insertRawTestBlob(t, store, sealedBody(t, key, id, direction, body, storage.AuditExposureShareable), storage.AuditExposureShareable)
	}
	if err := store.chunkRequestParts(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	return id
}

func queryCount(t *testing.T, store *Store, query string, args ...any) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func assertChunkedBody(t *testing.T, store *Store, key []byte, id contract.RequestID, direction storage.AuditDirection, want []byte) {
	t.Helper()
	blob := blobsByDirection(t, store, id)[direction]
	if blob.Layout != storage.AuditLayoutChunks || len(blob.Ciphertext) != 0 {
		t.Fatalf("%s %s: layout %q with %d inline bytes", id, direction, blob.Layout, len(blob.Ciphertext))
	}
	got, err := storage.OpenAuditChunks(key, blob.Chunks)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("%s %s reads back %d bytes (%v), want %d", id, direction, len(got), err, len(want))
	}
}

// openRawBody opens a raw part, following its recipe when it has one.
func openRawBody(t *testing.T, key rawTestKey, auditKey []byte, blob storage.AuditBlob) []byte {
	t.Helper()
	plain := []byte(openRawPart(t, key, blob))
	if blob.Layout != storage.AuditLayoutRecipe {
		return plain
	}
	assembled, err := storage.AssembleAuditRecipe(plain, auditKey, blob.Chunks)
	if err != nil {
		t.Fatalf("assemble %s: %v", blob.Direction, err)
	}
	return assembled
}

func TestAgentSessionBodiesGrowWithWhatEachCallAdds(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	key, err := store.GetOrCreateAuditKey(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const calls = 30
	sent := 0
	ids := make([]contract.RequestID, 0, calls)
	for call := 1; call <= calls; call++ {
		ids = append(ids, storeAgentCall(t, store, key, "session_agent", call))
		sent += len(agentBody(call, ""))
	}
	stored := queryCount(t, store, `SELECT COALESCE(SUM(length(ciphertext)), 0) FROM audit_chunks`)
	final := len(agentBody(calls, ""))
	t.Logf("sent %d bytes over %d calls, last body %d, stored %d", sent, calls, final, stored)
	if stored > sent/3 || stored > 5*final {
		t.Fatalf("stored %d bytes for %d sent; the last body alone is %d", stored, sent, final)
	}
	if payloads := queryCount(t, store, `SELECT COUNT(*) FROM audit_payloads`); payloads != 0 {
		t.Fatalf("%d whole payloads left after chunking", payloads)
	}
	for call, id := range ids {
		for _, direction := range []storage.AuditDirection{storage.AuditDirectionRequest, storage.AuditDirectionUpstreamRequest} {
			assertChunkedBody(t, store, key, id, direction, agentBody(call+1, ""))
		}
	}
}

func TestChunksAreSharedWithinASessionAndGoWithTheirLastPart(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	ctx := context.Background()
	key, err := store.GetOrCreateAuditKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first := storeAgentCall(t, store, key, "session_shared", 8)
	chunks := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`)
	if want := len(storage.SplitAuditChunks(agentBody(8, ""))); chunks != want {
		t.Fatalf("request and upstream bodies made %d chunks, want %d", chunks, want)
	}
	second := storeAgentCallAs(t, store, key, "session_shared", "request_shared_again", 8)
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`); got != chunks {
		t.Fatalf("a repeated body added chunks: %d, want %d", got, chunks)
	}
	// Another session keeps its own copy: chunks never link sessions.
	storeAgentCall(t, store, key, "session_other", 8)
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`); got != 2*chunks {
		t.Fatalf("another session's body made %d chunks in all, want %d", got, 2*chunks)
	}

	if err := store.DeleteRequestRecord(ctx, first); err != nil {
		t.Fatal(err)
	}
	assertChunkedBody(t, store, key, second, storage.AuditDirectionRequest, agentBody(8, ""))
	if err := store.DeleteRequestRecord(ctx, second); err != nil {
		t.Fatal(err)
	}
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`); got != chunks {
		t.Fatalf("chunks left after the session's last part went: %d, want %d", got, chunks)
	}
	if _, err := store.PurgeRequestRecords(ctx, contract.PurgeRequest{Scope: contract.PurgeScopeAll, Confirm: true}); err != nil {
		t.Fatal(err)
	}
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`) + queryCount(t, store, `SELECT COUNT(*) FROM audit_part_chunks`); got != 0 {
		t.Fatalf("purge left %d chunk rows", got)
	}
}

func TestOnlyEndedShareableRequestBodiesAreChunked(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	ctx := context.Background()
	key, err := store.GetOrCreateAuditKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	const id = contract.RequestID("request_in_flight")
	insertSessionRecord(t, store, id, "session_rules", contract.RequestStatusPending)
	body := agentBody(4, "")
	insertRawTestBlob(t, store, sealedBody(t, key, id, storage.AuditDirectionUpstreamRequest, body, storage.AuditExposureShareable), storage.AuditExposureShareable)
	insertRawTestBlob(t, store, sealedBody(t, key, id, storage.AuditDirectionRequest, body, storage.AuditExposurePending), storage.AuditExposurePending)
	insertRawTestBlob(t, store, sealedBody(t, key, id, storage.AuditDirectionResponse, body, storage.AuditExposureShareable), storage.AuditExposureShareable)
	if err := store.chunkRequestParts(ctx, id); err != nil {
		t.Fatal(err)
	}
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`); got != 0 {
		t.Fatalf("a request in flight was chunked: %d chunks", got)
	}

	record, err := store.GetRequestRecord(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	record.Status = contract.RequestStatusSucceeded
	if err := store.UpsertRequestRecord(ctx, record); err != nil {
		t.Fatal(err)
	}
	if err := store.chunkRequestParts(ctx, id); err != nil {
		t.Fatal(err)
	}
	blobs := blobsByDirection(t, store, id)
	assertChunkedBody(t, store, key, id, storage.AuditDirectionUpstreamRequest, body)
	// A body whose privacy decision never came, and responses, which agents
	// do not resend byte for byte, stay whole.
	for _, direction := range []storage.AuditDirection{storage.AuditDirectionRequest, storage.AuditDirectionResponse} {
		if blobs[direction].Layout != storage.AuditLayoutWhole {
			t.Fatalf("%s is laid out %q", direction, blobs[direction].Layout)
		}
	}
}

func TestSweepAndWorkerChunkFinishedRequests(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	ctx := context.Background()
	key, err := store.GetOrCreateAuditKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	insert := func(id contract.RequestID) []byte {
		insertSessionRecord(t, store, id, "session_background", contract.RequestStatusSucceeded)
		body := agentBody(3, string(id))
		insertRawTestBlob(t, store, sealedBody(t, key, id, storage.AuditDirectionUpstreamRequest, body, storage.AuditExposureShareable), storage.AuditExposureShareable)
		return body
	}

	// The sweep picks up a request the worker never heard of.
	missed := insert("request_missed")
	if _, err := store.SweepExpiredAuditData(ctx); err != nil {
		t.Fatal(err)
	}
	assertChunkedBody(t, store, key, "request_missed", storage.AuditDirectionUpstreamRequest, missed)

	processed := make(chan contract.RequestID, 1)
	store.chunker.processed = func(id contract.RequestID) { processed <- id }
	told := insert("request_told")
	store.ChunkRequestAudit("request_told")
	select {
	case id := <-processed:
		if id != "request_told" {
			t.Fatalf("worker processed %s", id)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the chunk worker never finished")
	}
	assertChunkedBody(t, store, key, "request_told", storage.AuditDirectionUpstreamRequest, told)

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// After Close nothing is queued or started.
	store.ChunkRequestAudit("request_told")
}

func TestRawBodyRefersOnlyToWhatItsSessionAlreadyShares(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	ctx := context.Background()
	auditKey, err := store.GetOrCreateAuditKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rawKey := createRawTestKey(t, store, 31)
	const session = contract.SessionID("session_redacted")
	for call := 1; call <= 10; call++ {
		storeAgentCall(t, store, auditKey, session, call)
	}
	chunksBefore := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`)

	// The eleventh call carries a secret the upstream body had redacted.
	const secret = "sk-live-7f3a9c2e1b4d4e8fa1c2-do-not-share"
	const id = contract.RequestID("request_redacted_11")
	insertSessionRecord(t, store, id, session, contract.RequestStatusPending)
	body := agentBody(11, " use "+secret+" to connect")
	insertRawTestBlob(t, store, sealedBody(t, auditKey, id, storage.AuditDirectionRequest, body, storage.AuditExposureRaw), storage.AuditExposureRaw)

	blob := blobsByDirection(t, store, id)[storage.AuditDirectionRequest]
	if blob.Layout != storage.AuditLayoutRecipe || len(blob.Chunks) == 0 {
		t.Fatalf("raw body is laid out %q with %d chunks", blob.Layout, len(blob.Chunks))
	}
	if len(blob.Ciphertext) > len(body)/4 {
		t.Fatalf("raw recipe is %d bytes for a %d byte body", len(blob.Ciphertext), len(body))
	}
	if got := openRawBody(t, rawKey, auditKey, blob); !bytes.Equal(got, body) {
		t.Fatalf("raw body reads back %d bytes, want %d", len(got), len(body))
	}
	// Raw content never becomes a chunk, and no chunk holds the secret.
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`); got != chunksBefore {
		t.Fatalf("a raw write added chunks: %d, want %d", got, chunksBefore)
	}
	rows, err := store.db.Query(`SELECT nonce, ciphertext FROM audit_chunks`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var chunk storage.AuditChunk
		if err := rows.Scan(&chunk.Nonce, &chunk.Ciphertext); err != nil {
			t.Fatal(err)
		}
		plain, err := storage.OpenAuditBlob(auditKey, chunk.Nonce, chunk.Ciphertext)
		if err != nil || strings.Contains(string(plain), "sk-live") {
			t.Fatalf("a shared chunk holds the secret (%v)", err)
		}
	}
	// A shareable reader sees neither the recipe nor its chunks.
	shareable, err := store.GetShareableAuditBlobsByRequest(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, part := range shareable {
		if part.Direction == storage.AuditDirectionRequest && (len(part.Ciphertext) != 0 || len(part.Chunks) != 0) {
			t.Fatal("the shareable view carries the raw recipe")
		}
	}

	// Forgetting the raw password drops the recipe and its references; the
	// shared chunks stay with the parts that share them.
	_, replacement := newRawTestKey(t, 32)
	if _, err := store.ReplaceRawSealingKey(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_part_chunks WHERE request_id = ?`, id); got != 0 {
		t.Fatalf("%d references left after the raw reset", got)
	}
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_chunks`); got != chunksBefore {
		t.Fatalf("the raw reset changed the shared chunks: %d, want %d", got, chunksBefore)
	}
	assertChunkedBody(t, store, auditKey, contract.RequestID("request_session_redacted_10"), storage.AuditDirectionUpstreamRequest, agentBody(10, ""))
}

func TestSettledAndResealedRawBodiesUseRecipes(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	ctx := context.Background()
	auditKey, err := store.GetOrCreateAuditKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rawKey := createRawTestKey(t, store, 41)
	const session = contract.SessionID("session_settle")
	for call := 1; call <= 6; call++ {
		storeAgentCall(t, store, auditKey, session, call)
	}

	// A body stored before its decision settles to raw while in flight.
	const settled = contract.RequestID("request_settled")
	insertSessionRecord(t, store, settled, session, contract.RequestStatusPending)
	settledBody := agentBody(7, " secret one")
	insertRawTestBlob(t, store, sealedBody(t, auditKey, settled, storage.AuditDirectionRequest, settledBody, storage.AuditExposurePending), storage.AuditExposurePending)
	if err := store.UpdateAuditExposure(ctx, settled, storage.AuditDirectionRequest, storage.AuditExposureRaw); err != nil {
		t.Fatal(err)
	}
	blob := blobsByDirection(t, store, settled)[storage.AuditDirectionRequest]
	if blob.Layout != storage.AuditLayoutRecipe || !bytes.Equal(openRawBody(t, rawKey, auditKey, blob), settledBody) {
		t.Fatalf("settled body is laid out %q", blob.Layout)
	}

	// One whose request ended undecided is resealed by the vault's pass.
	const ended = contract.RequestID("request_ended")
	insertSessionRecord(t, store, ended, session, contract.RequestStatusFailed)
	endedBody := agentBody(7, " secret two")
	insertRawTestBlob(t, store, sealedBody(t, auditKey, ended, storage.AuditDirectionRequest, endedBody, storage.AuditExposurePending), storage.AuditExposurePending)
	if _, err := store.ResealRawParts(ctx, 0); err != nil {
		t.Fatal(err)
	}
	blob = blobsByDirection(t, store, ended)[storage.AuditDirectionRequest]
	if blob.Layout != storage.AuditLayoutRecipe || !bytes.Equal(openRawBody(t, rawKey, auditKey, blob), endedBody) {
		t.Fatalf("resealed body is laid out %q", blob.Layout)
	}

	// A shareable body that must tighten leaves its chunks first, so its
	// content is never raw under dek_audit alone.
	tightened := storeAgentCall(t, store, auditKey, "session_tighten", 3)
	if err := store.UpdateAuditExposure(ctx, tightened, storage.AuditDirectionRequest, storage.AuditExposureRaw); err != nil {
		t.Fatal(err)
	}
	blob = blobsByDirection(t, store, tightened)[storage.AuditDirectionRequest]
	if blob.Sealing != storage.AuditSealingRawV1 || !bytes.Equal(openRawBody(t, rawKey, auditKey, blob), agentBody(3, "")) {
		t.Fatalf("tightened body is sealed %q, laid out %q", blob.Sealing, blob.Layout)
	}
	assertChunkedBody(t, store, auditKey, tightened, storage.AuditDirectionUpstreamRequest, agentBody(3, ""))
}

func TestStaleRecipeIsNeverStored(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "astrlink.db"))
	defer store.Close()
	ctx := context.Background()
	auditKey, err := store.GetOrCreateAuditKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	rawKey := createRawTestKey(t, store, 51)
	shared := storeAgentCall(t, store, auditKey, "session_stale", 5)
	const id = contract.RequestID("request_stale")
	insertSessionRecord(t, store, id, "session_stale", contract.RequestStatusPending)
	body := agentBody(6, " secret")
	captured := sealedBody(t, auditKey, id, storage.AuditDirectionRequest, body, storage.AuditExposureRaw)
	part, err := sealRawPart(ctx, store.db, auditKey, captured, rawKey.id, rawKey.public, true)
	if err != nil || part.blob.Layout != storage.AuditLayoutRecipe {
		t.Fatalf("recipe = %q, %v", part.blob.Layout, err)
	}
	// The chunks it names go before it is stored.
	if err := store.DeleteRequestRecord(ctx, shared); err != nil {
		t.Fatal(err)
	}
	if err := store.writeSealedRawPart(ctx, part); !errors.Is(err, errRawRecipeStale) {
		t.Fatalf("stale recipe stored: %v", err)
	}
	if got := queryCount(t, store, `SELECT COUNT(*) FROM audit_blobs WHERE request_id = ?`, id) +
		queryCount(t, store, `SELECT COUNT(*) FROM audit_payloads WHERE sealing = 'raw_v1'`); got != 0 {
		t.Fatalf("a stale recipe left %d rows", got)
	}
	// The capture path then seals the body whole.
	insertRawTestBlob(t, store, captured, storage.AuditExposureRaw)
	blob := blobsByDirection(t, store, id)[storage.AuditDirectionRequest]
	if blob.Layout != storage.AuditLayoutWhole || !bytes.Equal(openRawBody(t, rawKey, auditKey, blob), body) {
		t.Fatalf("raw body without shared chunks is laid out %q", blob.Layout)
	}
}
