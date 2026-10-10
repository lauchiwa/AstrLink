# Stable target and conflict-resolution research

## Scope and evidence conventions

Planning only. No merge, checkout, commit, task activation, product edit, or
user-database access was performed. Findings are persisted only in this task's
`research/` directory. Recommendations below are not executed changes.

- Active task resolved with `python3 ./.trellis/scripts/task.py current --source`:
  `.trellis/tasks/10-10-sync-upstream-latest`, status `planning`.
- `HEAD` is `7165c7465eec555490da5b1689fa10e0f2d022af`.
- User-selected target is **only** `v0.2.0`, resolving to
  `78c97c4065ec5ba2263d6d32f9f53355567ea585`.
- Merge base is `78cc0dc9fdd29d362fde3f5781bd7dcd2c0fd688`.
- `git rev-list --left-right --count HEAD...v0.2.0` returned `35 18`;
  `git diff --stat HEAD...v0.2.0` reported 162 changed files.
- Examined the caller-provided merge-tree object
  `59b02f52cc3cb4be8ed6ebfa956d026b03112fc8`; this is a conflict-containing tree,
  **not** a completed merge. Below, `merged:` anchors refer to this tree;
  `stable:` means `v0.2.0`; `HEAD:` means the pre-sync fork.
- `git merge-base --is-ancestor 5ceced517b34c57329eee075069f8fd46e22c71e
  v0.2.0` returned 1: the main-only cooldown fix is excluded.
- Read `AGENTS.md`, Trellis phase/package indexes, and
  `.trellis/spec/guides/upstream-sync-thinking-guide.md`. Preserve upstream
  behavior, adapt fork-owned seams, and revise outdated fork test premises rather
  than undoing upstream improvements.

## Important correction to the existing planning background

The stable-only commit is **not just a MiniMax test focus fix**. Its message
mentions that test, but `git show --stat 78c97c4` shows four files, 53 additions,
7 deletions:

- `stable:apps/desktop/src/components/BuiltinToolsEditor.test.tsx`: focus delay.
- `stable:core/contract/coding_plan.go:11-43`: Factory Droid model protocol mapping.
- `stable:core/contract/service.go:29,56,68,81-82,280`: Droid service kind and
  HTTP-only Responses selection.
- `stable:core/contract/subscription.go:46-55,72-73,96,114,185-199,411`:
  Factory Droid provider, capabilities, and device-code flow.

`git diff v0.2.0 upstream/main --stat` has 11 differing files, not merely the
MiniMax test and cooldown implementation. `git grep` for `factory_droid`,
`droid_subscription`, and the corresponding Go constants finds them only in
these three Go contract files across stable Core/contracts/desktop sources.
There is no corresponding stable frontend/schema/provider implementation in
that search. Treat this as an upstream stable cross-layer inconsistency to
investigate during validation, **not evidence of a tested failure**. Do not
silently substitute main, remove stable code, or complete an unrelated provider
feature during sync. If it causes an acceptance blocker, report the exact failure
and seek a scoped decision.

## Five textual conflicts

### Desktop package scripts

Anchor: `merged:apps/desktop/package.json:24-29`.

Keep fork `macos:notarize = bun scripts/macos-release.mjs verify`; also add
upstream `build:web = PUBLIC_ASTRLINK_EDITION=web bun run build`. The latter
is a new capability, not an alternative notarization policy. Keep both entries
and valid JSON commas. Do not replace the fork's release wrapper with
`notarize-macos.mjs`; that would bypass its explicit adhoc/signing mode handling.
No dependency change is implied by this conflict.

Checks: desktop typecheck/build, web build, and
`scripts/macos-release.test.ts` plus `scripts/release-updates.test.ts`. On
Windows, use a shell that supports the script's environment assignment rather
than changing upstream's command merely for shell preference.

### Rsbuild compile-time constants

Anchor: `merged:apps/desktop/rsbuild.config.ts:19-26`.

Union both definitions:

- `process.env.ASTRLINK_RELEASE_REPOSITORY` from `releaseRepository()`.
- `process.env.PUBLIC_ASTRLINK_EDITION` normalized to `web` or `desktop`.

Keep the fork import and upstream web-specific output behavior. Keeping only the
edition constant can silently send About/update metadata to the wrong release
repository; keeping only the repository constant can silently build desktop code
for the web edition. Do not replace the host-driven dev reload mechanism.

### Contract authorization classification

Anchors: `merged:contracts/validate.rb:327-346`;
`merged:core/internal/controlapi/handler.go:236` (network-address operator route).

Union operator-only reads for network addresses and all three fork identity
profile/capture paths. Preserve both rationale comments. All other method/role
checks remain upstream-shaped. The stable merge has **no textual conflict** in
`core/internal/controlapi/handler.go`; its console-session additions coexist with
fork identity wiring fields. This differs from the caller's earlier main-target
preflight.

Checks: `ruby contracts/validate.rb`, contracts Go tests, Core controlapi role,
identity profile, identity capture, and network-address tests. Observer/local
socket reads must still fail for fingerprints and capture consent; a validated
web console session may act as operator through upstream's context mechanism.

### Main process composition

Anchors: `merged:core/cmd/astrlink-core/main.go:24-28,164-323`;
`stable:core/cmd/astrlink-core/main.go:127-161`.

Use upstream's refactored main flow, not the old giant inline fork block. The
fork's entire substantive delta against the merge base is identity-capture
construction, two ingress dependency fields, one control dependency field, and
profile-aware model-discovery construction. Move those responsibilities to the
shared persistent-core construction seam; see
[Persistent core and identity lifecycle](persistent-core-identity.md).

Keeping only upstream main **without rewiring persistent.go** compiles past the
conflict but disables capture and breaks bound-profile forwarding/discovery.
Keeping only fork main loses server mode, shared cleanup, and network-listener
behavior. The new `NewInferenceHandler(address, networkExposed)` signature must
be retained.

### SQLite access-token writes

Anchor: `merged:core/internal/storage/sqlite/store.go:362-388`.

Upstream acquires a writer lock before counting tokens, via
`UPDATE local_access_tokens SET id = id WHERE 0`, then inserts and commits.
The fork retries a whole short configuration transaction after a failed
read-to-write upgrade. These solve the same race at different stages.

Recommended design: preserve upstream's write-before-count ordering. Retain the
fork's bounded transaction wrapper if desired by putting the upstream no-op
write **inside** the `retryConfigWrite` callback, before `insertAccessTokenTx`.
This preserves the upstream locking step and the fork's bounded error/commit
semantics without nested transactions or global connection pragmas. Keep broader
fork configuration-write helpers untouched. Validate this combination; it has
not been executed in research.

Evidence and constraints:

- `HEAD:core/internal/storage/sqlite/config_write.go:24-78`: at most 8 attempts,
  5-second budget, whole-attempt rollback, no retry after ambiguous commit or
  failed rollback, no network/publishing/caller mutation in callbacks.
- `stable:core/internal/storage/sqlite/store_test.go:482-532`:
  `TestAccessTokenCreateWaitsForAnotherWriter` exercises real ongoing writes;
  retain it, plus atomic limit/secret tests.
- `HEAD:core/internal/storage/sqlite/config_write_access_token_test.go:15-103`
  and `config_write_access_token_boundary_test.go:14-186` inject competing
  writes **after the count read**, and assert exactly two reads/retries. With
  write-first locking, those synchronous competitor writes cannot acquire the
  writer lock. These tests' premise is now obsolete and can stall until the
  busy timeout or fail; do not delete upstream's lock to satisfy them.
- Adapt the fork-only token tests to contention established before lock
  acquisition, concurrent last-slot/name winners, bounded cancellation,
  rollback/secret atomicity, and no replay after uncertain commit. Keep the
  existing read-upgrade tests for other configuration writes that still use
  read-first transactions. Prefer deterministic barriers with bounded waits.

## Schema, history, and scope gates

`git diff HEAD...v0.2.0 -- core/internal/storage/migrate` produced no output.
No new migration or renumbering is required for this sync; keep the fork's
existing profile migration/reconciliation files unchanged. Continue testing
known-history adoption and unknown-history rejection using disposable fixtures,
never user databases. No README file appeared in
`git diff --name-only HEAD...v0.2.0 -- '*README*'`.

Before an eventual authorized merge commit, require target ancestry, absence of
unmerged index entries/conflict markers, unchanged migration history where
expected, and `git diff --check`. Do not push, tag, release, or deploy.
