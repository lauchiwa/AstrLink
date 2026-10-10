# Persistent core and identity lifecycle integration

## Scope and evidence

Planning-only research for the missing link in
[stable-conflicts.md](stable-conflicts.md). No merge, checkout, implementation,
commit, task activation, user-database access, or external-service request was
performed. Only this research file is written.

`python3 ./.trellis/scripts/task.py current --source` resolved this task from
the current session; `get_context.py` reported status `planning`. Read the task
PRD, `AGENTS.md`, backend/guides indexes, upstream-sync, code-reuse, and
cross-layer thinking guides. The upstream-sync guide's general main-branch
examples do not replace this task's fixed stable target.

Object anchors below are reproducible with `git show <object>:<path>`; line
numbers refer to that object's file, not a future resolved working tree:

- **HEAD**: `7165c7465eec555490da5b1689fa10e0f2d022af`, verified unchanged.
- **stable**: `78c97c4065ec5ba2263d6d32f9f53355567ea585`, selected `v0.2.0`.
- **merged**: `59b02f52cc3cb4be8ed6ebfa956d026b03112fc8`, the supplied
  conflict-containing merge tree, not an executable completed merge.
- Fork delta was examined against merge base
  `78cc0dc9fdd29d362fde3f5781bd7dcd2c0fd688`.

## Main finding: the new composition root bypasses fork wiring

The entire fork startup delta is visible in
`git diff 78cc0dc9fdd29d362fde3f5781bd7dcd2c0fd688 HEAD -- core/cmd/astrlink-core/main.go`:

- **HEAD `core/cmd/astrlink-core/main.go:229-238`**: create one capture registry
  using the opened store, after hydrating learned subscription identities.
- **HEAD `core/cmd/astrlink-core/main.go:307-308`**: pass that registry and
  store to ingress as `IdentityCapture` and `IdentityProfiles`.
- **HEAD `core/cmd/astrlink-core/main.go:325-330`**: pass the same registry to
  the control API and construct `servicemodel.NewWithDependencies` with
  `Secrets`, `Subscriptions`, and `IdentityProfiles`.

Stable moves persistent composition into a new file. A diff of stable against
merged for `core/cmd/astrlink-core/persistent.go` and `serve.go` is empty: these
files merge cleanly **without any of the fork wiring**.

The critical anchors are **stable
`core/cmd/astrlink-core/persistent.go:220-280`**: its gateway literal omits both
fork fields; its control literal omits capture and uses
`servicemodel.New(store, subscriptionManager, nil)`. The merged constructors
still accept these omissions because the dependencies are optional.

Consequences if main is resolved to upstream without adapting this seam:

1. Operator capture requests return `503 identity_capture_unavailable`; ingress
   silently behaves as unarmed. Evidence: **HEAD
   `core/internal/controlapi/identity_profiles.go:46-49`**, **HEAD
   `core/internal/identitycapture/registry.go:129-139`**.
2. A bound HTTP identity profile cannot load during inference or aggregate model
   discovery. It fails the affected candidate closed, rather than forwarding
   without the pinned profile. Evidence: **HEAD
   `core/internal/accountauth/api_identity.go:102-123`**; **merged
   `core/internal/ingress/execution.go:473-499`** and
   `core/internal/ingress/discovery.go:274-288`.
3. Control-plane saved/draft model discovery loses its profile reader too. `New`
   delegates to `NewWithDependencies` without it. Probing a pinned profile
   returns `ErrConfiguration` before a request is sent. Evidence: **merged
   `core/internal/servicemodel/prober.go:50-81,200-218`**.
4. Profile CRUD can still work through `ServiceStore` while the actual consumers
   are broken. A UI-only CRUD test or healthy startup does not detect this.
   Evidence: **HEAD `core/internal/controlapi/identity_profiles.go:106-122`**.

These are static dependency-flow findings, not observed runtime test failures.

## Keep the upstream desktop and server split

Both editions already share the correct initialization seam:

- **stable `core/cmd/astrlink-core/main.go:127-161`** opens the persistent core,
  installs control/retention dependencies, and creates a production ingress
  handler for the actual listen address. Preserve the
  `NewInferenceHandler(address, networkExposed)` signature and its choice of
  `NewProduction` versus `NewNetworkProduction`.
- **stable `core/cmd/astrlink-core/serve.go:123-179`** locks the data directory,
  creates a private random control token and console sessions, opens the same
  persistent core, then builds network ingress and the console. It defers
  `core.close()` before password reset, gate creation, or listener startup.
- **stable `core/cmd/astrlink-core/serve.go:188-208`** supplies server-specific
  options: no loopback OAuth callback, shared console sessions, increased raw
  password backoff, and test-only offline clients. Do not replace these with
  desktop defaults.
- **stable `core/cmd/astrlink-core/persistent.go:179-219,249-276`** shares the
  hydrated `accountauth.IdentityRegistry` with subscription management, the
  authorizer, ingress, and control. **stable
  `core/cmd/astrlink-core/main.go:338-375`** forwards the identity registry and
  no-loopback setting to OAuth configuration. Keep all of this intact.
- **stable `core/cmd/astrlink-core/persistent.go:30-41`** installs outbound
  proxy policy before client/transport construction. The fork adaptation must
  retain normal discovery client construction, not introduce a parallel HTTP
  client or proxy configuration.

The no-data-directory headless handler remains deliberately separate and does
not need persistent capture/profile storage.

## Minimal additive integration recommendation

Expected product boundary for later authorized implementation:

1. Resolve `core/cmd/astrlink-core/main.go` to upstream's functional composition
   shape. Remove stale imports from the displaced inline fork setup; do not
   restore that block or initialize identities a second time in `serve.go`.
2. Add a small fork-owned `core/cmd/astrlink-core/persistent_identity.go` with
   two concrete helpers, using existing constructors rather than reimplementing
   capture, profile validation, or discovery:
   - A helper taking the opened `*sqlite.Store` and `*ingress.Dependencies`.
     Construct `identitycapture.New(store)`, then set `IdentityCapture` and
     `IdentityProfiles`; return the registry for control wiring. Propagate any
     constructor error before mutating the dependency bundle. Do not hide a
     failure by returning a nil capture dependency.
   - A profile-aware discovery constructor taking the same store and
     subscription manager. Return `servicemodel.NewWithDependencies` with
     `Secrets: store`, `Subscriptions: manager`, `IdentityProfiles: store`, and
     the existing nil client default.
3. In upstream-owned `persistent.go`, call the first helper immediately after
   the gateway dependency literal and **before** constructing control
   dependencies. Route an error through the existing
   `fail("configure identity capture: %w", err)` cleanup path. Add the returned
   registry to control `IdentityCapture`; use the second helper for
   `ServiceModels`.
4. Add fork-owned `core/cmd/astrlink-core/persistent_identity_test.go` for real
   composition regressions. Reuse upstream server test helpers where practical;
   do not rewrite upstream tests simply to expose the fork wiring.

Helper names are suggestions, not new public interfaces. Two small helpers are
sufficient; no plugin registry, generic lifecycle framework, global singleton,
new feature flag, migration, or per-edition implementation is warranted.

**Ordering matters:**
`servicetest.NewWithDependencies(gatewayDependencies, ...)` and
`ingress.NewWithDependencies(gatewayDependencies)` consume dependency values
inside the control literal (**stable `persistent.go:270-271`**). Mutating only
`core.gateway` after control construction leaves connection/built-in testers
with stale nil fields. Setting gateway dependencies first automatically gives
those consumers the same capture registry and profile reader. Do not claim that
this alone makes every specialized tool path profile-aware; see semantic
hazards.

## Identity lifetimes and cleanup contract

### Separate the three identity concepts

1. **Learned subscription identities**: upstream's shared
   `accountauth.IdentityRegistry` hydrates persistent learned state; hydration
   errors are logged and do not abort startup. Evidence: **stable
   `persistent.go:180-185`**, **HEAD
   `core/internal/accountauth/identity_registry.go:58-89`**.
2. **Explicit capture consent**: `identitycapture.Registry` is one in-memory map
   per opened persistent core, shared by control and ingress. It has no worker,
   timer goroutine, external connection, or `Close` method. Expiry is checked on
   access; do not invent a shutdown API for it. Evidence: **HEAD
   `core/internal/identitycapture/registry.go:1-10,59-79,222-247`**.
3. **Frozen HTTP profiles**: store-backed candidates require explicit
   confirmation and service-scoped binding. A captured or imported identity does
   not update subscription learning or automatically affect forwarding.
   Evidence: **HEAD
   `core/internal/accountauth/api_identity.go:30-34,77-92,125-159`** and
   `core/internal/controlapi/identity_profiles.go:101-105,183-195`.

Capture windows default/cap to ten minutes, close after one publication, and
stop at 64 recognition rejections. Concurrent observations reserve the exact
consent window before storage I/O. A write that already started may finish after
disarm or re-arm, but must not overwrite the newer window's state. Failed writes
release the reservation. Evidence: **HEAD
`core/internal/identitycapture/registry.go:28-34,84-102,144-219`**.

A new `openPersistentCore` call must create fresh, unarmed consent even when
using the same fixture database. Persisted candidates/confirmed profiles can
survive restart; arming must not. Tests should discard old handlers after close,
not assume a retained registry pointer implements revocation or rejects all
calls.

### Reuse existing ownership, including failure returns

**stable `core/cmd/astrlink-core/persistent.go:95-120`** resolves/clears local
keys, opens the store, and installs a failure function that closes the privacy
worker if present and closes the store. **Lines 281-305** start three monitors
only after successful handler construction; normal close revokes raw grants,
locks the vault, cancels and waits for monitors, closes the store, then closes
the worker.

The proposed capture helper acquires no independent closeable resource. Its
errors, and subsequent control-construction errors, belong in the existing
failure path. Do not use `os.Exit`, `log.Fatal`, a detached goroutine, or a
second store inside the helper. Do not make a helper defer close the store on
successful return. There is no need to persist or drain the capture map at
shutdown.

**Existing upstream caveat:** **stable `main.go:145,167-170`** defers core
cleanup but calls `os.Exit(1)` if `coreapp.RunWithDependencies` fails. Go defers
do not run on that exit path. Thus normal desktop return and server error return
have cleanup paths, but late desktop startup/runtime errors cannot be described
as running the deferred cleanup. This predates the proposed integration. Record
it in the final plan; if explicit cleanup on every desktop error is required for
acceptance, seek a narrowly scoped decision rather than silently rewriting main.
It is not evidence of a capture-specific resource leak, since capture owns none.

## Forwarding, discovery, and authorization invariants

- Ordinary inference chooses request rules against the effective upstream model
  and authentication scheme, overlays a confirmed profile, and fails a bad
  candidate closed. Capture runs from `ObserveOutbound` using **original**
  inbound headers and a bounded original body, after admission and candidate
  selection. Evidence: **merged
  `core/internal/ingress/execution.go:473-499,622-660`**. This observation is a
  transport dispatch hook, not proof of a successful upstream response; do not
  move capture to a global middleware or success-only response hook. **HEAD
  `core/internal/ingress/subscription_protection.go:58-79`** bounds recognition
  to 1 MiB and logs capture storage failures without failing inference.
- Aggregate discovery uses service defaults only, never a per-model catch-all; a
  bad profile excludes that service. Control discovery resolves the same
  defaults/profile once per probe and carries them through pagination. Evidence:
  **HEAD `core/internal/ingress/request_rules.go:42-90`**, **merged
  `core/internal/servicemodel/prober.go:200-225,329-338`**. Keep proxy
  selection, credential ownership, and same-origin redirect refusal in that
  prober.
- Actual HTTP and WebSocket forwarding strips local credentials, applies target
  overlays, then removes reserved gateway and proxy-chain headers. Preserve the
  stable-added post-overlay forwarding-header cleanup. Evidence: **merged
  `core/internal/transport/forwarder.go:145-168,227-301`** and
  `core/internal/transport/responses_websocket.go:95-118`. `X-AstrLink-Console`,
  other `X-AstrLink-*`, cookies, and local access tokens must not appear at an
  upstream stub. Check emitted requests, not just the helper's dependency
  struct.
- Full profile reads and capture status/arming remain operator-only. The
  same-UID control socket is observer, not operator. A console-authenticated
  request obtains operator status through trusted request context, not a caller
  header. Evidence: **merged `core/internal/controlapi/services.go:155-163`**,
  `core/internal/controlapi/roles.go:35-55`, and
  `core/internal/console/console.go:319-358`.
- Server console and inference credentials remain disjoint. Console writes need
  a live session plus the console request header; inference still requires a
  local access token and rejects browser origins. Evidence: **stable
  `core/internal/coreapp/serve.go:109-120`**, **merged
  `core/internal/ingress/handler.go:231-256,809-863`**. Do not grant operator
  rights based on network location, the mere presence of a cookie, or
  recognition of an official-looking client identity.

## Additional clean-merge semantic hazards

### Specialized image/tool paths are not ordinary inference

**merged `core/internal/ingress/handler.go:384-393`** dispatches new
image/search protocols before the usual routing path. **Merged
`core/internal/ingress/images_api.go:79-105,129-138`** can route Images requests
to `builtinServiceImages`. That function authorizes and forwards directly
without `applyRequestRules` or capture: **merged
`core/internal/ingress/builtin_tools.go:209-258`**.

The direct image helper already lacked these calls at HEAD
(**`builtin_tools.go:199-246`**); stable newly reaches it from standalone Images
requests and adds MiniMax conversion. This is a pre-existing compatibility gap
with a new entry point, not evidence that the shared-core fix regressed normal
inference. A pinned HTTP profile is not guaranteed on this specialized image
backend merely by adding `IdentityProfiles` to the gateway bundle. Capture must
not be added using that helper's synthesized headers: they are not the original
client identity.

**merged `core/internal/ingress/codex_tool_forward.go:63-115`** directly
forwards subscription search/images using the shared authorizer and transport.
Its callers select Codex subscriptions, not ordinary HTTP profile bindings; do
not treat the absence of HTTP-profile lookup there as the same defect. Preserve
upstream's single-send/no-failover behavior for potentially billable images.

Recommendation: explicitly retain these upstream flows, add a focused boundary
case to the validation plan, and report whether pinned HTTP image-backend
support is required under R2/R3 before widening implementation. Do not imply
this research has implemented that support or a failing test has already
demonstrated it.

### Header cleanup must win over fork compatibility overrides

Stable now strips forwarding-chain headers after target overlays, on both HTTP
and WebSocket paths. A fork rule trying to set such a header may therefore no
longer reach the provider. Preserve this upstream isolation behavior; do not
reorder overlaying after cleanup to make a compatibility assertion pass.

The newly added local `X-Openai-Actor-Authorization` switch is stripped from
inbound headers by transport (**merged `forwarder.go:241-249`**). Final-header
tests should include it alongside the console marker; do not repurpose it as a
provider identity or profile field.

There is also a concrete overlay review item: **merged
`core/contract/request_rules.go:51-65,111-145`** does not reserve that new actor
header. With ordinary bearer authentication it classifies as overridable. The
transport removes it before, not after, applying `Target.RequestHeaders`
(**merged `forwarder.go:145-162`**, `responses_websocket.go:98-103`). Thus a
fork-configured header can reintroduce a switch that stable describes as local
only. This is a static clean-merge contract mismatch, not an observed test
failure. Include both inbound and target-overlay cases in validation; prefer
reserving the field in the fork-owned header policy rather than weakening
upstream's removal. Review final transport behavior separately if the contract
requires protection against every overlay source, not just request rules.

## Required regression plan for later implementation

All following product checks are **proposed, not run** in this planning session.
Use temporary directories and fixture credentials only. Reuse **stable
`core/cmd/astrlink-core/serve_test.go:159-177,180-217`** (`offlineNetwork`,
`startServe`, `shortDataDir`). Its privacy metadata stub is loopback-only and
its pricing transport refuses all requests. Do not use a real data directory or
launch the production server with default `/data`/environment configuration.
Seed only fixture services/accounts and deterministic privacy policy; never let
subscription migration/refresh use a real account or local credential fixture.
Use bounded waits around the existing server helpers for failure cases.

### Composition tests that detect omitted wiring

- Open a real persistent core using temporary storage and offline clients.
  Through its actual control handler create an HTTP service, arm capture, then
  dispatch a recognized request through a production handler constructed from
  `core.gateway` to a loopback stub. Assert one unconfirmed candidate is visible
  through control; a second request does not capture again. This detects both
  missing wiring and accidentally constructing different registries for control
  and ingress.
- Confirm/bind a profile with a distinctive fixture fingerprint through control;
  assert the final HTTP request uses it while keeping the provider credential.
  Cover service default versus model rule, conversion, retry/fallback, and the
  WebSocket handshake path using existing lower-level suites.
- Probe saved **and draft** service models through the core's real control
  handler, plus aggregate `/v1/models` through production ingress. Assert
  profile/default headers at the upstream stub; include pagination,
  unconfirmed/missing profile, and cross-service scope. Control model tests that
  only use an injected fake `ServiceModels` implementation cannot catch the
  startup regression.
- Run a connection test from control to verify it received the populated gateway
  dependency copy. Keep capture on/off assertions separate from profile binding;
  capture must never change forwarding or grant official-client treatment.
- Close and reopen the same temporary database: confirmed profiles remain
  usable, pending candidates remain unconfirmed, and capture is unarmed. Also
  open two independent fixture cores to catch any accidental process-global
  consent.

### Server and trust-boundary tests

- Use the real `serve` entry, loopback listener with network-production
  semantics, console setup/login, and console-authenticated profile/capture
  operations. Send inference with a separate local access token. Verify the
  resulting profile and headers using the same stubs as the desktop composition
  test.
- Reject unauthenticated console requests, inference-token-as-operator, missing
  console write header, and stale session. Reject observer token/local socket
  profile reads and capture changes. Inference rejects cookie-only and browser
  Origin requests and must not record those pre-auth network rejections or
  create candidates. Valid network requests accept arbitrary Host without
  weakening authentication.
- Keep no-loopback OAuth callback and server backoff option tests; preserve raw
  password/session invalidation behavior. Include a final HTTP/WS assertion for
  proxy headers, local cookies/tokens, `X-AstrLink-*`, and the actor switch.
- The existing **stable `serve_test.go:263-266`** one-port integration test
  skips Windows because it drives a Unix control socket. Add portable
  HTTP/console identity coverage without a blanket Windows skip; run the socket
  case on Linux/macOS later. Do not report Unix behavior as verified on this
  machine.

### Failure and cleanup tests

- Fail before store open with an invalid temporary key configuration; fail after
  store open and before success by supplying a too-short control token or equal
  observer/control tokens. The actual constructor checks are **merged
  `core/internal/controlapi/handler.go:161-175`**. Require an error, no usable
  core, no started monitors, cleared injected key material where applicable, and
  no retained store/worker resources. Use an observable close seam only if
  needed; reopening alone is not proof of SQLite handle cleanup.
- A nonexistent privacy-worker executable is **not** a deterministic startup
  failure: **stable `core/internal/privacyworker/client.go:118-141`** constructs
  a lazy client without testing executable existence. Do not base a failure test
  on that assumption or launch a real worker/model merely for composition
  coverage.
- With a valid opened server core, force failure after open using an invalid
  listen address or a deliberately occupied loopback port. Also cover a
  fixture-only password-reset marker read error. Assert no listening callback,
  bounded return, monitor termination, and data-directory lock release; a
  subsequent fixture server can start. Keep upstream listener-setup failure
  tests too.
- Cancel an active server request and then close the core; check no writes
  continue against closed storage. Use deterministic barriers and deadlines
  rather than sleeps. Capture's existing reservation semantics allow an
  already-started write to finish; do not invent a stronger disarm guarantee.
- Test the desktop late-error path in a bounded subprocess if that caveat is
  made an acceptance requirement. A process exit proves termination, not that
  deferred cleanup ran. No capture constructor failure is normally reachable
  with the successfully opened non-nil store; do not add a broad factory
  framework just to manufacture that branch.

### Existing suites to retain and extend

- **HEAD `core/internal/identitycapture/registry_test.go:79-435`**: unarmed,
  scoped consent, expiry/disarm, reservation races, storage failure,
  at-most-once publish.
- **HEAD `core/internal/ingress/identity_capture_test.go:105-205`**: capture
  does not change forwarding; candidates exclude credentials/session/body
  content.
- **HEAD `core/internal/ingress/request_rules_test.go:155-427`** and
  `request_rules_conversion_test.go:18-169`: effective model/auth, default/rule
  precedence, unusable identity, credentials, conversion.
- **HEAD `core/internal/ingress/request_rules_discovery_test.go:48-99`** and
  **merged `core/internal/servicemodel/request_rules_test.go:12-62`**: discovery
  defaults and fail-closed behavior. The latter does not prove a successful
  pinned profile through the production control composition; add that case.
- **HEAD `core/internal/accountauth/api_identity_test.go:277-356`**, control
  profile, capture, binding, and ingress audit tests: scope/confirmation, final
  forwarding, operator rights, and no observer-readable configured header
  values.
- **stable `core/cmd/astrlink-core/serve_test.go:263-416,624-629`**, console
  tests, **merged `core/internal/ingress/network_production_test.go:15-118`**,
  and `core/internal/transport/forwarding_headers_test.go:52-84`: preserve
  server and network boundary behavior rather than replacing it with
  fork-specific gates.

After approval and implementation, run the applicable Go formatting/vet gates
and at least the focused package set below, then the task-wide module checks.
Run race tests where supported; record platform/toolchain restrictions rather
than claiming coverage from a skip. Resolve declared toolchain versions without
lowering repository requirements.

```bash
(cd core && go test ./cmd/astrlink-core ./internal/identitycapture \
  ./internal/accountauth ./internal/controlapi ./internal/ingress \
  ./internal/servicemodel ./internal/servicetest ./internal/transport \
  ./internal/coreapp ./internal/console ./internal/subscription)
(cd core && go vet ./...)
```

## Decisions, blockers, and research validation

- **Recommended design:** shared initialization in `persistent.go`, two small
  fork-owned helpers, one in-memory capture registry per core, existing cleanup,
  and integration tests through real control/inference consumers in both
  editions.
- **Acceptance blockers if omitted:** capture wiring, ingress profile reader,
  control discovery profile reader, and dependency-copy ordering. A
  conflict-free file or passing CRUD test is not sufficient evidence.
- **Review decisions:** the upstream desktop `os.Exit` cleanup caveat, the
  pre-existing direct image backend's profile/capture gap, and the new actor
  header's fork-rule overlay mismatch must be explicit in the plan. None
  authorizes reverting upstream flow or expanding this sync without a scoped
  decision. No runtime test failure is asserted by this research.
- **Scope:** no schema/history change is needed for this composition adaptation;
  migration boundaries remain those in `stable-conflicts.md`. README, releases,
  remotes, task status, product code, and personal workspace files are
  untouched.
- **Validation:** local Git-object inspection confirmed the missing wiring and
  preserved fork consumers. Cached Prettier `3.6.2` and markdownlint-cli
  `0.45.0` were found with `bunx --no-install`; formatting/lint results are
  recorded below. No product build, Go test, server process, or database fixture
  was run here.

### Markdown verification performed

From the repository root, the following commands completed successfully. The
`--no-install` flag uses already cached tool versions and prevents installation
or package fetching; no external service was needed.

```bash
bunx --no-install prettier@3.6.2 --write --prose-wrap always ".trellis/tasks/10-10-sync-upstream-latest/research/persistent-core-identity.md"
bunx --no-install --package markdownlint-cli@0.45.0 markdownlint --fix ".trellis/tasks/10-10-sync-upstream-latest/research/persistent-core-identity.md"
bunx --no-install prettier@3.6.2 --check ".trellis/tasks/10-10-sync-upstream-latest/research/persistent-core-identity.md"
bunx --no-install --package markdownlint-cli@0.45.0 markdownlint ".trellis/tasks/10-10-sync-upstream-latest/research/persistent-core-identity.md"
git diff --check
```

Prettier reported all matched files use its style; markdownlint and
`git diff --check` reported no diagnostics. These checks were repeated after the
final research edits. The target file is untracked with its task directory, so
Prettier/markdownlint explicitly checked its path; `git diff --check` alone does
not cover untracked content. Git status still contains only the task directory
and the pre-existing personal workspace directory, with no tracked product
changes. No implementation or product-test acceptance is claimed.
