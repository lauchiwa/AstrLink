# Migration collision (v50) and config_sync interaction

<!-- markdownlint-configure-file
{ "MD013": { "tables": false, "code_blocks": false } }
-->

Refs: fork `HEAD` = `1be7583`, upstream `v0.2.10` = `e8b9600`, merge-base
`b3af635`, trial merge tree `bec8f2b5` (`T` below). Fork paths are in the
working tree. `T:` and `v0.2.10:` paths are blob paths read with `git show`.

## 1. The collision

| Version | fork `HEAD`                      | upstream `v0.2.10`               |
| ------- | -------------------------------- | -------------------------------- |
| 48      | request_first_answer_timing      | request_first_answer_timing      |
| 49      | privacy_token_kinds_audit_chunks | privacy_token_kinds_audit_chunks |
| 50      | **service_identity_profiles**    | **config_sync**                  |
| 51      | —                                | request_privacy_ignored          |
| 52      | —                                | privacy_stand_ins                |

- Fork v50: `core/internal/storage/migrate/defaults.go:876-887`. It uses

  ```sql
  CREATE TABLE IF NOT EXISTS service_identity_profiles (... service_id ... REFERENCES services(id) ON DELETE CASCADE ...)
  ```

  plus `CREATE INDEX IF NOT EXISTS idx_service_identity_profiles_service_id`.

- Upstream v50/51/52:
  `v0.2.10:core/internal/storage/migrate/defaults.go:893-1028`. In the trial
  tree `T:.../defaults.go` the conflict hunk is at lines 876-1042: fork v50 sits
  at 881, upstream v50 at 906, v51 at 1028, v52 at 1031.
- Upstream added these through `b3ade1c` (sync #71), `bbb40d2` (privacy
  ignored), and `6d8614d` (stand-ins).
- Keeping both v50 entries fails `validate()` with "versions must be strictly
  increasing" (`migrate.go:100-122`). A renumber is required.

This is the third collision. Per
`.trellis/spec/guides/upstream-sync-thinking-guide.md:106-125` and AGENTS.md
"Database migrations", v50 is a pushed fork version, but upstream keeps its
number and the fork step moves. A database that applied fork 50 then needs
reconciliation, and every other mismatch must stay fail-closed.

## 2. Target layout

```text
48 request_first_answer_timing        (upstream)
49 privacy_token_kinds_audit_chunks   (upstream)
50 config_sync                        (upstream, verbatim)
51 request_privacy_ignored            (upstream, verbatim)
52 privacy_stand_ins                  (upstream, verbatim)
53 service_identity_profiles          (fork; unchanged IF NOT EXISTS statements)
```

Update the comment above the fork step (`defaults.go:876-879`) to read
`48 -> 49 -> 50 -> 53`. Profiles must stay the **last** migration (see §5,
`identity_profile_store_test.go:37`).

## 3. Historical database shapes that must converge

| Shape                 | Recorded history                                 | Source                                 |
| --------------------- | ------------------------------------------------ | -------------------------------------- |
| fresh                 | none                                             | new install                            |
| upstream ≤49          | 1..49 upstream                                   | upstream ≤ v0.2.0 / fork pre-48        |
| upstream 50/51/52     | 1..50/51/52 upstream                             | upstream v0.2.x user switching to fork |
| fork_48               | 48 = service_identity_profiles                   | fork build, first numbering            |
| fork_49               | 48 upstream, 49 = service_identity_profiles      | fork build, second numbering           |
| **fork_50 (new)**     | 48, 49 upstream, 50 = service_identity_profiles  | current pushed fork `1be7583`          |
| fork_50_applied (new) | fork_50, but config_sync objects already present | defensive, mirrors `_applied`          |

No fork build ever recorded 51/52. A database with
`50 = service_identity_profiles` therefore always has nothing at 51 or 52. After
renaming 50, the normal loop applies 51, 52, and then 53. 53 is an
`IF NOT EXISTS` no-op that adopts the existing table and rows. A database that
records 50 = profiles **and** has rows at 51/52 cannot come from any known
build. It must not be reconciled. With the design below, 50 is renamed and
`verifyHistory` then rejects it only if 51/52 carry wrong names. Section 4.4
adds an explicit guard that fails closed instead.

## 4. Extending `reconcileIdentityProfileHistory`

Current code (`core/internal/storage/migrate/identity_profile_history.go`):

- `:21-50`: collects claimed upstream steps for 48/49 by `(version, name)`. It
  runs only if `50 = service_identity_profiles` exists in this core's list
  (`:31`), then loops `[]int64{48, 49}` ascending (`:40`).
- `:54-100` `adoptClaimedProfileVersion` reads the recorded name. It skips
  unless the name is `service_identity_profiles`, checks that the profile table
  has its 3 columns (`:72`), probes, applies the upstream statements if they are
  missing, then `UPDATE schema_migrations SET name` (`:89-97`).
- `:105-128` `claimedVersionApplied` holds one probe per version (48:
  `first_answer_ms` column, 49: `audit_chunks` table). Any other version returns
  `ErrMigrationHistory`.
- Gate in `migrate.go:148`: `if currentVersion >= 48`. Keep it, because fork_48
  is the lowest reconcilable shape.

### 4.1 Changes

1. Gate on the new fork version. Change `:31` to
   `migration.Version == 53 && migration.Name == "service_identity_profiles"`.
2. Claim 50. Add

   ```go
   case migration.Version == 50 && migration.Name == "config_sync": claimed[50] = migration
   ```

3. Ascending chain. Change the loop at `:40` to `[]int64{48, 49, 50}`. The
   ordering invariant is unchanged: a fork_49 database already handed 48 back,
   and a fork_50 database already handed back 48 and 49. Only one version can
   hold the profile name in a valid shape, and each step is independent because
   each probe checks only its own schema.
4. Probe for 50. Add `case 50` to `claimedVersionApplied`. config_sync creates
   27 objects (5 tables, 1 index, 21 triggers; counted from
   `v0.2.10:.../defaults.go:893-1014`). It is all-or-nothing only inside Up's
   transaction. A single-object probe like `config_change_log` would accept a
   half-applied out-of-band state and then skip the 26 missing objects. A blind
   replay would fail with "table already exists" on plain `CREATE`. Recommended
   probe:
   - Derive the expected object names from the claimed migration's own
     statements. Match `^CREATE (TABLE|INDEX|TRIGGER) (\w+)` with no
     `IF NOT EXISTS` in config_sync. Query
     `SELECT COUNT(*) FROM sqlite_master WHERE name IN (...)`.
   - `0` means not applied, so replay. `len(expected)` means applied, so only
     rename. Anything in between returns `ErrMigrationHistory` ("partially
     applied config_sync"). This fails closed instead of guessing.
   - This makes `claimedVersionApplied` take the `*Migration` (or the statement
     list). Optionally refit 48/49 to the same mechanism. Keep the existing 48
     column probe either way, since an `ALTER TABLE ADD COLUMN` cannot be found
     in `sqlite_master` by name.
   - Hard-coding the list (`config_change_log`, `sync_state`,
     `sync_key_material`, `sync_tombstone`, `sync_baseline`,
     `config_change_log_changed_idx`, plus 21 `config_change_*` triggers) also
     works but duplicates the upstream statements.
5. Replay safety. Every config_sync trigger targets tables that exist since ≤
   v49: `services`, `service_credentials`, `service_proxy_credentials`,
   `builtin_tool_credentials`, `policies`, `privacy_model_installations`, and
   `pricing_configs`, all in fork `defaults.go`. Replaying inside the
   reconciliation is safe. Upstream does not backfill `config_change_log` or
   seed `sync_state` in v50, so the reconciled database matches an upstream
   database that ran 49→50. The sync engine creates `sync_state` on demand.
6. The profile-table sanity check at `:72` stays. It still guards "50 = profiles
   but no profile table".
7. Update the doc comment at `:10-20` (three collisions: 48, 49, 50) and the
   comment in `defaults.go`.

### 4.2 Resulting fork_50 upgrade inside one Up transaction

1. `currentVersion = 50 ≤ latest 53`. The gate `≥ 48` holds.
2. Reconcile 48 and 49: names are upstream, so skip (`:68`).
3. Reconcile 50: the name is profiles and the table has its columns. Probe: 0
   objects, so apply config_sync, then rename 50 → `config_sync`.
4. `verifyHistory(…, 50)` passes for 1..50.
5. Apply 51, 52, and 53 (53 is IF NOT EXISTS, so profile rows survive).
6. Commit. Any later failure rolls back the rename and the replay, because both
   share Up's transaction (`migrate.go:126-197`).

### 4.3 Read-only path

`RequireCurrent` (`migrate.go:201-224`) does not reconcile. A fork_50 database
returns `ErrDatabaseOlder` (50 < 53) before `verifyHistory`, which the existing
test asserts (`identity_profile_history_test.go:149-152`). Expect the same for
fork_50.

### 4.4 Optional extra guard

Before renaming 50, if `schema_migrations` already has a row with
`version > 50`, return `ErrMigrationHistory`. No known build produces that
shape. It keeps reconciliation scoped to known collisions. Without it,
`verifyHistory` still rejects any non-upstream names at 51/52. The only residual
case would be "fork 50 + upstream 51/52", which no build produces.

### 4.5 Reverse direction (expected, not a bug)

A converged database records 53, so the old fork build (`1be7583`, latest 50)
and upstream v0.2.10 (latest 52) both refuse it with `ErrDatabaseNewer`
(`migrate.go:145-147`). That is correct behavior per the guide's "Admission
gates" section. An upstream v0.2.10 build opening an **unconverged** fork_50
database fails with `ErrMigrationHistory` ("migration 50 is recorded as
service_identity_profiles, want config_sync") and changes nothing.

Per AGENTS.md, tell the user that any local DB at fork 50 gets converged on
first start of the new build and that older builds will then refuse it. Do not
touch their DB.

## 5. Tests that depend on position or number

By-index slicing, judged against the new layout:

| Location (fork paths)                                                                                                                                                                               | Pattern                                       | Impact                                                                                                                                                                               |
| --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `migrate/identity_profile_history_test.go:239`                                                                                                                                                      | `claimed := profiles.Version - 1` for fork_49 | **Breaks.** After the move, `profiles.Version-1 = 52`, but fork_49 records the profile at 49. Look it up by name: fork_48→timing, fork_49→chunks, fork_50→config_sync.               |
| `sqlite/identity_profile_store_test.go:36-37`                                                                                                                                                       | `previous := all[:len(all)-1]`                | Still works only while profiles is the last migration. Prefer `migrationsBelow`-style by-name slicing (a helper in the sqlite package, since the migrate test helper is unexported). |
| `migrate/migrate_test.go:419`                                                                                                                                                                       | latest == last element                        | Fine.                                                                                                                                                                                |
| `migrate/migrate_test.go:382,530,593,644,702,800,891`, `audit_exposure_migration_test.go:21`, `service_order_test.go:19`, `sqlite/policy_store_test.go:33`, `sqlite/keyring_test.go:430-433` (`42`) | `migrations[:N]`, N ≤ 42                      | Below every collision. Fine.                                                                                                                                                         |
| `migrate/migrate_test.go:952-960`, `audit_layout_migration_test.go:26-34`, `migrate_test.go:1012-1050`                                                                                              | lookup by name                                | Fine.                                                                                                                                                                                |

No upstream test in `T` references `config_sync`, `request_privacy_ignored`, or
`privacy_stand_ins` by version. `sqlite/privacy_stand_ins_test.go:84` uses the
table only.

## 6. Test cases to add or extend (`identity_profile_history_test.go`)

Fixture builder (`identityHistoryDatabase`, `:59-136`): add the
`configSyncStep = "config_sync"` constant next to `:41-44`, and the new kinds:

- `upstream_sync` (seed through upstream 50), `upstream_stand_ins` (through 52).
  These cover an upstream user switching to the fork.
- `fork_50`: seed `migrationsBelow(timing)` + timing + chunks +
  `legacyAt(configSync.Version, false)`, and insert the legacy service and
  profile (`seeded = true`).
- `fork_50_applied`: like fork_50, then run the config_sync statements out of
  band (matching `:118-131`).
- `fork_50_partial`: like fork_50, then create only `config_change_log` out of
  band. This must be rejected.
- `fork_50_missing_table`: `legacyAt(50, true)` with no profile table. This must
  be rejected.
- `unknown_50`: 48 and 49 upstream, `50 = "unknown_fork_schema"`. This must be
  rejected.

Convergence test (`:141-191`): add the new convergent kinds to `kinds`. Extend
the step loop (`:170`) with `configSyncStep`, `"request_privacy_ignored"`, and
`"privacy_stand_ins"`. Assert:

- All 27 config_sync objects exist (`sqlite_master WHERE name IN (...)` = 27).
- `pragma_table_info('request_records')` has `privacy_ignored_json`, and the
  `privacy_stand_ins` table exists.
- The profile row survives for every fork\_\* kind (extend `:184`).
- The config_sync triggers work after reconciliation:
  `UPDATE services SET updated_at = updated_at WHERE id = 'service_legacy'` adds
  a `config_change_log` row with `entity_kind='service'`, and the profile row is
  still there.
- Idempotence: two `Up` runs (already in `:154-161`).

Rejection test (`:195-214`): add the new rejecting kinds. Assert
`ErrMigrationHistory`, and that the history row at the affected version keeps
its old name. For `fork_50_partial`, also assert that `sync_state` still does
not exist, so nothing was replayed.

Rollback test (`:216-249`): add `fork_50`, and replace `:239` with a name-based
map. Assert that history and profile are unchanged after the injected failure,
and that no config_sync tables exist (the replay rolled back).

Also check that `identity_profile_store_test.go:29-84` still passes with
profiles at 53 (the legacy reader still sees `ErrDatabaseNewer`, and the cascade
delete still clears profiles).

## 7. config_sync × `service_identity_profiles` interaction

Engine files in `T`: `core/internal/sync/{engine,local,merge}.go`,
`core/internal/storage/sqlite/sync_{apply,entities,store}.go`,
`core/internal/storage/sync_store.go`.

### 7.1 Services are upserted, not delete+reinsert

`T:core/internal/storage/sqlite/sync_apply.go:298-328` `syncWriteService` uses
`INSERT … ON CONFLICT(id) DO UPDATE SET document_json, updated_at`. A sync write
never deletes the service row, so `ON DELETE CASCADE` does not fire and profiles
survive. Unchanged documents are skipped (`:311-318`, `syncDocumentUnchanged`
`:348-370`). The order rewrite (`:85-155`) and credential rebind (`:162-214`)
are `UPDATE`s only. The DSN enables FKs
(`T:core/internal/storage/sqlite/store.go:288` `foreign_keys(1)`).

### 7.2 Synced deletions cascade (acceptable, but note it)

`T:core/internal/sync/local.go:316-338` `deleteEntities` → `sync_apply.go:54-60`
`DELETE FROM services WHERE id = ?`. When a merge decides a service is gone
(deleted on another device, tombstone), the FK cascade deletes the local
profiles too. That matches a local delete (`service_store.go:227`, covered by
`identity_profile_store_test.go:78-83`). Profiles are not in the package, so
they **cannot be recovered** from the remote. This is acceptable semantics,
since profiles are scoped to their service. Document it in the sync PRD/spec,
and optionally add a test: sync-delete of a service removes its profiles.

### 7.3 Profiles are not synced, and that is the right default

- `Kinds()` (`T:core/internal/sync/merge.go:48-57`) and `SyncEntities`
  (`T:sync_entities.go:20-41`) cover service, credentials, policy, and pricing
  only.
- The config_sync migration adds **no trigger** on `service_identity_profiles`,
  so creating, confirming, or discarding a profile does not schedule a sync. The
  upstream rationale (`v0.2.10:defaults.go:880-892`) keeps learned identities
  and other per-observation data out of sync, and a captured profile is exactly
  that kind of data.
- Recommendation: do not add profiles to the package in this sync. Adding a kind
  changes the package format and the scope groups (`local.go:33-50`), needs a
  new trigger migration, and an upstream-only peer would reject the unknown
  kind. That is out of scope for an upstream sync, and should be a separate task
  if wanted.

### 7.4 Dangling `identity_profile_id` references (real gap)

The fork adds `HTTPConnection.IdentityProfileID`
(`T:core/contract/service.go:109-111`) and `ModelRule.IdentityProfile`
(`T:core/contract/request_rules.go:160-162`) to the **service document**, which
_is_ synced.

- Profile IDs are random per device (`identity_<hex>`,
  `T:core/internal/controlapi/identity_profiles.go:236-237`,
  `T:core/internal/identitycapture/registry.go:246-251`). Sync IDs equal local
  IDs (`T:local.go:86`), so the service ID matches across devices but the
  profile it names does not exist on the peer.
- `decodeSyncedService` (`T:sync_apply.go:376-410`) clears credential refs and
  subscription status, but leaves profile refs alone. `Validate`
  (`service.go:140-144`) checks only the ID format. The save-time existence
  check (`T:core/internal/controlapi/service_identity_bindings.go:38-77`) is not
  on the sync path.
- Effect on the receiving device: `LoadIdentityProfileHeaders`
  (`T:core/internal/accountauth/api_identity.go:102-124`) fails closed with
  "load identity profile …: not found". Every inference request
  (`T:core/internal/ingress/request_rules.go:78-83`), connection test, and model
  discovery for that service errors. No AstrLink identity leaks, which satisfies
  the AGENTS.md forwarding rule, but the provider is unusable there. A later
  local edit of that service fails `invalid_identity_binding` until the user
  clears the profile. Clearing it updates the document, which fires
  `config_change_services_update` and propagates the cleared binding back, by
  last-writer-wins over the whole document (`merge.go:254`), to the device that
  owns the profile.
- Options:
  1. Accept and document (recommended for this sync). This keeps fail-closed
     behavior, needs no format change, and adds a test that a synced service
     with an unknown profile ID is stored and refuses to forward.
  2. Strip unresolved profile refs on sync write and exclude them in
     `normalizeStoredService`. This needs care: the peer must not then upload a
     stripped document that erases the origin's binding. It is a non-trivial
     merge-semantics change.
  3. Sync profiles as a kind (§7.3). This is a separate task.
- Mixed fleet: an upstream-only peer decodes fork service documents with
  `DisallowUnknownFields` (`sync_apply.go:380-385`). The unknown
  `identity_profile_id`/`identity_profile` fails as `ErrInvalidArgument`, which
  `writeEntity` logs and skips (`T:local.go:296-303`). The peer silently lacks
  that service. That is not a leak, but note it.

### 7.5 Change log noise

Editing a service's profile binding fires `config_change_services_update`. That
is desired, because the document changed. Profile CRUD and the capture registry
do not write `services` (grep found no `UPDATE services` in `identitycapture` or
`identity_profile_store.go`), so they never schedule a sync.

## 8. Checklist

- [ ] Resolve `defaults.go`: upstream 50/51/52 verbatim, profiles → 53, comment
      updated.
- [ ] `identity_profile_history.go`: gate 53, claim 50, loop `{48,49,50}`,
      all-or-nothing probe for 50, docs.
- [ ] Fix `identity_profile_history_test.go:239`, and add the fork_50 /
      upstream_sync / partial / unknown_50 fixtures plus assertions.
- [ ] Harden `identity_profile_store_test.go:37` against a non-last profile
      step.
- [ ] Decide §7.4 (recommend option 1 + test), and note §7.2 in the PRD.
- [ ] Tell the user: fork-50 local DBs converge on first start, after which
      older builds refuse them.
