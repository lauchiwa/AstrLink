package forkcheckin_test

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// This is a separate causal comparison, not a replacement for the unchanged
// historical baseline in TestCheckinCoreAcceptance. Each round rotates scenario
// order and creates a fresh Core/database; compilation and setup are untimed.
// The repeated identical control checks the measurement environment itself.
// Its failure cannot be waived by a passing feature comparison.
func TestCheckinCoreIsolationAcceptance(t *testing.T) {
	binaries := buildAcceptanceExecutables(t, true)
	var denied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		denied.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer proxy.Close()
	modes := []string{"baseline_fixed", "baseline_fixed_repeat", "off", "on_success", "on_429", "on_timeout", "on_hung", "bad_ledger", "baseline_fixed_lock", "on_lock"}
	measurements := make(map[string][]time.Duration)
	started := time.Now()
	for round := 0; round < 5; round++ {
		t.Run(fmt.Sprintf("round_%d", round+1), func(t *testing.T) {
			for position := range modes {
				mode := modes[(position+round*2)%len(modes)]
				t.Run(mode, func(t *testing.T) {
					binary := binaries.current
					if strings.HasPrefix(mode, "baseline") {
						binary = binaries.control
					}
					t.Logf("sample_start_offset_ms=%d", time.Since(started).Milliseconds())
					p95 := acceptanceIsolationSample(t, binary, mode, proxy.URL)
					measurements[mode] = append(measurements[mode], p95)
				})
			}
		})
	}
	if denied.Load() != 0 {
		t.Errorf("unexpected HTTP(S) egress rejected by fixture proxy=%d", denied.Load())
	}
	if os.Getenv("ASTRLINK_CHECKIN_ACCEPTANCE_DIAGNOSTIC") == "1" || os.Getenv("ASTRLINK_CHECKIN_ACCEPTANCE_RACE") == "1" {
		t.Log("instrumented functional comparison only; timings are not performance-gate evidence")
		return
	}
	control, repeat := measurements["baseline_fixed"], measurements["baseline_fixed_repeat"]
	if len(control) != 5 || len(repeat) != 5 {
		t.Error("incomplete identical-control comparison; performance eligibility is unverified")
	} else {
		first, second := acceptanceMedian(control), acceptanceMedian(repeat)
		t.Logf("identical_control_p95=%v repeat_p95=%v median_delta_us=%d", control, repeat, (second - first).Microseconds())
		if acceptanceP95Exceeded(first, second) || acceptanceP95Exceeded(second, first) {
			t.Error("identical controls exceeded the unchanged 5% or 1ms gate; measurement environment is not sufficient for attribution")
		}
	}
	for _, mode := range modes {
		if strings.HasPrefix(mode, "baseline") {
			continue
		}
		reference := "baseline_fixed"
		if mode == "on_lock" {
			reference = "baseline_fixed_lock"
		}
		values, baseline := measurements[mode], measurements[reference]
		if len(values) != 5 || len(baseline) != 5 {
			t.Errorf("incomplete five-round comparison %s vs %s", mode, reference)
			continue
		}
		base, next := acceptanceMedian(baseline), acceptanceMedian(values)
		delta := next - base
		t.Logf("mode=%s p95=%v median=%v reference=%s reference_p95=%v median=%v delta=%v", mode, values, next, reference, baseline, base, delta)
		if acceptanceP95Exceeded(base, next) {
			t.Errorf("%s exceeded unchanged p95 gate (5%% or 1ms); retain and investigate", mode)
		}
	}
}

// Both limits must be exceeded: passing either the relative or absolute
// budget is permitted. Controls use the same rule in both directions.
func acceptanceP95Exceeded(base, next time.Duration) bool {
	return next-base > time.Millisecond && float64(next) > float64(base)*1.05
}

func acceptanceMedian(values []time.Duration) time.Duration {
	copy := append([]time.Duration(nil), values...)
	sort.Slice(copy, func(i, j int) bool { return copy[i] < copy[j] })
	return copy[len(copy)/2]
}

func acceptanceIsolationSample(t *testing.T, binary, mode, proxy string) time.Duration {
	t.Helper()
	site := newAcceptanceSite(t, mode)
	quota := newAcceptanceQuotaSite(t)
	directory := seedAcceptanceDirectory(t, mode, site.server.URL)
	seedAcceptanceSubscriptions(t, directory)
	core := startAcceptanceCoreWithCodex(t, binary, directory, proxy, quota.server.URL+"/backend-api/codex")
	waitAcceptanceExtension(t, core, mode)
	path, etag := prepareAcceptanceMain(t, core, site)
	// Equal untimed warm-up in every fresh process, including the control.
	for warm := 0; warm < 10; warm++ {
		requireAcceptanceStatus(t, core.request(t, http.MethodPost, "/v1/chat/completions", acceptanceChat, core.accessToken, ""), http.StatusOK)
	}
	snapshots := primeAcceptanceQuota(t, core, quota)
	stopLock := func() {}
	if strings.HasSuffix(mode, "_lock") {
		stopLock = acceptanceContention(t, directory)
	}
	job := ""
	if strings.HasPrefix(mode, "on_") {
		job = acceptanceJob(t, core, 0, mode)
	}
	loadStart := time.Now()
	p95 := acceptanceLoad(t, core)
	loadElapsed := time.Since(loadStart)
	stopLock()
	verifyAcceptanceQuota(t, core, quota, snapshots)
	if job != "" && mode != "on_hung" {
		waitAcceptanceJob(t, core, job, mode)
	}
	final := core.request(t, http.MethodGet, path, "", acceptanceOperator, "")
	requireAcceptanceStatus(t, final, http.StatusOK)
	if final.header.Get("ETag") != etag || bytes.Contains(final.body, []byte(acceptanceProviderSecret)) {
		t.Fatal("extension work changed a service or exposed its key")
	}
	requireAcceptanceStatus(t, core.request(t, http.MethodDelete, path, "", acceptanceOperator, etag), http.StatusNoContent)
	requireAcceptanceStatus(t, core.request(t, http.MethodGet, path, "", acceptanceOperator, ""), http.StatusNotFound)
	if mode == "on_hung" && site.active.Load() == 0 {
		t.Error("hung I/O was not active at shutdown")
	}
	stop := core.stop(t)
	core.logs.safe(t, acceptanceSubscriptionAccess, acceptanceSubscriptionRefresh)
	if mode == "off" || strings.HasPrefix(mode, "baseline") {
		if countAcceptanceTables(t, directory) != 0 || site.requests.Load() != 0 {
			t.Error("absent/disabled extension created schema or traffic")
		}
	}
	if strings.HasPrefix(mode, "on_") && site.requests.Load() == 0 {
		t.Error("extension fault/active workload never reached its fake site")
	}
	wantPosts := int32(0)
	if mode == "on_success" {
		wantPosts = 1
	}
	if site.posts.Load() != wantPosts || site.violations.Load() != 0 {
		t.Errorf("posts=%d want=%d; outgoing identity/secret violations=%d", site.posts.Load(), wantPosts, site.violations.Load())
	}
	if mode == "on_429" && site.requests.Load() != 1 {
		t.Errorf("rate-limited job issued %d site requests, want exactly one", site.requests.Load())
	}
	cpu := core.command.ProcessState.UserTime() + core.command.ProcessState.SystemTime()
	t.Logf("mode=%s p95=%v stop=%v inference=%d site_requests=%d posts=%d load_wall_ms=%d process_cpu_ms=%d", mode, p95, stop, site.inference.Load(), site.requests.Load(), site.posts.Load(), loadElapsed.Milliseconds(), cpu.Milliseconds())
	return p95
}
