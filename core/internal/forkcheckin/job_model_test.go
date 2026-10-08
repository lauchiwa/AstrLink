package forkcheckin

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testTime() time.Time {
	return time.Date(2026, time.March, 2, 9, 0, 0, 0, time.UTC)
}

// finishedJob is a valid terminal job the individual tests then break in one
// specific way.
func finishedJob() Job {
	completed := testTime().Add(time.Second)
	return Job{
		ID:               "job_01",
		AccountID:        "account_one",
		Action:           JobActionCheckIn,
		Trigger:          JobTriggerManual,
		RequestID:        "req_aaaaaaaa",
		InputDigest:      "digest",
		ExpectedRevision: 3,
		SiteDay:          "2026-03-02",
		Status:           JobStatusSuccess,
		Dispatched:       true,
		ProofSource:      ProofSourceSubmitResponse,
		Reward:           Reward{Known: true, Quota: 1000, Unit: "quota"},
		CreatedAt:        testTime(),
		CompletedAt:      &completed,
		Attempts:         1,
	}
}

func TestSuccessRequiresProofFromTheSiteAndItsOwnSubmission(t *testing.T) {
	if err := finishedJob().Validate(); err != nil {
		t.Fatalf("a proven success was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Job){
		// An HTTP 200 with no parsed answer leaves the source unset.
		"no proof": func(job *Job) { job.ProofSource = ProofSourceNone },
		"transport error proves nothing": func(job *Job) {
			job.ProofSource = ProofSourceTransportError
		},
		// A status read can show the day is done, but not that this job did it.
		"status read alone": func(job *Job) { job.ProofSource = ProofSourceSiteStatus },
		"never dispatched":  func(job *Job) { job.Dispatched = false },
		"read-only action": func(job *Job) {
			job.Action = JobActionStatusRefresh
			job.Dispatched = false
		},
	} {
		job := finishedJob()
		mutate(&job)
		if err := job.Validate(); err == nil {
			t.Fatalf("%s was accepted as a success", name)
		}
	}
	// A lost submit response resolved by re-reading status is a valid proof.
	job := finishedJob()
	job.ProofSource = ProofSourceStatusRecheck
	if err := job.Validate(); err != nil {
		t.Fatalf("a status recheck proof was rejected: %v", err)
	}
}

func TestADispatchedSubmissionIsUncertainRatherThanRetryable(t *testing.T) {
	completed := testTime().Add(time.Second)
	base := Job{
		ID: "job_02", AccountID: "account_one", Action: JobActionCheckIn,
		Trigger: JobTriggerAutomatic, RequestID: "req_bbbbbbbb", InputDigest: "digest",
		ExpectedRevision: 1, CreatedAt: testTime(), CompletedAt: &completed, Attempts: 1,
	}

	dispatchedRetryable := base
	dispatchedRetryable.Status = JobStatusRetryableFailure
	dispatchedRetryable.Dispatched = true
	dispatchedRetryable.ProofSource = ProofSourceTransportError
	if err := dispatchedRetryable.Validate(); err == nil {
		t.Fatal("a dispatched submission was marked retryable")
	}

	uncertain := base
	uncertain.Status = JobStatusUncertain
	uncertain.Dispatched = true
	uncertain.ProofSource = ProofSourceTransportError
	if err := uncertain.Validate(); err != nil {
		t.Fatalf("a dispatched, unresolved submission was rejected: %v", err)
	}
	// The day stays unresolved: it is neither a success nor a failure.
	if uncertain.Status.CountsAsDoneToday() {
		t.Fatal("an uncertain result closed the day")
	}
	if !uncertain.Status.Terminal() {
		t.Fatal("an uncertain job is not terminal")
	}

	undispatchedRetryable := base
	undispatchedRetryable.Status = JobStatusRetryableFailure
	undispatchedRetryable.ProofSource = ProofSourceTransportError
	if err := undispatchedRetryable.Validate(); err != nil {
		t.Fatalf("a failure before dispatch must stay retryable: %v", err)
	}
	if undispatchedRetryable.Status.CountsAsDoneToday() {
		t.Fatal("a retryable failure closed the day")
	}
}

func TestCancellationKeepsDispatchAndClaimsNoUpstreamRollback(t *testing.T) {
	completed := testTime().Add(time.Second)
	cancelled := Job{
		ID: "job_03", AccountID: "account_one", Action: JobActionCheckIn,
		Trigger: JobTriggerManual, RequestID: "req_cccccccc", InputDigest: "digest",
		ExpectedRevision: 1, Status: JobStatusCancelled, Dispatched: true,
		ProofSource: ProofSourceNone, CreatedAt: testTime(), CompletedAt: &completed,
	}
	if err := cancelled.Validate(); err != nil {
		t.Fatalf("cancelling after dispatch was rejected: %v", err)
	}
	// Cancelling is local, so the receipt must keep showing that the
	// request had already left the machine.
	if !cancelled.Receipt().Dispatched {
		t.Fatal("cancellation hid that a submission was dispatched")
	}
	if cancelled.Status.CountsAsDoneToday() {
		t.Fatal("a cancelled job closed the day")
	}

	withProof := cancelled
	withProof.ProofSource = ProofSourceSubmitResponse
	if err := withProof.Validate(); err == nil {
		t.Fatal("a local cancellation carried site proof")
	}
}

func TestOnlyACheckInCarriesARewardAndUnfinishedJobsCarryNoResult(t *testing.T) {
	for name, mutate := range map[string]func(*Job){
		"already checked with reward": func(job *Job) {
			job.Status = JobStatusAlreadyChecked
			job.ProofSource = ProofSourceSiteStatus
			job.Dispatched = false
		},
		"auth required with reward": func(job *Job) {
			job.Status = JobStatusAuthRequired
			job.ProofSource = ProofSourceTransportError
			job.Dispatched = false
		},
	} {
		job := finishedJob()
		mutate(&job)
		if err := job.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}

	queued := Job{
		ID: "job_04", AccountID: "account_one", Action: JobActionCheckIn,
		Trigger: JobTriggerManual, RequestID: "req_dddddddd", InputDigest: "digest",
		ExpectedRevision: 1, Status: JobStatusQueued, ProofSource: ProofSourceNone,
		CreatedAt: testTime(),
	}
	if err := queued.Validate(); err != nil {
		t.Fatalf("a queued job was rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Job){
		"proof":        func(job *Job) { job.ProofSource = ProofSourceSubmitResponse },
		"reward":       func(job *Job) { job.Reward = Reward{Known: true, Quota: 1, Unit: "quota"} },
		"failure code": func(job *Job) { job.FailureCode = "already_done" },
		"completed at": func(job *Job) { now := testTime(); job.CompletedAt = &now },
	} {
		job := queued
		mutate(&job)
		if err := job.Validate(); err == nil {
			t.Fatalf("an unfinished job with a %s was accepted", name)
		}
	}
	// A finished job must have a completion time.
	finished := finishedJob()
	finished.CompletedAt = nil
	if err := finished.Validate(); err == nil {
		t.Fatal("a finished job without a completion time was accepted")
	}
}

func TestAStatusRefreshCanNeverDispatchASubmission(t *testing.T) {
	completed := testTime().Add(time.Second)
	refresh := Job{
		ID: "job_05", AccountID: "account_one", Action: JobActionStatusRefresh,
		Trigger: JobTriggerManual, RequestID: "req_eeeeeeee", InputDigest: "digest",
		ExpectedRevision: 1, Status: JobStatusAlreadyChecked, ProofSource: ProofSourceSiteStatus,
		CreatedAt: testTime(), CompletedAt: &completed,
	}
	if err := refresh.Validate(); err != nil {
		t.Fatalf("a read-only refresh was rejected: %v", err)
	}
	refresh.Dispatched = true
	if err := refresh.Validate(); err == nil {
		t.Fatal("a status refresh reported a dispatched submission")
	}
}

func TestNotCheckedIsAProvenRefreshNotADailyCheckIn(t *testing.T) {
	job := finishedJob()
	job.Action, job.Status = JobActionStatusRefresh, JobStatusNotChecked
	job.Dispatched, job.Reward, job.ProofSource = false, Reward{}, ProofSourceSiteStatus
	if err := job.Validate(); err != nil || !job.Status.Terminal() || job.Status.CountsAsDoneToday() {
		t.Fatalf("valid not-checked refresh rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Job){
		"check-in action":  func(job *Job) { job.Action = JobActionCheckIn },
		"dispatched":       func(job *Job) { job.Dispatched = true },
		"no proof":         func(job *Job) { job.ProofSource = ProofSourceNone },
		"submission proof": func(job *Job) { job.ProofSource = ProofSourceSubmitResponse },
		"reward":           func(job *Job) { job.Reward = Reward{Known: true, Quota: 1, Unit: "quota"} },
		"failure":          func(job *Job) { job.FailureCode = "network" },
	} {
		invalid := job
		mutate(&invalid)
		if invalid.Validate() == nil {
			t.Fatalf("invalid not-checked outcome accepted: %s", name)
		}
	}
}

func TestTheSameRequestIDReplaysAndDifferentContentConflicts(t *testing.T) {
	request := JobRequest{
		RequestID: "req_ffffffff", AccountIDs: []AccountID{"account_one"},
		Action: JobActionCheckIn, Trigger: JobTriggerManual, ExpectedRevision: 2,
	}
	if err := request.Validate(); err != nil {
		t.Fatalf("a single-account request was rejected: %v", err)
	}
	if got := ClassifyRequest("", request); got != IdempotencyCreated {
		t.Fatalf("a new request id = %q", got)
	}
	// The desktop transport retries some writes on its own: the same ID and
	// content must return the first receipt, not start a second check-in.
	if got := ClassifyRequest(request.Digest(), request); got != IdempotencyReplayed {
		t.Fatalf("a retried identical request = %q", got)
	}
	for name, changed := range map[string]JobRequest{
		"another account":  {RequestID: request.RequestID, AccountIDs: []AccountID{"account_two"}, Action: JobActionCheckIn, Trigger: JobTriggerManual, ExpectedRevision: 2},
		"another action":   {RequestID: request.RequestID, AccountIDs: []AccountID{"account_one"}, Action: JobActionStatusRefresh, Trigger: JobTriggerManual, ExpectedRevision: 2},
		"another revision": {RequestID: request.RequestID, AccountIDs: []AccountID{"account_one"}, Action: JobActionCheckIn, Trigger: JobTriggerManual, ExpectedRevision: 3},
		"another trigger":  {RequestID: request.RequestID, AccountIDs: []AccountID{"account_one"}, Action: JobActionCheckIn, Trigger: JobTriggerAutomatic, ExpectedRevision: 2},
	} {
		if got := ClassifyRequest(request.Digest(), changed); got != IdempotencyConflict {
			t.Fatalf("reusing an id with %s = %q, want a conflict", name, got)
		}
	}
	// Account order is not content: the same set must still replay.
	batch := JobRequest{
		RequestID: "req_gggggggg", AccountIDs: []AccountID{"account_one", "account_two"},
		Action: JobActionCheckIn, Trigger: JobTriggerManual,
	}
	reordered := batch
	reordered.AccountIDs = []AccountID{"account_two", "account_one"}
	if got := ClassifyRequest(batch.Digest(), reordered); got != IdempotencyReplayed {
		t.Fatalf("a reordered batch = %q, want a replay", got)
	}
}

func TestJobRequestBoundsBatchesAndPinsRevisionOnlyForOneAccount(t *testing.T) {
	many := make([]AccountID, 0, MaxBatchAccounts+1)
	for index := 0; index <= MaxBatchAccounts; index++ {
		many = append(many, AccountID("account_"+string(rune('a'+index%26))+string(rune('a'+index/26))))
	}
	for name, request := range map[string]JobRequest{
		"no account":                 {RequestID: "req_hhhhhhhh", Action: JobActionCheckIn, Trigger: JobTriggerManual, ExpectedRevision: 1},
		"too many":                   {RequestID: "req_hhhhhhhh", AccountIDs: many, Action: JobActionCheckIn, Trigger: JobTriggerManual},
		"duplicate":                  {RequestID: "req_hhhhhhhh", AccountIDs: []AccountID{"account_one", "account_one"}, Action: JobActionCheckIn, Trigger: JobTriggerManual},
		"short id":                   {RequestID: "req", AccountIDs: []AccountID{"account_one"}, Action: JobActionCheckIn, Trigger: JobTriggerManual, ExpectedRevision: 1},
		"unsafe id":                  {RequestID: "req ffffffff/../x", AccountIDs: []AccountID{"account_one"}, Action: JobActionCheckIn, Trigger: JobTriggerManual, ExpectedRevision: 1},
		"single without revision":    {RequestID: "req_hhhhhhhh", AccountIDs: []AccountID{"account_one"}, Action: JobActionCheckIn, Trigger: JobTriggerManual},
		"batch pinning one revision": {RequestID: "req_hhhhhhhh", AccountIDs: []AccountID{"account_one", "account_two"}, Action: JobActionCheckIn, Trigger: JobTriggerManual, ExpectedRevision: 2},
	} {
		if err := request.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
	valid := JobRequest{
		RequestID: "req_iiiiiiii", AccountIDs: []AccountID{"account_one", "account_two"},
		Action: JobActionCheckIn, Trigger: JobTriggerManual,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("a valid batch was rejected: %v", err)
	}
}

func TestJobReceiptPublishesNoSecretAndNoSiteMessage(t *testing.T) {
	encoded, err := json.Marshal(finishedJob().Receipt())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{}{
		"id": {}, "account_id": {}, "action": {}, "trigger": {}, "request_id": {},
		"status": {}, "dispatched": {}, "proof_source": {}, "site_day": {},
		"reward": {}, "created_at": {}, "completed_at": {}, "attempts": {},
	}
	for field := range fields {
		if _, ok := want[field]; !ok {
			t.Fatalf("receipt published an unexpected field %q", field)
		}
	}
	// The private digest and revision are bookkeeping, not public fields.
	for _, forbidden := range []string{"input_digest", "expected_revision", "credential", "session", "cookie"} {
		if _, leaked := fields[forbidden]; leaked {
			t.Fatalf("receipt published %q", forbidden)
		}
	}
	if strings.Contains(string(encoded), "digest") {
		t.Fatalf("receipt body carried the input digest: %s", encoded)
	}

	// An unknown reward is omitted rather than rendered as zero.
	unknown := finishedJob()
	unknown.Status = JobStatusAlreadyChecked
	unknown.ProofSource = ProofSourceSiteStatus
	unknown.Reward = Reward{}
	if unknown.Receipt().Reward != nil {
		t.Fatal("an unknown reward was published as a value")
	}
}

func TestExtensionStatusIsAvailableWhileDisabledAndReportsNoStorage(t *testing.T) {
	disabled := ExtensionStatus{ProtocolVersion: ProtocolVersion}
	encoded, err := json.Marshal(disabled)
	if err != nil {
		t.Fatal(err)
	}
	// The workspace needs an entry point while the feature is off, so the
	// shape must be renderable with no storage behind it.
	for _, field := range []string{`"enabled":false`, `"initialized":false`, `"account_count":0`} {
		if !strings.Contains(string(encoded), field) {
			t.Fatalf("disabled status is missing %s: %s", field, encoded)
		}
	}
	failed := ExtensionStatus{ProtocolVersion: ProtocolVersion, Enabled: true, LastInitError: "storage_unavailable"}
	if !strings.Contains(mustJSON(t, failed), `"last_init_error":"storage_unavailable"`) {
		t.Fatal("a failed enable must stay visible so it can be retried")
	}
}

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestListOptionsRejectOversizedPagesRatherThanClampingThem(t *testing.T) {
	if (ListOptions{}).EffectiveLimit() != DefaultPageSize {
		t.Fatal("an omitted limit did not fall back to the default")
	}
	if (ListOptions{Limit: 5}).EffectiveLimit() != 5 {
		t.Fatal("an explicit limit was not used")
	}
	if err := (ListOptions{Limit: MaxPageSize}).Validate(); err != nil {
		t.Fatalf("the maximum page size was rejected: %v", err)
	}
	// Silently clamping would let a caller believe it had every row.
	for name, options := range map[string]ListOptions{
		"over maximum": {Limit: MaxPageSize + 1},
		"negative":     {Limit: -1},
		"long cursor":  {Cursor: strings.Repeat("c", 257)},
	} {
		if err := options.Validate(); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestJobStatusAndProofSourceCoverTheDocumentedSets(t *testing.T) {
	for _, status := range []JobStatus{
		JobStatusQueued, JobStatusRunning, JobStatusSuccess, JobStatusAlreadyChecked,
		JobStatusAuthRequired, JobStatusManualRequired, JobStatusUnsupported, JobStatusNotChecked,
		JobStatusRateLimited, JobStatusRetryableFailure, JobStatusUncertain,
		JobStatusCancelled,
	} {
		if err := status.Validate(); err != nil {
			t.Fatalf("documented status %q was rejected", status)
		}
	}
	if err := JobStatus("checked_in_maybe").Validate(); err == nil {
		t.Fatal("an unknown status was accepted")
	}
	if err := ProofSource("balance_changed").Validate(); err == nil {
		t.Fatal("a balance change was accepted as proof")
	}
	for _, source := range []ProofSource{ProofSourceSiteStatus, ProofSourceSubmitResponse, ProofSourceStatusRecheck} {
		if err := source.Validate(); err != nil {
			t.Fatalf("documented proof source %q was rejected", source)
		}
	}
	if JobStatusQueued.Terminal() || JobStatusRunning.Terminal() {
		t.Fatal("a pending status was reported terminal")
	}
}
