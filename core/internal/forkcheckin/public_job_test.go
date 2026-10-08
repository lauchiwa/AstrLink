package forkcheckin

import (
	"encoding/json"
	"testing"
)

func TestPublicJobMatchesTheExtensionWireShape(t *testing.T) {
	job := finishedJob()
	for _, test := range []struct {
		private ProofSource
		public  PublicProof
	}{
		{ProofSourceNone, PublicProofNone},
		{ProofSourceTransportError, PublicProofNone},
		{ProofSourceSubmitResponse, PublicProofSubmission},
		{ProofSourceSiteStatus, PublicProofStatusRead},
		{ProofSourceStatusRecheck, PublicProofStatusRead},
	} {
		job.ProofSource = test.private
		public := job.Receipt().Public()
		if public.ProofSource != test.public {
			t.Fatalf("proof %s mapped to %s", test.private, public.ProofSource)
		}
		body, err := json.Marshal(public)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil {
			t.Fatal(err)
		}
		allowed := map[string]bool{"id": true, "account_id": true, "action": true, "status": true,
			"dispatched": true, "proof_source": true, "site_date": true, "reward": true,
			"failure_code": true, "created_at": true, "finished_at": true, "children": true}
		for name := range fields {
			if !allowed[name] {
				t.Fatalf("unexpected wire field: %s", name)
			}
		}
		for _, required := range []string{"id", "action", "status", "dispatched", "proof_source", "created_at", "children"} {
			if _, ok := fields[required]; !ok {
				t.Fatalf("missing wire field: %s", required)
			}
		}
		if string(fields["children"]) != "[]" || public.Reward == nil || !public.Reward.Known {
			t.Fatal("single-job children/reward do not match the contract")
		}
	}
	job.Reward = Reward{}
	if job.Receipt().Public().Reward != nil {
		t.Fatal("unknown reward was presented as zero")
	}
	batch := BatchReceipt{ParentID: "batch_wire", RequestID: "request_wire_batch", Children: []JobReceipt{job.Receipt()}}
	public := batch.Public()
	if public.AccountID != "" || public.ID != batch.ParentID || len(public.Children) != 1 || public.CreatedAt.IsZero() || public.Status != JobStatusQueued {
		t.Fatalf("invalid public batch acceptance: %+v", public)
	}
}
