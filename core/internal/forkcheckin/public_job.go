package forkcheckin

import "time"

// PublicJob is the wire DTO in extensions/checkin.openapi.yaml. JobReceipt
// retains internal scheduler context; serialize this projection at the control
// boundary so private proof values and bookkeeping never enter a response.
type PublicJob struct {
	ID          JobID         `json:"id"`
	AccountID   AccountID     `json:"account_id,omitempty"`
	Action      JobAction     `json:"action"`
	Status      JobStatus     `json:"status"`
	Dispatched  bool          `json:"dispatched"`
	ProofSource PublicProof   `json:"proof_source"`
	SiteDate    string        `json:"site_date,omitempty"`
	Reward      *PublicReward `json:"reward,omitempty"`
	FailureCode string        `json:"failure_code,omitempty"`
	CreatedAt   time.Time     `json:"created_at"`
	FinishedAt  *time.Time    `json:"finished_at,omitempty"`
	Children    []PublicJob   `json:"children"`
}

type PublicProof string

const (
	PublicProofNone       PublicProof = "none"
	PublicProofSubmission PublicProof = "submission_response"
	PublicProofStatusRead PublicProof = "status_read"
)

type PublicReward struct {
	Known bool   `json:"known"`
	Quota int64  `json:"quota"`
	Unit  string `json:"unit"`
}

func (receipt JobReceipt) Public() PublicJob {
	proof := PublicProofNone
	switch receipt.ProofSource {
	case ProofSourceSubmitResponse:
		proof = PublicProofSubmission
	case ProofSourceSiteStatus, ProofSourceStatusRecheck:
		proof = PublicProofStatusRead
	}
	result := PublicJob{
		ID: receipt.ID, AccountID: receipt.AccountID, Action: receipt.Action,
		Status: receipt.Status, Dispatched: receipt.Dispatched, ProofSource: proof,
		SiteDate: receipt.SiteDay, FailureCode: receipt.FailureCode,
		CreatedAt: receipt.CreatedAt, FinishedAt: receipt.CompletedAt,
		Children: make([]PublicJob, 0),
	}
	if receipt.Reward != nil {
		result.Reward = &PublicReward{Known: true, Quota: receipt.Reward.Quota, Unit: receipt.Reward.Unit}
	}
	return result
}

// Public projects the immutable acceptance of a batch. Current aggregate
// states are computed separately from the child jobs, not from this receipt.
func (receipt BatchReceipt) Public() PublicJob {
	result := PublicJob{
		ID: receipt.ParentID, Status: JobStatusQueued, ProofSource: PublicProofNone,
		Children: make([]PublicJob, 0, len(receipt.Children)),
	}
	for index, child := range receipt.Children {
		if index == 0 {
			result.Action, result.CreatedAt = child.Action, child.CreatedAt
		}
		result.Children = append(result.Children, child.Public())
	}
	return result
}
