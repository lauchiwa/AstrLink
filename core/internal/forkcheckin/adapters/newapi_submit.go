package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

// NewAPISubmit adds the single standard POST to a read-only adapter. The owner
// must persist its dispatch intent before calling Submit, and must never call
// Submit again for an uncertain job. Reconciliation uses ReadStatus separately,
// with a fresh bounded context; a readback cannot prove which POST earned quota.
// This type does not enable browser capture or refresh dashboard credentials.
type NewAPISubmit struct{ *NewAPIRead }

var _ forkcheckin.SiteAdapter = (*NewAPISubmit)(nil)

func NewNewAPISubmit(read *NewAPIRead) (*NewAPISubmit, error) {
	// Modern browser session acquisition has not passed G02. Fail closed rather
	// than silently treating successful Go fixtures as a browser admission gate.
	if read == nil || read.dialect != NewAPILegacy {
		return nil, forkcheckin.ErrManualRequired
	}
	return &NewAPISubmit{NewAPIRead: read}, nil
}

func (adapter *NewAPISubmit) Submit(ctx context.Context, snapshot forkcheckin.AccountSnapshot) (forkcheckin.SubmitOutcome, error) {
	outcome := forkcheckin.SubmitOutcome{}
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	if adapter == nil || adapter.NewAPIRead == nil || adapter.dialect != NewAPILegacy {
		return outcome, forkcheckin.ErrManualRequired
	}
	// Pin one client across preflight and POST. Snapshot invalidation must stop
	// this call rather than reacquiring a newer client's credentials mid-flight.
	read, err := adapter.prepare(snapshot)
	if err != nil {
		return outcome, err
	}
	var site struct {
		Enabled   *bool `json:"checkin_enabled"`
		Turnstile *bool `json:"turnstile_check"`
	}
	if err = read.get(ctx, "/api/status", &site); err != nil {
		return outcome, err
	}
	if site.Enabled == nil || site.Turnstile == nil {
		return outcome, forkcheckin.ErrUnsupported
	}
	if !*site.Enabled {
		return outcome, forkcheckin.ErrCheckInDisabled
	}
	if *site.Turnstile {
		return outcome, forkcheckin.ErrManualRequired
	}
	if _, err = read.identity(ctx); err != nil {
		return outcome, err
	}
	var dispatched atomic.Bool
	trace := &httptrace.ClientTrace{
		// Conservatively treat a partially written POST as dispatched too. A write
		// error is not evidence that the server did not receive the operation.
		WroteHeaders: func() { dispatched.Store(true) },
		WroteRequest: func(httptrace.WroteRequestInfo) { dispatched.Store(true) },
	}
	request, err := http.NewRequestWithContext(httptrace.WithClientTrace(ctx, trace), http.MethodPost, read.base+"/api/user/checkin", bytes.NewBufferString("{}"))
	if err != nil {
		return outcome, forkcheckin.ErrNetworkScope
	}
	request.Header = read.headers.Clone()
	request.Header.Set("Content-Type", "application/json")
	response, err := read.client.Do(request)
	outcome.Dispatched = dispatched.Load()
	if err != nil {
		if outcome.Dispatched {
			return outcome, errors.Join(forkcheckin.ErrUncertain, err)
		}
		return outcome, err
	}
	defer response.Body.Close()
	outcome.Dispatched = true // A server response establishes dispatch as well.
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReadBytes+1))
	defer clear(body)
	if err != nil || len(body) > maxReadBytes {
		return outcome, forkcheckin.ErrUncertain
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	trimmed := bytes.TrimSpace(body)
	if contentType == "text/html" || len(trimmed) > 0 && trimmed[0] == '<' {
		return outcome, errors.Join(forkcheckin.ErrUncertain, forkcheckin.ErrManualRequired)
	}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return outcome, forkcheckin.ErrAuthRequired
	case http.StatusForbidden:
		return outcome, forkcheckin.ErrPermissionDenied
	case http.StatusNotFound:
		return outcome, forkcheckin.ErrUnsupported
	case http.StatusTooManyRequests:
		return outcome, forkcheckin.ErrRateLimited
	case http.StatusOK:
	default:
		return outcome, forkcheckin.ErrUncertain
	}
	if contentType != "application/json" || !unambiguousJSON(body) {
		return outcome, forkcheckin.ErrUncertain
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Code    string          `json:"code"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Success == nil {
		return outcome, forkcheckin.ErrUncertain
	}
	defer clear(envelope.Data)
	outcome.ResponseRead = true
	if !*envelope.Success {
		// Exact standard upstream response, not substring matching site prose.
		if envelope.Code == "" && envelope.Message == "今日已签到" {
			outcome.AlreadyCheckedIn = true
			return outcome, nil
		}
		err := apiFailure(envelope.Code, envelope.Message)
		if errors.Is(err, forkcheckin.ErrCheckInDisabled) || errors.Is(err, forkcheckin.ErrAuthRequired) || errors.Is(err, forkcheckin.ErrPermissionDenied) {
			return outcome, err
		}
		return outcome, forkcheckin.ErrUncertain
	}
	var data struct {
		Quota *int64 `json:"quota_awarded"`
		Date  string `json:"checkin_date"`
	}
	if len(envelope.Data) == 0 || envelope.Data[0] != '{' || json.Unmarshal(envelope.Data, &data) != nil {
		return outcome, forkcheckin.ErrUncertain
	}
	date, err := time.Parse(time.DateOnly, data.Date)
	if err != nil || date.Year() < 1 || date.Format(time.DateOnly) != data.Date || data.Quota != nil && *data.Quota < 0 {
		return outcome, forkcheckin.ErrUncertain
	}
	outcome.Succeeded, outcome.SiteDate = true, data.Date
	if data.Quota != nil {
		outcome.Reward = forkcheckin.Reward{Known: true, Quota: *data.Quota, Unit: "quota"}
	}
	return outcome, nil
}
