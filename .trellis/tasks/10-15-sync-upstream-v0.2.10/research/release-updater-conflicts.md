# Release / updater conflicts (v0.2.10 sync)

Trial merge tree: `bec8f2b5` (`HEAD`=1be7583, `v0.2.10`=e8b9600, base=b3af635).
Hunk counts: release.yml 1, macos-package.yml 1, windows-package.yml 1,
release-updates.mjs 1, release-updates.test.ts 3, updates.rs 3, update-model.ts
1, About.test.tsx 1.

Line numbers below are in `git show bec8f2b5:<path>` unless prefixed `HEAD:` or
`v0.2.10:`.

Upstream commits involved:

- `cffc4f8` fix(desktop): check updates through the official mirror (updates.rs,
  About.tsx/test, locales).
- `3779884` stapler retry — only touches `notarize-macos.mjs` / its test; merged
  cleanly. Fork's `macos-release.mjs:106` calls `notarizeMacOS({...})` without
  `retryDelaysMs`, so it inherits the default `staplerRetryDelaysMs` for free.
  No action.
- `94e2bff` (and `abbf8ad`) dependabot action bumps: checkout v6.1.0→v7.0.1,
  upload-artifact v7.0.1→v7.0.2, download-artifact →v8.0.2, setup-go →v7.0.0,
  cache →v6.1.0.
- `5ade60a` macOS `arch` input; `6b3d721` release gated on tagged commit's CI
  run (`scripts/require-ci-success.sh`), CI checks removed from package jobs;
  new `release-macos-x86_64.yml` + `scripts/wait-for-release.sh`.

---

## 1. `.github/workflows/release.yml` (1 hunk, :65-70)

```text
<<<<<<< HEAD
      release_tag: ${{ inputs.release_tag || github.ref_name }}
=======
      release_tag: ${{ github.ref_name }}
      arch: arm64
>>>>>>> v0.2.10
```

- Fork: `workflow_dispatch` retry of an existing tag; every `release_tag:` uses
  `inputs.release_tag || github.ref_name`.
- Upstream (`5ade60a`): `macos-package.yml` now takes a required `arch` input;
  Release builds arm64 only, Intel moves to `release-macos-x86_64.yml`.
- Resolution: keep both — union.

  ```yaml
  release_tag: ${{ inputs.release_tag || github.ref_name }}
  arch: arm64
  ```

Auto-merged parts of the same file that need fork adaptation (not conflicts):

- `:27` — upstream's `if: github.repository == 'Calcium-Ion/AstrLink'` is
  already absent in the merge (fork's deletion won). Good; fork test
  `release-updates.test.ts` asserts it stays absent.
- `:52-57` new step "Require a passing CI run for the tagged commit" uses
  `SHA: ${{ github.sha }}`. On a tag push that is the tagged commit. On the
  fork's `workflow_dispatch` retry, `github.sha` is the head of the dispatch ref
  (usually `main`), **not** the tag being rebuilt → the gate would check the
  wrong commit (could pass for an untested tag or block a good one). Resolution
  (minimal additive): keep upstream's step and script; resolve the SHA from the
  checked-out tag in a preceding line, e.g.
  ```yaml
  run: SHA="$(git rev-parse HEAD)" scripts/require-ci-success.sh
  ```
  (checkout `:35-38` already pins `ref: refs/tags/<tag>`) and drop the `SHA:`
  env line, or keep `SHA: ${{ github.sha }}` and override only when
  `inputs.release_tag` is set. Annotate as a fork deviation (dispatch retry).
  Also requires fork CI (`ci.yml`, push on `branches: ["**"]`) to have run on
  that commit — true for commits pushed to fork `main`.
- `:52` permissions `actions: read` added by upstream — keep.

## 2. `.github/workflows/macos-package.yml` (1 hunk, :247-253)

```text
<<<<<<< HEAD
        if: always() && steps.signing.outputs.mode == 'developer-id'
        uses: actions/upload-artifact@043fb46d... # v7.0.1
=======
        if: always()
        uses: actions/upload-artifact@cf430e03... # v7.0.2
>>>>>>> v0.2.10
```

- Fork: ad-hoc macOS release (`macos-release.mjs prepare` sets
  `steps.signing.outputs.mode`); notarization diagnostics only exist in
  `developer-id` mode.
- Upstream (`94e2bff`): upload-artifact pin bump.
- Resolution: fork's `if:` + upstream's pin:

  ```yaml
  if: always() && steps.signing.outputs.mode == 'developer-id'
  uses: actions/upload-artifact@cf430e030ddbb5b0abf93d22962f4752f3646cd9 # v7.0.2
  ```

  (The fork `if:` is arguably unneeded since `if-no-files-found: ignore`, but it
  is an existing fork hunk; keeping it is cheaper than re-deciding now.)

Auto-merged, verified fine: `arch` input / `inputs.arch` everywhere (no
`matrix.` left), fork's checkout `ref:` (:54) on upstream's v7.0.1 pin, fork's
early `setup-bun` (:57) with upstream's removal of the later duplicate, upstream
removal of the "Check frontend and Core in parallel" step, `macos:notarize` step
(:190) unchanged (fork repoints it via `package.json:24` →
`macos-release.mjs verify`). Concurrency group now includes `inputs.arch` —
keep.

## 3. `.github/workflows/windows-package.yml` (1 hunk, :28-91)

- Fork (HEAD side, :29-88): the whole `checks` job, with the fork's only edit
  being the `ref:` line on its checkout (:40).
- Upstream (`6b3d721`): deletes the `checks` job (CI now runs once per branch
  push; release gates on it), replaces the comment with
  `# Only publish exposes an installer that passed its smoke test.`
  `check-no-production-models.mjs` moved into `package` (v0.2.10:112).
- Resolution: take upstream (delete `checks`). The fork's `ref:` line has
  nothing left to apply to. `publish` already `needs: package` (:230) only.

## Fork-owned workflow gates that must follow upstream

`HEAD:apps/desktop/scripts/release-updates.test.ts`:

- `:162-183` "binds every packaging workflow…": list is `release`,
  `macos-package`, `windows-package`, `linux-package`. Upstream added
  `release-macos-x86_64.yml`, which (v0.2.10:20) has
  `if: github.repository == 'Calcium-Ion/AstrLink'`, no
  `ASTRLINK_RELEASE_REPOSITORY`, no `MACOS_SIGNING_MODE`, no
  `workflow_dispatch`, and `release_tag: ${{ github.ref_name }}`. Without
  adaptation the fork **silently stops shipping Intel macOS** (validate job is
  skipped ⇒ no `darwin-x86_64` in `latest.json`). Required:
  - In `release-macos-x86_64.yml`: drop the repo `if:` (same as fork did for
    release.yml), add the top-level `env:` with
    `ASTRLINK_RELEASE_REPOSITORY: ${{ github.repository }}` and
    `MACOS_SIGNING_MODE: ${{ vars.MACOS_SIGNING_MODE }}` (validate step runs
    `release-updates.mjs preflight`, and `attach` runs `publishingRepository`
    logic — confirm `attach` reads it; macos-package.yml has its own env so the
    reusable call is covered).
  - Add `"release-macos-x86_64"` to the list at `:163-168`.
  - Decide whether to add `workflow_dispatch` retry to it too. If yes, mirror
    release.yml (`RELEASE_TAG`, two `ref: refs/tags/...` checkouts, SHA
    resolution) and `scripts/wait-for-release.sh` filters
    `event=push -f branch="$TAG" -f head_sha="$SHA"` on `release.yml` runs
    (`scripts/wait-for-release.sh:14-16`) — a dispatch-retried Release is not
    `event=push` and the script would error "No Release run exists" unless the
    release is already published (draft==false check comes first, so a retry
    after successful publish works). Minimum viable: no dispatch for x86 (keep
    upstream shape), document it.
- `:211-234` "retries an existing release tag…": counts
  `ref: refs/tags/${{ inputs.release_tag || github.ref_name }}` in release.yml
  `toHaveLength(2)` — still 2 after merge (validate :37, publish :95). For the
  package workflows, `windows-package` expects **2** matches; after upstream
  deletes `checks` there is **1** (only `:100`). Change to `toHaveLength(1)` for
  all three (remove the `windows-package ? 2 : 1` special case).
- `:185-209` Finder/notarize test: still satisfied (no
  `TAURI_BUNDLER_DMG_IGNORE_CI`, both
  `--config src-tauri/tauri.signing.conf.json` lines,
  `run: bun run macos:notarize`). If `release-macos-x86_64` is added to the bind
  list, nothing extra here since it reuses macos-package.yml.

## 4. `apps/desktop/scripts/release-updates.mjs` (1 hunk, :366-~470)

Upstream (`5ade60a`) refactored `collectRelease` into `verifiedPlatform()`,
`releaseAssets()`, `writeRelease()`; added `releaseTargets` (all but
`darwin-x86_64`), `extendRelease()`, `uploadAssets()`, `attachRelease()` and the
`attach` CLI command. Fork added `releaseRepository()`/`publishingRepository()`/
`macOSSigningMode()`, `repository` + `macos_signing_mode` fragment fields, their
checks in the per-platform loop, and the `adhocNotice` prefix on notes.

The textual hunk is the fork's old inline loop body + manifest/SHA256SUMS block
(HEAD side) vs upstream's new `releaseAssets`/`writeRelease`/`extendRelease`
(theirs side).

### Latent breakage the hunk hides (auto-merged, not marked)

The fork deleted the module constant `const repository = "Calcium-Ion/AstrLink"`
(HEAD:15 → `releaseRepository()`). Upstream's new code still references the bare
identifier, and git merged those lines cleanly:

- `:311` `verifiedPlatform` → `url: \`https://github.com/${repository}/…\``
- `:501`, `:503` `uploadAssets` → `--repo`, repository
- `:585-586` `attachRelease` → `env.GITHUB_REPOSITORY === repository` (and the
  old "project repository" message), `:591`, `:601` and the later
  `gh release download/upload` calls.

In ESM strict mode these throw `ReferenceError: repository is not defined` at
call time (`verifiedPlatform` is called by `collectRelease`, so **every**
publish breaks, not only attach). Lint (`oxlint no-undef`) / vitest will catch
it; the build will not. This is the "upstream code bypasses a fork indirection"
class from the sync guide.

### Resolution

Keep upstream's structure exactly; thread the fork values through it.

1. `verifiedPlatform(files, target, tag, version, publicKey, repository, macosSigningMode)`
   — add the two fork checks (signing-mode check for `darwin-*`,
   `fragment.repository === repository`) right after `readJSON(fragments[0])`,
   before upstream's version check (same order the fork used). URL keeps
   upstream's template using the parameter.
2. `collectRelease(…, now, repository = releaseRepository(), macosSigningMode = macOSSigningMode())`
   keeps the fork's normalisation lines (:353-354), loops `releaseTargets`
   (upstream), passes both to `verifiedPlatform`, then upstream's
   `releaseAssets` + `writeRelease`. Manifest becomes
   `{ version, notes: macosSigningMode === "adhoc" ? `${adhocNotice}\n\n${notes}`.trim() : notes, pub_date, platforms }`.
   Drop the HEAD-side inline `assets`/`mkdirSync`/`cpSync`/`SHA256SUMS` block
   (`writeRelease` replaces it).
3. `extendRelease(input, published, output, tag, target, publicKey, repository = releaseRepository(), macosSigningMode = macOSSigningMode())`
   with the same normalisation; pass both to `verifiedPlatform`. Do **not**
   re-prefix `adhocNotice`: `manifest.notes` comes from the published
   `latest.json` (already prefixed by `collectRelease`). Optional extra guard:
   refuse when `macosSigningMode === "adhoc"` and published notes lack the
   notice (mixed modes in one release).
4. `uploadAssets(repository, tag, assets)` — take repository as a parameter
   (upstream has no other callers); `publishRelease` passes its local
   `repository` (already from `publishingRepository(env)`, :516).
5. `attachRelease`: replace upstream's equality check with
   `const repository = publishingRepository(env);` (same substitution the fork
   made in `publishRelease`), and call
   `extendRelease(…, publicKey, repository, macOSSigningMode(env))`.
6. Keep upstream's `attach` CLI branch and error string unchanged.

Guard: add a fork test asserting `release-updates.mjs` source contains no
`"Calcium-Ion` literal outside `release-repository.json` defaults, or simpler:
call `attachRelease`/`collectRelease` paths in tests (they already exist for
collect) so a bare `repository` reference fails.

## 5. `apps/desktop/scripts/release-updates.test.ts` (3 hunks)

### 5a. imports (:18-27)

Union:
`collectRelease, extendRelease, macOSSigningMode, newestStable, publishingRepository, releaseRepository, releaseTargets, releaseVersion, …`.

### 5b. fixture (:461-489)

- HEAD: inline `platform()` writes fragments with
  `repository: releaseRepository()` and `macos_signing_mode` for darwin; stages
  `targets.slice(0, -1)`.
- Upstream: shared top-level `stagePlatform()` (:67-88) + `releaseTargets`.
- Resolution: take upstream's `stagePlatform` call and `releaseTargets` (fork's
  `targets.slice(0, -1)` would now be wrong: `releaseTargets` already excludes
  x86_64, the test removes the last one), and add the fork fields **inside
  `stagePlatform`** (`repository: releaseRepository()`,
  `...(target.startsWith("darwin-") ? { macos_signing_mode: macOSSigningMode() } : {})`).
  That also makes upstream's new "adds a late platform…" test (:634-697) pass
  the fork's new checks in `verifiedPlatform`.

### 5c. mode/repository assertions (:502-570)

- HEAD: adhoc/developer-id notes and mismatch loop, "missing-mode" check, then
  `expect(manifest.platforms[targets[0]].url)`.
- Upstream: `expect(Object.keys(manifest.platforms)).toEqual(releaseTargets)`
  and `releaseTargets[0]`.
- Resolution: keep fork block, but switch every `targets` in the fork block and
  in the auto-merged repository loop below (:484 slice, :503, :505 and :544
  darwin filters, :566, :574, :589, :607) to `releaseTargets`. The input dir now
  only contains `releaseTargets`; iterating `targets` would `readFileSync` a
  non-existent `darwin-x86_64/darwin-x86_64.json` (ENOENT). The "mixed" case
  uses `targets[1]` (= `darwin-x86_64`) at :552 → use `"darwin-aarch64"` (the
  only darwin target left in Release). Use upstream's `toEqual(releaseTargets)`
  and `releaseTargets[0]` for the shared assertions.
- New fork cases worth adding to upstream's late-platform test: `extendRelease`
  rejects a `darwin-x86_64` fragment with another `repository` ("Repository
  mismatch") and with the other `macos_signing_mode` ("signing mode mismatch");
  and with repository `Calcium-Ion/AstrLink` and `lauchiwa/AstrLink` the added
  URL contains that repository.

Auto-merged upstream round-trip test (:237-341) stages all four `targets` via
`stageUpdate` (fragments get the fork fields from env), collects
`releaseTargets`, then `extendRelease(... "darwin-x86_64" ...)` — passes once
`extendRelease` defaults repository/mode from env. No edit needed.

Workflow-gate edits in this file are listed under "Fork-owned workflow gates"
above (bind list + windows checkout count).

(sections for Rust / TS follow)
