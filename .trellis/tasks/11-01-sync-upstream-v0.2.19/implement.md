# Implement: sync upstream v0.2.19

## Checklist

1. [ ] Re-verify `v0.2.19` = `752791a` (SSH fetch exit code, then
       `gh api repos/Calcium-Ion/AstrLink/commits/v0.2.19 --jq .sha`); `main` =
       `origin/main` = `9619d5b`, clean tree.
2. [ ] Remove the trial worktree `../AstrLink-trial-0219`
       (`git worktree remove`), then `git merge --no-ff --no-commit v0.2.19` on
       `main`.
3. [x] Resolve the 12 conflicts per `research/conflicts.md`.
4. [x] Migrations: profiles to 57, reconciliation adds 56 with the dedicated
       probe, comments updated; tests in `identity_profile_history_test.go` by
       name (design.md list).
5. [x] `(cd core && go build ./... && go vet ./...)` before any test (upstream
       signature changes, e.g. `probeVertex`, `Prober` fields).
6. [x] D1: `fork_request_rules.go` helper; non-Vertex path calls it; Vertex
       branch applies it; `probeVertex(ctx, connection, token, headers)`; tests
       for PRD AC7, plus mutation: drop the Vertex helper call → test fails.
7. [x] Forwarding loopback check for Vertex (PRD AC4); add a case only if
       upstream's existing test does not cover a fork `X-AstrLink-*` overlay.
8. [x] Desktop: `ServiceTestResultView` test for cancelled/success tone; i18n
       key parity test passes in all three locales.
9. [x] Post-merge greps (already clean in the trial tree; re-run on the real
       merge): `Calcium-Ion` in production paths, new `github.repository ==`,
       new outbound senders, fork-changed defaults in new upstream tests.
10. [ ] Format/lint/test per AGENTS.md on changed files; record below.
11. [x] `git diff v0.2.19 -- <conflicted paths>` explainable line by line.
12. [x] Ask the user before committing.

## Validation commands

```bash
(cd core && go build ./... && go vet ./... && go test -count=1 ./...)
(cd convo && go vet ./... && go test ./...)
(cd contracts && go vet ./... && go test ./...)
gofmt -l <changed go files>
(cd apps/desktop && bun run typecheck && bunx vitest run)
bunx oxlint@1.19.0 --deny-warnings <changed ts/tsx>
bunx prettier@3.6.2 --check <changed ts/tsx/json>
(cd apps/desktop/src-tauri && cargo fmt --all -- --check && cargo clippy --locked --all-targets -- -D warnings && cargo test --locked --lib)
ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink \
  bash -c 'cd apps/desktop/src-tauri && cargo test --locked --lib'
git diff --check
```

Rust crates only need the full gate if the merge changes them; upstream v0.2.19
touches `apps/desktop/src-tauri` (Touch ID), so run it there.

## Rollback points

- During the merge: `git merge --abort`.
- After the local merge commit, before push: `git reset --hard 9619d5b`, only
  with user approval.

## Results

Items 1–2 were done by the main session before dispatch (merge in progress,
`MERGE_HEAD` = `752791a`); not re-verified here.

Conflicts: `git ls-files -u` empty, no conflict markers; all 12 files staged.

Deviation from design.md (D1): the Vertex branch applies the service rules
before `vertexauth.AccessToken`, not after, so a missing profile or bad rule
sends no request at all (AC7). `Authorization` and `X-Goog-User-Project` are
still set last in `probeVertex`, so the connection wins.

Go:

- `core`: `go build ./...`, `go vet ./...` clean; `go test -count=1 ./...` 5096
  passed in 50 packages.
- `convo`: vet clean, 167 passed. `contracts`: vet clean, 28 passed.
- `gofmt -l` on changed Go files prints nothing (after `gofmt -w`; the tree is
  CRLF via `core.autocrlf`).

Desktop:

- `bun run typecheck` (`tsc --noEmit`): exit 0.
- `bunx vitest run`: 169 files, 2130 passed, 1 skipped, 1 failed. The failure is
  `scripts/release-updates.test.ts` > "does not force Finder layout on headless
  macOS runners": the `\\\n` join regex does not match the CRLF checkout of
  `.github/workflows/macos-package.yml`. Neither file is changed by this merge;
  the same check passes against the LF blob from `HEAD`. Not fixed
  (pre-existing, Windows checkout only).
- Upstream `BatchModelTest.test.tsx`: three assertions adapted to the
  fork's 请求完成 / 完成 wording (marked `// Fork:`); 14/14 pass.
- Oxlint `--deny-warnings` and Prettier `--check` pass on the changed TS/TSX and
  the three locale JSON files.

Rust:

- `cargo fmt --all -- --check` passes in all three crates.
- `classifier-worker`, `privacy-worker`:
  `cargo clippy --locked --all-targets -- -D warnings` passes.
  `cargo test --locked` fails to link in both (`LNK2001 __std_max_8u` from
  `ort_sys` DirectML objects, local MSVC STL mismatch); `classifier-worker` is
  untouched by the merge.
- `apps/desktop/src-tauri`: clippy and tests not run. `tauri_build` fails:
  `binaries\astrlink-privacy-worker-x86_64-pc-windows-msvc.exe` does not exist
  (sidecars not staged; `make desktop-sidecar` needs the worker builds, and D:
  has 3.9 GB free). Unverified: the upstream Touch ID changes in this crate.

Mutations (each restored afterwards):

- v56 probe forced to `return true, nil`: convergence and partial_1/partial_2
  cases fail.
- Vertex `applyDiscoveryRules` call removed: "applied" fails (rule and profile
  headers missing), "missing profile" fails (1 request sent).
- Cancelled branch removed from the result tone: the cancelled case fails.

Greps: no added `Calcium-Ion` lines; `github.repository ==` only at the
pre-existing `docker.yml:93`; the only new outbound sender is
`vertexauth/tokens.go` (no AstrLink identity); no fork-changed defaults in new
upstream Rust tests.

`git diff --check` and `git diff --cached --check`: clean.
