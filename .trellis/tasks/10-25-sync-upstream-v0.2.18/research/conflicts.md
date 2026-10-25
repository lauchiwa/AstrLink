# v0.2.18 trial merge: conflicts and post-merge risks

Trial: `git merge --no-ff --no-commit v0.2.18` in a disposable worktree at
`e87d4e4`. Merge base `e8b9600` (= v0.2.10). Upstream: 93 commits, 446 files.
Fork since base: 45 commits, 341 files. Both touched: 36 files; 7 conflict.

## Text conflicts

| File                                                | Hunks | Resolution                                                                                                                                                                                                                                                                                                                           |
| --------------------------------------------------- | ----- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `.github/workflows/macos-package.yml`               | 2     | Keep both configs: upstream `--config src-tauri/binaries/tauri.onnxruntime.conf.json` plus fork `--config src-tauri/tauri.signing.conf.json`. Keep upstream's new comment and line wrapping; keep the fork's DMG Finder deviation comment (not in conflict).                                                                         |
| `apps/desktop/package.json`                         | 1     | Take upstream `desktop:dev/start/build` (onnxruntime config). Keep fork `macos:notarize = bun scripts/macos-release.mjs verify`.                                                                                                                                                                                                     |
| `apps/desktop/scripts/release-updates.mjs`          | 4     | Take upstream's injected `gh = runGh`, existing-release handling, `releaseComplete`, latest.json-last upload. Keep fork `publishingRepository(env)` instead of the bare `repository` check, `uploadAssets(repository, tag, assets, gh)`, and `collectRelease(..., new Date(), repository, macOSSigningMode(env))`. See D1 for notes. |
| `apps/desktop/scripts/release-updates.test.ts`      | 2     | Union of imports (`attachRelease` once). Keep both sides' test blocks; upstream's new publish tests need fork env (`ASTRLINK_RELEASE_REPOSITORY`, `MACOS_SIGNING_MODE`) and the expected `api repos/<repo>/releases` call, parameterized over both repositories.                                                                     |
| `apps/desktop/src-tauri/src/preferences.rs`         | 1     | Keep both: fork preview-default tests and upstream `autostart_hidden` test.                                                                                                                                                                                                                                                          |
| `apps/desktop/src/components/MultiFilterSelect.tsx` | 1     | Take upstream (`selectTriggerClassName` no longer carries Button's `font-medium`, so the fork's `font-normal` is redundant). Fork `contentClassName` auto-merged and stays.                                                                                                                                                          |
| `core/internal/storage/migrate/defaults.go`         | 1     | Upstream keeps 53 `session_cursor_lookup_order`, 54 `access_token_sync`, 55 `request_output_activity`. Fork profiles move 53 → 56.                                                                                                                                                                                                   |

## Workflow commands asserted by tests

`release-updates.test.ts` pins the macOS build/bundle command strings (fork
lines ~229–232). They must be updated to the combined `--config` form.

Tauri merges repeated `--config` as JSON merge patch. The onnxruntime config
writes `bundle.macOS.frameworks` and `bundle.resources`; the signing config
writes `bundle.macOS.signingIdentity`. Disjoint keys, so order does not matter.
Order chosen: onnxruntime first (upstream's line), signing appended.

## Migration collision (fourth time)

Fork v53 `service_identity_profiles` is pushed (`origin/main`). Upstream claims
53/54/55. Upstream's `access_token_sync_test.go` requires the step before 54 to
be `session_cursor_lookup_order`, confirming the fork must go after 55.

A database that applied fork 53 has: v1..52 upstream +
`53 service_identity_profiles`, nothing later. Reconcile 53 like 48/49/50: apply
upstream 53's statements, rename the row, then 54/55/56 run normally.

`session_cursor_lookup_order` is not CREATE-only (ALTER, UPDATE, DROP INDEX,
CREATE INDEX/TRIGGER), so `createdObjectsApplied` cannot probe it. A dedicated
probe is needed. Upstream 53 is all-or-nothing inside its transaction, so the
probe can check the CREATE objects it introduces (indexes + triggers): none →
replay, all → rename only, partial → fail closed. `ALTER ADD COLUMN` columns are
covered by the same transaction guarantee.

Older fork histories (profiles recorded at 48/49/50) reconcile through 48 → 49 →
50 as today, then must also hold 51/52, which a fork build recorded only if it
had them; the existing "profiles is the newest row" rule still applies per
version.

## Post-merge checks run on the trial tree

- `Calcium-Ion` in production paths (`apps/desktop/src`, `src-tauri/src`,
  `scripts`, `.github`, `core`; excluding tests/md): identical to HEAD. No new
  hardcoded upstream repository.
- `.github` upstream diff touches only `ci.yml` and `macos-package.yml`. No new
  workflow, no new `github.repository ==` gate (the `docker.yml` one predates
  this sync).
- New core outbound: `relaykitbridge/media.go` switches to a dedicated
  `mediaHTTPClient` (GET with `Referer` cleared, no AstrLink headers). New
  `X-AstrLink-Service` ingress header is consumed locally; the forwarder strips
  the `x-astrlink-` prefix at `transport/forwarder.go:282`. Needs a loopback
  assertion that it never reaches upstream (HTTP and WebSocket).
- No new upstream test lines reference `UpdateChannel::`, `official_mirror`, or
  `astrlink.com`.
- i18n: `en`, `zh-CN`, `ja` each have 3605 leaf keys after merge.
