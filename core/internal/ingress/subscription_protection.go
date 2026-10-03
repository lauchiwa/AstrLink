package ingress

import (
	"context"
	"log"
	"net/http"

	"github.com/QuantumNous/astrlink/core/contract"
)

// subscriptionProtection holds the Routing switches of the subscription
// account protections for one request.
type subscriptionProtection struct {
	risk                bool
	codexRequests       bool
	claudeRequests      bool
	sessionIsolation    bool
	claudeIdentity      bool
	officialPassthrough bool
	claudeAutoLearn     bool
	codexAutoLearn      bool
	grokAutoLearn       bool
}

func subscriptionProtectionFrom(settings contract.RoutingSettings) subscriptionProtection {
	return subscriptionProtection{
		risk:                settings.SubscriptionRiskProtection,
		codexRequests:       settings.CodexRequestNormalization,
		claudeRequests:      settings.ClaudeRequestNormalization,
		sessionIsolation:    settings.SubscriptionSessionIsolation,
		claudeIdentity:      settings.ClaudeIdentityEnforcement,
		officialPassthrough: settings.OfficialClientPassthrough,
		claudeAutoLearn:     settings.ClaudeIdentityAutoLearn,
		codexAutoLearn:      settings.CodexIdentityAutoLearn,
		grokAutoLearn:       settings.GrokIdentityAutoLearn,
	}
}

// learnClientIdentity records the identity of a recognized official client.
// A failed write keeps the identity in memory until restart and is only
// logged: learning never fails the request.
func (handler *Handler) learnClientIdentity(ctx context.Context, provider contract.SubscriptionProvider, header http.Header) {
	learn := handler.identities.LearnClaude
	if provider == contract.SubscriptionProviderOpenAICodex {
		learn = handler.identities.LearnCodex
	} else if provider == contract.SubscriptionProviderXAIGrok {
		learn = handler.identities.LearnGrok
	}
	if _, err := learn(ctx, header); err != nil {
		logf := handler.recordLogger
		if logf == nil {
			logf = log.Printf
		}
		logf("learned %s client identity was not persisted: %v", provider, err)
	}
}

// identityCaptureBodyLimit bounds the body read used only for the Claude
// recognition check. It matches accountauth's own recognition bound, and an
// oversized body simply fails recognition instead of being partially scanned.
const identityCaptureBodyLimit = 1 << 20

// captureIdentityCandidate publishes an unconfirmed candidate for a service
// whose capture window an operator explicitly armed. Capture never changes what
// is forwarded and never fails the request: a storage failure is only logged.
// The candidate still requires operator confirmation before any use.
func (handler *Handler) captureIdentityCandidate(
	ctx context.Context,
	serviceID contract.ServiceID,
	original http.Header,
	body []byte,
) {
	if _, published, err := handler.identityCapture.Observe(ctx, serviceID, original, body); err != nil && !published {
		logf := handler.recordLogger
		if logf == nil {
			logf = log.Printf
		}
		logf("identity candidate for service %s was not saved: %v", serviceID, err)
	}
}

// subscriptionProtection reuses the settings the request already read. A
// store without routing settings, or a failed read, keeps every protection
// on: turning one off is always an explicit choice.
func (handler *Handler) subscriptionProtection(ctx context.Context) subscriptionProtection {
	if session := recordSessionFromContext(ctx); session != nil && session.routingSettings != nil {
		return subscriptionProtectionFrom(*session.routingSettings)
	}
	settings, loaded, err := handler.loadRoutingSettings(ctx)
	if err != nil || !loaded {
		settings = contract.DefaultRoutingSettings()
	}
	return subscriptionProtectionFrom(settings)
}
