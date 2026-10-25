# Implement: sync upstream v0.2.18

## Checklist

1. [x] Re-verify `upstream/main` = `v0.2.18` = `2cba3d6` (fetch exit code, then
       `gh api repos/Calcium-Ion/AstrLink/commits/v0.2.18 --jq .sha`).
2. [x] `git merge --no-ff --no-commit v0.2.18` on clean `main`.
3. [x] Resolve the 7 conflicts per `research/conflicts.md`.
4. [x] Migrations: renumber profiles to 56, extend reconciliation to 53, update
       comments; tests in `identity_profile_history_test.go` by name.
5. [x] Release notes D1 in `release-updates.mjs`; tests for the matrix in PRD
       AC5; update pinned workflow command strings.
6. [x] Forwarding loopback test for `X-AstrLink-Service` (HTTP + WebSocket).
7. [x] Post-merge greps: `Calcium-Ion` in production paths, new
       `github.repository ==`, new outbound senders, fork-changed defaults in
       new upstream tests.
8. [x] Build/vet every Go module before tests (upstream signature changes).
9. [x] Format/lint/test per AGENTS.md on changed files; record below.
10. [x] `git diff v0.2.18 -- <conflicted paths>` explainable line by line.
11. [ ] Ask the user before committing.

## Validation commands

```bash
(cd core && go build ./... && go vet ./... && go test ./...)
(cd convo && go vet ./... && go test ./...)
(cd contracts && go vet ./... && go test ./...)
(cd apps/desktop && bun run typecheck && bunx vitest run)
(cd apps/privacy-worker && cargo clippy --locked --all-targets -- -D warnings)
(cd apps/desktop/src-tauri && cargo test --lib)
ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink \
  bash -c 'cd apps/desktop/src-tauri && cargo test --lib'
git diff --check
```

## Results

Merge: `MERGE_HEAD` = `2cba3d6`; `e87d4e4` is an ancestor of `HEAD`;
`git ls-files -u` is empty; `git diff --check HEAD` passes.

Post-merge fixes beyond the 7 conflicts:

- `accountauth`: upstream removed `maxUserIDScan` (220bdb7). The fork test now
  asserts that a 2 MiB Claude body is captured.
- `cmd/astrlink-core`: upstream lets browser origins call inference with an
  access token (ddad083). The pre-auth case now sends no token and expects 401.
- `storage/sqlite` service cache: upstream's clone shared the fork
  `ExtraHeaders`/`ModelRules` with the cached record. Fixed in fork-owned
  `fork_service_cache.go`. Removing the call fails
  `TestCachedServiceCloneOwnsEveryMutableField`.
- `storage/sqlite` config writes: removed the fork's `retryConfigWrite`
  (04478e4) and its 4 test files; `service_store.go`/`policy_store.go`/
  `CreateAccessToken` now match v0.2.18. Upstream ef5b14b (`_txlock=immediate`
  plus writer gate) fixes the same 5/517 errors, so the fork's read-upgrade
  scenario can no longer occur. A temporary 16-worker, two-pool stress test of
  create/update/delete service plus policy update passed 3/3 with upstream's
  DSN. Without `_txlock=immediate` it reproduced 210 BUSY/517 errors. The test
  was deleted afterwards.

Commands and results:

- `(cd core && go vet ./... && go test -count=1 ./...)`: 4798 passed, 49
  packages
- `(cd convo && go vet ./... && go test ./...)`: pass
- `(cd contracts && go vet ./... && go test ./...)`: pass
- `gofmt -l` on changed Go files: empty
- `(cd apps/desktop && bun run typecheck)`: pass
- oxlint `--deny-warnings` + prettier `--check` on changed JS/TS/JSON/YAML: pass
- `(cd apps/desktop && bunx vitest run)`: 167 files, 2051 passed
- src-tauri `cargo fmt --check`; `cargo clippy --locked --all-targets`: pass
- src-tauri `cargo test --locked --lib` (default repository): 414 passed, 1
  ignored
- same with `ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink`: 414 passed, 1
  ignored
- classifier-worker, privacy-worker `cargo fmt --check` + clippy: pass
- mutation: drop 53 from reconciliation loop: `fork_53`, `fork_53_applied` fail
- mutation: D1 edit condition / idempotency: 2 adhoc tests fail each
- mutation: exempt `X-AstrLink-Service` from strip: loopback test fails

One earlier full vitest run failed "verifies a real ad-hoc signed app, nested
Mach-O files and DMG" after 101 s under load. The test passes alone (17 s) and
in the full rerun above.

Greps: new `Calcium-Ion` lines are only in test files; `github.repository ==` is
unchanged (`docker.yml` existed before). Upstream added no new outbound request
sites. Every file upstream changed that still differs from v0.2.18 already
differed before the merge.
