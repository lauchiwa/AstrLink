# Design: sync upstream v0.2.19

## Merge shape

One `git merge --no-ff --no-commit v0.2.19` on `main` (`9619d5b`). Conflict
resolutions plus whatever the tree needs to build go in the merge commit. The
migration renumber and reconciliation must be in the merge commit too: without
them two migrations share version 56 and Core refuses to start. D1 and extra
tests may follow as separate commits on `main` so the merge stays reviewable.

Per-file resolutions are in `research/conflicts.md`.

## Migrations

Order after merge: upstream 1..56 unchanged, then fork
`{Version: 57, Name: "service_identity_profiles"}` with the same
`CREATE TABLE IF NOT EXISTS` / `CREATE INDEX IF NOT EXISTS` statements.

`reconcileIdentityProfileHistory`:

- `claimed[56]` =
  `Version == 56 && Name == "install_defaults_biometric_subagent_links"`; the
  profiles marker becomes `Version == 57`.
- Loop `48, 49, 50, 53, 56` ascending. `adoptClaimedProfileVersion` is
  unchanged: act only when that row is `service_identity_profiles` with no later
  row, require the 3-column profile table, probe, replay if absent, rename.
- `claimedVersionApplied` case 56 gets a dedicated probe. Upstream 56 cannot use
  `createdObjectsApplied`: `config_change_policies_update` exists before and
  after the step, `raw_key_envelopes_new` is renamed away inside it, and the
  rest are `UPDATE` / `ALTER TABLE`. Probe the three end states it leaves:
  1. `request_records.parent_session_id` column (`pragma_table_info`);
  2. `request_records_parent_session_idx` index (`sqlite_master`);
  3. `raw_key_envelopes` table SQL containing `'biometric'`
     (`sqlite_master.sql`).

  0 of 3 → replay, 3 of 3 → rename only, otherwise `ErrMigrationHistory`
  (partial). The step runs in one transaction, so a normal build cannot leave a
  partial state; partial means out-of-band edits and must fail closed.

- Replay safety on a fork-56 database: the fork never changed the privacy policy
  seed (only +13 lines in `defaults.go` since v0.2.18), so upstream's guarded
  `UPDATE`s match exactly as they would on an upstream database. The replay runs
  inside `Up`'s transaction, same as a normal application of step 56.
- Update the header comment (48 → 49 → 50 → 53 → 56 → 57, five collisions) and
  the comment above the fork step in `defaults.go`.

Tests in `identity_profile_history_test.go`, all looking migrations up by name:
fresh database; fork-56 history replay (then compare v56 end state with a fresh
database: column, index, CHECK accepts `biometric`, policy trigger present);
fork-56 with upstream 56 already present (rename only); partial (1 or 2 of 3)
fails closed; fork-56 followed by a later row is rejected; the existing
48/49/50/53 cases keep passing with the profiles step at 57.

## D1: Vertex catalog probe applies fork rules

Today (upstream) the Vertex branch in `probeHTTP` returns
`prober.probeVertex(ctx, connection, token)` before the fork's
`requestrewrite.Compile` / `plan.Apply`.

Change:

- Move the fork's service-tier rule application into a fork-owned helper in a
  new `core/internal/servicemodel/fork_request_rules.go`:
  `func (prober *Prober) applyDiscoveryRules(ctx, serviceID, connection, auth, headers http.Header) error`
  = `Compile` → `DecideDefault` → `LoadIdentityProfileHeaders` if bound →
  `plan.Apply`. Errors wrap `ErrConfiguration`, as today.
- Non-Vertex path in `prober.go` calls the helper instead of the inline block
  (behavior unchanged).
- Vertex branch: after the token is obtained, build
  `headers := make(http.Header)`, call the helper with the protocol-effective
  `auth`, then pass `headers` to `probeVertex`.
- `vertex.go`: `probeVertex` takes a `headers http.Header` argument and sets
  `Authorization` and `X-Goog-User-Project` on it instead of on a fresh map.
  Smallest upstream edit; annotated as a fork deviation. Setting after the rules
  matches inference, where `VertexRequest` sets `X-Goog-User-Project` after the
  overlay.

Why not keep the logic in `vertex.go`: the rule code would then live in two
upstream files; one fork-owned helper keeps both call sites one line each.

## Forwarding identity

No production change expected. Upstream already strips
`X-AstrLink-Vertex-Project` in `VertexRequest` and the forwarder strips all
gateway headers afterwards. Add a loopback assertion that the Vertex stub
receives no `X-Astrlink-*` header when the fork overlay also carries one, if
upstream's test does not already cover the fork's headers.

## Rollback

Before commit: `git merge --abort`. After a local commit, before push:
`git reset --hard 9619d5b` only with user approval.
