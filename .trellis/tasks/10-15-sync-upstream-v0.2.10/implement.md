# v0.2.10 同步执行计划

## 当前门禁

用户已批准最终方案，`task.py start`
已运行。合并已解决并暂存，尚未提交；提交、推送、打标签、发布均未授权。实际结果见文末「执行记录」。

## 1. 执行前检查

- [ ] `git fetch upstream --tags`、`git fetch origin` 分别检查退出码；确认
      `v0.2.10` 仍为 `e8b9600`、`origin/main` 仍为
      `1be7583`、最新 Release 未变化。变化则返回规划。
- [ ] `git diff --name-only`、`git ls-files -u`
      为空（autocrlf 噪音不算改动，不做重归一化提交）。
- [ ] 确认 D 盘剩余空间（当前约 12 GB）与工具链：`go version`、`bun --version`、
      `cargo --version`、`cargo fmt --version`、`cargo clippy --version`、`ruby`、`gcc`。

## 2. 合并与冲突解决（按 design §1）

- [ ] `git merge --no-ff --no-commit v0.2.10`。
- [ ] Go：`subscription.go`、`controlapi/handler.go`、`ingress/handler.go`、
      `subscription_protection.go`。
- [ ] 迁移：`defaults.go` → design §4，同步修改 `identity_profile_history.go`
      与测试。
- [ ] 发布：三个 workflow、`release-updates.mjs`、`release-updates.test.ts` →
      design §1/§3。
- [ ] 更新器：`updates.rs`、`update-model.ts`、`About.test.tsx` → design §1/§2。
- [ ] 桌面 UI：`ServiceManager.tsx`、`App.navigation.test.tsx`、`BuiltinToolsEditor.test.tsx`。
- [ ] 冲突标记扫描：`git grep -n -E '^(<<<<<<<|=======|>>>>>>>)( |$)'` 无结果；
      `git ls-files -u` 为空。

## 3. 合并后适配

- [ ] D1：`build.rs`/`release_repository.rs` 输出
      `ASTRLINK_OFFICIAL_MIRROR`；`updates.rs` 门控与 `REPOSITORY`
      硬编码修正；快照新增 `official_mirror_available`；About 页隐藏开关与按钮。
- [ ] D3：`release-macos-x86_64.yml` 去门禁、补 env；`release.yml`
      CI 门禁 SHA 改为标签提交。
- [ ] R6：`subscription/droid.go` 删除
      `X-AstrLink-Factory-Endpoint` + 出站回归测试。
- [ ] D2：`ja.json` 补 46 键并更新 3 条文案。
- [ ] D4：悬空档案引用回归测试 + 注释。
- [ ] 间接层扫描：
      `git grep -n -i calcium-ion -- <桌面源码、脚本与 .github>`（路径见 upstream
      sync 指南）；生产代码只允许出现在镜像门控判断中；测试中的出现须断言两种仓库。
- [ ] 出站身份扫描：`git grep -n -i -e 'X-AstrLink' -e 'User-Agent' -- core ':!*_test.go'`，逐条确认新增出站点不注入品牌、本地头不外发。

## 4. 格式与静态检查（AGENTS.md）

文件清单来自
`git diff --name-only 1be7583 -- <类型>`（含上游带入文件），跳过删除文件。

- [ ] Go：`gofmt -w`，复查 `gofmt -l` 无输出；`core`、`convo`、`contracts` 各自
      `go vet ./...`。
- [ ] JS/TS：`bunx oxlint@1.19.0 --fix --deny-warnings`、`bunx prettier@3.6.2 --write`；复查无
      `--fix` 与 `--check`；`(cd apps/desktop && bun run typecheck)`。
- [ ] JSON/YAML/CSS/HTML：Prettier `--write` 后 `--check`。
- [ ] Markdown（任务文件）：Prettier `--prose-wrap always` + markdownlint。
- [ ] Rust：三个 crate `cargo fmt --all` / `-- --check`；worker
      crate 与桌面 crate 运行 Clippy
      `-D warnings`（桌面 crate 若因 sidecar/MSVC/磁盘阻断，按指南记录为环境阻断）。
- [ ] `git diff --check`、`git diff --cached --check`。

## 5. 测试矩阵

```bash
bun scripts/check-no-production-models.mjs
(cd core && ASTRLINK_CI_NO_REMOTE_MODELS=1 go test ./...)
(cd convo && go test ./...)
(cd contracts && go test ./...)
(cd apps/desktop && bun install --frozen-lockfile && bun run test)
(cd apps/desktop && bun run test scripts/release-updates.test.ts \
  scripts/macos-release.test.ts src/i18n/catalog.test.ts src/About.test.tsx)
(cd apps/desktop/src-tauri && cargo test --locked --lib updates::)
(cd apps/desktop/src-tauri && \
  ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink \
  cargo test --locked --lib updates::)
ruby contracts/validate.rb   # 缺 Ruby 则记录
```

重点包：`internal/storage/migrate`、`internal/storage/sqlite`、`internal/subscription`、
`internal/accountauth`、`internal/ingress`、`internal/controlapi`、`internal/identitycapture`、
`internal/servicemodel`、`internal/servicetest`、`internal/transport`、`internal/configsync`
（实际包名以合并后为准）、`cmd/astrlink-core`。

- [ ] AC3 迁移矩阵全部通过。
- [ ] AC4 更新器 fork/上游两种仓库配置均通过。
- [ ] AC5 发布脚本与 workflow 门禁测试通过。
- [ ] AC6 Droid EU/非 EU 最终出站头断言通过。
- [ ] AC7 i18n 键一致性通过。
- [ ] AC8 悬空档案回归通过。
- [ ] 不能运行的项（race/gcc、Ruby、原生壳布局、sidecar）写明命令与原因，不记为通过。

## 6. 评审与提交门

- [ ] 派发 `trellis-check` 做全范围检查，主会话复跑关键命令核实。
- [ ] 逐文件 `git diff v0.2.10 -- <冲突文件>`，确认剩余差异都可解释。
- [ ] 确认 `1be7583` 与 `e8b9600` 都是合并结果的祖先。
- [ ] 加载
      `trellis-update-spec`：把 v50 第三次撞号和「上游新增出站点绕过 overlay 剥离」写进
      `upstream-sync-thinking-guide.md`（若确有新知识）。
- [ ] 汇总提交方案与文件清单，请用户确认后提交；不 amend、不 push。

## 回滚点

- 合并未提交：`git merge --abort`（先征得用户同意）。
- 已提交：`git revert -m 1 <merge>`。

## 执行记录（2026-10-15）

### 合并

- 两个 fetch 退出码均为 0；`v0.2.10` = `e8b9600`，`origin/main` = `HEAD` =
  `1be7583`，与规划一致。
- 首次 `git merge` 因 autocrlf
  stat 噪音被拒（`Please commit your changes`），未改动任何文件；该次尝试刷新了 stat 缓存，噪音清零后重试成功。
- `git merge --no-ff --no-commit v0.2.10`：16 个冲突文件与预检一致，已全部解决；
  `git ls-files -u` 为 0，无冲突标记，`MERGE_HEAD` = `e8b9600`。
- 预检未发现的语义冲突：上游把 `ProbeHTTP` 返回值由 `[]string` 改为 `Discovery`
  结构体，fork 的请求规则/身份档案分支仍 `return nil`，导致 `servicemodel`
  编译失败。已改为 `Discovery{}`，fork 自有测试改读 `.Models`。

### 格式与静态检查

- 通过：`go vet ./...`（core、convo、contracts）；变更 Go 文件按 LF 内容
  `gofmt -l` 为空（`controlapi/handler.go` 已 `gofmt -w`）。工作区 CRLF 导致的
  `gofmt -l` 误报不计。
- 通过：205 个变更 JS/TS 文件 `bunx oxlint@1.19.0 --deny-warnings`
  0 警告；221 个 JS/TS/JSON/YAML 文件 Prettier
  `--check`（按 LF 内容；工作区 CRLF 误报不计）。
- 通过：`(cd apps/desktop && bun run typecheck)`。
- 通过：三个 Rust crate `cargo fmt --all -- --check`；`apps/privacy-worker`、
  `apps/classifier-worker`
  `cargo clippy --locked --all-targets -- -D warnings`。
- `git diff --cached --check`：仅上游原样带入的 `README.ja.md`
  报 21 处行尾空格（Markdown 硬换行，与 v0.2.10 完全一致，不改）；排除后通过。

### 测试

- 通过：core 全量 `go test ./... -count=1`（启用
  `ASTRLINK_CI_NO_REMOTE_MODELS=1`），46 个包全部 ok。
- 通过：`convo`、`contracts` 的 `go test ./...`。
- 通过：`bun scripts/check-no-production-models.mjs`。
- 通过：`(cd apps/desktop && bun run test)`，157 个文件、1837 个测试通过，1 个既有 skip。
- AC3：`migrate`
  覆盖 fresh、upstream_timing/chunks/sync/stand_ins、fork_48/49/50 及其
  `_applied`、fork_50_partial、fork_50_missing_table、fork_50_successor、unknown_50；测试按迁移名查找。全部通过。
- AC6：`TestDroidUsageSendsNoGatewayHeaders` 在 EU/非 EU
  token 下断言最终出站头不含
  `X-Astrlink-*`。出站扫描：本次新增生产代码中无 AstrLink 品牌头；
  `X-AstrLink-Factory-Endpoint` 仅为本地 overlay 常量。
- AC7：`i18n/catalog.test.ts` 通过；ja 与 en 键集一致（3456），占位符一致。
- AC8：`TestSyncedServiceWithUnknownIdentityProfileFailsClosed` 走真实
  `SyncWriteEntity` + `SyncBindCredentialRefs`，断言推理非 200、连接测试
  `invalid_configuration`、模型发现与聚合列表不返回该服务、上游零请求。去掉悬空引用后测试失败（转发成功），确认断言有效。
- AC4（部分）：`release_repository.rs` 单元测试 3/3 通过（`official_mirror` 只对
  `Calcium-Ion/AstrLink` 为真）；`About.test.tsx`、`update-model.test.ts` 通过。

### 环境阻断（未计为通过）

- 桌面 crate `cargo test --lib updates::`（含
  `ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink`
  变体）与 Clippy：`tauri_build` 报
  `binariesstrlink-privacy-worker-x86_64-pc-windows-msvc.exe doesn't exist`。
- `bun run sidecar:build`：privacy-worker release 链接失败，`libort_sys` 报
  `LNK1120: 19 个无法解析的外部命令`（`__std_min_element_d`、`__std_max_8u`
  等），与上一轮相同的 MSVC STL 工具集不匹配。worker crate
  Clippy 通过，说明 Rust 工具链本身正常。影响：`updates.rs`
  的镜像门控 Rust 测试未在本机执行，需在 CI 或具备新版 MSVC 的机器上运行。
- `ruby` 不在 PATH：`contracts/validate.rb` 未运行（`validate.rb`
  自动合并，仅上游改动）。
- `gcc` 不在 PATH：`-race` 未运行。
- 原生壳布局测量未做（本次未改布局，About 页仅条件隐藏一行）。
- D 盘剩余约 8.7 GB。

### 间接层扫描

- `git grep -i calcium-ion`：生产命中仅 `release_repository.rs` 的
  `UPSTREAM_REPOSITORY` 镜像门控常量、`updates.rs` 注释，以及 fork 未改动的
  `.github/ISSUE_TEMPLATE`、PR 模板、`docker.yml`（合并前已存在）。
- `release-updates.mjs` 无裸 `repository` 标识符；模块可正常导入并导出
  `attachRelease`/`extendRelease`/`collectRelease`。

### 复核（trellis-check）

检查子代理审完 `updates.rs`
后因上游 API 错误中止（该部分无问题），其余项由主会话逐项复核，未发现新缺陷、未改文件：

- 发布：`release-macos-x86_64.yml` 顶层 env 供 validate/attach 读取，reusable
  `macos-package.yml` 自带同名 env；`wait-for-release.sh` 按 tag push 的
  `head_sha` 过滤，与标签推送触发一致；`release.yml` 门禁用已检出标签的提交。
  `extendRelease` 不重复加 adhoc 前缀，`uploadAssets`/`attachRelease`
  都显式传仓库。
- 迁移：仅认领 48/49/50 三个已知碰撞；后继版本、残缺档案表、`config_sync`
  部分存在均 `ErrMigrationHistory`；协调在 `Up` 事务内，`RequireCurrent`
  不协调。
- 转发身份：`ApplyDroidAPIHeaders` 的调用方中，`droidAPIRequest` 与 `droidUsage`
  显式删除 overlay 头，authorizer 路径（推理、发现、内置工具、网页搜索）都经
  `forwarder.Forward` 剥离 `X-AstrLink-*`。
- 16 个冲突文件对 `v0.2.10` 的剩余差异均为 fork 能力或带注释的偏离。
