# Upstream Sync Thinking Guide

> **Purpose**: Keep this fork cheap to sync. Every hunk that diverges from
> upstream is a hunk that re-conflicts on the next sync.

This repository is a fork of `Calcium-Ion/AstrLink` and syncs from it
continuously. Add the remote as `upstream` and only `fetch` it:

```bash
git remote add upstream https://github.com/Calcium-Ion/AstrLink.git
```

---

## The Rule

**When a local change diverges from upstream, preserve upstream's functional
logic and adapt the fork's feature around it.**

The fork's convenience is worth less than a merge that stays reviewable.

---

## Five Checks Before Resolving a Conflict

### 1. Is upstream's logic preserved?

Keep upstream's step shape, command form, default values, and data-structure
semantics. Do not rewrite them to match local preference. Two sides adding a
field to the same struct is not a conflict of intent — merge both.

### 2. Does the adaptation live in a fork-owned file?

Find out who owns the file before editing it:

```bash
git cat-file -e upstream/main:<path> && echo upstream || echo "fork-only"
```

Put the fork's capability in fork-owned files. Touch upstream files with the
smallest additive change that works. Prefer repointing an indirection the fork
already owns over editing upstream's implementation.

### 3. Are you deleting an upstream step? (last resort)

A deleted hunk re-conflicts on every sync, forever. Even when the fork's
replacement is functionally equivalent, keeping upstream's step and adapting
underneath costs less over time.

### 4. If upstream's logic cannot be kept, is the reason written down?

Sometimes upstream's choice regresses the fork's feature or CI. Then adaptation
wins — but annotate it where it lives, stating that it is a deliberate deviation
and why. An unexplained deviation gets "fixed" back by the next person syncing,
including a future session of yours.

### 5. Does a fork-owned gate constrain upstream's evolution?

A fork-owned test or admission gate must adapt to upstream, never the reverse. A
gate that fails because upstream legitimately advanced is a gate whose premise
expired, not an upstream regression.

---

## Quick Reference: Sync Triggers

### Before merging

- [ ] Confirm `upstream/main` is current. A `git fetch` can time out, and in a
      pipeline (`git fetch … | tail -1`) `$?` reports the **last** command, so a
      failed fetch reads as success and every later comparison silently uses a
      stale ref. Check the fetch's own exit code, then verify the ref matches
      upstream's newest release:
      `gh api repos/Calcium-Ion/AstrLink/commits/<tag> --jq .sha`.
- [ ] Quantify the conflict surface first:
      `git diff --name-only HEAD...upstream/main`
- [ ] Trial-merge in a disposable worktree, never by stashing the working tree
- [ ] Check for **schema migration version collisions** — the highest-risk class

### After merging: upstream code that bypasses a fork indirection

Where the fork replaced an upstream constant with an indirection (release
repository, endpoint, path prefix), upstream's **new** code still hardcodes the
original value. Git merges it cleanly because the fork never edited those lines,
so the build succeeds and the defect only shows at runtime.

- [ ] Grep the merged tree for the values the indirection replaced, e.g.
      `rg -n 'Calcium-Ion' apps/desktop/src apps/desktop/src-tauri/src apps/desktop/scripts`.
      Production code should hold none; test occurrences belong in cases that
      assert both repositories.
- [ ] Adapt through the pattern the fork already uses — add a `*_for_repository`
      parameterized form and keep the no-argument wrapper — rather than
      inventing a second mechanism.
- [ ] Assert the derived value against the build's own configuration
      (`assert_eq!(LATEST_MANIFEST, format!("…/{REPOSITORY}/…"))`). Verify the
      assertion fails when the constant is hardcoded again; otherwise it proves
      nothing.
- [ ] Run the fork's own tests under upstream's configuration too
      (`ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink cargo test --lib`), so
      the adaptation does not assume the fork's value.

Found in the v0.1.9 sync: upstream's new `manifest_tag` kept
`/Calcium-Ion/AstrLink/releases/download/`, which would have rejected the fork's
own `latest.json` once the fork published a stable release.

### Schema migrations

The fork and upstream both append migrations, so they collide on version
numbers. This has already happened twice for `service_identity_profiles` (48 →
49 → 50).

- [ ] Upstream keeps its number. The fork's migration moves up.
- [ ] A database that already applied the fork's old number needs
      reconciliation: apply the schema that version is now supposed to carry,
      then rename the recorded history entry.
- [ ] Reconcile only known collisions. All other history mismatches must still
      fail closed.
- [ ] Probe whether the reclaimed schema is already present instead of replaying
      statements blindly.
- [ ] Use `CREATE TABLE IF NOT EXISTS` in the moved migration so it adopts a
      table created under the earlier numbering.
- [ ] Look migrations up **by name** in tests. Index-based fixtures
      (`migrations[48]`) silently seed a different migration after a renumber.

See `core/internal/storage/migrate/identity_profile_history.go`.

### Admission gates that pin a baseline commit

- [ ] A gate comparing against a pinned commit assumes that commit's migration
      set equals the current one. Any sync breaks that assumption.
- [ ] Isolate what the gate actually proves: compare the fork's footprint
      against the **current** build before the fork's feature is seeded, not
      against an older binary's report.
- [ ] Across a schema bump, an older reader refusing the database
      (`ErrDatabaseNewer`) is correct behavior. Assert the graceful refusal.
- [ ] Never pin the baseline to the implementation under test; the gate goes
      vacuous.

### Release workflows

- [ ] Fork-added capabilities (signing policy, ad-hoc releases) belong in
      fork-owned scripts, reached through an indirection upstream already calls.
- [ ] Do not gate a security-relevant step with a YAML `if:` on an env var that
      may be empty — resolve the mode in code so it fails closed.

---

## Verification

A resolution is finished when the diff against upstream is explainable line by
line:

```bash
git diff upstream/main -- <path>
```

Every remaining difference should be either a fork feature or an annotated
deviation. Anything you cannot explain is an accident.

---

## Environment Blockers Are Not Code Defects

A sync is verifiable only to the extent the local toolchain allows. Separate
"upstream broke something" from "this machine cannot run the check", and never
mark an unrun check as passing.

### Prove the toolchain works before blaming the code

`cargo clippy` failing in `apps/desktop/src-tauri` says nothing on its own: the
crate's `build.rs` calls `tauri_build::try_build`, which validates every
`tauri.conf.json` `externalBin` path and panics when a sidecar binary is absent.
There is no documented skip flag, and `ASTRLINK_REUSE_WINDOWS_WORKERS=1`
(`apps/desktop/scripts/build-sidecar.mjs`) needs a populated CI worker cache, so
it does not help on a machine that has never built one.

Run the same check on `apps/privacy-worker` and `apps/classifier-worker` first.
If Clippy passes there, the Rust toolchain is sound end to end and the desktop
failure is a missing build prerequisite, not a type error. Record it that way.

Do not manufacture a path around the gate: a placeholder file in `binaries/`
leaves a fake executable behind and hides the real problem, and an unverified
binary pulled from a release is worse.

### Check disk and linker facts before rewriting code

Two failures in this family look like compiler errors and are not:

- `failed to build archive ...: 磁盘空间不足 (os error 112)` means the volume
  holding `target/` is full. Confirm with `df -h` before touching source. Go
  caches may sit on another volume (`GOCACHE`, `GOMODCACHE`), so Go tests can
  keep passing while Rust builds fail. Deleting a multi-gigabyte `target/` is
  the user's call; it forces a long rebuild.
- `LNK1120: unresolved external` naming `__std_*` symbols (`__std_find_end_1`,
  `__std_mismatch_8`, `__std_min_element_f`, ...) from `libort_sys` is an MSVC
  STL toolset mismatch: the prebuilt ONNX Runtime static library was compiled
  against vectorized STL helpers newer than the installed toolset. Report the
  installed version as a fact; do not assert a required minimum the repository
  never documents. Symbol spelunking with `strings` or `dumpbin /SYMBOLS` on
  `libcpmt.lib` produced zero hits even for control symbols, so it proves
  nothing.

### Verify a tool exists, not just its launcher

On a rustup-managed install, `cargo-fmt` and `cargo-clippy` shims can resolve on
`PATH` while the components are absent. Confirm with `cargo fmt --version` and
`cargo clippy --version` rather than `command -v`, and install with
`rustup component add rustfmt clippy` before concluding a format gate is
unrunnable.

### `git status` noise on Windows

With `core.autocrlf=true`, `git status --porcelain` can list many files as `M`
while `git diff` is empty. Verify with `git diff --name-only` and by comparing
`git ls-files -s` against `git hash-object` per file; equal hashes mean zero
content change and the entries are stat-cache artifacts. Do not "fix" them with
a renormalizing commit during a merge.
