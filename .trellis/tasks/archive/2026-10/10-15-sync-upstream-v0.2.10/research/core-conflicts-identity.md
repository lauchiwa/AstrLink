# Core conflicts and upstream identity audit (v0.2.0 → v0.2.10)

Scope: `HEAD=1be7583` (fork main, contains v0.2.0) merged with `v0.2.10`
(`e8b9600`), merge-base `b3af635`, trial tree `bec8f2b5`. Research only.

Status: complete (research only; no code, index or branch changed).

## Topology note (affects every hunk)

- Upstream tag `v0.2.0` = `78c97c4` is **not** an ancestor of `v0.2.10`.
  Upstream rewrote it as `cb10dda` (same message). So the merge-base is still
  `b3af635`, and git replays upstream's v0.2.0 content (Factory Droid, health
  checks) against the fork's copy of it. Several "conflicts" below are this
  double application, not real intent clashes.
- In every one of the 4 core files the conflict is purely positional: two sides
  appended to the same comment/struct/import block. No hunk is a semantic clash.
  All 4 resolve as "take both" or "take upstream".

## Part 1 — conflict hunks

### 1. `core/contract/subscription.go:187-191` (doc comment) — severity: note

- Fork side (`HEAD`): comment says `ServiceKind.ModelNativeProtocol` (old name,
  inherited from v0.2.0 via 78c97c4; fork did not author it).
- Upstream side: `d54bd01` renamed the method to `BuiltinModelProtocol`
  (`v0.2.10:core/contract/coding_plan.go:10`) and updated this comment.
- Resolution: take upstream (`BuiltinModelProtocol`). Merged tree has no other
  `ModelNativeProtocol` reference (`git grep bec8f2b5` → only this comment), so
  the rename landed cleanly in `endpoint/store_resolver.go`,
  `ingress/execution.go`, `ingress/responses_websocket.go`. After resolving,
  `git diff v0.2.10 -- core/contract/subscription.go` should be empty.

### 2. `core/internal/controlapi/handler.go:69-79` (`Dependencies` struct) — severity: note

- Fork side: adds `IdentityCapture IdentityCaptureController` (interface in
  fork-owned `controlapi/identity_profiles.go:29`) before `Subscriptions`.
- Upstream side (`5ceced5`): adds `RateLimits RateLimitReleaser` after
  `Subscriptions` (used to clear stale 429 cooldowns on subscription restore).
- Resolution: keep both, upstream field order; fork field kept immediately above
  `Subscriptions` as today:

  ```go
  ClientIdentities ClientIdentityReporter
  // IdentityCapture arms service-scoped identity capture windows. Optional;
  // nil reports capture as unavailable instead of silently never capturing.
  IdentityCapture IdentityCaptureController
  Subscriptions   *subscription.Manager
  // RateLimits releases gateway cooldowns when subscription risk is cleared.
  // Supply the same resolver used by ingress. Optional.
  RateLimits RateLimitReleaser
  ```

  Run `gofmt` (alignment differs between sides). The `Handler` struct fields
  (`handler.go:155,157`) and `newHandler` assignments (`:209,:229`) already
  auto-merged with both. Wiring in
  `core/cmd/astrlink-core/persistent.go:288,290` already passes both
  `IdentityCapture` and `RateLimits: resolver`. Also note
  `controlapi/services.go:934` still uses `subscriptions.ForgetUsage` (upstream
  kept it for subscriptions; `CodingPlanUsage.ForgetUsage` was replaced by
  `Refresh` in `a544b1f`). Check fork test fakes of `CodingPlanUsage` (if any)
  implement `Refresh` instead of `ForgetUsage`; merged tree has none
  (`git grep ForgetUsage bec8f2b5` → only `subscription.Manager`).

### 3. `core/internal/ingress/handler.go:31-35` (imports) — severity: note

- Fork side: imports `core/internal/identitycapture` (used by
  `Dependencies.IdentityCapture *identitycapture.Registry`,
  `handler.go:96,152`).
- Upstream side (`0d254ed`): imports `core/internal/imagecompress` (used by
  `Handler.images imagecompress.Compressor`, `handler.go:130`).
- Resolution: keep both imports, alphabetical (`identitycapture` then
  `imagecompress`). The rest of the file (Dependencies, Handler fields,
  constructor) auto-merged with fork's `IdentityCapture`/`IdentityProfiles`
  intact (`handler.go:93-100,152-153,291-292`).

### 4. `core/internal/ingress/subscription_protection.go:58-93` — severity: must-adapt (small)

- Fork side: inserts `identityCaptureBodyLimit` const and
  `captureIdentityCandidate(...)` method (fork-only, no upstream counterpart)
  directly above the unchanged-in-fork `subscriptionProtection(ctx)` method.
- Upstream side (`bd86c2f`, thinking level + global system prompt): replaces
  `subscriptionProtection(ctx) subscriptionProtection` with
  `requestRoutingSettings(ctx) contract.RoutingSettings`; callers now do
  `routingSettings := handler.requestRoutingSettings(ctx)` then
  `subscriptionProtectionFrom(routingSettings)`, `gatewayRewriteFrom(...)`,
  `imageCompressionFrom(...)` (`execution.go:155-158`), and
  `builtin_tools.go:339` passes the settings into `forwardCodexTool`.
- Resolution: keep the fork's const + `captureIdentityCandidate` block, then
  upstream's `requestRoutingSettings` header + doc comment. **Drop** the fork
  side's `subscriptionProtection(ctx)` signature line/comment — it is not a fork
  feature, just stale v0.2.0 text. The function body after the markers is
  upstream's (`return *session.routingSettings` / `return settings`), so keeping
  the old signature would not compile. Merged tree has zero remaining callers of
  `handler.subscriptionProtection(` (only the definition at `:86`), confirming
  it goes away. After resolving, `git diff v0.2.10 -- <file>` must show only the
  fork's +24-line capture block.
- Identity toggles: `subscriptionProtection` struct and
  `subscriptionProtectionFrom` (`:13-37`) are upstream-owned and unchanged;
  fork's `CodexIdentityEnforcement`/`ClaudeIdentityEnforcement`/
  `GrokIdentityEnforcement` (`contract/failure_policy.go:140-142,207-209`) and
  their use in `endpoint/authorizer.go:85,112,118,124` and
  `controlapi/routing_settings.go:50-54` auto-merged. Because routing settings
  are now passed whole, upstream's new `requestRoutingSettings` reads the same
  record session cache the fork's toggles use — no behavior change.
- Fork identity capture call site auto-merged at `execution.go:688-701` and
  still reads `body.originalBody(identityCaptureBodyLimit)` — i.e. pre-rewrite.
  Verify after merge that upstream's new gateway rewrite (global system prompt,
  reasoning override) and image compression do not mutate the buffer returned by
  `originalBody`. Confirmed safe in Part 2 §2.5.

## Part 2 — new upstream outgoing paths since v0.2.0

Method: `git diff --stat v0.2.0 v0.2.10 -- core`, then reading each new sender.
Branding grep:
`git grep -n -i -e astrlink -e User-Agent -e originator -e '"Via"' bec8f2b5 -- <41 files added since v0.2.0>`
and the same pattern over `git diff v0.2.0 v0.2.10 -- core` `+` lines.

Reference for "shared transport" (`bec8f2b5`): `transport/forwarder.go:145-166`
runs `removeInboundCredentials`, overlays `target.RequestHeaders`, provider
adapters, then `removeGatewayHeaders` (strips every `x-astrlink-*`, `:257-267`)
and `removeForwardingHeaders` (`Via`, `Forwarded`, CDN headers, `:269+`). Fork
request header rules and HTTP identity profiles go into that overlay only
through `ingress.applyRequestRules` (`request_rules.go:29-98`), which is a no-op
for subscription kinds (`applyCompiledRules` returns early unless
`Kind.IsHTTP()`, `:67`).

### 2.1 Embeddings / rerank routing (`d6a8bde`) — note

- (a) Shared path. New protocols `openai.embeddings`, `google.embeddings`,
  `rerank` are ordinary Alpha protocols: they use the main loop in
  `ingress/execution.go` (`requiresInspectedJSON` gained them,
  `execution.go:~1150`), so they get `applyRequestRules`
  (`execution.go:522-524`, identity profile + header rules), the authorizer and
  `forwarder.Forward`. No new sender.
- (b) No branding added (`model_rewrite.go`, `protocol_restore.go`,
  `usage_scanner.go` only rewrite model/restore documents).
- (c) No control route. Contract change is the protocol list in
  `contracts/validate.rb:98-103`; validate.rb auto-merges (the trial tree has no
  conflict markers; `git diff HEAD bec8f2b5 -- contracts/validate.rb` = +5/-1,
  upstream only).
- Follow-up: identity capture fires for any `Kind.IsHTTP()` candidate
  (`execution.go:692`). That now includes embeddings/rerank traffic from an
  armed service. That is harmless (candidates still need confirmation), but add
  one test if the capture contract is meant to be inference-only.

### 2.2 Provider web search, `service_search` (`9e148ee`) — note

- (a) Shared transport, not a separate client. `builtinServiceSearch`
  (`ingress/builtin_tools.go:325-358`) and `serveSearch`
  (`ingress/search_api.go:113-130,301`) both go through `forwardCodexTool`
  (`codex_tool_forward.go:70-125`): `networkproxy.Bind`, `authorizer.Headers`,
  `forwarder.Forward`. So it gets X-AstrLink stripping, forwarding-header
  stripping, and the subscription identity policy (Codex identity +
  `CodexIdentityEnforcement` via `endpoint/authorizer.go:124`). It does **not**
  call `applyRequestRules`, so a New API service's fork request header rules or
  pinned HTTP identity profile are **not** applied to `/v1/alpha/search`. This
  matches the gap already recorded for `builtinServiceImages` in
  `10-10-sync-upstream-latest/research/persistent-core-identity.md`
  ("Specialized image/tool paths are not ordinary inference"). Keep upstream's
  single-send, no-failover behaviour. Whether profiles should apply to search is
  a product decision: list it, don't silently widen scope.
- (b) No branding. It sets `User-Agent: ""` to suppress Go's default
  (`builtin_tools.go:336`). The body `id` is a random UUIDv4
  (`builtintools/executor.go` `searchSessionID`), scoped per account when
  session isolation is on (`search_api.go:~290`). Nothing names AstrLink.
- (c) No new route. The settings change rides `PUT routing-settings`
  (`controlapi/routing_settings.go`), which is already classified.

### 2.3 Coding-plan usage refresh (`a544b1f`) — note

- (a) Separate `http.Client`, which is not new: `codingplan.New(store, nil)`
  (`cmd/astrlink-core/persistent.go:291`) builds
  `networkproxy.WrapClient(&http.Client{Timeout})`
  (`codingplan/usage.go:145-151`) and binds the service proxy (`:245`). It is a
  provider-native quota API with the service's own key. Fork header rules and
  identity profiles never applied here before v0.2.10, and still don't. The
  change only adds the `Refresh` method, a per-origin `/api/status` cache, and
  429 `RateLimitError` handling (`:74-78,291-300,375-400`).
- (b) Headers set: `Accept`, `Authorization`, `Accept-Language` (GLM)
  (`usage.go:328-337,392`). No User-Agent is set, so Go's default
  `Go-http-client/1.1` goes out. That is not AstrLink branding, and v0.2.0 did
  the same. No `X-AstrLink-*`.
- (c) No new route. `GET /control/v1/services/{id}/usage?refresh=1` is the
  existing route with a query flag (`controlapi/services.go:920-940`). Interface
  change: `CodingPlanUsage.ForgetUsage` was replaced by `Refresh`
  (`controlapi/handler.go` upstream diff). Fork fakes, if any, must follow.

### 2.4 Config sync remote, WebDAV / S3 (`b3ade1c`, `915509b`) — note

- (a) Separate `http.Client{Timeout: 60s}` (`sync/engine.go:125`,
  `remote/webdav.go:98`, `remote/s3.go:59`), not wrapped by networkproxy. That
  is correct: the remote is the user's own storage, not an upstream model
  provider. So the forwarding identity rule does not apply.
- (b) Not provider-facing. Object names `astrlink-config.enc` and
  `astrlink-config.lock` (`webdav.go:40-41`), folder default `astrlink`
  (`contract/sync.go:37`), and AAD/salt prefixes `astrlink/sync-*`
  (`syncseal.go:52-55`) sit on user-owned storage. They are product-named local
  artifacts, which AGENTS.md allows. No User-Agent is set.
- (c) **New route** `/control/v1/sync/{status,config,passphrase,run}`,
  registered `RoleOperator` (`controlapi/handler.go` "Sync configuration names
  the remote ...", `sync.go:15,58-164`). It is **not** in
  `contracts/control-api.openapi.yaml` (`git show v0.2.10:...` has 0
  `/control/v1/sync` matches), so validate.rb's per-path role loop
  (`validate.rb:347-360`) never sees it and needs no list entry. Nothing to add
  to `operator_reads`. Note that GET `sync/status` and GET `sync/config` are
  operator-only reads, so if upstream later documents them in OpenAPI, they must
  go into `operator_reads`.
- Fork interaction: whether synced entities carry fork
  `service_identity_profiles` or request rules is covered in
  `research/migrations-config-sync.md`.

### 2.5 Image compression to WebP (`0d254ed`, `bbb40d2`) — note

- (a) No outgoing request. `imagecompress` is pure in-process
  (`imports: image/*, github.com/deepteams/webp`, no `net/http`).
  `applyImageCompression` (`ingress/image_compression.go:71-92`) rewrites
  `attemptRequest`'s body in the main loop (`execution.go:359-361`), before
  `applyRequestRules` (`:522`), so header rules and profiles still apply.
- (b) None.
- (c) None. The setting rides routing settings.
- Fork identity capture is safe. It reads `body.originalBody(...)`
  (`execution.go:699`), a fresh copy from the source factory
  (`execution.go:1266-1284`). Gateway rewrite and compression only
  `replaceRecoveryRequestBody(attemptRequest, ...)`. They never mutate the
  source, so captured candidates still describe the client, not the gateway's
  rewrite (global system prompt, reasoning override, WebP).

### 2.6 (extra) Factory Droid subscription (`51e43c4` rewritten as `cb10dda`) — **must-adapt**

New since v0.2.0 (v0.2.0 had contract stubs only).

- (a) Inference uses the shared transport. `forwarder.go:160-163` calls
  `providerapi.DroidRequest`, which reads `X-AstrLink-Factory-Endpoint` from the
  overlay. `removeGatewayHeaders` then strips it (`:165`). OAuth and profile
  calls use `droidAPIRequest`, which explicitly does
  `Header.Del(DroidEndpointHeader)` (`accountauth/droid.go:115-124`).
- (b) **Leak**. `subscription/droid.go:54-62` (`droidUsage`, quota for
  `GET /api/billing/limits`) calls `accountauth.ApplyDroidAPIHeaders`. That
  function sets `X-AstrLink-Factory-Endpoint` for EU organizations
  (`accountauth/droid.go:95-98`). The request then goes out through
  `manager.droidConfig.HTTPClient.Do` (a `networkproxy.WrapClient`, which does
  not strip headers) without the `Del`. So an EU Factory account sends
  `X-AstrLink-Factory-Endpoint: https://api.eu...` to Factory on every usage
  read. This breaks the AGENTS.md rule "strip the reserved `X-AstrLink-*`
  namespace", which covers quota/profile requests. US accounts are unaffected
  (the header stays nil).
- Minimal fix, which touches upstream code but is tiny and upstreamable: add
  `request.Header.Del(providerapi.DroidEndpointHeader)` right after
  `ApplyDroidAPIHeaders` in `droidUsage`, or build the request with
  `droidAPIRequest`. Add a test with an EU token
  (`tokens.Region = DroidRegionEU`) asserting that no `X-Astrlink-*` header
  reaches the test server. Annotate it as a deliberate deviation, or better,
  file or upstream it, so the next sync does not revert it.
- `providerapi.DroidRequest` keeps only `droidKeptHeaders`
  (`providerapi/droid.go:41-44`) and prepends Droid's system prompt line
  (`:24`). That is a provider-required identity, not AstrLink branding. Fork
  request rules don't apply to subscription kinds (`applyCompiledRules` is a
  no-op). The identity is fixed at `factory-cli/<ver>`
  (`identity_defaults.go:107`). AGENTS.md's enforcement toggles name only
  Codex/Claude/Grok, so no Droid toggle is required.

### 2.7 Other new senders and routes — note

- `modelmeta` fetches `https://basellm.github.io/llm-metadata/api`
  (`modelmeta/modelmeta.go:20-41`). That is public metadata, not a provider, and
  no branding header is set. New `GET /control/v1/model-metadata` is
  `RoleObserver` in code and `x-astrlink-role: observer` in OpenAPI
  (`openapi.yaml:340-342`), which matches the validate.rb default (GET →
  observer).
- New `GET /control/v1/privacy-summary` (`controlapi/requests.go:36`) and
  `GET /control/v1/privacy/warnings` are observer in both code and OpenAPI
  (`openapi.yaml:1554-1557,1606-1609`). No list change is needed.
- Fork operator reads (`identity-profiles`, `identity-profiles/{profile_id}`,
  `identity-capture`) remain in `operator_reads` (`validate.rb:335-344`) after
  the auto-merge. validate.rb and control-api.openapi.yaml both auto-merge with
  0 conflict markers in `bec8f2b5`.
- `ruby` is not installed on this machine, so `make contracts-check`
  (`Makefile:25`, CI `ci.yml:90`) could not be run during research. Run it after
  the merge.

## Summary

| #   | Item                                                                                                                             | Severity                |
| --- | -------------------------------------------------------------------------------------------------------------------------------- | ----------------------- |
| 1.1 | `contract/subscription.go` comment: take upstream `BuiltinModelProtocol`                                                         | note                    |
| 1.2 | `controlapi/handler.go` `Dependencies`: keep fork `IdentityCapture` and upstream `RateLimits`; gofmt                             | note                    |
| 1.3 | `ingress/handler.go` imports: keep `identitycapture` and `imagecompress`                                                         | note                    |
| 1.4 | `subscription_protection.go`: keep fork capture block, adopt upstream `requestRoutingSettings`, drop stale v0.2.0 signature      | must-adapt (mechanical) |
| 2.1 | Embeddings/rerank use the main path (rules, profiles, stripping)                                                                 | note                    |
| 2.2 | `service_search` skips fork request rules and HTTP profiles (same class as `builtinServiceImages`)                               | note (product decision) |
| 2.3 | Coding-plan refresh uses its own proxied client, as before; `ForgetUsage` replaced by `Refresh` (merged tree has no stale fakes) | note                    |
| 2.4 | `/control/v1/sync/*` is operator-only and absent from OpenAPI, so validate.rb needs no change                                    | note                    |
| 2.5 | Image compression is in-process; capture still reads the original body                                                           | note                    |
| 2.6 | `subscription/droid.go:54-62` leaks `X-AstrLink-Factory-Endpoint` for EU Factory orgs                                            | **must-adapt**          |

No blocker. All 4 core conflict files resolve as "take both" or "take upstream".
The fork's identity toggles, capture, profiles, and request rules survive
intact.

Post-merge checks:

- `(cd core && go build ./... && go vet ./...)`
- `go test ./internal/ingress/... ./internal/controlapi/... ./internal/subscription/... ./internal/accountauth/...`
- `git grep -n -e ModelNativeProtocol -e 'subscriptionProtection(ctx' -- core`
  should return nothing.
- `make contracts-check` on a machine with Ruby.
