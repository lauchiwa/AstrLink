package adapters

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

func TestSubmitStandardOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, body              string
		code                    int
		success, already, known bool
		want                    error
	}{
		{"award", `{"success":true,"data":{"checkin_date":"2024-02-29","quota_awarded":17}}`, 200, true, false, true, nil},
		{"unknown amount", `{"success":true,"data":{"checkin_date":"2024-02-29","amount":17}}`, 200, true, false, false, nil},
		{"already", `{"success":false,"message":"今日已签到"}`, 200, false, true, false, nil},
		{"business failure", `{"success":false,"message":"private-canary"}`, 200, false, false, false, forkcheckin.ErrUncertain},
		{"disabled", `{"success":false,"message":"签到功能未启用"}`, 200, false, false, false, forkcheckin.ErrCheckInDisabled},
		{"invalid date", `{"success":true,"data":{"checkin_date":"2023-02-29","quota_awarded":17}}`, 200, false, false, false, forkcheckin.ErrUncertain},
		{"negative", `{"success":true,"data":{"checkin_date":"2024-02-29","quota_awarded":-1}}`, 200, false, false, false, forkcheckin.ErrUncertain},
		{"duplicate", `{"success":false,"success":true}`, 200, false, false, false, forkcheckin.ErrUncertain},
		{"auth", `{}`, 401, false, false, false, forkcheckin.ErrAuthRequired},
		{"permission", `{}`, 403, false, false, false, forkcheckin.ErrPermissionDenied},
		{"not found", `{}`, 404, false, false, false, forkcheckin.ErrUnsupported},
		{"rate limit", `{}`, 429, false, false, false, forkcheckin.ErrRateLimited},
		{"html", `<html>private-canary</html>`, 200, false, false, false, forkcheckin.ErrManualRequired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			read, snapshot := readFixture(t, NewAPILegacy, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/status"):
					reply(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":false}}`)
				case strings.HasSuffix(r.URL.Path, "/self"):
					reply(w, `{"success":true,"data":{"id":7}}`)
				default:
					posts.Add(1)
					body, _ := io.ReadAll(r.Body)
					if r.Method != "POST" || r.URL.Path != "/mounted app/api/user/checkin" || string(body) != "{}" {
						t.Error("unexpected write")
					}
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.code)
					_, _ = io.WriteString(w, tc.body)
				}
			})
			adapter, err := NewNewAPISubmit(read)
			if err != nil {
				t.Fatal(err)
			}
			out, err := adapter.Submit(context.Background(), snapshot)
			if !errors.Is(err, tc.want) || out.Succeeded != tc.success || out.AlreadyCheckedIn != tc.already || out.Reward.Known != tc.known || !out.Dispatched || posts.Load() != 1 {
				t.Fatalf("out=%+v err=%v posts=%d", out, err, posts.Load())
			}
			if err != nil && strings.Contains(err.Error(), "canary") {
				t.Error("site prose leaked")
			}
			if err := out.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSubmitLostResponseOnlyReadBack(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "timeout"}[timeout], func(t *testing.T) {
			var posts atomic.Int32
			read, snapshot := readFixture(t, NewAPILegacy, func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/status"):
					reply(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":false}}`)
				case strings.HasSuffix(r.URL.Path, "/self"):
					reply(w, `{"success":true,"data":{"id":7}}`)
				case r.Method == http.MethodPost:
					posts.Add(1)
					_, _ = io.Copy(io.Discard, r.Body)
					if timeout {
						<-r.Context().Done()
						return
					}
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					conn.Close()
				default:
					reply(w, `{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":true,"records":[]}}}`)
				}
			})
			adapter, _ := NewNewAPISubmit(read)
			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			out, err := adapter.Submit(ctx, snapshot)
			if !errors.Is(err, forkcheckin.ErrUncertain) || !out.Dispatched || out.ResponseRead || out.Succeeded {
				t.Fatalf("%+v %v", out, err)
			}
			status, err := adapter.ReadStatus(context.Background(), snapshot)
			if err != nil || !status.CheckedInToday || posts.Load() != 1 {
				t.Fatalf("readback=%+v err=%v posts=%d", status, err, posts.Load())
			}
		})
	}
}

func TestSubmitPreflightAndModernGate(t *testing.T) {
	for _, body := range []string{`{"checkin_enabled":false,"turnstile_check":false}`, `{"checkin_enabled":true,"turnstile_check":true}`, `{"checkin_enabled":true}`} {
		read, snapshot := readFixture(t, NewAPILegacy, func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet || !strings.HasSuffix(r.URL.Path, "/status") {
				t.Error("blocked preflight performed write")
			}
			reply(w, `{"success":true,"data":`+body+`}`)
		})
		adapter, _ := NewNewAPISubmit(read)
		out, err := adapter.Submit(context.Background(), snapshot)
		if err == nil || out.Dispatched {
			t.Fatalf("%+v %v", out, err)
		}
	}
	read, snapshot := readFixture(t, NewAPILegacy, func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
	adapter, _ := NewNewAPISubmit(read)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	out, err := adapter.Submit(ctx, snapshot)
	if !errors.Is(err, context.DeadlineExceeded) || out.Dispatched {
		t.Fatalf("preflight timeout: %+v %v", out, err)
	}
	modern, _ := NewNewAPIRead(read.factory, NewAPIModern)
	if _, err := NewNewAPISubmit(modern); !errors.Is(err, forkcheckin.ErrManualRequired) {
		t.Fatal("modern capture gate bypassed")
	}
}
