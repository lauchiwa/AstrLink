package controlapi

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	storage "github.com/QuantumNous/astrlink/core/internal/storage"
)

// A body stored as session chunks, and a raw body stored as a recipe that
// refers to them, read back through the API exactly as they were captured.
func TestAuditContentReadsChunkedAndRecipeBodies(t *testing.T) {
	vault := configuredRawVault()
	fixture := newRawAccessFixture(t, vault)
	vault.unlocked = true
	store := fixture.store
	ctx := context.Background()

	random := rand.New(rand.NewPCG(7, 11))
	history := make([]byte, 64<<10)
	for index := range history {
		history[index] = "abcdefghijklmnopqrstuvwxyz "[random.IntN(27)]
	}
	shared := `{"input":"` + string(history) + `"}`
	raw := `{"input":"` + string(history) + ` mail ` + rawTestSecret + `"}`

	session := contract.SessionID("session_chunked_audit")
	for _, id := range []contract.RequestID{"request_chunked_shared", "request_chunked_raw"} {
		if err := store.InsertRequestRecord(ctx, contract.RequestRecord{
			ID: id, StartedAt: time.Now().UTC(), Status: contract.RequestStatusSucceeded, SessionID: &session,
			InputProtocol: contract.ProtocolOpenAIChat,
			Audit:         contract.AuditRecordSummary{RequestBodyCaptured: true, UpstreamRequestBodyCaptured: true},
		}); err != nil {
			t.Fatal(err)
		}
	}
	insertRawTestPartsFor(t, store, "request_chunked_shared", storage.AuditExposureShareable,
		map[storage.AuditDirection]string{storage.AuditDirectionUpstreamRequest: shared})
	if _, err := store.SweepExpiredAuditData(ctx); err != nil {
		t.Fatal(err)
	}
	insertRawTestPartsFor(t, store, "request_chunked_raw", storage.AuditExposureRaw,
		map[storage.AuditDirection]string{storage.AuditDirectionRequest: raw})

	blobs, err := store.GetAuditBlobsByRequest(ctx, "request_chunked_raw")
	if err != nil || len(blobs) != 1 || blobs[0].Layout != storage.AuditLayoutRecipe {
		t.Fatalf("raw body blobs = %+v, %v", blobs, err)
	}
	content := readRawAudit(t, fixture.handler, rawAsOperator, rawAuditPath("request_chunked_shared"), "")
	if content.UpstreamRequestBody == nil || content.UpstreamRequestBody.Content != shared {
		t.Fatalf("chunked body = %#v", content.UpstreamRequestBody)
	}
	content = readRawAudit(t, fixture.handler, rawAsOperator, rawAuditPath("request_chunked_raw"), "")
	if content.RequestBody == nil || content.RequestBody.Withheld || content.RequestBody.Content != raw {
		t.Fatalf("recipe body = %#v", content.RequestBody)
	}

	vault.unlocked = false
	content = readRawAudit(t, fixture.handler, rawAsOperator, rawAuditPath("request_chunked_raw"), "")
	if content.RequestBody == nil || !content.RequestBody.Withheld || content.RequestBody.Reason != contract.AuditWithheldRawLocked {
		t.Fatalf("locked recipe body = %#v", content.RequestBody)
	}
}
