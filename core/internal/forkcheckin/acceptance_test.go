package forkcheckin_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/accesstoken"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
)

const acceptanceChat = `{"model":"acceptance-model","messages":[{"role":"user","content":"synthetic acceptance message"}],"stream":false}`

type acceptanceSite struct {
	server     *httptest.Server
	requests   atomic.Int32
	posts      atomic.Int32
	inference  atomic.Int32
	active     atomic.Int32
	violations atomic.Int32
	release    chan struct{}
	once       sync.Once
}

func newAcceptanceSite(t *testing.T, mode string) *acceptanceSite {
	t.Helper()
	site := &acceptanceSite{release: make(chan struct{})}
	site.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
		for key, values := range r.Header {
			if strings.HasPrefix(strings.ToLower(key), "x-astrlink-") || strings.Contains(strings.ToLower(strings.Join(values, " ")), "astrlink") {
				site.violations.Add(1)
			}
		}
		for _, marker := range []string{acceptanceOperator, acceptanceObserver, acceptancePrivatePrompt, acceptanceSubscriptionAccess, acceptanceSubscriptionRefresh} {
			if bytes.Contains(body, []byte(marker)) || strings.Contains(fmt.Sprint(r.Header), marker) {
				site.violations.Add(1)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v1/chat/completions" {
			site.inference.Add(1)
			if r.Header.Get("Authorization") != "Bearer "+acceptanceProviderSecret || strings.Contains(fmt.Sprint(r.Header), acceptanceSessionSecret) {
				site.violations.Add(1)
			}
			_, _ = io.WriteString(w, `{"id":"completion_fixture","object":"chat.completion","model":"acceptance-model","choices":[{"index":0,"message":{"role":"assistant","content":"fixture response"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}}`)
			return
		}
		site.requests.Add(1)
		site.active.Add(1)
		defer site.active.Add(-1)
		if r.Header.Get("Authorization") != "Bearer "+acceptanceSessionSecret || strings.Contains(fmt.Sprint(r.Header), acceptanceProviderSecret) {
			site.violations.Add(1)
		}
		if mode == "on_429" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"success":false}`)
			return
		}
		if (mode == "on_timeout" || mode == "on_hung") && r.URL.Path == "/api/user/self" {
			select {
			case <-r.Context().Done():
			case <-site.release:
			}
			return
		}
		switch r.URL.Path {
		case "/api/status":
			_, _ = io.WriteString(w, `{"success":true,"data":{"checkin_enabled":true,"turnstile_check":false}}`)
		case "/api/user/self":
			fmt.Fprintf(w, `{"success":true,"data":{"id":%s}}`, r.Header.Get("New-Api-User"))
		case "/api/user/checkin":
			if r.Method == http.MethodPost {
				site.posts.Add(1)
				if string(body) != "{}" {
					site.violations.Add(1)
				}
				fmt.Fprintf(w, `{"success":true,"data":{"checkin_date":%q,"quota_awarded":1}}`, time.Now().UTC().Format(time.DateOnly))
			} else {
				_, _ = io.WriteString(w, `{"success":true,"data":{"enabled":true,"stats":{"checked_in_today":false,"records":[]}}}`)
			}
		default:
			site.violations.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(func() { site.once.Do(func() { close(site.release) }); site.server.Close() })
	return site
}

func prepareAcceptanceMain(t *testing.T, core *acceptanceCore, site *acceptanceSite) (string, string) {
	t.Helper()
	requireAcceptanceStatus(t, core.request(t, http.MethodGet, "/control/v1/services", "", "", ""), http.StatusUnauthorized)
	requireAcceptanceStatus(t, core.request(t, http.MethodPost, "/control/v1/services", `{}`, acceptanceObserver, ""), http.StatusForbidden)
	body := fmt.Sprintf(`{"name":"Acceptance provider","kind":"openai_compatible","models":["acceptance-model"],"http":{"base_url":%q,"auth":{"scheme":"bearer"},"credential":{"secret":%q}},"capabilities":[{"protocol":"openai.chat","mode":"native","streaming":true}]}`, site.server.URL+"/v1", acceptanceProviderSecret)
	created := core.request(t, http.MethodPost, "/control/v1/services", body, acceptanceOperator, "")
	requireAcceptanceStatus(t, created, http.StatusCreated)
	var service contract.Service
	if err := json.Unmarshal(created.body, &service); err != nil || service.ID == "" {
		t.Fatal("invalid service creation receipt")
	}
	if bytes.Contains(created.body, []byte(acceptanceProviderSecret)) {
		t.Fatal("service creation exposed its API key")
	}
	path := "/control/v1/services/" + string(service.ID)
	requireAcceptanceStatus(t, core.request(t, http.MethodPatch, path, `{"name":"Renamed"}`, acceptanceOperator, `"stale"`), http.StatusPreconditionFailed)
	updated := core.request(t, http.MethodPatch, path, `{"name":"Renamed"}`, acceptanceOperator, created.header.Get("ETag"))
	requireAcceptanceStatus(t, updated, http.StatusOK)
	if updated.header.Get("ETag") == "" || updated.header.Get("ETag") == created.header.Get("ETag") {
		t.Fatal("service CAS did not advance its ETag")
	}
	token := core.request(t, http.MethodPost, "/control/v1/access-tokens", `{"name":"Acceptance client"}`, acceptanceOperator, "")
	requireAcceptanceStatus(t, token, http.StatusCreated)
	var access accesstoken.CreatedToken
	if err := json.Unmarshal(token.body, &access); err != nil || access.Value == "" {
		t.Fatal("invalid access-token creation receipt")
	}
	core.accessToken = access.Value
	tokens := core.request(t, http.MethodGet, "/control/v1/access-tokens", "", acceptanceObserver, "")
	requireAcceptanceStatus(t, tokens, http.StatusOK)
	if bytes.Contains(tokens.body, []byte(access.Value)) {
		t.Fatal("token metadata exposed its secret")
	}
	requireAcceptanceStatus(t, core.request(t, http.MethodPost, "/v1/chat/completions", acceptanceChat, "", ""), http.StatusUnauthorized)
	requireAcceptanceStatus(t, core.request(t, http.MethodPost, "/v1/chat/completions", acceptanceChat, access.Value, ""), http.StatusOK)
	requireAcceptanceStatus(t, core.request(t, http.MethodGet, "/control/v1/routes", "", acceptanceObserver, ""), http.StatusGone)
	for _, read := range []string{"/control/v1/routing-settings", "/control/v1/policies", "/control/v1/requests?limit=5", "/control/v1/request-sessions?limit=5"} {
		requireAcceptanceStatus(t, core.request(t, http.MethodGet, read, "", acceptanceObserver, ""), http.StatusOK)
	}
	policy := "/control/v1/policies/" + string(contract.DefaultPrivacyPolicyID)
	before := core.request(t, http.MethodGet, policy, "", acceptanceOperator, "")
	requireAcceptanceStatus(t, before, http.StatusOK)
	patch := fmt.Sprintf(`{"enabled":true,"regex_source":"custom","custom_regex_rules":[{"kind":"common_secret","pattern":%q}],"request_action":"block"}`, acceptancePrivatePrompt)
	changed := core.request(t, http.MethodPatch, policy, patch, acceptanceOperator, before.header.Get("ETag"))
	requireAcceptanceStatus(t, changed, http.StatusOK)
	count := site.inference.Load()
	blocked := strings.Replace(acceptanceChat, "synthetic acceptance message", acceptancePrivatePrompt, 1)
	requireAcceptanceStatus(t, core.request(t, http.MethodPost, "/v1/chat/completions", blocked, access.Value, ""), http.StatusForbidden)
	if site.inference.Load() != count {
		t.Fatal("blocked private content reached the provider")
	}
	requireAcceptanceStatus(t, core.request(t, http.MethodPatch, policy, `{"enabled":false}`, acceptanceOperator, changed.header.Get("ETag")), http.StatusOK)
	return path, updated.header.Get("ETag")
}

func waitAcceptanceExtension(t *testing.T, core *acceptanceCore, mode string) {
	t.Helper()
	path := forkcheckin.APIPrefix + "/status"
	if strings.HasPrefix(mode, "baseline") {
		requireAcceptanceStatus(t, core.request(t, http.MethodGet, path, "", acceptanceOperator, ""), http.StatusNotFound)
		return
	}
	requireAcceptanceStatus(t, core.request(t, http.MethodGet, path, "", acceptanceObserver, ""), http.StatusForbidden)
	deadline := time.Now().Add(10 * time.Second)
	for {
		response := core.request(t, http.MethodGet, path, "", acceptanceOperator, "")
		requireAcceptanceStatus(t, response, http.StatusOK)
		var status struct {
			Enabled bool   `json:"enabled"`
			Ready   bool   `json:"storage_ready"`
			Error   string `json:"last_error_code"`
		}
		if err := json.Unmarshal(response.body, &status); err != nil {
			t.Fatal(err)
		}
		if mode == "off" && !status.Enabled && !status.Ready {
			return
		}
		if mode == "bad_ledger" && status.Enabled && !status.Ready && status.Error != "" {
			return
		}
		if strings.HasPrefix(mode, "on_") && status.Enabled && status.Ready {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("extension failed to reach its expected state")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func acceptanceJob(t *testing.T, core *acceptanceCore, round int, mode string) string {
	t.Helper()
	action := "status_refresh"
	if round == 0 && mode == "on_success" {
		action = "check_in"
	}
	body := fmt.Sprintf(`{"request_id":"request_acceptance_%d","accounts":["acct_accept_%c"],"action":%q,"expected_revision":1}`, round, 'a'+round, action)
	response := core.request(t, http.MethodPost, forkcheckin.APIPrefix+"/jobs", body, acceptanceOperator, "")
	requireAcceptanceStatus(t, response, http.StatusAccepted)
	var job struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(response.body, &job); err != nil || job.ID == "" {
		t.Fatal("invalid job receipt")
	}
	return job.ID
}

func waitAcceptanceJob(t *testing.T, core *acceptanceCore, id, mode string) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for {
		response := core.request(t, http.MethodGet, forkcheckin.APIPrefix+"/jobs/"+id, "", acceptanceOperator, "")
		requireAcceptanceStatus(t, response, http.StatusOK)
		if bytes.Contains(response.body, []byte(acceptanceSessionSecret)) {
			t.Fatal("job receipt leaked a session")
		}
		var job struct {
			Status     string `json:"status"`
			Action     string `json:"action"`
			Dispatched bool   `json:"dispatched"`
		}
		if err := json.Unmarshal(response.body, &job); err != nil {
			t.Fatal(err)
		}
		if job.Status != "queued" && job.Status != "running" {
			if mode == "on_success" || mode == "on_lock" {
				want := "not_checked"
				if job.Action == "check_in" {
					want = "success"
				}
				if job.Status != want {
					t.Fatalf("successful fixture action=%s status=%s want=%s", job.Action, job.Status, want)
				}
			}
			if mode == "on_429" && job.Status != "rate_limited" {
				t.Fatalf("429 status=%s", job.Status)
			}
			if mode == "on_timeout" && (job.Status != "retryable_failure" || job.Dispatched) {
				t.Fatalf("timeout status=%s dispatched=%t", job.Status, job.Dispatched)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("job did not settle in its bounded observation window")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func acceptanceLoad(t *testing.T, core *acceptanceCore) time.Duration {
	t.Helper()
	const samples = 40
	durations := make([]time.Duration, samples)
	for index := range durations {
		start := time.Now()
		response := core.request(t, http.MethodPost, "/v1/chat/completions", acceptanceChat, core.accessToken, "")
		durations[index] = time.Since(start)
		requireAcceptanceStatus(t, response, http.StatusOK)
	}
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	return durations[(samples*95+99)/100-1]
}

func countAcceptanceTables(t *testing.T, directory string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(directory, "astrlink.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name LIKE 'fork_checkin_%'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// Deliberately holds the shared SQLite writer, identically for baseline and
// feature. It models contention, not an assertion of process-level isolation.
func acceptanceContention(t *testing.T, directory string) func() {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(directory, "astrlink.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var acquired atomic.Int32
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if _, err := db.ExecContext(ctx, `BEGIN IMMEDIATE`); err == nil {
				acquired.Add(1)
				time.Sleep(5 * time.Millisecond)
				_, _ = db.Exec(`ROLLBACK`)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			<-done
			_ = db.Close()
			if acquired.Load() == 0 {
				t.Error("contention fixture never acquired the writer lock")
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func TestCheckinCoreAcceptance(t *testing.T) {
	binaries := buildAcceptanceExecutables(t, false)
	var denied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { denied.Add(1); w.WriteHeader(http.StatusForbidden) }))
	defer proxy.Close()
	measurements := make(map[string][]time.Duration)
	for _, mode := range []string{"baseline", "off", "on_success", "on_429", "on_timeout", "on_hung", "bad_ledger", "baseline_lock", "on_lock"} {
		t.Run(mode, func(t *testing.T) {
			site := newAcceptanceSite(t, mode)
			directory := seedAcceptanceDirectory(t, mode, site.server.URL)
			binary := binaries.current
			if strings.HasPrefix(mode, "baseline") {
				binary = binaries.baseline
			}
			core := startAcceptanceCore(t, binary, directory, proxy.URL)
			waitAcceptanceExtension(t, core, mode)
			path, etag := prepareAcceptanceMain(t, core, site)
			stopLock := func() {}
			if strings.HasSuffix(mode, "_lock") {
				stopLock = acceptanceContention(t, directory)
			}
			for round := 0; round < 5; round++ {
				job := ""
				if strings.HasPrefix(mode, "on_") {
					job = acceptanceJob(t, core, round, mode)
				}
				measurements[mode] = append(measurements[mode], acceptanceLoad(t, core))
				if job != "" && mode != "on_hung" {
					waitAcceptanceJob(t, core, job, mode)
				}
			}
			stopLock()
			final := core.request(t, http.MethodGet, path, "", acceptanceOperator, "")
			requireAcceptanceStatus(t, final, http.StatusOK)
			if final.header.Get("ETag") != etag || bytes.Contains(final.body, []byte(acceptanceProviderSecret)) {
				t.Fatal("extension work changed a service or exposed its key")
			}
			deleted := core.request(t, http.MethodDelete, path, "", acceptanceOperator, etag)
			if deleted.code != http.StatusNoContent {
				t.Errorf("service delete after load returned %d (body omitted); retain as a baseline/feature regression observation", deleted.code)
				if os.Getenv("ASTRLINK_CHECKIN_ACCEPTANCE_DIAGNOSTIC") == "1" {
					probe, err := sql.Open("sqlite", "file:"+filepath.Join(directory, "astrlink.db")+"?_pragma=busy_timeout(100)")
					if err != nil {
						t.Fatal(err)
					}
					_, err = probe.Exec(`BEGIN IMMEDIATE; ROLLBACK`)
					t.Logf("independent writer available after failed delete=%t", err == nil)
					_ = probe.Close()
				}
			} else {
				requireAcceptanceStatus(t, core.request(t, http.MethodGet, path, "", acceptanceOperator, ""), http.StatusNotFound)
			}
			if mode == "on_hung" && site.active.Load() == 0 {
				t.Error("hung request was no longer active at shutdown; this sample cannot prove hung-I/O cancellation")
			}
			stop := core.stop(t)
			if deleted.code != http.StatusNoContent {
				diagnoseAcceptanceDelete(t, directory, path, etag)
			}
			if mode == "off" || strings.HasPrefix(mode, "baseline") {
				if countAcceptanceTables(t, directory) != 0 || site.requests.Load() != 0 {
					t.Error("disabled or absent extension created schema or site traffic")
				}
			}
			if strings.HasPrefix(mode, "on_") && site.requests.Load() == 0 {
				t.Error("fault/active workload never reached the fake site")
			}
			wantPosts := int32(0)
			if mode == "on_success" {
				wantPosts = 1
			}
			if site.posts.Load() != wantPosts {
				t.Errorf("submitted check-ins=%d want=%d", site.posts.Load(), wantPosts)
			}
			if site.violations.Load() != 0 {
				t.Errorf("final outgoing request identity/secret violations: %d", site.violations.Load())
			}
			t.Logf("mode=%s samples=5x40 p95=%v stop=%v inference=%d site_reads_and_posts=%d posts=%d", mode, measurements[mode], stop, site.inference.Load(), site.requests.Load(), site.posts.Load())
		})
	}
	if denied.Load() != 0 {
		t.Errorf("unexpected external HTTP(S) attempts rejected by fixture proxy: %d", denied.Load())
	}
	if os.Getenv("ASTRLINK_CHECKIN_ACCEPTANCE_DIAGNOSTIC") == "1" || os.Getenv("ASTRLINK_CHECKIN_ACCEPTANCE_RACE") == "1" {
		t.Log("instrumented timings are not used for the performance gate")
		return
	}
	// Five rounds reduce one-off scheduling noise; retain every observation in
	// the log and compare the median p95. Never relax the published threshold.
	median := func(values []time.Duration) time.Duration {
		copy := append([]time.Duration(nil), values...)
		sort.Slice(copy, func(i, j int) bool { return copy[i] < copy[j] })
		return copy[len(copy)/2]
	}
	for mode, values := range measurements {
		if strings.HasPrefix(mode, "baseline") || len(values) != 5 {
			continue
		}
		reference := "baseline"
		if mode == "on_lock" {
			reference = "baseline_lock"
		}
		if len(measurements[reference]) != 5 {
			t.Errorf("missing complete baseline for %s", mode)
			continue
		}
		base, next := median(measurements[reference]), median(values)
		delta := next - base
		t.Logf("mode=%s median_p95=%v baseline=%v delta=%v", mode, next, base, delta)
		if delta > time.Millisecond && float64(next) > float64(base)*1.05 {
			t.Errorf("%s exceeded the unchanged p95 gate (5%% or 1ms); investigate rather than adjust the threshold", mode)
		}
	}
}
