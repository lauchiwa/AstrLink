# 同步上游 v0.1.9

## 背景

上一次同步（`873e98b`）合入的是上游 `69fce40`，早于上游打出的 `v0.1.9`
（`78cc0dc`）。当时本地 `upstream/main` 过期：`git fetch` 超时，退出码被管道里的
`tail` 掩盖，误判为已同步到 v0.1.9。按版本命名规则，`v0.1.9-rc.0`
必须建立在已同步的上游 `0.1.9` 之上，因此先同步，再发布。

上游缺失的两个提交：

- `e0c5ade`：更新检查识别 GitHub 限流并给出 `retry_at`；stable 频道改读
  `releases/latest/download/latest.json`；发布时用 `newestStable` 决定
  `--latest`；迁移 49 中 `audit_blobs.layout`
  的列级 CHECK 改为触发器，避免大库在桌面端就绪超时内迁移不完。
- `78cc0dc`：导航测试的焦点处理。

## 目标

合入上游 `v0.1.9`，上游逻辑完整保留，fork 功能不回退；CI 全绿后发布
`v0.1.9-rc.0`。

## 必须保留的 fork 功能

- 发布与更新来源跟随
  `release-repository.json`（`lauchiwa/AstrLink`），不指向上游。
- 默认更新频道 `preview`；stable 过滤（`updates.rs` 中
  `select_release`）不放宽。
- macOS adhoc 发布流程、`repository` 感知的更新 URL 与 About 页仓库链接。
- 迁移 50 `service_identity_profiles` 与 `identity_profile_history.go`
  版本对账。
- `core/go.mod` 的 `go 1.26.9`。

## 验收标准

1. `main` 包含上游 `v0.1.9`：`git merge-base --is-ancestor v0.1.9 main` 成立。
2. 三处冲突按上游优先解决，双方新增内容都在（见 `design.md`）。
3. 合并后 `apps/desktop` 生产代码中不新增指向 `Calcium-Ion/AstrLink`
   的写死地址；测试中的上游仓库名只用于「两个仓库都要通过」的参数化用例。
4. fork 构建下 stable 频道的 manifest
   URL 与 tag 解析都指向 fork 仓库，有单测覆盖两个仓库。
5. 合并后相对 `upstream/main`
   的差异中，fork 部分与合并前一致，只多出本次为适配所需的改动，每一处都能说明原因。
6. 全量验证通过：Rust
   fmt/clippy/`cargo test --lib`；前端 typecheck、oxlint、prettier、vitest（含
   `scripts/release-updates.test.ts`）；Go 三模块 vet/test，迁移相关测试；`git diff --check`；推送后 CI 全绿。
7. 发布：打 `v0.1.9-rc.0`，Release workflow 成功，产物为 Pre-release，含
   `latest.json`。

## 不在范围

- 不改上游迁移 49 的新写法，不为已按旧写法迁移的库补触发器（上游的设计决定，注释已说明旧库保留列级 CHECK）。
- 不放宽 stable 过滤，不改默认频道。
- 不处理 `feat/headless-server` 分支。

## 已知行为（不是回归）

fork 只发布预发布版本，GitHub 的 `releases/latest`
不返回预发布版本，所以 fork 构建在 stable 频道下仍报「没有可用版本」，与同步前一致，已在版本命名规则中接受。
