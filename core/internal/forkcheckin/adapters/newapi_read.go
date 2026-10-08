// Package adapters contains extension-only site protocols. No provider quota
// cache writer, Service or database handle is supplied to these adapters.
package adapters

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

// Wire references: QuantumNous/new-api v0.12.0 (legacy middleware/auth.go),
// and commit 973cf8ef4600947a4270e95ada7916740fa8264c (modern dashboard auth).
// Both use controller/{misc,user,checkin}.go and model/checkin.go. In particular
// GetUserCheckinStats does NOT report a current site date or a next opening time.
const (
	NewAPILegacy = "newapi_legacy"
	NewAPIModern = "newapi_dashboard"
	maxReadBytes = 1 << 20
	readTimeout  = 15 * time.Second
)

// NewAPIRead deliberately implements only ReadOnlySiteAdapter, not Submit.
// The owner controls the factory lifecycle and serializes snapshot changes.
// Auth dialect selection is explicit; there is no fallback or guessed user ID.
type NewAPIRead struct {
	factory *forkcheckin.TransportFactory
	dialect string
}

var _ forkcheckin.ReadOnlySiteAdapter = (*NewAPIRead)(nil)

func NewNewAPIRead(factory *forkcheckin.TransportFactory, dialect string) (*NewAPIRead, error) {
	if factory == nil || dialect != NewAPILegacy && dialect != NewAPIModern {
		return nil, forkcheckin.ErrUnsupported
	}
	return &NewAPIRead{factory: factory, dialect: dialect}, nil
}

func (adapter *NewAPIRead) Dialect() string { return adapter.dialect }

func (adapter *NewAPIRead) Inspect(ctx context.Context, snapshot forkcheckin.AccountSnapshot) (forkcheckin.SiteCapability, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	capability := forkcheckin.SiteCapability{Dialect: adapter.dialect}
	read, err := adapter.prepare(snapshot)
	if err != nil {
		return capability, err
	}
	var status struct {
		Enabled   *bool `json:"checkin_enabled"`
		Turnstile *bool `json:"turnstile_check"`
	}
	if err = read.get(ctx, "/api/status", &status); err != nil {
		capability.RequiresManual = errors.Is(err, forkcheckin.ErrManualRequired)
		return capability, err
	}
	if status.Enabled == nil {
		return capability, forkcheckin.ErrUnsupported
	}
	if !*status.Enabled {
		return capability, forkcheckin.ErrCheckInDisabled
	}
	capability.Supported = true
	capability.RequiresManual = status.Turnstile != nil && *status.Turnstile
	return capability, nil
}

func (adapter *NewAPIRead) ValidateIdentity(ctx context.Context, snapshot forkcheckin.AccountSnapshot) (forkcheckin.SiteIdentity, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	read, err := adapter.prepare(snapshot)
	if err != nil {
		return forkcheckin.SiteIdentity{}, err
	}
	return read.identity(ctx)
}

func (adapter *NewAPIRead) ReadStatus(ctx context.Context, snapshot forkcheckin.AccountSnapshot) (forkcheckin.CheckInStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	read, err := adapter.prepare(snapshot)
	if err != nil {
		return forkcheckin.CheckInStatus{}, err
	}
	// A draft cannot consume account status. Authorization first reads self,
	// then the owner persists the verified UID. Never mutate the snapshot here.
	if read.expectedID == "" {
		return forkcheckin.CheckInStatus{}, forkcheckin.ErrIdentityMismatch
	}
	if _, err = read.identity(ctx); err != nil {
		return forkcheckin.CheckInStatus{}, err
	}
	var data struct {
		Enabled *bool `json:"enabled"`
		Stats   *struct {
			Checked *bool `json:"checked_in_today"`
			Records []struct {
				Date  string `json:"checkin_date"`
				Quota *int64 `json:"quota_awarded"`
			} `json:"records"`
		} `json:"stats"`
	}
	// Omit month: the server chooses its own month, not this machine's month.
	if err = read.get(ctx, "/api/user/checkin", &data); err != nil {
		return forkcheckin.CheckInStatus{}, err
	}
	if data.Enabled == nil {
		return forkcheckin.CheckInStatus{}, forkcheckin.ErrUnsupported
	}
	if !*data.Enabled {
		return forkcheckin.CheckInStatus{}, forkcheckin.ErrCheckInDisabled
	}
	if data.Stats == nil || data.Stats.Checked == nil || len(data.Stats.Records) > 31 {
		return forkcheckin.CheckInStatus{}, forkcheckin.ErrUnsupported
	}
	result := forkcheckin.CheckInStatus{CheckedInToday: *data.Stats.Checked, Records: make([]forkcheckin.CheckInRecord, 0, len(data.Stats.Records))}
	seen := make(map[string]bool)
	for _, record := range data.Stats.Records {
		date, err := time.Parse(time.DateOnly, record.Date)
		if err != nil || date.Format(time.DateOnly) != record.Date || date.Year() < 1 || seen[record.Date] || record.Quota == nil || *record.Quota < 0 {
			return forkcheckin.CheckInStatus{}, forkcheckin.ErrUnsupported
		}
		seen[record.Date] = true
		result.Records = append(result.Records, forkcheckin.CheckInRecord{
			SiteDate: record.Date, Reward: forkcheckin.Reward{Known: true, Quota: *record.Quota, Unit: "quota"},
		})
	}
	// A historical record or HTTP Date cannot establish today's site-local
	// date. Even checked=true leaves today's date/reward unknown on this wire.
	return result, nil
}

type newAPIReadRequest struct {
	client     *forkcheckin.AccountClient
	base       string
	headers    http.Header
	expectedID string
}

func (adapter *NewAPIRead) prepare(snapshot forkcheckin.AccountSnapshot) (*newAPIReadRequest, error) {
	id := snapshot.Account.RemoteUserID
	if id != "" && !validUserID(id) {
		return nil, forkcheckin.ErrIdentityMismatch
	}
	if adapter.dialect == NewAPILegacy && id == "" {
		// Legacy middleware rejects /self without New-Api-User. No enumerating
		// IDs or trusting a page-supplied ID as an authenticated identity.
		return nil, forkcheckin.ErrManualRequired
	}
	client, err := adapter.factory.Client(snapshot)
	if err != nil {
		return nil, err
	}
	base, err := forkcheckin.NormalizeDashboardURL(snapshot.Account.DashboardBaseURL)
	if err != nil {
		return nil, forkcheckin.ErrNetworkScope
	}
	headers := http.Header{"Accept": {"application/json"}, "Cache-Control": {"no-cache"}}
	if adapter.dialect == NewAPILegacy {
		headers.Set("New-Api-User", id)
	}
	return &newAPIReadRequest{client: client, base: base, headers: headers, expectedID: id}, nil
}

func validUserID(id string) bool {
	value, err := strconv.ParseInt(id, 10, 64)
	return err == nil && value > 0 && strconv.FormatInt(value, 10) == id
}

func (read *newAPIReadRequest) identity(ctx context.Context) (forkcheckin.SiteIdentity, error) {
	var data struct {
		ID *int64 `json:"id"`
	}
	if err := read.get(ctx, "/api/user/self", &data); err != nil {
		return forkcheckin.SiteIdentity{}, err
	}
	if data.ID == nil || *data.ID <= 0 {
		return forkcheckin.SiteIdentity{}, forkcheckin.ErrUnsupported
	}
	id := strconv.FormatInt(*data.ID, 10)
	if read.expectedID != "" && id != read.expectedID {
		return forkcheckin.SiteIdentity{}, forkcheckin.ErrIdentityMismatch
	}
	// Do not publish username, email, balance, or arbitrary site text as a hint.
	return forkcheckin.SiteIdentity{RemoteUserID: id}, nil
}

func (read *newAPIReadRequest) get(ctx context.Context, endpoint string, target any) error {
	response, err := read.client.ReadOnlyGET(ctx, read.base+endpoint, read.headers)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReadBytes+1))
	defer clear(body)
	if err != nil {
		return forkcheckin.ErrNetwork
	}
	if len(body) > maxReadBytes {
		return forkcheckin.ErrResponseTooLarge
	}
	if response.StatusCode == http.StatusNotFound {
		return forkcheckin.ErrUnsupported
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return forkcheckin.RateLimited(response.Header.Get("Retry-After"), time.Now())
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	trimmed := bytes.TrimSpace(body)
	if contentType == "text/html" || len(trimmed) > 0 && trimmed[0] == '<' {
		return forkcheckin.ErrManualRequired
	}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return forkcheckin.ErrAuthRequired
	case http.StatusForbidden:
		return forkcheckin.ErrPermissionDenied
	case http.StatusOK:
	default:
		if response.StatusCode >= 300 && response.StatusCode < 400 {
			return forkcheckin.ErrNetworkRedirect
		}
		return forkcheckin.ErrNetwork
	}
	if contentType != "application/json" {
		return forkcheckin.ErrUnsupported
	}
	if !unambiguousJSON(body) {
		return forkcheckin.ErrUnsupported
	}
	var envelope struct {
		Success *bool           `json:"success"`
		Message string          `json:"message"`
		Code    string          `json:"code"`
		Data    json.RawMessage `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Success == nil {
		return forkcheckin.ErrUnsupported
	}
	defer clear(envelope.Data)
	if !*envelope.Success {
		return apiFailure(envelope.Code, envelope.Message)
	}
	if len(envelope.Data) == 0 || envelope.Data[0] != '{' || json.Unmarshal(envelope.Data, target) != nil {
		return forkcheckin.ErrUnsupported
	}
	return nil
}

// Only exact, upstream-owned values classify application errors. Site wording
// is never returned/logged or interpreted as successful check-in proof.
func apiFailure(code, message string) error {
	switch code {
	case "AUTH_TOKEN_EXPIRED", "AUTH_SESSION_REVOKED", "AUTH_UNAUTHORIZED", "AUTH_USER_DISABLED", "AUTH_USER_INVALID", "AUTH_SESSION_REQUIRED":
		return forkcheckin.ErrAuthRequired
	case "AUTH_INSUFFICIENT_PRIVILEGE":
		return forkcheckin.ErrPermissionDenied
	}
	switch message {
	case "签到功能未启用":
		return forkcheckin.ErrCheckInDisabled
	case "无权进行此操作，未登录且未提供 access token", "无权进行此操作，access token 无效", "无权进行此操作，用户信息无效", "用户已被封禁":
		return forkcheckin.ErrAuthRequired
	case "无权进行此操作，权限不足":
		return forkcheckin.ErrPermissionDenied
	default:
		return forkcheckin.ErrUnsupported
	}
}

// Unknown fields are tolerated for upstream evolution, but duplicate (including
// case-variant) keys, trailing documents and excessive nesting are ambiguous.
func unambiguousJSON(body []byte) bool {
	if !utf8.Valid(body) {
		return false
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value func(int) bool
	value = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := decoder.Token()
		if err != nil {
			return false
		}
		delimiter, ok := token.(json.Delim)
		if !ok {
			return true
		}
		switch delimiter {
		case '{':
			seen := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				name, ok := key.(string)
				if err != nil || !ok {
					return false
				}
				name = strings.ToLower(name)
				if seen[name] {
					return false
				}
				seen[name] = true
				if !value(depth + 1) {
					return false
				}
			}
		case '[':
			for decoder.More() {
				if !value(depth + 1) {
					return false
				}
			}
		default:
			return false
		}
		end, err := decoder.Token()
		return err == nil && (delimiter == '{' && end == json.Delim('}') || delimiter == '[' && end == json.Delim(']'))
	}
	if !value(0) {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}
