# 同步 GitHub 上游最新正式版 v0.2.10

## 目标

将本地 fork `main` 从已合入的 `v0.2.0` 同步到 `Calcium-Ion/AstrLink` 最新正式版
`v0.2.10`，获得上游功能与修复，同时保留 fork 定制。

## 背景与已确认事实

- 用户需求：同步上游发布的最新版本；已同意创建本任务进入规划。
- 2026-10-15 重新 fetch 成功，GitHub Releases
  API 确认最新非预发布、非草稿 Release 为
  `v0.2.10`（2026-10-15T07:37:54Z），目标提交
  `e8b96003c4529a8c39e4c31e47033b10cf925c05`，即当前 `upstream/main` 头部。
- 本地 `HEAD` = `origin/main` =
  `1be7583f5beddf261bf4ad1a4cd41827a51aca98`（已推送）。工作区 141 个 `M` 为
  `core.autocrlf` stat 噪音，`git diff` 为空。
- 合并基为 `b3af635`：上游重写过 `v0.2.0` 标签（现为
  `cb10dda`），部分冲突是 v0.2.0 内容被重复应用。fork 自有 39 个提交，上游 89 个提交、499 个文件；上一轮未纳入的
  `5ceced5` 已包含在 v0.2.10 中。
- `git merge-tree --write-tree HEAD v0.2.10` 得到冲突树
  `bec8f2b521126b8f86f5458239ab6bcf5f060ee3`，16 个文件文本冲突；逐块分析见
  `research/`。
- 上游本轮修改 README（含新增 `README.ja.md`），随合并原样进入，不额外改动。

## 要求

- R1 合并目标：以非快进合并纳入固定提交
  `e8b9600`，保留双亲历史；不重写已推送历史。
- R2 冲突解决：保留上游功能逻辑，fork 能力尽量放在 fork 自有文件或既有间接层中，无法保留上游逻辑之处加注释说明偏离原因。保留的 fork 定制：
  - 身份档案与捕获、请求规则、Codex/Claude/Grok 订阅身份开关；
  - 发布仓库间接层、默认 preview 频道、macOS adhoc 发布；
  - SQLite 写入兼容修复。
- R3 迁移：上游 v50 `config_sync`、v51 `request_privacy_ignored`、v52
  `privacy_stand_ins` 保持原样；fork `service_identity_profiles`
  从已推送的 v50 移到 v53。已记录旧 v50 的数据库在首次启动时协调：`config_sync`
  对象全无则补建、全有则只改名、部分存在则 fail closed；其他未知历史仍 fail
  closed。不操作任何真实数据库。
- R4 更新来源（D1）：fork 构建不提供上游官方镜像更新。检查、下载、安装只走
  `ASTRLINK_RELEASE_REPOSITORY`
  对应的 GitHub 仓库；镜像开关和限流时的「改用官方镜像」入口不显示，已持久化的
  `official_mirror` 不生效且不导致偏好读取失败。
- R5 发布流水线（D3）：合并后的 `release-updates.mjs` 不再引用已删除的
  `repository` 常量；`release-macos-x86_64.yml` 在 fork 仓库运行，继续发布 Intel
  macOS 包，保持标签推送触发，不加
  `workflow_dispatch`。主 Release 的“标签提交 CI 通过”门禁在手动重试时检查被重建标签的提交。
- R6 转发身份：修复上游新增的 Factory Droid 用量请求在 EU 账号下把
  `X-AstrLink-Factory-Endpoint`
  发往 Factory 的问题，并对最终出站请求做回归断言。
- R7 日语目录（D2）：补齐 fork 独有的 46 个键（`compatibility.*` 42 个、
  `serviceTest.*`
  4 个）的日语译文；`batchTest.successCount`、`batchTest.state.success`、
  `serviceTest.success`
  改为与中英文一致的「请求完成」语义；不放宽上游键一致性测试。
- R8 配置同步与身份档案（D4）：不改同步语义、不同步档案。增加回归测试，固定「同步来的服务引用本机不存在的档案时，推理、连接测试、模型发现均不转发并明确报错」，并在代码注释中记录该限制。
- R9 验证：受影响层执行 AGENTS.md 规定的格式、静态检查与测试；环境限制如实记录，不把未运行的检查记为通过，不为通过测试回退上游逻辑。

## 范围外

- 网页搜索工具接入 fork 请求规则与身份档案（D5）：原样保留上游实现，与既有
  `builtinServiceImages` 一并列为已知延期项。
- 身份档案跨设备同步、同步语义修改（D4 选项 B/C）。
- Intel macOS 工作流的手动重试（D3 选项 B）。
- 推送、打标签、发布 Release、创建 PR、部署；提交需在检查后单独征得同意。
- 修改 README（上游随合并带入的除外）；收尾旧任务 `10-10-sync-upstream-latest`
  另行处理。

## 验收标准

- [ ] AC1 / R1：`1be7583` 与 `e8b9600` 均为合并结果的祖先；`git ls-files -u`
      为空，无冲突标记，`git diff --check` 通过。
- [ ] AC2 / R2：16 个冲突文件逐块按研究方案解决；对上游
      `git diff v0.2.10 -- <path>`
      的剩余差异均可解释为 fork 能力或有注释的偏离。
- [ ] AC3 / R3：`migrate` 包测试覆盖全新库、旧 fork-48/49/50 历史、`config_sync`
      全无/全有/部分存在、未知 50 名称；测试按迁移名而非下标查找。
- [ ] AC4 / R4：Rust 测试断言 fork 构建不访问 `astrlink.com`
      镜像端点、下载候选只来自本仓库，旧偏好文件含 `official_mirror: true`
      仍可读取且不生效；`About` 测试断言镜像开关与限流按钮不显示；在
      `ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink` 下 Rust 测试也通过。
- [ ] AC5 / R5：`release-updates.test.ts`
      覆盖 collect/extend/attach 路径、两种仓库、adhoc/developer-id 模式，以及
      `release-macos-x86_64` 的 workflow 绑定与无仓库门禁。
- [ ] AC6 / R6：Droid 用量请求在 EU 与非 EU token 下最终出站头都不含
      `X-AstrLink-*`；生产代码无新增 AstrLink 品牌出站头。
- [ ] AC7 / R7：`i18n/catalog.test.ts` 通过，3 条文案语义与中英文一致。
- [ ] AC8 / R8：悬空档案引用的回归测试通过，且断言请求未到达上游 stub。
- [ ] AC9 / R9：各项检查结果连同命令记录在
      `implement.md`；阻断项注明原因与影响。

## 风险与需告知用户的影响

- 本机已应用旧 fork
  v50 的数据库，新版首次启动会完成协调；之后旧 fork 构建与上游 v0.2.10 都会因版本更新而拒绝打开该数据库（预期行为，不做降级）。
- 多设备配置同步时，带身份档案绑定的服务在其他设备上不可用，需各自重新配置；在副设备解绑会被同步回原设备。
- 网页搜索在绑定身份档案的服务上使用共享 transport 默认身份，指纹与普通推理不一致。
- 上次任务的已知环境阻断（桌面 crate Clippy/测试需 sidecar、MSVC 14.39
  STL 符号、D 盘剩余约 12 GB、缺 Ruby/gcc）可能再次出现。
