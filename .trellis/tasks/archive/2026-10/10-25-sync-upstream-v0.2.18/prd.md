# 同步 GitHub 上游最新正式版 v0.2.18

## 目标

将本地 fork `main` 从已合入的 `v0.2.10` 同步到 `Calcium-Ion/AstrLink` 最新正式版
`v0.2.18`，获得上游功能与修复，同时保留 fork 定制。

## 背景与已确认事实

- 用户需求：「同步上游 v0.2.18」。
- 2026-10-25 `git fetch upstream --tags` 退出码 0；GitHub Releases
  API 确认最新正式版为 `v0.2.18`（2026-10-24T09:33:01Z），提交
  `2cba3d6c98e2ae57cc9e6b0e3308eb481f74766e`，即当前 `upstream/main` 头部。
- 本地 `HEAD` = `origin/main` = `e87d4e4`，工作区干净。合并基 `e8b9600`
  （v0.2.10）。上游 93 个提交、446 个文件；fork 自 v0.2.10 起 45 个提交、341 个文件；双方都改过 36 个文件。
- 试合并有 7 个文件文本冲突，逐块方案见 `research/conflicts.md`。
- 迁移版本第四次冲突：fork 已推送的 v53 `service_identity_profiles` 与上游 v53
  `session_cursor_lookup_order`、v54 `access_token_sync`、v55
  `request_output_activity` 撞号。
- 上游本轮修改 README，随合并原样进入，不额外改动。

## 要求

- R1 合并目标：以非快进合并纳入 `2cba3d6`，保留双亲历史；不重写已推送历史。
- R2 冲突解决：保留上游功能逻辑，fork 能力放在 fork 自有文件或既有间接层；无法保留上游逻辑之处加注释说明偏离原因。保留的 fork 定制：身份档案与捕获、请求规则、订阅身份开关；发布仓库间接层、默认 preview 频道、macOS
  adhoc 发布与签名配置；SQLite 写入兼容修复。
- R3 迁移：上游 v53/v54/v55 保持原样；fork `service_identity_profiles`
  从 v53 移到 v56。已记录旧 v53 的数据库首次启动时协调：上游 v53 对象全无则补建、全有则只改名、部分存在则 fail
  closed；旧的 48/49/50 协调保持不变；其他未知历史仍 fail
  closed。不操作任何真实数据库。
- R4 macOS 打包（上游 Core ML ONNX Runtime）：workflow 与 `package.json`
  同时传入上游 `tauri.onnxruntime.conf.json` 与 fork
  `tauri.signing.conf.json`；保留 fork `macos:notarize` 指向
  `macos-release.mjs verify`。
- R5 发布说明（D1，用户已选）：采纳上游「CI 前已写好的 Release 保留正文、latest.json 最后上传、已完整的 Release 不覆盖」。adhoc 模式下若正文缺少「macOS 未公证」提示，前置该提示并用
  `--notes-file`
  更新；正文已含提示时不重复添加、不改写。latest.json 说明与 Release 正文一致。
- R6 转发身份：上游新增的 `X-AstrLink-Service`
  入口头不得到达上游（HTTP 与 WebSocket）；新的媒体下载客户端不带 AstrLink 标识。
- R7 验证：受影响层执行 AGENTS.md 规定的格式、静态检查与测试；环境限制如实记录，不把未运行的检查记为通过，不为通过测试回退上游逻辑。

## 范围外

- 推送、打标签、发布 Release、创建 PR；提交需在检查后单独征得同意。
- 修改 README（上游随合并带入的除外）。
- 上游新功能接入 fork 请求规则或身份档案（如网页搜索）的延期项维持原状。

## 验收标准

- [ ] AC1 / R1：`e87d4e4` 与 `2cba3d6` 均为合并结果的祖先；`git ls-files -u`
      为空，无冲突标记，`git diff --check` 通过。
- [ ] AC2 / R2：7 个冲突文件按研究方案解决；对 `git diff v0.2.18 -- <path>`
      的剩余差异均可解释为 fork 能力或有注释的偏离。
- [ ] AC3 / R3：`migrate`
      包测试覆盖全新库、旧 fork-48/49/50/53 历史、上游 v53全无/全有/部分存在、fork-53 后面还有记录的拒绝；按迁移名查找。
- [ ] AC4 / R4：`release-updates.test.ts` 断言 macOS build/bundle 命令同时带两个
      `--config`；`package.json` 的 `macos:notarize` 仍为 fork 包装脚本。
- [ ] AC5 / R5：测试覆盖 adhoc/developer-id
      × 新建/预写草稿/预写已发布 ×正文含/不含提示，以及两种仓库；已完整 Release 拒绝覆盖。
- [ ] AC6 / R6：回环测试断言上游 stub 收到的请求头不含 `X-AstrLink-Service`。
- [ ] AC7 / R7：各项检查命令及结果记录在 `implement.md`；阻断项注明原因与影响。

## 风险与需告知用户的影响

- 本机已应用 fork
  v53 的数据库，新版首次启动完成协调；之后旧 fork 构建与上游 v0.2.18 都会因版本更新而拒绝打开该数据库（预期行为，不做降级）。
- 桌面 crate Clippy/测试需要 sidecar 产物，可能再次受环境阻断。
