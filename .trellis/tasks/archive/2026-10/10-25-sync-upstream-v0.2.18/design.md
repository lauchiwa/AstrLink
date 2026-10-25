# Design: sync upstream v0.2.18

## Merge shape

One `git merge --no-ff 2cba3d6` on `main`, conflicts resolved in the merge
commit. Adaptations that are not conflict resolutions (migration renumber,
reconciliation, tests) go in the same merge commit only if needed to build;
otherwise in follow-up commits on `main` so the merge stays reviewable.

## Migrations

Order after merge: upstream 1..55 unchanged, then fork
`{Version: 56, Name: "service_identity_profiles"}` with
`CREATE TABLE IF NOT EXISTS` (unchanged statements).

`reconcileIdentityProfileHistory` gains version 53:

- `claimed[53]` = the migration with
  `Version == 53 && Name == "session_cursor_lookup_order"`; the profiles marker
  becomes `Version == 56`.
- Loop over `48, 49, 50, 53` ascending. Each version keeps the existing rule:
  act only when that row's name is `service_identity_profiles` and no later row
  exists; require the 3-column profile table; probe; replay if absent; rename.
- `claimedVersionApplied` case 53: probe the objects upstream 53 creates
  (`request_record_cursors_latest_idx`,
  `request_record_cursors_principal_latest_idx`, the four
  `request_records_*_latest_idx`, triggers `request_cursor_fill_order`,
  `request_cursor_update_order`). Derive the names from the step's own
  statements by matching only `CREATE (INDEX|TRIGGER) name`, skipping ALTER /
  UPDATE / DROP, and fail closed if the derived set is empty. None present →
  replay; all present → rename only; partial → `ErrMigrationHistory`.

Why a new probe rather than `createdObjectsApplied`: that helper rejects any
non-CREATE statement on purpose so config_sync's probe cannot silently lose a
statement. Keep that guarantee; add a sibling that states which statement kinds
it skips and why (they cannot leave a partial state outside the transaction).

Update the file's header comment: 48 → 49 → 50 → 53 → 56, and the v53 comment in
`defaults.go`.

## Release notes (D1)

`collectRelease` prepends `adhocNotice` only when `notes` does not already
contain its heading line. `publishRelease`:

- `written` = pre-existing non-empty body (upstream).
- `notes` = `written ?? generated` (upstream).
- manifest = `collectRelease(..., notes, ...)`; notice added if missing.
- `writeFileSync(notesFile, manifest.notes)` (fork: the file carries the
  notice).
- Final `release edit` passes `--notes-file` when `!written` (upstream) **or**
  `manifest.notes !== written` (fork: notice was prepended). Annotate as a fork
  deviation.

Repository and signing mode keep flowing through `publishingRepository(env)` and
`macOSSigningMode(env)`; `uploadAssets(repository, tag, assets, gh)`.

## macOS packaging

`--config src-tauri/binaries/tauri.onnxruntime.conf.json --config src-tauri/tauri.signing.conf.json`
in both build and bundle steps; disjoint merge-patch keys (see research).
`desktop:build` locally takes upstream's line only (local builds are unsigned;
the signing config is CI-generated).

## Forwarding identity

No production change expected. Add/extend a loopback test that sends
`X-AstrLink-Service` through ingress → forwarder and asserts the stub's received
headers, for HTTP and the Responses WebSocket handshake.

## Rollback

Before commit: `git merge --abort`. After local commit, before push:
`git reset --hard e87d4e4` only with user approval.
