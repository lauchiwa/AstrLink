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

- [ ] Quantify the conflict surface first:
      `git diff --name-only HEAD...upstream/main`
- [ ] Trial-merge in a disposable worktree, never by stashing the working tree
- [ ] Check for **schema migration version collisions** — the highest-risk class

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
