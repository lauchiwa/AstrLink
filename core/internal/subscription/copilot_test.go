package subscription_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/subscription"
)

func TestDecodeCopilotUsageKeepsLimitedQuotasUntilTheMonthlyReset(t *testing.T) {
	var entitlement accountauth.CopilotEntitlement
	if err := json.Unmarshal([]byte(`{
		"copilot_plan": "individual_pro",
		"quota_reset_date": "2026-11-01",
		"quota_snapshots": {
			"chat": {"entitlement": 0, "remaining": 0, "percent_remaining": 100, "unlimited": true},
			"completions": {"entitlement": 0, "remaining": 0, "percent_remaining": 100, "unlimited": true},
			"premium_interactions": {"entitlement": 1500, "remaining": 375, "percent_remaining": 25.004, "unlimited": false},
			"agent_sessions": {"entitlement": 50, "remaining": 60, "unlimited": false}
		}
	}`), &entitlement); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC)
	usage := subscription.DecodeCopilotUsage(entitlement, now)
	usage.ServiceID, usage.FetchedAt = "service_copilot", now
	if err := usage.Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
	reset := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	if usage.PlanType != "individual_pro" || usage.Primary == nil || usage.Primary.UsedPercent != 75 ||
		usage.Primary.ResetAt == nil || !usage.Primary.ResetAt.Equal(reset) ||
		usage.Primary.LimitWindowSeconds == nil || *usage.Primary.LimitWindowSeconds != 31*24*3600 ||
		usage.LimitReached == nil || *usage.LimitReached {
		t.Fatalf("usage = %#v primary=%#v", usage, usage.Primary)
	}
	// Unlimited quotas are left out; a quota over its allowance reads 0%, not negative.
	if len(usage.AdditionalRateLimits) != 1 || usage.AdditionalRateLimits[0].LimitName != "agent_sessions" ||
		usage.AdditionalRateLimits[0].Primary.UsedPercent != 0 {
		t.Fatalf("additional = %#v", usage.AdditionalRateLimits)
	}

	past := subscription.DecodeCopilotUsage(accountauth.CopilotEntitlement{
		QuotaResetDate: "2026-10-01",
		QuotaSnapshots: map[string]accountauth.CopilotQuotaSnapshot{"premium_interactions": {PercentRemaining: new(-12.5)}},
	}, now)
	if past.Primary == nil || past.Primary.UsedPercent != 112.5 || past.Primary.ResetAt != nil || !*past.LimitReached {
		t.Fatalf("overage usage = %#v", past.Primary)
	}
}

func TestDecodeCopilotModelsKeepsOfferedChatModels(t *testing.T) {
	models, err := subscription.DecodeCopilotModels([]byte(`{"object":"list","data":[
		{"id":"gpt-5.4","model_picker_enabled":true,"supported_endpoints":["/responses","/chat/completions"]},
		{"id":"claude-sonnet-4.6","model_picker_enabled":true,"supported_endpoints":["/v1/messages","/chat/completions"]},
		{"id":"claude-sonnet-4.6","model_picker_enabled":true,"supported_endpoints":["/v1/messages"]},
		{"id":"text-embedding-3-small","model_picker_enabled":false,"supported_endpoints":["/embeddings"]}
	]}`))
	if err != nil || strings.Join(models, ",") != "claude-sonnet-4.6,gpt-5.4" {
		t.Fatalf("DecodeCopilotModels() = %v, %v", models, err)
	}
	if _, err := subscription.DecodeCopilotModels([]byte(`{"models":[]}`)); err == nil {
		t.Fatal("accepted a catalog without data")
	}
}
