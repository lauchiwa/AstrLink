# v0.2.10 同步技术方案

## 目标与证据

- 原 `HEAD`：`1be7583f5beddf261bf4ad1a4cd41827a51aca98`（= `origin/main`）。
- 固定目标：`v0.2.10` = `e8b96003c4529a8c39e4c31e47033b10cf925c05`，合并基
  `b3af635`。
- 试合并树：`bec8f2b521126b8f86f5458239ab6bcf5f060ee3`（16 个冲突文件）。
- 研究：
  - [发布与更新](research/release-updater-conflicts.md)
  - [核心冲突与转发身份审计](research/core-conflicts-identity.md)
  - [迁移与配置同步](research/migrations-config-sync.md)
  - [桌面 UI 与日语目录](research/desktop-ui-conflicts.md)
- 规范：`.trellis/spec/guides/upstream-sync-thinking-guide.md`、
  `version-naming-guide.md`、AGENTS.md「Upstream forwarding identity」「Database
  migrations」。

这是一次需要统一验证的同步交付，不拆子任务。执行
`git merge --no-ff --no-commit v0.2.10`，解决后由用户另行授权提交。

## 1. 文本冲突解决

<!-- markdownlint-configure-file { "MD013": { "tables": false } } -->

| 文件                                                      | 方案                                                                                                                                                             |
| --------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.github/workflows/release.yml`                           | 合并为 `release_tag: ${{ inputs.release_tag \|\| github.ref_name }}` + `arch: arm64`                                                                             |
| `.github/workflows/macos-package.yml`                     | fork 的 `if: always() && steps.signing.outputs.mode == 'developer-id'` + 上游 upload-artifact v7.0.2 pin                                                         |
| `.github/workflows/windows-package.yml`                   | 采用上游：删除 `checks` job                                                                                                                                      |
| `apps/desktop/scripts/release-updates.mjs`                | 保留上游 `verifiedPlatform`/`releaseAssets`/`writeRelease`/`extendRelease`/`attachRelease` 结构，fork 值作参数穿入（见 §3）                                      |
| `apps/desktop/scripts/release-updates.test.ts`            | 导入取并集；fixture 用上游 `stagePlatform` + `releaseTargets`，fork 字段放进 `stagePlatform`；fork 断言块改用 `releaseTargets`，mixed 用例改 `darwin-aarch64`    |
| `apps/desktop/src-tauri/src/updates.rs`                   | 常量保留 fork 的 `env!` 派生；默认 `Preview` + `official_mirror: false`；测试保留 fork 双仓库断言并纳入上游 download_site 断言（受 §2 门控）                     |
| `apps/desktop/src/update-model.ts`                        | `channel: "preview"`（保留注释）+ `official_mirror: false`                                                                                                       |
| `apps/desktop/src/About.test.tsx`                         | `channel: ready.preferences.channel` + `official_mirror` 取载入值                                                                                                |
| `apps/desktop/src/ServiceManager.tsx`                     | 两行都保留：fork `compatibility` + 上游 `privacyTrusted`                                                                                                         |
| `apps/desktop/src/App.navigation.test.tsx`                | 导入取并集；注释采用上游，保留 fork 的 `updateRoutingSettings` 断言                                                                                              |
| `apps/desktop/src/components/BuiltinToolsEditor.test.tsx` | 采用上游 `settleClosedLayer()`（fork 行是上游旧提交的 cherry-pick）                                                                                              |
| `core/contract/subscription.go`                           | 采用上游注释（`ModelNativeProtocol` → `BuiltinModelProtocol`）                                                                                                   |
| `core/internal/controlapi/handler.go`                     | `Dependencies` 同时保留 fork `IdentityCapture` 与上游 `RateLimits`                                                                                               |
| `core/internal/ingress/handler.go`                        | 导入保留 `identitycapture` 与 `imagecompress`                                                                                                                    |
| `core/internal/ingress/subscription_protection.go`        | 保留 fork 捕获块（`identityCaptureBodyLimit`、`captureIdentityCandidate`），采用上游 `requestRoutingSettings`，删除无调用者的旧 `subscriptionProtection(ctx)` 头 |
| `core/internal/storage/migrate/defaults.go`               | 上游 50–52 原样，fork 档案迁移移到 53（见 §4）                                                                                                                   |

每个文件解决后执行
`git diff v0.2.10 -- <path>`，剩余差异必须能解释为 fork 能力或有注释的偏离。

## 2. 更新来源只走本仓库（D1）

上游镜像涉及：`DOWNLOAD_SITE`、`MIRROR_RELEASE_API`、`download_site_asset`、
`official_asset`、`download_candidate(.., mirror)`、`fetch_mirror_releases`、
`UpdatePreferences.official_mirror`、`reschedules`、About 页开关与限流按钮。

方案（保留上游代码结构，只加一个门控点）：

1. `build.rs` 在已解析的发布仓库基础上再输出
   `cargo:rustc-env=ASTRLINK_OFFICIAL_MIRROR=1|0`，仅当仓库为
   `Calcium-Ion/AstrLink` 时为 1。规则放在 fork 自有的 `release_repository.rs`
   中，与仓库解析在同一处。
2. `updates.rs` 新增
   `const OFFICIAL_MIRROR: bool`（读取该 env），并加 fork 注释。生效值统一为
   `prefs.updates.official_mirror && OFFICIAL_MIRROR`，用在 `check`（`mirror`
   变量）、 `download_candidate` 调用处和 `endpoint` 选择处。`official_asset` 的
   `download_site_asset` 分支同样受 `OFFICIAL_MIRROR` 门控，fork 构建不会接受
   `download.astrlink.com` 资产。
3. 修正上游绕过 fork 间接层的硬编码：`download_candidate` 的前缀改为
   `format!("/{REPOSITORY}/releases/download/")`，GitHub 分支 URL 改为
   `https://github.com/{REPOSITORY}/...`（fork 的 `REPOSITORY`
   是 slug，上游是完整 URL）。
4. 偏好兼容：保留 `official_mirror` 字段与默认 `false`，旧偏好文件含 `true`
   仍可反序列化（`deny_unknown_fields` 不受影响），只是不生效。`set_preferences`
   不改写用户已存的值。
5. 前端：快照新增 `official_mirror_available: bool`（Rust `UpdateSnapshot` 与 TS
   `UpdateSnapshot`/`browserUpdateSnapshot` 同步加字段，浏览器默认
   `false`）。About 页在 `!official_mirror_available`
   时不渲染镜像开关和「改用官方镜像」按钮。i18n 键保留（上游文件，不删）。

测试：

- Rust：fork 配置下 `OFFICIAL_MIRROR == false`；`download_candidate(.., true)`
  仍返回本仓库URL；`official_asset` 拒绝 download_site；在
  `ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink`
  下上游镜像测试原样通过。上游依赖镜像的测试按 `OFFICIAL_MIRROR`
  分支断言，不删除。
- 前端：About 测试覆盖 `official_mirror_available=false`
  时开关和限流按钮都不出现，`true` 时保留上游行为。

## 3. 发布流水线（D3 与隐性断裂）

- `release-updates.mjs`：上游新代码引用已删除的模块常量
  `repository`（`verifiedPlatform`、
  `uploadAssets`、`attachRelease`），在 ESM 中会抛
  `ReferenceError`。按研究 §4 的 6 步处理：
  `verifiedPlatform`/`collectRelease`/`extendRelease` 接收 `repository` 与
  `macosSigningMode`
  参数并保留 fork 的两项校验；`uploadAssets(repository, ...)`； `attachRelease`
  使用 `publishingRepository(env)`；`extendRelease` 不重复添加 adhoc 前缀。
- `release.yml`：保留上游 `require-ci-success.sh` 门禁。fork 的
  `workflow_dispatch` 重试时 `github.sha` 不是标签提交，所以改为用已检出标签的
  `git rev-parse HEAD` 作为 SHA，并加 fork 注释。
- `release-macos-x86_64.yml`（D3 选 A）：
  - 去掉 `if: github.repository == 'Calcium-Ion/AstrLink'`。
  - 顶层加 `ASTRLINK_RELEASE_REPOSITORY: ${{ github.repository }}` 和
    `MACOS_SIGNING_MODE: ${{ vars.MACOS_SIGNING_MODE }}`。
  - 不加 `workflow_dispatch`。
  - 执行时核对 `attach` 步骤读取的环境变量。
- fork 自有门禁 `release-updates.test.ts`：
  - workflow 绑定列表加入 `release-macos-x86_64`。
  - Windows checkout 计数从 2 改为 1。
  - 新增 extend/attach 的仓库与签名模式不匹配用例。

## 4. 迁移 v50 碰撞

- `defaults.go`：上游 `{50 config_sync}`、`{51 request_privacy_ignored}`、
  `{52 privacy_stand_ins}` 原样；fork 改为
  `{53 service_identity_profiles}`，保持最后一项，语句保留 `IF NOT EXISTS`。fork
  v50 已推送，所以按指南上移 fork，而不是改上游。
- `identity_profile_history.go`：
  - 档案门控改为 53。
  - `claimed` 认领 `50 config_sync`，升序循环 `{48, 49, 50}`。
  - `claimedVersionApplied(50)` 对 `config_sync`
    的 27 个对象（5 表、1 索引、21 触发器）做全有/全无探测：全无则执行上游 50 语句，全有则只改历史名，部分存在则
    `ErrMigrationHistory`。
  - 其他不匹配继续 fail closed。
  - 可选：若 50 被 fork 占用且存在 >50 的行，fail closed。
- 测试：
  - `identity_profile_history_test.go:239` 的 `profiles.Version - 1`
    改为按名查找。
  - `identity_profile_store_test.go:37` 改为按名定位。
  - 新增 fixture：fork_50、fork_50_applied、fork_50_partial、fork_50_missing_table、unknown_50、upstream_sync、upstream_stand_ins。
- 影响：已应用 fork
  v50 的库首次启动即完成协调；之后旧 fork 构建和上游 v0.2.10 都会拒绝该库（`ErrDatabaseNewer`）。只在临时目录测试，不触碰真实库。

## 5. 转发身份修复（R6）

`core/internal/subscription/droid.go` 的 `droidUsage` 调用
`accountauth.ApplyDroidAPIHeaders`，该函数会为 EU 账号写入网关本地 overlay
`X-AstrLink-Factory-Endpoint`。usage 请求直接用 `HTTPClient.Do`
发出，不经过 forwarder 的剥离，于是该头被发往 Factory。

修复：在 `ApplyDroidAPIHeaders` 之后紧跟
`request.Header.Del(providerapi.DroidEndpointHeader)`，与同包 `droidAPIRequest`
的做法一致（最小增量，不改共享函数语义）。回归：使用 loopback
stub，以 EU 与非 EU token 调用 usage，断言最终收到的请求头不含任何
`X-AstrLink-*`，且 `Authorization`/`User-Agent` 符合 Droid 身份。

合并后另扫一遍：在 `core` 生产代码中
`git grep -n -i -e 'X-AstrLink' -e 'astrlink'`，与出站请求构造点交叉核对，确认无新增品牌注入。

## 6. 日语目录（D2）

在 `apps/desktop/src/i18n/locales/ja.json` 补齐 `compatibility.*`（42 个）与
`serviceTest.*` （4 个），并更新
`batchTest.successCount`、`batchTest.state.success`、`serviceTest.success`
为「请求完成」语义。占位符（`{{count}}`
等）与 en 保持完全一致。键序遵循 en 的结构。

## 7. 配置同步与身份档案（D4）

- 现状保持：同步对服务用 upsert（`sync_apply.go:320-323`），不触发级联删除。同步删除服务时级联删除本机档案，与本地删除行为一致。档案不进入同步包。
- 新增回归（`core/internal/...`
  中真实存储与 loopback 上游）：写入一个携带不存在的 `identity_profile_id`
  的同步服务，断言推理、连接测试、模型发现都返回明确的档案缺失错误，且上游 stub 未收到请求。
- 在 `decodeSyncedService`
  附近（或 fork 自有的档案读取处）加注释，说明：档案是设备本地数据；其他设备需各自配置；副设备解绑会经同步回写原设备。

## 8. 明确延期

- 网页搜索（`service_search`）、`builtinServiceImages`：走共享 transport，剥离
  `X-AstrLink-*`，但不应用 fork 请求规则和身份档案。本次不扩展，另开任务统一处理。
- 混合设备（上游原版 + fork）同步时，上游端会因 `DisallowUnknownFields`
  跳过带档案字段的服务文档。不泄露信息，记录即可。

## 9. 回滚

- 未提交前：`git merge --abort`（需用户确认），不触碰任务和工作区记录。
- 提交后：`git revert -m 1 <merge>`，不强推、不改写历史。
