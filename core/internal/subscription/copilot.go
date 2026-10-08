package subscription

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accountauth"
	"github.com/QuantumNous/astrlink/core/internal/providerapi"
)

// CopilotModels lists the models the account's Copilot plan offers, as
// OpenCode reads them: GET {api}/models with the Copilot API identity.
func (manager *Manager) CopilotModels(ctx context.Context, tokens accountauth.AccountTokens) ([]string, error) {
	endpoint := strings.TrimRight(manager.copilotConfig.APIBaseURL, "/") + "/models"
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	accountauth.ApplyCopilotAPIHeaders(request.Header, tokens)
	// OpenCode's catalog call carries no chat intent.
	request.Header.Del("Openai-Intent")
	request.Header.Set("Accept", "application/json")
	response, err := manager.copilotConfig.HTTPClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("copilot models returned status %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	return DecodeCopilotModels(body)
}

// DecodeCopilotModels keeps the offered chat models of a Copilot catalog,
// sorted and without duplicates.
func DecodeCopilotModels(body []byte) ([]string, error) {
	var catalog struct {
		Data []providerapi.CopilotModelEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode copilot models: %w", err)
	}
	if catalog.Data == nil {
		return nil, fmt.Errorf("copilot catalog omitted data")
	}
	seen := make(map[string]bool, len(catalog.Data))
	models := make([]string, 0, len(catalog.Data))
	for _, entry := range catalog.Data {
		if !entry.Offered() || seen[entry.ID] || len(entry.ID) > 256 {
			continue
		}
		seen[entry.ID] = true
		models = append(models, entry.ID)
	}
	sort.Strings(models)
	return models, nil
}

func (manager *Manager) copilotUsage(ctx context.Context, tokens accountauth.AccountTokens) (contract.SubscriptionUsage, error) {
	entitlement, err := accountauth.FetchCopilotEntitlement(ctx, manager.copilotConfig.HTTPClient, manager.copilotConfig.UsageURL, tokens.RefreshToken)
	if err != nil {
		return contract.SubscriptionUsage{}, fmt.Errorf("%w: %w", ErrUsageUnavailable, err)
	}
	usage := DecodeCopilotUsage(entitlement, manager.now().UTC())
	if usage.PlanType == "" {
		usage.PlanType = tokens.PlanType
	}
	return usage, nil
}

// DecodeCopilotUsage maps the plan's quota snapshots onto the usage
// snapshot: premium_interactions (the monthly premium allowance) is the
// primary window and every other limited quota an additional one. Unlimited
// quotas are left out, so an all-unlimited plan reports only its plan type.
func DecodeCopilotUsage(entitlement accountauth.CopilotEntitlement, now time.Time) contract.SubscriptionUsage {
	usage := contract.SubscriptionUsage{PlanType: strings.TrimSpace(entitlement.Plan)}
	if len(usage.PlanType) > 64 {
		usage.PlanType = ""
	}
	reset, hasReset := copilotQuotaReset(entitlement)
	names := make([]string, 0, len(entitlement.QuotaSnapshots))
	for name := range entitlement.QuotaSnapshots {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		window := copilotQuotaWindow(entitlement.QuotaSnapshots[name])
		if window == nil {
			continue
		}
		if hasReset && reset.After(now) {
			resetAt := reset
			window.ResetAt = &resetAt
			if seconds := int64(reset.Sub(reset.AddDate(0, -1, 0)) / time.Second); seconds > 0 {
				window.LimitWindowSeconds = &seconds
			}
		}
		if name == "premium_interactions" {
			usage.Primary = window
			limitReached := window.UsedPercent >= 100
			usage.LimitReached = &limitReached
			continue
		}
		if len(name) == 0 || len(name) > 128 {
			continue
		}
		usage.AdditionalRateLimits = append(usage.AdditionalRateLimits, contract.AdditionalRateLimit{
			LimitName: name, MeteredFeature: name, Primary: window,
		})
	}
	return usage
}

func copilotQuotaWindow(snapshot accountauth.CopilotQuotaSnapshot) *contract.RateLimitWindow {
	if snapshot.Unlimited {
		return nil
	}
	var used float64
	switch {
	case snapshot.PercentRemaining != nil:
		used = 100 - *snapshot.PercentRemaining
	case snapshot.Entitlement != nil && *snapshot.Entitlement > 0 && snapshot.Remaining != nil:
		used = (*snapshot.Entitlement - *snapshot.Remaining) / *snapshot.Entitlement * 100
	default:
		return nil
	}
	if math.IsNaN(used) || math.IsInf(used, 0) {
		return nil
	}
	used = math.Round(used*100) / 100
	return &contract.RateLimitWindow{UsedPercent: math.Min(math.Max(used, 0), 1000)}
}

// copilotQuotaReset reads the monthly reset: the UTC timestamp when present,
// otherwise the reset date at midnight UTC.
func copilotQuotaReset(entitlement accountauth.CopilotEntitlement) (time.Time, bool) {
	if reset, ok := parseRFC3339(entitlement.QuotaResetUTC); ok {
		return reset, true
	}
	reset, err := time.Parse("2006-01-02", strings.TrimSpace(entitlement.QuotaResetDate))
	if err != nil {
		return time.Time{}, false
	}
	return reset.UTC(), true
}
