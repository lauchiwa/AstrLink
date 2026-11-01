# 同步 GitHub 上游最新正式版 v0.2.19

## 目标

将 fork `main` 从已合入的 `v0.2.18` 同步到 `Calcium-Ion/AstrLink` 最新正式版
`v0.2.19`，获得上游功能与修复，同时保留 fork 定制。

## 背景与已确认事实

- 用户需求：「同步上游发布的最新版本」；已同意创建本任务进入规划。
- 2026-11-01 HTTPS 访问 github.com 失败，改用 SSH 拉取上游 `main`
  与标签，退出码 0。GitHub Releases API 确认最新正式版为
  `v0.2.19`（2026-10-26T08:41:25Z）， `gh api .../commits/v0.2.19` =
  `752791ad8feec21499a72199be0f1d292f2b5ee3`，与本地标签一致。
  `upstream/main`（`24ddf0d`）已超过该标签，本次只合并到标签。
- 开始时本地 `main` 落后 `origin/main` 95 个提交且无独有提交，已
  `git merge --ff-only` 到 `9619d5b`（含 v0.2.18 同步）。工作区干净。
- 合并基
  `2cba3d6`（v0.2.18）。上游新增 17 个提交、214 个文件（+16792/−2266），主要是 Google
  Vertex AI、压缩请求体、凭据默认保护、Touch
  ID 解锁原始数据、子代理会话分组、提供商测试取消。
- 试合并 12 个文件文本冲突，逐块方案与合并后检查结果见 `research/conflicts.md`。
- 迁移版本第五次冲突：fork 已推送的 v56 `service_identity_profiles` 与上游 v56
  `install_defaults_biometric_subagent_links` 撞号。
- 上游本轮修改的 README 随合并原样进入，不额外改动。

## 要求

- R1 合并目标：以非快进合并纳入 `752791a`，保留双亲历史；不重写已推送历史。
- R2 冲突解决：保留上游功能逻辑，fork 能力放在 fork 自有文件或既有间接层；无法保留上游逻辑之处加注释说明偏离原因。保留的 fork 定制：身份档案与捕获、请求规则、订阅身份开关；发布仓库间接层、默认 preview 频道、macOS
  adhoc 发布与签名配置；「请求完成」测试结果语义。
- R3 迁移：上游 v56 保持原样；fork `service_identity_profiles`
  从 v56 移到 v57。已记录 fork
  v56 的数据库首次启动时协调：上游 v56 终态全无则补跑、全有则只改名、部分存在则 fail
  closed；旧的 48/49/50/53 协调保持不变；fork 档案行之后还有记录的历史仍拒绝；其他未知历史仍 fail
  closed。不操作任何真实数据库。
- R4 转发身份：上游新增的 `X-AstrLink-Vertex-Project` 与 fork 的 `X-AstrLink-*`
  入口头不得到达 Vertex；Vertex 令牌交换与目录探测不带 AstrLink 标识。
- R5 测试结果文案：取消相关文案采用上游；成功语义保持 fork「请求完成」，三种语言一致，不放宽上游键一致性测试。
- R6 验证：受影响层执行 AGENTS.md 规定的格式、静态检查与测试；环境限制如实记录，不把未运行的检查记为通过，不为通过测试回退上游逻辑。
- R7
  Vertex 目录探测（D1，用户已选「目录探测也应用规则」）：服务编辑器探测 Vertex 模型目录时，与推理一样应用 fork 服务级规则（`extra_headers`、绑定的身份档案；按模型规则不参与，与其他服务的探测一致）。`Authorization`
  与 `X-Goog-User-Project` 仍以连接配置为准，与推理时 `VertexRequest`
  在规则之后设置的顺序一致。档案不可用时报配置错误，不静默探测。

## 范围外

- 推送、打标签、发布 Release、创建 PR；提交需在检查后单独征得同意。
- 修改 README（上游随合并带入的除外）。
- 合并 `upstream/main` 上 v0.2.19 之后的未发布提交。
- 既有延期项（网页搜索接入 fork 请求规则或身份档案）维持原状。

## 验收标准

- [ ] AC1 / R1：`9619d5b` 与 `752791a` 均为合并结果的祖先；`git ls-files -u`
      为空，无冲突标记，`git diff --check` 通过。
- [ ] AC2 / R2：12 个冲突文件按研究方案解决；`git diff v0.2.19 -- <path>`
      的剩余差异均可解释为 fork 能力或有注释的偏离。
- [ ] AC3 / R3：`migrate`
      包测试覆盖全新库、旧 fork-48/49/50/53/56 历史、上游 v56 全无/全有/部分存在、fork-56 后面还有记录的拒绝；按迁移名查找。补跑后的库与全新库的 v56 终态一致（列、索引、`raw_key_envelopes`
      CHECK、`config_change_policies_update` 触发器）。
- [ ] AC4 / R4：回环测试断言 Vertex stub 收到的请求头不含任何 `X-Astrlink-*`。
- [ ] AC5 / R5：`ServiceTestResultView`
      测试覆盖取消为 neutral、成功为 neutral；i18n 键一致性测试通过。
- [ ] AC6 / R6：各项检查命令及结果记录在 `implement.md`；阻断项注明原因与影响。
- [ ] AC7 / R7：`servicemodel`
      回环测试中，Vertex 目录 stub 收到配置的额外头与身份档案头；配置
      `X-Goog-User-Project` 规则时 stub 收到的仍是连接的项目；`*`
      按模型规则不出现；档案缺失时探测返回配置错误且 stub 未被请求。去掉 Vertex 分支的规则调用后该测试失败。

## 风险与需告知用户的影响

- 已应用 fork
  v56 的开发版数据库，新版首次启动完成协调；之后旧 fork 构建与上游 v0.2.19 都会因版本更新或历史不符而拒绝打开该数据库（预期行为，不做降级）。
- 上游 v56 会把未被保存过的默认隐私策略改为默认开启凭据保护，并重建
  `raw_key_envelopes`，这是上游行为，fork 数据库同样生效。
- 桌面 crate Clippy/测试需要 sidecar 产物，可能受环境阻断。
