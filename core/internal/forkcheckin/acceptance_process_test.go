package forkcheckin_test

// This harness builds the real executable, including the production main and
// shutdown ordering. Only fixtures may seed the temporary database directly;
// behavior under test is exercised through the public HTTP surfaces.

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/astrlink/core/contract"
	"github.com/QuantumNous/astrlink/core/internal/forkcheckin"
	"github.com/QuantumNous/astrlink/core/internal/pricing"
	"github.com/QuantumNous/astrlink/core/internal/storage/sqlite"
	sqlitedriver "modernc.org/sqlite"
)

const acceptanceBaseline = "e7ebad25b30e6fc78bfb0145cddfaac5fad662a2"
const acceptanceOperator = "acceptance_operator_01234567890123456789"
const acceptanceObserver = "acceptance_observer_01234567890123456789"
const acceptanceProviderSecret = "CHECKIN-ACCEPTANCE-PROVIDER-CANARY"
const acceptanceSessionSecret = "CHECKIN-ACCEPTANCE-SESSION-CANARY"
const acceptancePrivatePrompt = "CHECKIN-ACCEPTANCE-PRIVATE-PROMPT"

func acceptanceKey() []byte { return bytes.Repeat([]byte{0x63}, 32) }

type acceptanceLogs struct {
	mu       sync.Mutex
	body     bytes.Buffer
	overflow bool
}

func (log *acceptanceLogs) Write(value []byte) (int, error) {
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.body.Len()+len(value) <= 1<<20 {
		_, _ = log.body.Write(value)
	} else {
		log.overflow = true
	}
	return len(value), nil
}

func (log *acceptanceLogs) safe(t *testing.T, extra ...string) {
	t.Helper()
	log.mu.Lock()
	defer log.mu.Unlock()
	if log.overflow {
		t.Error("process log capture exceeded its bound")
	}
	for _, match := range regexp.MustCompile(`acceptance-sqlite-code=([0-9]+)`).FindAllStringSubmatch(log.body.String(), -1) {
		t.Logf("diagnostic SQLite code=%s", match[1])
	}
	for _, match := range regexp.MustCompile(`acceptance-store-stage=([a-z_]+)`).FindAllStringSubmatch(log.body.String(), -1) {
		t.Logf("diagnostic storage stage=%s", match[1])
	}
	for _, marker := range append([]string{acceptanceOperator, acceptanceObserver, acceptanceProviderSecret, acceptanceSessionSecret, acceptancePrivatePrompt, acceptanceSubscriptionAccess, acceptanceSubscriptionRefresh, hex.EncodeToString(acceptanceKey())}, extra...) {
		if marker != "" && bytes.Contains(log.body.Bytes(), []byte(marker)) {
			t.Error("a seeded secret escaped into the process log")
		}
	}
}

type acceptanceExecutables struct {
	baseline string
	current  string
	control  string // Historical baseline plus only the reviewed configuration-write fix.
}

func buildAcceptanceExecutables(t *testing.T, withControl bool) acceptanceExecutables {
	t.Helper()
	if testing.Short() {
		t.Skip("real Core baseline acceptance requires builds; run without -short")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	run := func(dir, name string, args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GOWORK=off")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("acceptance build %s failed: %v\n%s", name, err, out)
		}
		return out
	}
	root := strings.TrimSpace(string(run("", "git", "rev-parse", "--show-toplevel")))
	paths := run(root, "git", "ls-tree", "-r", "--name-only", acceptanceBaseline, "core")
	if bytes.Contains(paths, []byte("fork_checkin")) || bytes.Contains(paths, []byte("/forkcheckin/")) {
		t.Fatal("baseline already contains the extension")
	}
	dir := t.TempDir()
	tar := filepath.Join(dir, "baseline.tar")
	run(root, "git", "archive", "--format=tar", "--output="+tar, acceptanceBaseline, "core", "convo")
	run(dir, "tar", "-xf", tar)
	baseline, current := filepath.Join(dir, "baseline-core"), filepath.Join(dir, "current-core")
	if runtime.GOOS == "windows" {
		baseline += ".exe"
		current += ".exe"
	}
	build := func(source, target string) {
		args := []string{"build", "-mod=readonly"}
		if os.Getenv("ASTRLINK_CHECKIN_ACCEPTANCE_DIAGNOSTIC") == "1" {
			// Only a diagnostic build prints the SQLite numeric error code.
			// Overlay a temporary copy; never edit either source tree or use
			// this binary's timings as uninstrumented acceptance evidence.
			replace := map[string]string{}
			patch := func(name, needle, replacement, suffix string) {
				original, err := filepath.EvalSymlinks(filepath.Join(source, "internal", "controlapi", name))
				if err != nil {
					t.Fatal(err)
				}
				body, err := os.ReadFile(original)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Count(string(body), needle) != 1 {
					t.Fatalf("diagnostic overlay no longer matches %s", name)
				}
				copyPath := target + "-" + suffix + ".go"
				if err := os.WriteFile(copyPath, []byte(strings.Replace(string(body), needle, replacement, 1)), 0o600); err != nil {
					t.Fatal(err)
				}
				replace[original] = copyPath
			}
			needle := `func (handler *Handler) writeStoreError(writer http.ResponseWriter, err error) {`
			// The helper prints only a numeric code and a fixed stage name.
			patch("control_helpers.go", needle, `func acceptanceDiagnose(err error) {
var diagnostic interface { Code() int }
if errors.As(err, &diagnostic) { fmt.Printf("acceptance-sqlite-code=%d\n", diagnostic.Code()) }
for cause := err; cause != nil; cause = errors.Unwrap(cause) {
 for _, stage := range []string{"begin service delete", "read service for delete", "delete service", "commit service delete", "read service", "begin service update", "read service for update", "update service", "begin policy update", "read policy for update", "update policy", "read policy", "begin access token create", "count access tokens", "insert access token metadata", "seal access token secret", "insert access token secret", "commit access token create"} {
  if strings.HasPrefix(cause.Error(), stage+":") { fmt.Printf("acceptance-store-stage=%s\n", strings.ReplaceAll(stage, " ", "_")); return }
 }
}
}
`+needle+`
acceptanceDiagnose(err)
`, "service")
			tokenNeedle := `func (handler *Handler) writeAccessTokenError(writer http.ResponseWriter, err error) {`
			patch("access_tokens.go", tokenNeedle, tokenNeedle+"\nacceptanceDiagnose(err)\n", "token")
			overlay, err := json.Marshal(map[string]any{"Replace": replace})
			if err != nil {
				t.Fatal(err)
			}
			overlayPath := target + "-overlay.json"
			if err := os.WriteFile(overlayPath, overlay, 0o600); err != nil {
				t.Fatal(err)
			}
			args = append(args, "-overlay="+overlayPath)
		}
		if os.Getenv("ASTRLINK_CHECKIN_ACCEPTANCE_RACE") == "1" {
			args = append(args, "-race")
		}
		run(source, "go", append(args, "-o", target, "./cmd/astrlink-core")...)
	}
	build(filepath.Join(dir, "core"), baseline)
	build(filepath.Join(root, "core"), current)
	result := acceptanceExecutables{baseline: baseline, current: current}
	if withControl {
		prepareAcceptanceStorageControl(t, filepath.Join(dir, "core"), filepath.Join(root, "core"))
		result.control = filepath.Join(dir, "storage-fixed-core")
		if runtime.GOOS == "windows" {
			result.control += ".exe"
		}
		build(filepath.Join(dir, "core"), result.control)
	}
	t.Logf("real Core comparison against no-extension commit %s; race=%t", acceptanceBaseline, os.Getenv("ASTRLINK_CHECKIN_ACCEPTANCE_RACE") == "1")
	return result
}

// Short paths are required by macOS's UNIX socket address limit. No installed
// application's data directory, keychain, proxy setting or profile is used.
func seedAcceptanceDirectory(t *testing.T, mode, site string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "checkin-core-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Error(err)
		}
	})
	ctx := context.Background()
	store, err := sqlite.Open(ctx, filepath.Join(dir, "astrlink.db"), sqlite.WithLocalKey(acceptanceKey()))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	// Prevent the unrelated price monitor from downloading a public catalog.
	err = store.SavePricingCatalog(ctx, pricing.Catalog{Version: "acceptance-local", ActivatedAt: time.Now().UTC(), Prices: []pricing.Price{{Provider: "openai", Model: "acceptance-model", Expression: `tier("standard", p * 0)`}}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(mode, "on_") {
		if err := store.EnsureForkCheckinSchema(ctx); err != nil {
			t.Fatal(err)
		}
		for index := 0; index < 5; index++ {
			id := forkcheckin.AccountID("acct_accept_" + string(rune('a'+index)))
			_, err := store.CreateForkCheckinAccount(ctx, forkcheckin.Account{ID: id, DashboardBaseURL: site, State: forkcheckin.AccountStateConnected, Network: forkcheckin.Network{Mode: forkcheckin.NetworkModeDirect}, TimeZone: "UTC", RemoteUserID: strconv.Itoa(7 + index)})
			if err != nil {
				t.Fatal(err)
			}
			credential, err := forkcheckin.EncodeNetworkCredential(forkcheckin.NetworkCredential{Version: 1, Bearer: acceptanceSessionSecret})
			if err != nil {
				t.Fatal(err)
			}
			err = store.ForkCheckinSessions().Put(ctx, id, credential)
			clear(credential)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	if strings.HasPrefix(mode, "on_") || mode == "bad_ledger" {
		if err := forkcheckin.SaveSettings(filepath.Join(dir, "fork-checkin.json"), forkcheckin.Settings{Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	if mode == "bad_ledger" {
		db, err := sql.Open("sqlite", filepath.Join(dir, "astrlink.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		_, err = db.Exec(`CREATE TABLE fork_checkin_schema_migrations(version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL); INSERT INTO fork_checkin_schema_migrations VALUES (999999, 'incompatible_fixture', 'fixture')`)
		if err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

type acceptanceCore struct {
	client      *http.Client
	control     string
	inference   string
	logs        *acceptanceLogs
	command     *exec.Cmd
	cancel      context.CancelFunc
	done        chan error
	outputDone  chan struct{}
	stopped     bool
	accessToken string
}

func startAcceptanceCore(t *testing.T, binary, dir, proxy string) *acceptanceCore {
	t.Helper()
	return startAcceptanceCoreWithCodex(t, binary, dir, proxy, "")
}

func startAcceptanceCoreWithCodex(t *testing.T, binary, dir, proxy, codexBase string) *acceptanceCore {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	cmd := exec.CommandContext(ctx, binary, "--data-dir", dir, "--control-token-stdin", "--kek-stdin", "--observer-token-stdin", "--inference-listen", "127.0.0.1:0", "--control-listen", "127.0.0.1:0", "--outbound-proxy", "environment")
	// Disallow public HTTP(S) egress in the child even if an unrelated monitor
	// unexpectedly wakes. Explicitly remove inherited proxy/key-file settings.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		key = strings.ToUpper(key)
		if strings.HasPrefix(key, "ASTRLINK_") {
			continue
		}
		switch key {
		case "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY":
		default:
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "HTTP_PROXY="+proxy, "HTTPS_PROXY="+proxy, "NO_PROXY=localhost,127.0.0.1,::1")
	if codexBase != "" {
		cmd.Env = append(cmd.Env, "ASTRLINK_CODEX_API_BASE_URL="+codexBase, "ASTRLINK_CODEX_OAUTH_ISSUER="+codexBase)
	}
	cmd.Stdin = strings.NewReader(acceptanceOperator + "\n" + hex.EncodeToString(acceptanceKey()) + "\n" + acceptanceObserver + "\n")
	logs := &acceptanceLogs{}
	cmd.Stderr = logs
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	core := &acceptanceCore{command: cmd, cancel: cancel, done: make(chan error, 1), outputDone: make(chan struct{}), logs: logs, client: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}}
	go func() { core.done <- cmd.Wait() }()
	t.Cleanup(func() {
		if !core.stopped {
			cancel()
			select {
			case <-core.done:
			case <-time.After(10 * time.Second):
				t.Error("Core cleanup timed out")
			}
		}
		core.client.CloseIdleConnections()
		select {
		case <-core.outputDone:
		case <-time.After(time.Second):
			t.Error("stdout capture did not finish")
		}
		logs.safe(t, core.accessToken)
	})
	type handshake struct {
		ready contract.ReadyEvent
		err   error
	}
	ready := make(chan handshake, 1)
	go func() {
		defer close(core.outputDone)
		var value contract.ReadyEvent
		err := json.NewDecoder(io.LimitReader(stdout, 16<<10)).Decode(&value)
		ready <- handshake{value, err}
		_, _ = io.Copy(logs, stdout)
	}()
	select {
	case value := <-ready:
		if value.err != nil || value.ready.Event != "ready" {
			t.Fatalf("Core failed its bounded ready handshake: %v", value.err)
		}
		core.control, core.inference = value.ready.ControlURL, value.ready.InferenceURL
	case <-time.After(15 * time.Second):
		t.Fatal("Core did not become ready within 15s")
	}
	return core
}

type acceptanceResponse struct {
	code   int
	header http.Header
	body   []byte
	route  string
}

func (core *acceptanceCore) request(t *testing.T, method, path, body, token, etag string) acceptanceResponse {
	t.Helper()
	url := core.control + path
	if strings.HasPrefix(path, "/v1/") {
		url = core.inference + path
	}
	request, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		request.Header.Set("Content-Type", "application/json")
	}
	if method == http.MethodPatch {
		request.Header.Set("Content-Type", "application/merge-patch+json")
	}
	if etag != "" {
		request.Header.Set("If-Match", etag)
	}
	if strings.HasPrefix(path, "/v1/") {
		request.Header.Set("X-AstrLink-Acceptance", "local-only")
	}
	response, err := core.client.Do(request)
	if err != nil {
		t.Fatalf("%s %s failed: %v", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	// Only the fixed method and path template are recorded; never bodies or tokens.
	route := method + " " + path
	if strings.HasPrefix(path, "/control/v1/services/") {
		route = method + " /control/v1/services/{id}" + path[strings.LastIndex(path, "/"):]
		if strings.Count(path, "/") == 4 {
			route = method + " /control/v1/services/{id}"
		}
	}
	return acceptanceResponse{response.StatusCode, response.Header, data, route}
}

func requireAcceptanceStatus(t *testing.T, response acceptanceResponse, want int) {
	t.Helper()
	if response.code != want {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(response.body, &envelope)
		t.Fatalf("%s status=%d want=%d code=%s (body omitted)", response.route, response.code, want, envelope.Error.Code)
	}
}

func diagnoseAcceptanceDelete(t *testing.T, directory, path, etag string) {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(directory, "astrlink.db"), sqlite.WithExistingDatabase(), sqlite.WithLocalKey(acceptanceKey()))
	if err != nil {
		t.Fatal("cannot reopen isolated diagnostic store")
	}
	defer store.Close()
	err = store.DeleteService(context.Background(), contract.ServiceID(strings.TrimPrefix(path, "/control/v1/services/")), etag)
	var sqlError *sqlitedriver.Error
	code := 0
	if errors.As(err, &sqlError) {
		code = sqlError.Code()
	}
	t.Logf("failed HTTP delete after process shutdown: offline_succeeded=%t sqlite_code=%d", err == nil, code)
}

func (core *acceptanceCore) stop(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	requireAcceptanceStatus(t, core.request(t, http.MethodPost, "/control/v1/shutdown", "", acceptanceOperator, ""), http.StatusAccepted)
	select {
	case err := <-core.done:
		core.stopped = true
		core.cancel()
		if err != nil {
			t.Errorf("Core shutdown exit: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Core exceeded the 10s shutdown target")
	}
	return time.Since(start)
}
