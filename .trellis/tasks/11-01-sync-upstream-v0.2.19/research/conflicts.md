# v0.2.19 试合并冲突与合并后检查

试合并：一次性 worktree `../AstrLink-trial-0219`（`origin/main` = `9619d5b`），
`git merge --no-ff --no-commit v0.2.19`，12 个文件文本冲突，以 diff3 查看。

## 逐文件方案

上游本轮的主线是 Google Vertex
AI（`ec8fba3`、`8801847`）与提供商测试取消（`a57d6f5`）。fork 冲突侧几乎都是请求规则（`ExtraHeaders`
/ `ModelRules` / `IdentityProfileID`）与「请求完成」文案。

| 文件                                            | 块  | 方案                                                                                                                                                                                                                       |
| ----------------------------------------------- | --- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `core/contract/service.go`                      | 1   | `HTTPConnection` 两侧字段都保留：上游 `Vertex` 放在 `ModelListPath` 后，fork 字段及注释其后                                                                                                                                |
| `core/internal/controlapi/services.go`          | 4   | 输入结构体两侧字段合并；草稿探测与 `connection =` 两处构造同时带 `Vertex` 和 fork 三字段，保留 fork 的 `ValidateRequestRules`；patch 白名单 `switch` 增加 `"vertex"`                                                       |
| `core/internal/servicemodel/prober.go`          | 3   | `Prober` 增加上游 `vertex` 字段；`NewWithDependencies` 保留 fork 重定向检查与 `Dependencies`，返回值加 `vertex: vertexauth.New(client)`（与上游 `New` 同源 client）；保留上游 `WithVertexTokens`；第三块见下文 Vertex 探测 |
| `core/cmd/astrlink-core/persistent.go`          | 1   | `persistentServiceModels(store, subscriptionManager).WithVertexTokens(vertexTokens)`，与推理共用令牌缓存                                                                                                                   |
| `core/internal/storage/sqlite/service_cache.go` | 1   | 两行都留：fork `cloneForkHTTPConnection(service.HTTP)` 与上游 `Vertex` 指针克隆                                                                                                                                            |
| `core/internal/storage/migrate/defaults.go`     | 1   | 上游 v56 原样；fork 档案移到 v57，见迁移一节                                                                                                                                                                               |
| `apps/desktop/src/service-model.ts`             | 3   | `interface HTTPServiceConnection extends RequestCompatibility` 前插入上游 Vertex 函数；已知键并集加 `"vertex"`；解析结果用上游 `auth` 变量，再展开 `parseRequestCompatibility(connection, auth)` 和 `vertex`               |
| `apps/desktop/src/ServiceManager.tsx`           | 2   | 两侧 import 都留；`modelDiscoveryKey` 保留上游四项并追加 `draft.compatibility`                                                                                                                                             |
| `apps/desktop/src/ServiceTestResultView.tsx`    | 1   | 上游 `cancelled → neutral` 在前，其后保持 fork「成功也为 neutral」语义                                                                                                                                                     |
| `i18n/locales/{zh-CN,en,ja}.json`               | 2×3 | `stopping`/`stoppingShort` 取上游取消文案；`successCount`、`success` 保持 fork「请求完成」；上游新增 `cancel*`、`alreadyRunning`、`invalidId` 全部保留，fork `resultHelp`/`completionHint`/`empty*` 保留                   |

## 迁移 v56 撞号（第五次）

- 上游 v56 `install_defaults_biometric_subagent_links`；fork 已推送到
  `origin/main` 的 v56 `service_identity_profiles`（`b15748a`）。fork 的
  `v*-lauchiwa*` / `v0.1.9-rc.0` 标签都不包含 `b15748a`，但从 `origin/main`
  构建的开发版可能已记录 fork v56。
- fork 自 v0.2.18 起对 `defaults.go`
  只追加了档案一步（+13 行），未改隐私策略种子，所以上游 v56 的 `WHERE`
  条件对 fork 数据库同样成立。
- 上游 v56 的语句无法套用
  `createdObjectsApplied`：`config_change_policies_update`
  触发器迁移前后都存在，`raw_key_envelopes_new` 在步骤结束时已改名消失，其余是
  `UPDATE` 与 `ALTER TABLE`。需要专用探测，三项终态同时判断：
  `request_records.parent_session_id` 列、`request_records_parent_session_idx`
  索引、`raw_key_envelopes` 建表 SQL 含
  `'biometric'`。三项全无则重放，全有则只改名，其余 fail
  closed。整步在一个事务内，正常构建不会留下部分状态。

## 合并后检查（已在试合并树完成）

- 新出站请求：`git diff v0.2.18 v0.2.19 -- core` 中 `NewRequest`/`.Do(`
  的新增行都在测试里。生产新增出站为 Vertex 推理（经
  `forwarder.RoundTrip`）、Vertex 目录探测（`probeHTTPPages`）、Vertex 令牌交换（`golang.org/x/oauth2/jwt`，不设 AstrLink
  UA）。
- 新网关头 `X-AstrLink-Vertex-Project`：`endpoint/authorizer.go:178`
  写入 overlay， `providerapi.VertexRequest` 读取后删除，`forwarder.go` 之后还有
  `removeGatewayHeaders`。上游测试断言 stub 收到的请求不含 `X-AstrLink-Test`。
- `X-Goog-User-Project` 在 fork 规则里属于可覆盖头，但 `VertexRequest`
  在 overlay 之后设置，最终以连接配置为准。
- `Calcium-Ion`：上游新增行没有。`github.repository ==`：新增 workflow 改动只有
  `ci.yml`、`dependabot.yml`，未新增门禁。
- 新增上游 Rust 测试没有 `UpdateChannel::` 用法。

## Vertex 与 fork 请求规则（D1：目录探测也应用规则，见 PRD R7）

- 推理：`ingress/request_rules.go` 对所有 `IsHTTP()`
  服务应用 fork 规则，Vertex 属于 HTTP 类，所以推理会带上规则头和身份档案。
- 编辑器目录探测：上游 Vertex 分支在 `authorizationHeaders` 之前就
  `return prober.probeVertex(...)`，跳过 fork 的
  `requestrewrite.Compile`/`plan.Apply`。
- 聚合模型列表：`ingress/discovery.go:223`
  对 Vertex 只合成已配置的模型，不请求上游。
- 桌面端「兼容性」页对所有非订阅类服务显示，包括 Vertex。
