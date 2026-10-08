package adapters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

func submitPreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	switch {
	case strings.HasSuffix(r.URL.Path, "/status"):
		reply(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":false}}`)
	case strings.HasSuffix(r.URL.Path, "/self"):
		reply(w, `{"success":true,"data":{"id":7}}`)
	default:
		return false
	}
	return true
}

func TestSubmitUncertainResponseBoundaries(t *testing.T) {
	cases := []struct {
		name    string
		respond http.HandlerFunc
	}{
		{"oversize", func(w http.ResponseWriter, _ *http.Request) { reply(w, strings.Repeat("x", maxReadBytes+1)) }},
		{"truncated", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Length", "1000")
			reply(w, `{"success":true`)
		}},
		{"missing success", func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"data":{}}`) }},
		{"missing data", func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"success":true}`) }},
		{"null data", func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"success":true,"data":null}`) }},
		{"false success text", func(w http.ResponseWriter, _ *http.Request) { reply(w, `{"success":"true","data":{}}`) }},
		{"non JSON", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, `{"success":true}`)
		}},
		{"server failure", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) }},
	}
	for _, code := range []int{301, 302, 303, 307, 308} {
		cases = append(cases, struct {
			name    string
			respond http.HandlerFunc
		}{fmt.Sprint(code), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "/mounted%20app/api/user/logout")
			w.WriteHeader(code)
		}})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			read, snapshot := readFixture(t, NewAPILegacy, func(w http.ResponseWriter, r *http.Request) {
				if submitPreflight(w, r) {
					return
				}
				if r.Method != http.MethodPost || r.URL.Path != "/mounted app/api/user/checkin" {
					t.Error("redirect or unexpected write")
				}
				posts.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				tc.respond(w, r)
			})
			adapter, _ := NewNewAPISubmit(read)
			out, err := adapter.Submit(context.Background(), snapshot)
			if !errors.Is(err, forkcheckin.ErrUncertain) || !out.Dispatched || out.Succeeded || out.AlreadyCheckedIn || out.Reward.Known || posts.Load() != 1 {
				t.Fatalf("out=%+v err=%v posts=%d", out, err, posts.Load())
			}
			if err := out.Validate(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSubmitCancellationAndInvalidationAfterSelf(t *testing.T) {
	for _, invalidate := range []bool{false, true} {
		t.Run(fmt.Sprint(invalidate), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var posts atomic.Int32
			read, snapshot := readFixture(t, NewAPILegacy, func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/self") {
					close(entered)
					<-release
					reply(w, `{"success":true,"data":{"id":7}}`)
					return
				}
				if submitPreflight(w, r) {
					return
				}
				posts.Add(1)
				w.WriteHeader(500)
			})
			adapter, _ := NewNewAPISubmit(read)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type result struct {
				out forkcheckin.SubmitOutcome
				err error
			}
			done := make(chan result, 1)
			go func() { out, err := adapter.Submit(ctx, snapshot); done <- result{out, err} }()
			<-entered
			want := error(context.Canceled)
			if invalidate {
				read.factory.Invalidate(snapshot.Account.ID)
				want = forkcheckin.ErrClientInvalidated
			} else {
				cancel()
			}
			close(release)
			got := <-done
			if !errors.Is(got.err, want) || got.out.Dispatched || posts.Load() != 0 {
				t.Fatalf("%+v %v posts=%d", got.out, got.err, posts.Load())
			}
		})
	}
}

func TestSubmitIdentityMismatchNeverWrites(t *testing.T) {
	var posts atomic.Int32
	read, snapshot := readFixture(t, NewAPILegacy, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/self") {
			reply(w, `{"success":true,"data":{"id":8}}`)
			return
		}
		if submitPreflight(w, r) {
			return
		}
		posts.Add(1)
	})
	adapter, _ := NewNewAPISubmit(read)
	out, err := adapter.Submit(context.Background(), snapshot)
	if !errors.Is(err, forkcheckin.ErrIdentityMismatch) || out.Dispatched || posts.Load() != 0 {
		t.Fatalf("%+v %v", out, err)
	}
}
