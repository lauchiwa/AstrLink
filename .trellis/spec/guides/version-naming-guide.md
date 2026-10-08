# Version Naming Guide

> **Purpose**: Make every fork release say which upstream version it came from
> and how many times we have repackaged on top of it, without breaking the
> desktop updater.

This repository is a fork of `Calcium-Ion/AstrLink`. Releases are published from
`lauchiwa/AstrLink` and consumed by the desktop updater in
`apps/desktop/src-tauri/src/updates.rs`.

---

## The Scheme

```text
X.Y.Z-rc.N
```

- `X.Y.Z` is **the upstream version we synced from**, copied verbatim.
- `N` is our revision counter on top of that upstream version, starting at `0`.
- `rc.0` means "sync finished, no fork changes yet".

```text
synced upstream 0.1.9        v0.1.9-rc.0
changed fork logic           v0.1.9-rc.1
changed again                v0.1.9-rc.2
tenth revision               v0.1.9-rc.10
upstream ships 0.1.10        v0.1.10-rc.0
```

Find the upstream version before tagging. An empty
`git ls-remote --tags upstream` is **not** evidence that upstream has no tags:

```bash
gh release list --repo Calcium-Ion/AstrLink
```

---

## Hard Rule: Never Publish a Plain `X.Y.Z`

SemVer ranks a prerelease **below** the same numeric release. Publishing a plain
`0.1.9` installs a ceiling that every later `0.1.9-rc.N` sorts underneath, and
the updater pins users to that ceiling forever:

```text
0.1.9-rc.1 vs 0.1.9 → Less
0.1.9-rc.9 vs 0.1.9 → Less
select_release([0.1.9, 0.1.9-rc.1, 0.1.9-rc.2]) → 0.1.9 on both channels
```

Skip the plain version and the ceiling never exists:

```text
0.1.9-rc.0 → rc.1 → rc.2 → rc.10 → 0.1.10-rc.0 → 0.1.10-rc.1
monotonically increasing = true
0.1.10-rc.0 vs 0.1.9-rc.99 → Greater
```

Crossing upstream versions is safe: when the numeric triple differs, the
prerelease part does not take part in the comparison.

If the fork ever wants a "real" release, raise the base (upstream `1.0.0` → our
`1.0.0-rc.N`). **Do not** publish a bare `1.0.0`.

---

## Keep the Counter Purely Numeric

SemVer compares all-digit prerelease identifiers numerically and everything else
as ASCII. `rc.10 > rc.2` holds; `rc.10a` would sort as text and break past nine.

---

## Rejected Alternatives

Each was measured, not assumed. They are recorded so nobody re-runs the
experiment.

**Four parts, `0.1.9.0`** — the `semver` crate rejects it, and `cargo metadata`
exits 101 with `unexpected character '.' after patch version number`. The
version is compiled in through Cargo, so the app cannot even build.

**`0.1.9+lauchiwa.N`** — build metadata is excluded from SemVer precedence.
`cmp_precedence` returns `Equal`, so the updater never sees a new version.

**`0.1.10-rc.N`** — works, but the base is the patch _below_ the number you
read. Its only gain would be honest ordering against upstream's own tags, and
the client reads releases from this fork's repository only.

**Plain `0.1.9`, then `0.1.9-rc.N`** — inverted; see the hard rule above.

### What `new-api` does, and what is borrowable

`Calcium-Ion/new-api` publishes `v1.0.0-rc.42` as Latest and has never released
`v1.0.0`. **That shape — never publish the base, count in the suffix — is the
part we borrow.**

Its other habits are not borrowable: it carries 19 four-part tags, and it marks
`v1.0.0-rc.1` as `prerelease=false` while `v1.0.0-alpha.1` is `prerelease=true`.
That only works because new-api is a Docker-deployed server with no
SemVer-comparing updater. Ours compares with `cmp_precedence`.

---

## Consequence: The Default Channel Is Preview

Every fork release carries `-rc.N`, and the stable channel drops prereleases:

```rust
// updates.rs
if channel == UpdateChannel::Stable && (r.prerelease || !v.pre.is_empty()) {
    return None;
}
```

The two conditions are OR'd, so flipping GitHub's `prerelease` flag to false
would not expose an `-rc.N` build to the stable channel. `UpdateChannel`
therefore defaults to `Preview`; a stable default would leave a new install with
`no_releases` forever.

- **Do not relax that filter** to make stable accept `-rc.N`. It would empty out
  the channel concept and leave nowhere to put a genuine preview build.
- Keep `defaultUpdatePreferences()` in `apps/desktop/src/update-model.ts`
  aligned with the Rust default.
- Serde substitutes a default only for an **absent** field, so an operator who
  explicitly chose stable keeps it. An install whose file already recorded
  `"channel": "stable"` without a deliberate choice is indistinguishable from a
  deliberate one and must switch manually. Do not guess intent and overwrite it.

---

## Replacing a Mis-Tagged Release

A tag with the wrong base does not need deleting. A correctly based tag outranks
it:

```text
0.1.8-lauchiwa.1 < .2 < .3 < .4 < 0.1.9-rc.0 < 0.1.9-rc.1
select_release → 0.1.9-rc.1
```

Leave the stale tag unused and publish the correct one. Deleting a pushed tag or
a published release is destructive and can confuse clients that already saw it.
