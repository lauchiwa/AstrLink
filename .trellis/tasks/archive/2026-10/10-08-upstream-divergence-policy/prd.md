# 上游同步分歧处理：以上游逻辑为准，二开功能适配

## 背景

本仓库是 `Calcium-Ion/AstrLink`
的 fork，会持续同步上游。本次同步（`upstream/main` =
`69fce40`，17 个提交，145 文件）出现 6 处冲突，解冲突时需要在「保留我方实现」与「保留上游实现」之间反复取舍。缺少统一原则会让每次同步都重新讨论，并且我方改动越偏离上游，后续冲突面越大。

## 原则

本地修改与上游产生分歧时：

1. **优先保留上游的功能逻辑**：步骤形状、命令、默认值、数据结构语义尽量与上游一致，不因我方偏好改写。
2. **由二开功能去适配**：我方能力的实现落在 fork 自有文件中；对上游文件只做最小、可叠加的改动。
3. **删除上游步骤是最后手段**：删掉的 hunk 会在每次同步重新冲突。
4. **例外必须就地写明理由**：无法保留上游逻辑时（上游做法会让我方功能或 CI回归），在代码/配置处注明是有意偏离及原因，避免被当作疏漏改回。
5. **门槛与测试属于我方资产时，由它适配上游**，不得反过来约束上游的演进。

## 范围

### 1. 原则落入 spec

写入 `.trellis/spec/guides/`，使后续会话无需重新讨论。

### 2. 重做 `macos-package.yml` 的冲突解法

本次合并我采取的解法是「丢弃上游的 `Notarize, staple and verify app and DMG`
步骤，改用我方 `macos-release.mjs verify`」。虽然 `verifyMacOSRelease`
在 developer-id 模式下调用的是同一个
`notarizeMacOS`（功能等价），但删掉上游步骤违反原则 3。

需改为：

- 上游的三个步骤（`Build release app` / `Bundle ... DMG` /
  `Notarize, staple and verify app and DMG`）保持上游的名称与命令形状。
- 我方「无付费证书也能发布」的能力通过 fork 自有文件适配：把 `macos:notarize`
  指向 fork 的 `macos-release.mjs`，模式判定留在代码里（`macOSSigningMode`
  对非法值 fail closed），**不要**用 YAML `if:` 判定 `env.MACOS_SIGNING_MODE`
  —— 该变量为空时模式取自
  `release-repository.json`，用 YAML 判定会让公证被静默跳过。
- 签名策略配置（`--config src-tauri/tauri.signing.conf.json`）作为对上游命令的 additive 改动保留。

已知例外（按原则 4 就地注明）：`TAURI_BUNDLER_DMG_IGNORE_CI: "true"`。上游设为 true 以应用 DMG 的 Finder 布局并靠重试兜底；我方
`c613b6d` 的证据是 Intel
CI 会因依赖 Finder 失败。本机无法验证 CI 行为，暂保留我方做法并注明，待真实 CI 上复核。

### 3. 修正签到 compat 准入门槛

`TestForkCheckinCompatBaselineUpgradeAndDisabledReopen` 当前失败。差异（新增
`audit_chunks`/`audit_part_chunks`、`audit_blobs` 新增 `layout` 列、
`audit_settings` 的 1MiB→32MiB、`schema_migrations`
行变化）**全部来自上游迁移49**，没有一项来自签到扩展。

根因：该门槛把基线钉在扩展提交前的
`e7ebad2`，隐含要求「基线与当前的主迁移集合完全相等」。主 schema 一旦前进，旧 Core 就按设计抛
`ErrDatabaseNewer`
（日志：`database=50 core=49`）。因此任何一次上游同步都会打破它。

按原则 5，这是我方门槛，应由它适配：

- 扩展足迹的比对基准改为**当前 Core 在播种扩展之前**的快照，真正隔离「扩展是否改动了基表」，而不是与旧基线二进制的报告比对。
- 旧读取器那一支改为断言**优雅拒绝**（`ErrDatabaseNewer`），而不是断言能成功打开；主 schema 前进时拒绝才是正确行为。
- 保留原钉子注释的用意：基线不得等于被测实现，以免门槛变空。

## 验收标准

1. `.trellis/spec/guides/` 中存在该原则文档，覆盖上述 5 条与「例外需就地注明」。
2. `macos-package.yml` 中上游三个步骤的名称与命令形状与 `upstream/main` 一致，
   `git diff upstream/main -- .github/workflows/macos-package.yml`
   的差异仅为 additive 的签名策略配置与注明理由的 IGNORE_CI 偏离。
3. `release-updates.test.ts`
   的断言仍能约束：公证不得无条件执行、签名策略配置不得丢失、不得设
   `IGNORE_CI`，且三项突变各自转红。
4. `TestForkCheckinCompatBaselineUpgradeAndDisabledReopen`
   通过，且突变验证证明它仍能抓到「扩展改动基表」与「基线等于被测实现」。
5. Go `gofmt`/`vet`、Rust `fmt --check`/clippy/test、前端
   `tsc`/vitest 全部通过，仅允许本机缺 bun 导致的 `release-updates.test.ts`
   环境失败。
6. 合并提交完成，**不 push**。

## 不在范围

- 推送到任何远端。
- 原生登录窗口相关的增强性待办（C07/R03/R04/V02/V03）。
- 在真实 CI 上复核 IGNORE_CI 偏离（需要 CI 运行，另行跟踪）。
