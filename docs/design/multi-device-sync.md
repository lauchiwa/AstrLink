# Multi-device configuration: audit and detailed design

<!-- markdownlint-configure-file { "MD013": { "tables": false } } -->

## Status and decision

This is a design deliverable, not an implemented feature or a security
certification. The original conceptual proposal does **not** pass an
unconditional audit. The corrections below close its architectural omissions.
Implementation may proceed through the dependency gates in the task ledger;
production rollout requires the protocol, dependency, independent review, and
real-device gates. No existing user database, credential, encryption key, or
cloud account was read or changed for this audit.

The target is local-first, end-to-end encrypted configuration management. Each
device continues to run its own gateway. The coordination service never forwards
inference traffic. Synchronization is explicitly opt-in.

## 1. Evidence and reuse boundaries

| Existing source                                         | Verified behavior                                                                                     | Consequence                                                                  |
| ------------------------------------------------------- | ----------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------- |
| `core/internal/controlapi/roles.go`                     | Observer reads and operator-controlled mutation; operator token belongs to the desktop shell          | Sync export, approval, and apply must remain operator-only                   |
| `core/internal/controlapi/services.go`                  | Credential input is separate; configured compatibility values are operator-sensitive                  | Do not expose decrypted sync secrets in observer endpoints or UI events      |
| `core/internal/storage/sqlite/service_store.go`         | Service and its credential update share a transaction; existing operations each own their transaction | A multi-resource apply cannot loop over existing HTTP mutation endpoints     |
| `core/internal/storage/sqlite/secrets.go`               | `dek_secrets` seals secret columns with table/primary-key AAD                                         | Re-seal imported secrets locally; never copy source ciphertext or local keys |
| `core/internal/secretstore/secretstore.go`              | Storage-neutral opaque credential boundary                                                            | Read selected secrets internally, wipe buffers, refuse unreadable secrets    |
| `core/internal/storage/identity_profile_store.go`       | Service-scoped immutable snapshots; creation is unconfirmed; confirmation is exact-ETag consent       | Source confirmation is provenance, not target consent                        |
| `core/internal/controlapi/service_identity_bindings.go` | Bound profiles are validated against service and authentication                                       | Apply must validate remapped bindings before enabling a provider             |
| `core/contract/request_rules.go`                        | Header ownership is enforced; nonempty body rewrites are not supported                                | Preserve these restrictions; sync cannot introduce arbitrary rewrites        |
| `core/internal/controlapi/routing_settings.go`          | Routing PATCH uses a process mutex, without a multi-resource ETag                                     | Add a configuration generation covering all participating write paths        |
| `apps/desktop/src-tauri/src/service_identity.rs`        | Native bridge accepts closed operations, not arbitrary URLs/methods                                   | Reuse this pattern for sync commands                                         |
| `apps/desktop/src-tauri/src/kek_store.rs`               | Ad-hoc/dev builds use the local file-key path, not the signed-build Keychain flow                     | Do not promise hardware-backed sync storage or depend on Developer ID        |
| `apps/desktop/src/components/ConfirmDialog.tsx`         | Existing in-app confirmation primitive                                                                | Never use JavaScript confirmation dialogs                                    |
| `apps/desktop/src/components/ui`                        | Shared controls and panel/tab primitives                                                              | Extend shared components before adding bespoke UI                            |

No complete multi-device configuration synchronization subsystem was found.
Provider link import and client configuration integration do not supply a bulk,
transactional, encrypted synchronization protocol.

New modules are proposed, not existing:

- `sync/protocol`: independent Go module for bounded wire types, codecs,
  cryptographic envelopes, certificates, and proof verification.
- `sync/server`: independent Go module for account authentication, opaque object
  storage, prepared-head CAS, and APIs. It must not import inference or secret
  storage implementations.
- `sync/witness`: independent Go module for durable checkpoint witnesses.
- `core/internal/configsync`: client state machine, projection, merge, trust,
  previews, and orchestration.
- `core/internal/storage/sqlite/config_sync*`: local mappings, encrypted vault,
  inbox/outbox, generations, and transactional materialization.
- `apps/desktop/src-tauri/src/config_sync.rs`: closed native bridge.
- `apps/desktop/src/ConfigSync*`: UI composition using existing shared
  components.

## 2. Audit findings

P0 blocks the original design. P1 requires an explicit invariant or verification
before enabling the affected capability.

| ID  | Severity | Original omission                                                                | Required correction                                                                                           |
| --- | -------- | -------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------- |
| A01 | P0       | Account login could be confused with decryption authorization                    | Separate service account authentication, device enrollment, and cryptographic grants                          |
| A02 | P0       | Encrypted data can still be replayed, withheld, or forked                        | Signed revisions, anchored membership, persistent high-water marks, and strict checkpoint certificates        |
| A03 | P0       | Revocation might be presented as erasing copied API keys                         | Revoke future access, rotate recipients/keys, and disclose upstream credential rotation requirements          |
| A04 | P0       | Base URL changes can send an existing key to an attacker                         | Bind credentials to locally approved origins and gate sensitive changes before activation                     |
| A05 | P0       | Arbitrary configured header values can contain undisclosed secrets               | Treat every arbitrary literal compatibility value as sensitive; give it a separate encrypted object and grant |
| A06 | P0       | Ordinary per-resource updates can leave half an import applied                   | One transaction for the full materialization, mappings, consent, secrets, and receipt                         |
| A07 | P0       | Imported identity confirmation could bypass local consent                        | Recreate candidates and confirm exact reviewed digests only under target operator approval                    |
| A08 | P1       | Two writers can silently overwrite routing or configuration                      | Durable generations, strong CAS, three-way merge, and explicit conflicts                                      |
| A09 | P1       | Recovery and password reset were not distinguished                               | High-entropy user-held recovery capability; server account reset cannot decrypt                               |
| A10 | P1       | Offline devices cannot observe instantaneous revocation                          | Define last-known operation and optional bounded local leases; no remote-erasure claim                        |
| A11 | P1       | Mature primitives were confused with an audited composition                      | Pin dependencies, run vectors/interop, and review the complete protocol independently                         |
| A12 | P1       | Restoring old local state can also restore anti-rollback counters                | Persist anchors separately where available; restored/new devices require trusted re-anchoring                 |
| A13 | P1       | Same-cloud witnesses do not supply independent trust                             | Independent administration/failure domains; reject unsupported security claims                                |
| A14 | P1       | Sync traffic, recovery, and telemetry could expose metadata/secrets              | Minimal public headers, redacted events, explicit recovery disclosure, and no payload telemetry               |
| A15 | P1       | Database atomicity does not guarantee consistent in-flight request configuration | Pin runtime configuration generations and keep origin/authentication/credential resolution consistent         |

All findings above have design mitigations below. None is marked as an
implementation test passed. Dependency eligibility, witness protocol review, and
independent security acceptance remain release gates.

### Quorum correction

A two-of-three certificate is not sufficient to guarantee an honest intersection
when one witness can be Byzantine: two certificates can intersect only at the
Byzantine witness. Strict mode instead uses four pinned independent witnesses
and three signatures. Any two quorums intersect in at least two witnesses, at
least one of which is honest under the stated one-fault assumption.

This proves a safety condition, not universal availability. Witnesses must
persist their vote before returning a signature and never sign incompatible
successors at the same sequence. An honest coordinator serializes one prepared
candidate. A malicious coordinator can withhold data or cause a stalled prepare;
that denial of service is not solved by encryption. Do not invent an ad-hoc BFT
liveness claim. Protocol review must either accept this fail-closed model or
select and verify an established consensus implementation before production.

## 3. Threat model and non-goals

Protect against a compromised coordination service, object storage, network
intermediary, stolen account password/session, unauthorized new device,
unauthorized writer, malformed import, stale retry, and interrupted local apply.
Strict checkpoint safety assumes at most one of four witnesses is Byzantine, and
an uncompromised enrollment/recovery trust anchor.

A device authorized to read a secret can copy it. A compromised local OS, an
untrusted software update, or a compromised authorized authority can defeat
protection on that endpoint. File-backed local key storage is weaker than a
hardware-backed vault; describe its actual protection instead of claiming more.

Do not claim:

- Instant revocation of offline endpoints or cryptographic erasure of their
  data.
- Historical forward secrecy with long-lived HPKE recipient keys and retained
  archives. Future-only recipient changes are not MLS-style forward secrecy.
- Complete metadata anonymity: the service can observe IPs, timing, sizes,
  random identifiers, recipient counts, and authorized public-key relationships.
- Availability against a malicious service or a lost witness quorum.
- Company-policy compliance merely because data is encrypted.

Inference requests/responses, history, sessions, raw-capture passwords, audit
keys, local access tokens, OAuth refresh tokens, control tokens, executable
settings, and local privacy policy are outside the shareable model.

## 4. Model and ownership

### Shareable logical objects

`ProviderSpec` contains a random stable sync ID, name, kind, models,
capabilities, base URL, authentication scheme, failure policy, and logical
references. It does not contain local IDs, credential references, OAuth account
state, local proxy, local ports, or runtime risk/health/usage.

`CompatibilitySpec` contains rule IDs, rule order, match expressions, enabled
flags, header names, sensitive-value object references, and identity references.
The body-rewrite field follows the existing empty-only contract.

`IdentitySpec` contains the existing closed fingerprint tuple, client, and
provenance. It contains neither credentials nor session/request/device IDs.
Source confirmation is retained as provenance only.

`RoutingSpec` is a closed subset: selected provider order, model redirects,
strategy, maximum attempts, and default failure policy. Do not serialize the
entire current RoutingSettings struct: it also contains device/security
controls. A device chooses at most one active shared routing profile, avoiding
ambiguity between multiple spaces. Routes reference stable provider IDs, not
local IDs.

`SensitiveItem` is either an API credential or literal compatibility values. It
has its own random ID, revision, recipient policy, origin binding, and
ciphertext. Do not infer that a custom header is safe because its name is absent
from a known-key list. Private values remain private even inside an otherwise
encrypted configuration snapshot with a wider reader audience.

`Tombstone` is a signed explicit deletion of a known stable ID. Missing objects
in a response, partial export, timeout, or pagination never mean deletion.

### Local-only objects

`WorkspaceBinding` maps stable sync IDs to local service/profile IDs, records
adoption decisions, active routing selection, verified/applied heads, and origin
approvals. Adopt an existing provider only after an exact preview; never match
by name alone. Kind is immutable; a kind change creates a distinct provider.

`DeviceOverlay` applies a closed allowlist of device-specific settings and
explicit field overrides. It is stored locally. Shared updates cannot reset it.
An overridden field remains locally owned until the operator explicitly releases
it. No executable hooks, arbitrary JSON patches, or environment expansion.

`LocalConsent` records reviewed fingerprint/content hash, source revision, local
generation, policy decision, operator action, and timestamp. It is not published
as another device's permission.

### Permissions

`reader` can decrypt granted configuration objects. `writer` may propose shared
configuration changes. `credential_publisher` can publish separately granted
sensitive items. `device_admin` can issue membership and permission changes
under an authority delegation. These scopes are explicit; account authentication
does not create any scope. Cryptographic access is per space/item and recipient.

A reader who lacks sensitive values can inspect a provider template but cannot
activate a configuration needing those values. Show `needs_credential` or
`needs_private_values`, not a false ready state.

## 5. Cryptography, enrollment, and recovery

### Key hierarchy

Each space has an Ed25519 authority root. Each device has separate Ed25519
signing and X25519 recipient key pairs. Authority delegations bind exact device
keys, scopes, epoch, and expiration. Private keys live in a new local sync-vault
purpose, sealed using the existing local secret encryption. Vault and private
plan columns must participate in existing key rotation/re-sealing paths, with
purpose-bound AAD and tests; they must not enter generic credential export. They
are never exported through generic observer APIs. They are not updater keys or
audit keys.

Device-key rotation publishes an authority-approved key-version transition and
re-wraps or re-encrypts the current permitted objects before retiring the old
key. Suspected compromise requires fresh DEKs, recipient revocation, and
upstream API-key rotation where relevant; merely re-wrapping the same DEK is
insufficient. Old key versions cannot authorize new publications. Existing
archives and offline copies retain the explicitly disclosed historical exposure.

Each object revision receives a fresh 256-bit DEK and XChaCha20-Poly1305 nonce
from the OS CSPRNG. Encrypt the payload once, then wrap the DEK separately for
each allowed recipient using HPKE Base mode with DHKEM(X25519, HKDF-SHA256),
HKDF-SHA256, and ChaCha20-Poly1305. Base mode alone does not authenticate the
publisher: the signed manifest supplies that authentication and authorization.

The fixed suite is a proposed protocol choice. The dependency task must verify
RFC 9180 vectors, cross-implementation interoperability, malformed-input
behavior, maintenance/security status, and licenses before freezing a release.
CIRCL is a candidate, **not** an assertion of comprehensive audit: its current
HPKE documentation references draft-07 and its project publishes an experimental
security disclaimer. No hand-written X25519/HPKE implementation is acceptable.

Use RFC 8785 JCS for signed structured metadata. Reject duplicate JSON keys,
invalid Unicode, unknown required fields, and unsupported versions. Represent
sequence/epoch/generation counters as bounded decimal strings, not imprecise
JavaScript numbers. Sign with an explicit protocol-domain prefix.

HPKE `info` and payload AAD bind protocol version, space, object, revision,
epoch, content type, and recipient where applicable. The publisher signature
binds ciphertext hash, complete recipient-wrap list, membership hash, and
parents. Ciphertexts cannot be swapped between spaces, recipients, or revisions.

### Enrollment sequence

1. New device authenticates to the service and generates fresh local keys.
2. It submits a one-time challenge bound to both device public keys.
3. An authorized device displays/checks an out-of-band QR or matching code.
4. Approval binds service origin, space authority, target keys, nonce, scopes,
   current membership epoch, and a trusted checkpoint digest/sequence.
5. The administrator signs membership and grants. The checkpoint is finalized.
6. The new device verifies the anchor/certificate before obtaining wrapped DEKs.

A server cannot enroll its own keys by presenting a successful login. Enrollment
expires after ten minutes and is single-use. Never use trust-on-first-use solely
from server-returned authority keys. Privilege elevation requires fresh
approval.

### Recovery

Generate a 256-bit per-space recovery capability, presented through an explicit
native recovery interaction and saved by the operator outside the server. It
protects an encrypted recovery bundle containing authority recovery material and
a recovery recipient capability. Its broad access must be disclosed. Recovery
recipients for sensitive items are an explicit policy, not hidden server escrow.
Removing recovery access affects future versions, not archives.

Account password reset restores account access only. Without an authorized
device or valid recovery capability, old encrypted data cannot be recovered.
Recovery also needs a trusted checkpoint/authority anchor; the service cannot
supply an unquestioned old state. A stale offline recovery bundle requires
trusted re-anchoring before accepting new shared writes or remote secret data.

Password-protected portable exports are separate from enrollment: Argon2id
version 19, 64 MiB, three iterations, four lanes, 128-bit random salt, 256-bit
output is the proposed interoperable baseline from RFC 9106's memory-constrained
recommendation. Reject attacker-selected unbounded KDF parameters; do not lower
parameters silently. Random recovery material does not need password stretching.

## 6. Revisions, strict checkpoints, and merge

### Envelope

Required fields are `protocol_version`, `suite`, `space_id`, `object_id`,
`revision_id`, `object_kind`, `membership_epoch`, `parent_revision_ids`,
`publisher_device_id`, `membership_hash`, `payload_nonce`, `ciphertext_hash`,
`wraps`, and `signature`. The ciphertext is an immutable blob addressed by its
hash; the nonce is signed metadata. IDs use random 128-bit, 26-character
lowercase base32 tokens. SHA-256 digests use 64 lowercase hexadecimal
characters. Wraps are sorted by recipient ID, contain recipient ID/key
version/HPKE encapsulation/wrapped DEK, and are bounded by the device limit. A
revision ID identifies one immutable signed envelope; resending the same
revision must resend the same bytes.

Wire types are closed: version is integer `1`; IDs/digests/counters are strings
with the formats above; parents are a bounded array of revision IDs; object kind
is a schema-defined enum; wraps are a bounded array; signatures, nonce, and HPKE
encapsulation are unpadded base64url strings with exact decoded algorithm
lengths. Wrapped DEKs have the suite's fixed length; payload ciphertext has a
bounded variable length including the authentication tag. Unknown algorithms are
rejected, not negotiated downward. Only genesis may omit its predecessor.
Optional portable fields mean retain or unresolved template according to their
schema, never implicit deletion; explicit clear/delete is a distinct typed
action. Local database IDs never cross this wire. The implementation schema task
freezes these types and golden fixtures.

Example of a decrypted portable provider template, not an encrypted envelope or
a live provider configuration:

```json
{
  "schema_version": 1,
  "sync_id": "aaaaaaaaaaaaaaaaaaaaaaaaaa",
  "kind": "openai_compatible",
  "name": "Example provider",
  "base_url": "https://api.example.org/v1",
  "auth_scheme": "bearer",
  "model_ids": ["example-model"],
  "credential_item_id": "bbbbbbbbbbbbbbbbbbbbbbbbba",
  "shared_enabled": false
}
```

Without a grant for the referenced sensitive item this is a template requiring
credentials, not a usable service. Base URL validation retains the existing ban
on user-info, queries, and fragments. Fields containing private literal values
are separate sensitive objects. An explicit grant never changes local origin
approval or profile confirmation automatically.

Only identifiers, cryptographic evidence, and bounded structural metadata are
public. Names, provider domains, model details, credential origins, and private
values remain inside encrypted content. Do not use plaintext-derived secret
hashes as public identifiers.

### Checkpoint

`Checkpoint` binds space, decimal sequence, previous checkpoint hash, membership
epoch/hash, revision-index Merkle root, protocol version, and coordinator
prepare ID. The authority/writer authorization and the log transition are
verified separately. Certificates require three of four pinned witness keys. The
index maps stable IDs to revision digests; proof binds an envelope to the
certified current index. Inclusion alone cannot prove it is the latest version.

Strict publication:

1. Upload immutable encrypted objects with verified lengths and hashes.
2. Prepare exactly one successor using strong `If-Match` against the current
   head. Durably persist the candidate; conflicting prepare returns 412.
3. Collect durable witness votes for the identical candidate and valid parent.
4. Finalize only with a valid three-of-four certificate and all blobs present.
5. Advance head atomically; retries with identical IDs return the existing
   result.

A witness persists `(space, sequence, checkpoint_hash)` before signing, verifies
parent continuity and authorization, and refuses a second hash at that sequence.
Crash/restart must not erase the vote lock. A pending prepare resumes the same
candidate. No timeout may silently authorize another candidate or reset
counters.

Static witness membership is v1. Changing the witness trust set requires an
explicit trusted re-anchoring ceremony. Deploying four processes under one
administrator does not meet independent-fault-domain claims. Lost quorum blocks
shared publication and fresh sensitive imports, not existing local inference.

For freshness, clients query independently pinned witnesses using a fresh nonce
and verify nonce-bound signed head responses; require a coherent quorum before
sensitive activation. This is an online observation, not an instantaneous
revocation guarantee. Persist verified high-water marks before materialization.
Old/equal-sequence different heads, missing parents, or bad proofs quarantine
the space. New/restored devices require enrollment/recovery anchors.

### Merge and effective configuration

Perform a three-way merge using the last synchronized base, local draft, and
verified remote snapshot. Merge disjoint object/field edits; same-field edits
become explicit conflicts. Rule ordering, routing/security changes, recipient
sets, and deletions are not automatic set unions or last-write-wins.

The effective configuration is shared selection plus explicitly owned local
field overrides plus local credential bindings and local safety approvals.
Persist the base needed to distinguish overrides from accidental divergence.
Queued proposals are revalidated against the latest membership after offline
reconnect; revoked or stale-authorized proposals are never rebased
automatically.

A user rollback creates a **new** revision containing reviewed old values. It
never lowers the accepted checkpoint or silently revives revoked credentials,
origin approvals, or device membership.

## 7. Local projection and atomic application

### Required storage interfaces

Interfaces below are design contracts; their implementations do not exist yet.
`Context` always carries cancellation, never secrets in loggable fields.

| Interface                                                         | Inputs                                                            | Result and invariants                                                                                                 |
| ----------------------------------------------------------------- | ----------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------- |
| `ExportSelection(ctx, Selection)`                                 | Required space/object selection; explicit sensitive-item policy   | Portable logical snapshot plus internal sensitive buffers; unreadable selected secret is an error, not empty deletion |
| `VerifyRemote(ctx, EnvelopeSet, TrustState)`                      | Bounded ciphertexts and certificate chain                         | Verified revision/index and updated high-water evidence, or typed verification error                                  |
| `BuildApplyPlan(ctx, VerifiedSnapshot, LocalGeneration, Choices)` | Verified remote data and explicit local adoption/override choices | Redacted `ApplyPlan`; no plaintext credentials returned                                                               |
| `ApplyPlan(ctx, PlanID, DecisionDigest, ExpectedGeneration)`      | Required operator-approved plan and exact generation              | One `ApplyReceipt` or no changes; plan ID cannot be used as bearer authorization                                      |
| `BuildProposal(ctx, BaseRevision, LocalDraft)`                    | Exact base and selected local edits                               | Encrypted signed proposal or typed conflict/authorization failure                                                     |
| `SyncTransport`                                                   | Pinned service configuration and device session                   | Get/upload/prepare/finalize bounded encrypted objects; never redirects credentials to another origin                  |
| `WitnessClient`                                                   | Pinned witness set, nonce, known head                             | Verified checkpoint/proofs or stale/fork/quorum error                                                                 |
| `SyncVault`                                                       | Purpose-bound key ID and internal byte buffers                    | Locally sealed key material; zeroizing lifecycle; separate from generic UI reads                                      |

`ApplyPlan` contains `plan_id`, `space_id`, remote revision/checkpoint hashes,
local generation, expiry, action summaries, conflict IDs, local-ID mappings,
required consents, origin-binding decisions, and a digest of reviewed decisions.
Use a five-minute default expiry. Persist private plan payload encrypted;
frontend receives only redacted summaries. Commit rechecks every binding and
fingerprint. Required decision fields are typed action, stable object ID,
selected local ID where adoption is requested, conflict resolution, explicit
consent IDs, and origin-approval digest. `plan_id` and decimal
`expected_generation` are required strings; a decision digest is a required
SHA-256 string. Expiry is RFC3339 UTC and is not used to order remote revisions.
Example rejection is HTTP 409 with
`{"code":"stale_plan","retryable":false,"request_id":"example-request"}`;
rebuild and review the plan instead of blindly retrying ApplyPlan.

Schema additions: sync spaces/trust anchors, encrypted device/authority vault,
membership state, immutable encrypted inbox, outbox, prepared/apply plans,
object mappings, overlays, base snapshots, local consent, and receipts. Allocate
migration numbers against HEAD at implementation time; do not preassign an
occupied number.

Add a single configuration generation covering service/order/profile/routing and
sensitive-item mutations participating in plans. Every relevant existing write
path must advance it in the same transaction. A generation mismatch returns
`stale_plan`, including changes made by another window or runtime credential
mutation when relevant to the selected plan.

Apply sequence:

1. Outside the database transaction: validate proofs, limits, schema,
   permission, decrypt into bounded buffers, compute full dependency graph, and
   obtain required local decisions/freshness evidence.
2. Inside one SQLite transaction: check generation and plan digest; create or
   explicitly adopt local services; recreate immutable identity candidates;
   confirm only exactly approved candidates; validate rewritten references;
   re-seal selected credentials/private values; write routing selection,
   mappings, overlays, local consents, applied revision, receipt, and new
   generation.
3. Commit; only then publish runtime invalidation/events. Wipe temporary secret
   buffers on every success/error path. No network I/O under the transaction.
4. Retry after a crash uses the receipt/idempotency ID. The database presents
   the full old or full new effective configuration, not half of each.

Database atomicity alone does not make multiple runtime reads atomic. A request
must pin a committed configuration generation, including origin, authentication,
identity/rules, routing references, and compatible credential resolution.
Retries stay on that generation; new requests switch only after the new view is
ready. Keep old sealed credential material/views bounded until pinned users
finish, then clear them. If a safe view cannot be prepared, reject/defer the
apply with a typed busy error instead of mixing generations or silently
canceling active streams. Device-local OAuth refresh remains outside shared
credential history. Verify the final outgoing HTTP/WebSocket request during
concurrent apply, not only the DB.

Reuse existing validators and encryption functions through transaction-aware
helpers. Do not disable existing single-resource validation or use an unsafe
bulk bypass. Local services not adopted by the space remain untouched.

## 8. Safety policy and runtime behavior

Always require target-device review for new activation, origin/authentication
changes, literal compatibility values, identity selection changes, expanded
routing/failover, new recipients, deletions, adoption, and release of local
field overrides. Labels and other non-activating metadata may auto-apply under
an explicit per-space preference. Shared data never enables raw capture, changes
privacy allowlists, installs code, or writes IDE credentials automatically.

Credential origin approvals bind normalized scheme/host/port and the relevant
base-path scope. Origin/path/auth changes invalidate the binding before any
model discovery, quota probe, connection test, retry, or inference can send a
secret. No credential forwarding across redirects. Private network/local
provider endpoints require explicit device approval, not a universal ban.

Imported OAuth subscriptions remain `needs_authorization`; do not duplicate
refresh tokens. Missing/decryption-failed credentials produce actionable status
without removing existing local data. Empty secret is deletion only under an
explicit typed action.

The local state machine is:

```text
disabled -> unpaired -> enrolled -> checking -> verified
         -> preview -> applying -> current
```

Side states are `offline`, `conflict`, `needs_approval`, `needs_credential`,
`needs_authorization`, `locked`, `quarantined`, and `revoked`. Quarantine or
loss of sync stops shared updates, not the already-ready local gateway. Leaving
a space requires a choice to keep detached local configuration or explicitly
remove its adopted material; never erase unrelated services.

## 9. Service and native API contracts

### Remote service

All routes require HTTPS in production, bounded bodies, authenticated account
sessions, device proof-of-possession, and cryptographic authorization for the
operation. Account WebAuthn/passkey authentication is separate from provider
OAuth. Session refresh/revocation must be independent from configuration keys.
Device request signatures bind method, normalized target, body digest, server
challenge, and request ID; a cached server response is not evidence of a new
head.

| Route                                       | Request                                                     | Response                                              |
| ------------------------------------------- | ----------------------------------------------------------- | ----------------------------------------------------- |
| `POST /sync/v1/enrollments`                 | Device public keys, one-time challenge                      | Enrollment ID and expiry; no grants                   |
| `POST /sync/v1/enrollments/{id}/approve`    | Authority-signed approval and finalized membership evidence | Enrolled public device record                         |
| `GET /sync/v1/spaces/{id}/head`             | Known checkpoint and challenge                              | Head, certificate, bounded proofs                     |
| `PUT /sync/v1/spaces/{id}/objects/{digest}` | Exact encrypted envelope bytes, length/digest               | Immutable object receipt; conflicting bytes rejected  |
| `GET /sync/v1/spaces/{id}/objects/{digest}` | Authorized object request                                   | Encrypted bytes, never plaintext                      |
| `POST /sync/v1/spaces/{id}/prepare`         | Signed candidate, idempotency ID, strong `If-Match`         | Durable prepare ID/head, or 412                       |
| `POST /sync/v1/spaces/{id}/finalize`        | Prepare ID and strict certificate                           | Certified head, or idempotent existing result         |
| `GET /sync/v1/spaces/{id}/changes`          | Cursor and limit                                            | Bounded encrypted index changes; authenticated cursor |

Witness routes expose challenge-bound signed heads, evidence/proofs, and a vote
endpoint restricted to validated candidates. Device administration changes are
signed membership objects finalized through the same head flow, not unsigned
server-admin writes.

Errors have `{code, retryable, request_id}` plus redacted user-safe detail.
Define `unauthorized`, `permission_denied`, `stale_head`, `stale_plan`,
`schema_unsupported`, `invalid_envelope`, `invalid_signature`, `fork_detected`,
`quorum_unavailable`, `origin_approval_required`, `credential_unreadable`,
`identity_confirmation_required`, `needs_authorization`, and `limit_exceeded`.
Use 401/403/409/412/413/422/429/503 as appropriate; error strings never include
secret values, complete envelopes, passkeys, or recovery capabilities.

### Local control and native bridge

Propose operator-only `/control/v1/config-sync` operations for status,
enrollment, selection export, preview, apply, publish, conflicts,
grants/revocation, and recovery/export ceremonies. Observer gets at most coarse
enabled/state counts, not remote object names, plan detail, fingerprints, or
credentials.

Native `ConfigSyncOperation` is a closed tagged enum. The WebView cannot select
a remote URL, HTTP verb, filesystem path, or arbitrary credential reference.
User-selected encrypted input and clipboard operations require explicit user
actions. Import accepts bounded ciphertext from a user file selection, not an
arbitrary frontend path. Export writes only within a host-managed exports
directory using restrictive permissions and a validated filename; the OS can
move/reveal the result. Do not introduce an unapproved dialog dependency or a
generic filesystem bridge. Opaque encrypted exports may pass through a bounded
file capability; private keys/API secrets do not appear in ordinary command
responses or events. Recovery entry/display is a specifically reviewed sensitive
ceremony, not a claim that all frontend strings are secret-free.

Events carry space ID, state, progress/counts, and redacted errors only. Unmount
and app shutdown cancel UI subscriptions safely; durable jobs live in Core.

## 10. Limits and operational design

Initial limits are design decisions to verify, not measured existing capacity:
64 devices/space, 2,000 providers/space, 8 MiB plaintext configuration snapshot,
16 MiB encrypted revision, 32 MiB encrypted portable package, 1 MiB sensitive
item, 100 MiB encrypted inbox cache, 500 items per remote change page, and 16
queued revisions per space. Enforce limits before allocation at codec, HTTP,
crypto, and storage boundaries.

No compressed archive import in v1, avoiding decompression bombs/path traversal.
Pagination must not imply deletion. Ciphertext object keys are validated hashes,
not user-provided paths. Queue backoff uses jitter and Retry-After; only one
local writer per space; manual check can join, not duplicate, a job.

The server uses PostgreSQL for account/public membership indexes, CAS prepare,
head references, nonces, quotas, and redacted events. Immutable objects use a
storage interface with an initial restricted filesystem adapter; optional S3
adapter is a separate task. Witness vote storage is independent and durable. A
server-side index is a transport aid, never the client's cryptographic trust.

Object garbage collection follows certified references, pending prepares,
retention policy, and explicit purge. HTTP retry must not allocate a fresh
revision ID or nonce for an already-uploaded envelope. Backups preserve
encrypted objects and index consistency; server restore is not allowed to lower
client anchors. Metrics contain counts/timing/result codes, not provider names
or URLs.

Do not deploy or expose local Control API/inference ports as part of this work.
Self-hosting packages are separate from the experimental headless gateway
branch.

## 11. UI design

Add one compact configuration-space workspace reachable from Settings. Tabs are
`Spaces`, `Devices`, `Changes`, and `Recovery`, with shared controls. The main
region is the selected list/diff/preview; progress and state belong in one
compact toolbar. Descriptions/help are expandable, not permanently stacked
chrome.

Import preview shows add/update/adopt/delete, local overrides, identity consent,
credential/private-value requirements, and origin changes. Never display a
secret just to make a diff readable. A sensitive change cannot be hidden behind
an undifferentiated "sync all" action.

Reuse `ConfirmDialog`, `Field`, `Panel`, shared Tabs, EmptyState, progress, and
status components. Keep workspace overflow hidden and scroll inside active
panels. Verify 1280x720, 1024x600, and a narrow layout in the native shell;
record primary-region height and target at least 60% of usable workspace. Verify
long lists, expanded help, empty states, conflicts, and tab/focus transitions.

## 12. Delivery gates and acceptance

G0: schema, ownership/consent matrix, trust assumptions, and normative golden
fixtures are reviewed. All architectural P0 findings have explicit invariants.

G1: crypto dependencies and protocol composition pass vectors, independent
interop, malformed-input fuzzing, replay/recipient/context tests, and review.
Checkpoint review proves quorum safety and explicitly documents availability
limits. No shared-secret rollout before this gate.

G2: fault-injected storage apply and generation coverage demonstrate atomic,
idempotent behavior without reading/resetting real user keys.

G3: adversarial-server, revoked-device, stale-recovery, poisoned-origin,
concurrent-edit, and witness split/restore tests pass. Independent reviewers
assess the actual implementation; critical/high findings block release.

G4: real macOS Intel/ARM, Windows, and Linux device-pair tests verify initial
import, bidirectional proposals/conflicts, device-specific keys/overrides,
recovery, offline operation, revocation/reconnect, and upgrade compatibility. CI
packaging alone is not this gate. Existing provider behavior and upstream
identity policy must be tested on the final outgoing request.

G5: native UI height/accessibility checks, cancellation, consent, no secret in
logs/events/clipboard without explicit action, restore drill, and operational
runbook pass. Credentials remain opt-in for sharing. Deployment and production
publication require separate authorization.

## 13. Task ledger

Each task is a single-domain 8-16 hour implementation/verification work package.
External reviewer lead time and follow-up remediation are not included. The 49
task manager entries carry the implementation guide, explicit dependencies,
files, and acceptance criteria. No implementation task is completed by this
document. Eight submission batches contain 6/6/6/6/7/6/6/6 tasks. The estimate
is a planning unit, not a delivery guarantee; split again if code inspection
reveals a package exceeds two developer-days.

| ID  | Domain       | Deliverable                                                          |
| --- | ------------ | -------------------------------------------------------------------- |
| P01 | Protocol     | Portable schemas, scope matrix, bounds, fixtures                     |
| P02 | Protocol     | Strict JCS codec and deterministic identifiers/digests               |
| P03 | Crypto       | Dependency freeze and RFC/interoperability eligibility gate          |
| P04 | Crypto       | Payload/recipient envelopes and publisher signatures                 |
| P05 | Protocol     | Membership/delegation/enrollment certificate verification            |
| P06 | Crypto       | Recovery and portable export envelopes                               |
| S01 | Server DB    | Account/device/object/head/prepare/nonce schema and transactions     |
| S02 | Server       | Passkey sessions and device proof-of-possession                      |
| S03 | Server       | Immutable bounded ciphertext object storage                          |
| S04 | Server       | Prepared-head CAS and idempotent finalization                        |
| S05 | Server       | Enrollment/membership transport and encrypted change API             |
| S06 | Server       | Quotas, redacted observability, encrypted backup/GC                  |
| W01 | Witness DB   | Durable vote locks and crash-safe state                              |
| W02 | Witness      | Parent/authorization validation and signing service                  |
| W03 | Protocol     | Strict certificates, index proofs, nonce-bound head checks           |
| W04 | Protocol     | Quorum/state-machine model and failure eligibility gate              |
| W05 | Witness      | Independent-node integration and trusted re-anchor ceremony          |
| W06 | Server       | End-to-end prepare/certificate/finalize integration                  |
| D01 | Local DB     | Sync metadata schema and migration compatibility                     |
| D02 | Local DB     | Locally sealed sync vault and private plan storage                   |
| D03 | Local DB     | Transaction-aware service/identity/secret/order/routing helpers      |
| D04 | Local DB     | Configuration generation coverage for existing write paths           |
| D05 | Local DB     | Stable-ID mappings, adoption, overlays, consent, receipts            |
| D06 | Local DB     | Atomic materialization, rollback, idempotency fault tests            |
| C01 | Core         | Closed export projection and sensitive-value classification          |
| C02 | Core         | Remote transport and durable inbox/outbox lifecycle                  |
| C03 | Core         | Trust engine, high-water persistence, revocation/reconnect           |
| C04 | Core         | Three-way merge, drafts, conflicts, and new-revision rollback        |
| C05 | Core         | Redacted apply planning, approval/origin gates, commit orchestration |
| C06 | Core         | Operator API, sync state machine, and redacted events                |
| C07 | Core         | Runtime generation pinning and concurrent-request consistency        |
| R01 | Rust         | Closed native sync operation preparation and typed errors            |
| R02 | Rust         | Core bridge/status/event subscriptions and lifecycle cancellation    |
| R03 | Rust         | Controlled pairing/recovery sensitive interactions                   |
| R04 | Rust         | Controlled encrypted import/export file capabilities                 |
| R05 | Rust         | Native preferences and shutdown/background behavior                  |
| R06 | Rust         | Capability isolation, observer denial, and leak regressions          |
| U01 | Frontend     | Shared sync state/diff/picker controls and typed models              |
| U02 | Frontend     | Space enrollment, selection, and status workspace                    |
| U03 | Frontend     | Provider adoption/import preview and target consent                  |
| U04 | Frontend     | Device grants, enrollment review, and revocation UI                  |
| U05 | Frontend     | Conflicts, history rollback, and device overlays UI                  |
| U06 | Frontend     | Recovery/export UX, i18n, native-shell layout/accessibility          |
| V01 | Verification | Adversarial service/transport/recipient test harness                 |
| V02 | Verification | Crash/concurrency/restore and witness-failure acceptance             |
| V03 | Verification | Secret-leak and upstream-identity regression matrix                  |
| V04 | Verification | Real cross-platform multi-device acceptance                          |
| V05 | Verification | Independent protocol/implementation review acceptance                |
| V06 | Verification | Opt-in rollout, operational restore drill, release acceptance        |

Critical path: P01-P05 -> server/witness eligibility -> local atomic apply ->
Core trust/planning -> native bridge -> native UI -> adversarial and real-device
acceptance -> independent review -> authorized release. Separate schema, server,
local storage, and UI teams may work in parallel once their contracts and
prerequisite gates are frozen.

## 14. Normative and reviewed sources

- [RFC 9180](https://www.rfc-editor.org/rfc/rfc9180.html): HPKE, identity
  binding, context separation, and application non-goals. HPKE alone is not
  enrollment.
- [RFC 8785](https://www.rfc-editor.org/rfc/rfc8785.html): JCS/I-JSON, duplicate
  properties, Unicode, and numeric representation.
- [RFC 9106](https://www.rfc-editor.org/rfc/rfc9106.html): Argon2id and the
  memory-constrained recommendation. Parameters remain bounded by this design.
- [RFC 9110](https://www.rfc-editor.org/rfc/rfc9110.html#name-if-match):
  If-Match and lost-update prevention. CAS is not malicious-server protection.
- [RFC 9162](https://www.rfc-editor.org/rfc/rfc9162.html): Merkle inclusion and
  append-only consistency proof building blocks. This is not a claim that the
  complete sync protocol implements Certificate Transparency.
- [RFC 9420](https://www.rfc-editor.org/rfc/rfc9420.html): MLS epoch/security
  properties, used to distinguish goals this snapshot protocol does not promise.
- [CIRCL package documentation](https://pkg.go.dev/github.com/cloudflare/circl)
  and [HPKE source](https://github.com/cloudflare/circl/blob/main/hpke/hpke.go):
  actual APIs, draft-reference caveat, and experimental security disclaimer.

External searches were evidence gathering, not substitutes for protocol review.
All limits, module locations, task sizes, and new interfaces are explicit design
choices, not claims about existing measured behavior.
