# Desktop UI conflicts and the ja locale

<!-- markdownlint-configure-file
{ "MD013": { "tables": false, "code_blocks": false } }
-->

Refs: fork `HEAD` = `1be7583`, upstream `v0.2.10` = `e8b9600`, merge-base
`b3af635`, trial merge tree `bec8f2b5` (`T`). Line numbers refer to the
conflicted blobs in `T` unless noted.

## 1. `apps/desktop/src/ServiceManager.tsx` (1 hunk)

`T:apps/desktop/src/ServiceManager.tsx:677-681`. This is the HTTP branch of
`draftFromRecord`:

```text
<<<<<<< HEAD
    compatibility: compatibilityDraft(service.http),
=======
    privacyTrusted: service.privacy_trusted === true,
>>>>>>> v0.2.10
```

- Fork (`8728094`, "在提供商编辑器中配置请求兼容与身份档案"): seeds the request
  compatibility and identity-profile editor draft from `service.http` (headers,
  model rules, `identity_profile_id`).
- Upstream (`0ac1bfb`, "let requests to a trusted provider skip the policy"):
  adds the `privacyTrusted` toggle.
- Both sides add a line after `responsesWebSocket` in the same object literal,
  so the change is additive and independent.
- Resolution: keep both lines.

  ```ts
  responsesWebSocket: responsesWebSocketEnabled(service),
  compatibility: compatibilityDraft(service.http),
  privacyTrusted: service.privacy_trusted === true,
  ```

- Everything else auto-merged consistently (checked in `T`):
  - `Draft` type has both `compatibility` (`:266`) and `privacyTrusted`
    (`:273`).
  - New-draft defaults set both (`:601`/`:608`, `:629`/`:636`). The subscription
    branch of `draftFromRecord` sets `privacyTrusted` (`:656`) and inherits
    `compatibility` from `draftForKind`.
  - The save patch has `privacy_trusted` (`:1815`) plus
    `compatibilityInput(...)` in `patch.http` (`:1823-1828`). Both create paths
    have conditional `privacy_trusted` (`:1856`, `:1881`) and
    `compatibilityInput` (`:1891`).
  - The model-discovery key includes `draft.compatibility` (`:585`).
- Dirty-tracking and equality compare whole drafts, so they need no extra
  change. Confirm with `bun run typecheck` plus `ServiceManager*.test.tsx` after
  resolving.

## 2. `apps/desktop/src/App.navigation.test.tsx` (2 hunks)

### 2.1 Imports (`T:…/App.navigation.test.tsx:133-137`)

```text
<<<<<<< HEAD
import { routingAutosaveDelay } from "./RoutingSettingsPanel";
=======
import { settleClosedLayer } from "./lib/test-layer-settle";
>>>>>>> v0.2.10
```

- Fork `e7ebad2` ("隔离路由自动保存计时以稳定慢速 CI") imports
  `routingAutosaveDelay` for its fork-only test
  `"settles routing autosave before allowing navigation"` (`T:…:1889`, it uses
  the import at `:1909`). `routingAutosaveDelay` is still exported upstream
  (`v0.2.10:apps/desktop/src/RoutingSettingsPanel.tsx:42`, value 500).
- Upstream `016e24c` ("settle closed Radix layers…") imports `settleClosedLayer`
  (used at `T:…:245`). The helper lives at
  `apps/desktop/src/lib/test-layer-settle.ts` (new upstream file).
- Resolution: keep both imports. The fork mock wiring it depends on auto-merged:
  `updateRoutingSettings: vi.fn()` (`:31`) and its `mockImplementation`
  (`:405`).

### 2.2 Comment in "protects default-policy drafts when leaving routing" (`T:…:1919-1924`)

```text
<<<<<<< HEAD
    // Keep the autosave deadline from depending on the CI runner's speed.
=======
    // The routing panel autosaves 500 ms after an edit. The draft must stay
    // unsaved however slowly a runner moves through the steps below.
>>>>>>> v0.2.10
    vi.useFakeTimers();
```

- Both sides independently fixed the same CI flake: fork in `e7ebad2`, upstream
  in `016e24c`. Each added `vi.useFakeTimers()` with a different comment. The
  code is identical, so only the comment conflicts.
- Resolution: take upstream's comment. It is more explicit, and choosing it
  shrinks the fork diff. Keep the fork-only assertion
  `expect(bridgeMocks.updateRoutingSettings).not.toHaveBeenCalled();`
  (`T:…:1948`), which auto-merged. Upstream does not mock
  `updateRoutingSettings` (its `bridgeMocks` has none), so the assertion relies
  on the fork mock in §2.1.
- No duplicate `useFakeTimers()` remains after the merge. Each test has one
  (`:1890`, `:1925`).

## 3. `apps/desktop/src/components/BuiltinToolsEditor.test.tsx` (1 hunk)

`T:…/BuiltinToolsEditor.test.tsx:452-456`:

```text
<<<<<<< HEAD
  await act(async () => new Promise((resolve) => setTimeout(resolve, 0)));
=======
  await settleClosedLayer();
>>>>>>> v0.2.10
```

- Fork `78c97c4` is a cherry-pick of upstream `cb10dda` ("improve MiniMax model
  focus handling"). It is not an ancestor of v0.2.10, and it is the only fork
  change to this file (`git diff b3af635 HEAD` shows +3 lines). Upstream later
  moved the inline wait into the shared helper (`016e24c`). The merged file
  already imports it (`:9`) and uses it at `:301/:370/:396`.
- Resolution: take upstream (`await settleClosedLayer();`). The file then
  matches upstream exactly, and `git diff v0.2.10 -- <file>` should be empty.
  The surrounding comment (`:450-451`) is identical on both sides.

## 4. i18n and the new `ja` locale (`4a477bb`)

Structure in `T`:

- Catalogs: `apps/desktop/src/i18n/locales/{en,zh-CN,ja}.json`. They are
  registered in `apps/desktop/src/i18n/index.ts:6-8,28-30`, and
  `LOCALES = ["en", "zh-CN", "ja"]` is set in `i18n/locale.ts:1`. `fallbackLng`
  is `en`, so a missing ja key renders English at runtime.
- A parity test enforces identical key sets:
  `apps/desktop/src/i18n/catalog.test.ts:20-24` (`flattenKeys(ja).sort()` must
  equal `flattenKeys(en).sort()`).
- All three catalogs auto-merged with no conflict markers.
- Measured on `T`: en = 3456 keys, zh-CN = 3456, ja = 3410. Upstream v0.2.10 has
  en = ja = zh key sets. Fork `HEAD` has en = zh.

**Result: `catalog.test.ts` fails after the merge unless ja gains the 46
fork-only keys.** They are exactly the keys the fork added (none are missing for
any other reason, and ja has no extras):

- `compatibility.*` (42 keys, the whole block; en.json places it first, at
  `T:…/en.json:2`): `arm`, `armConsent`, `back`, `builtin`, `candidate`,
  `captureHint`, `captured`, `capturing` (`{{client}}`, `{{expires}}`,
  `{{count}}`), `client`, `confirm`, `confirmConsent`, `confirmed`, `create`,
  `discard`, `discardConsent`, `editor`, `empty`, `emptyHint`, `failed`,
  `headers`, `headersHelp`, `headersJSON`, `help`, `identity`, `invalid`
  (`{{path}}`), `manage`, `noIdentity`, `profileHelp`, `request_capture`,
  `review`, `reviewHelp`, `reviewNamed` (`{{id}}`), `rules`, `rulesHelp`,
  `rulesJSON`, `safetyHelp`, `saveFirst`, `source`, `stop`,
  `subscription_import`, `title`, `useIdentity`.
- `serviceTest.*` (4 keys, en.json `T:…:143-146`): `resultHelp`,
  `completionHint`, `emptyTitle`, `emptyDescription`.

Keep interpolation placeholders identical to en and zh-CN. Keep the literal
tokens `identity_profile`, `enabled: false`, `body`, `*`, and `X-AstrLink-*`
untranslated. A machine draft translated from zh-CN/en is acceptable, matching
how upstream produced ja.

### 4.1 Value drift that the parity test cannot catch

The fork reworded 3 existing keys ("passed" → "request completed", semantics:
protocol-valid response ≠ usable model). The en/zh values come from the fork,
but ja keeps upstream's original meaning:

| Key                       | en (merged)                  | zh-CN (merged)     | ja (upstream)  |
| ------------------------- | ---------------------------- | ------------------ | -------------- |
| `batchTest.successCount`  | Requests completed {{count}} | 请求完成 {{count}} | 成功 {{count}} |
| `batchTest.state.success` | Completed                    | 完成               | 成功           |
| `serviceTest.success`     | Request completed            | 请求完成           | テスト成功     |

Update ja to match, for example `リクエスト完了 {{count}}`, `完了`, and
`リクエスト完了`. Otherwise the ja UI contradicts the fork's
`serviceTest.completionHint`.

### 4.2 Other ja-related checks

- `request-privacy-audit.test.ts:326` and `format-compact-number.test.ts:64`
  iterate or apply `ja`. These are upstream tests over upstream keys, and the
  fork does not affect them.
- The Rust host i18n (`apps/desktop/src-tauri/src/i18n.rs`) has no fork changes
  and no conflict, so nothing is needed there.
- No other fork component hard-codes locale lists.

## 5. Resolution summary

| File                                | Resolution                                                                          |
| ----------------------------------- | ----------------------------------------------------------------------------------- |
| ServiceManager.tsx:677-681          | Keep both lines (`compatibility` + `privacyTrusted`).                               |
| App.navigation.test.tsx:133-137     | Keep both imports.                                                                  |
| App.navigation.test.tsx:1919-1924   | Upstream comment, single `vi.useFakeTimers()`. Keep the fork assertion at `:1948`.  |
| BuiltinToolsEditor.test.tsx:452-456 | Take upstream `settleClosedLayer()`. The file then equals upstream.                 |
| i18n/locales/ja.json                | Add 42 `compatibility.*` + 4 `serviceTest.*` keys, and reword the 3 drifted values. |

Verification after resolving: run `(cd apps/desktop && bun run typecheck)` and

```bash
(cd apps/desktop && bun run test -- src/i18n/catalog.test.ts src/App.navigation.test.tsx src/components/BuiltinToolsEditor.test.tsx src/ServiceManager)
```

(`package.json:17`: `"test": "vitest run"`). Then run oxlint and prettier on the
touched files.
