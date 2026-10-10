# 正式版同步执行计划

## 当前门禁

用户已对最终方案回复“可以”，批准本地合并与验证。 `task.py start`
已运行，任务进入
`in_progress`。提交、推送及发布未获授权。以下未勾选项目仍待实际执行验证。

## 1. 规划与基线

- [x] 从历史会话恢复用户决定，并更新 `prd.md`。
- [x] 分别检查 origin/upstream fetch 退出码；通过 GitHub
      API 核对最新正式版和标签 SHA。
- [x] 固定目标 `78c97c4065ec5ba2263d6d32f9f53355567ea585`，记录原 HEAD
      `7165c7465eec555490da5b1689fa10e0f2d022af`。
- [x] `merge-tree` 确认 5 个文本冲突；保存启动依赖、SQLite 和契约风险研究。
- [x] 完成 planning artifact 格式检查与上下文校验。
- [x] 用户审阅最终摘要并明确批准实施。
- [x] 用户批准后执行
      `python ./.trellis/scripts/task.py start .trellis/tasks/10-10-sync-upstream-latest`。

## 2. 执行前检查与实施

- [x] `git status --porcelain`、`git diff --name-only`、`git ls-files -u`
      确认没有新增未知产品改动或残留合并。保留开始时已有任务目录和
      `.trellis/workspace/vsofolqh/`，不 stash、不擅自纳入提交。
- [x] 核对 HEAD、tag 和 origin 是否变化；若目标正式版或基线发生实质变化，返回规划复审。
- [x] 核实工具链与依赖。
  - 核对 Go/Bun 声明版本、Ruby/Make 可用性及 Rust/Tauri 编译与测试依赖。
  - 不降低 go.mod/package.json 要求，不擅自全局升级或安装系统包。
  - 可使用现有版本管理器或局部工具链，无法解决则记录阻断。
- [x] 记录 `git diff v0.2.0 HEAD --stat`
      与 fork 能力基线。如需可执行试合并，仅在一次性 detached
      worktree 中进行，不碰现有工作区改动。
- [x] 获取 `task.py current` 输出作为调度提示首行，派发
      `trellis-implement`；子代理按 design 直接实施，不递归派发 implement/check。
- [x] 使用
      `git merge --no-ff --no-commit 78c97c4065ec5ba2263d6d32f9f53355567ea585`。预期冲突退出不代表完成，直到逐个解决并验证。
- [x] 按 design 解决 package.json、rsbuild.config.ts、validate.rb、main.go、store.go。
- [x] 增加共享持久化初始化的 fork 身份 helper 与组合回归测试，保留 desktop/server 上游流程。
- [x] 适配 token
      write-first 锁对应的 fork 测试，保留上游并发测试与有界事务重试。
- [x] 对 actor 本地标记增加保留分类及最终 HTTP/WS 回归，检查其他 overlay 来源。
- [x] 逐个复核自动合并文件，尤其控制 API、模型发现、请求规则、订阅身份、Web
      edition 和更新路径。

## 3. 格式与静态检查

以下命令在执行阶段从仓库根目录运行；文件清单必须展开为**明确、带引号的受影响路径**，包括从上游纳入的变更与新增文件，跳过删除文件，不把无关文件混入修复。根据
`git diff`/合并索引与原 HEAD 的比较生成清单，不能仅检查最后一次手改文件。

- Go：对受影响 `.go` 执行 `gofmt -w`，复查 `gofmt -l` 无输出；三模块各自运行
  `go vet ./...`，诊断手动修复。
- JS/TS/JSX/TSX/MJS/CJS：`bunx oxlint@1.19.0 --fix --deny-warnings`，然后
  `bunx prettier@3.6.2 --write`；复查去掉 `--fix` 并使用 Prettier `--check`。
- JSON/JSONC/CSS/HTML/YAML：Prettier `--write`，然后 `--check`。
- Markdown：Prettier `--write --prose-wrap always`，
  `bunx --package markdownlint-cli@0.45.0 markdownlint --fix`；然后 Prettier
  `--check` 与 markdownlint 不带 `--fix`。
- Rust：在 `apps/desktop/src-tauri` 运行 `cargo fmt --all`、
  `cargo clippy --locked --all-targets --fix --allow-dirty --allow-staged -- -D warnings`、
  `cargo fmt --all`；复查 `cargo fmt --all -- --check` 和
  `cargo clippy --locked --all-targets -- -D warnings`。crate-wide 工具如触及无关文件，只保留本任务必要修复，不覆盖用户内容。
- Ruby：运行契约验证；项目未规定额外 Ruby 自动格式器，不引入新工具统一重排。

## 4. 功能检查矩阵

以下为验证矩阵；实际结果和未完成项见「第一轮实施与实际验证」。远程模型禁用，集成测试只使用临时数据库与本地 stub，不读取用户数据。

### Go、契约与模型安全

```bash
bun scripts/check-no-production-models.mjs
(cd core && ASTRLINK_CI_NO_REMOTE_MODELS=1 go test ./...)
(cd convo && go vet ./... && go test ./...)
(cd contracts && go vet ./... && go test ./...)
(cd core && go vet ./...)
ruby contracts/validate.rb
(cd core && go test ./contract -run TestDefaultCapabilitiesMatchesFrozenFixture)
```

优先定位的包：`cmd/astrlink-core`、`internal/identitycapture`、`internal/accountauth`、
`internal/controlapi`、`internal/ingress`、`internal/servicemodel`、`internal/servicetest`、
`internal/transport`、`internal/storage/sqlite`、`internal/storage/migrate`、
`internal/coreapp`、`internal/console`、`internal/subscription`。

- [x] 真实组合路径：共享捕获实例、候选确认/绑定、控制端保存/草稿和聚合模型发现、连接测试。
- [x] capture 不影响转发/授权；重启 unarmed，两个 core 不共享 consent；profile 持久化正确。
- [x] 规则优先级、协议转换/重试/WS 最终身份与 credential、最终本地/代理链头清理。
- [ ] console/operator、observer、inference token 与网络 Host/Origin 权限边界。
- [ ] SQLite 限额竞争、原子性、取消、失败回滚/不确定提交不重放，已知/未知迁移历史。
- [ ] 组合初始化错误走现有清理；server 监听失败/取消的返回及资源释放有界。
- [ ] 新工具/图片路径单次发送及安全边界；不新增完整 profile/capture 能力。
- [ ] 条件允许时运行 race 检查。
  - 命令：`ASTRLINK_CI_NO_REMOTE_MODELS=1 go test -race ./...`。
  - 记录 C 编译器、平台及超时限制，skip 不算该行为验证成功。

### 桌面、Web 与发布间接层

```bash
(cd apps/desktop && bun install --frozen-lockfile)
(cd apps/desktop && bun run typecheck && bun run test && bun run build)
(cd apps/desktop && bun run test \
  scripts/macos-release.test.ts scripts/release-updates.test.ts)
(cd apps/desktop && bun run build:web)
bun apps/desktop/scripts/stage-web.mjs
(cd core && go test -tags webui ./internal/console ./cmd/astrlink-core)
(cd core && go build -tags webui -o bin/astrlink-core ./cmd/astrlink-core)
bun apps/desktop/scripts/test-web-console.mjs
```

- Web smoke 使用 Playwright、loopback 和脚本自建临时目录；缺少浏览器依赖须报告。
- 核实 Windows 二进制路径和进程结束行为，不能为运行脚本部署生产服务。
- Tauri sidecar：有 Make 则 `make desktop-sidecar`，否则等价
  `(cd apps/desktop && bun run sidecar:build)`；遵循模型隔离约束。
- Rust 运行 `cargo test --locked --all-targets`，以及
  `ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink cargo test --locked --lib updates::`
  验证仓库可配置而非只适配 fork 值。
- 搜索 `Calcium-Ion`
  等旧常量，逐条审核生产代码是否绕过 releaseRepository 间接层；测试覆盖两种仓库，保留 preview 默认与 macOS
  adhoc 行为。
- 在实际应用 shell 检查 1280×720、1024×600 和窄布局，记录主区域实际高度及占比（目标 ≥60%），覆盖长列表、空态、展开帮助、切换 tabs。Web
  smoke 的测量不能替代原生壳验证；无法运行原生壳时明确列为未验证。
- Docker 新代码可做静态检查；本地镜像 smoke 仅在工具就绪且不发布/不挂真实数据的条件下执行。不擅自进行部署或远程 CI 操作。

## 5. 全范围评审与提交门

- [x] 主会话派发 `trellis-check`
      全范围检查；读取受影响层索引、AGENTS、任务产物及研究。子代理未返回最终报告，主会话另行复跑一手验证，不采信其结论。
- [x] `git diff --check`、`git diff --cached --check`、`git ls-files -u`
      与冲突标记扫描无未解决项。
- [ ] 复查相对固定正式版的差异，每个差异均可解释为 fork 能力或有记录的最小兼容修复。
- [x] 比较迁移和 README 与原 HEAD 不变；确认没有把未授权的主线提交作为合并来源。
- [ ] 有失败时区分合并缺陷、目标正式版既有问题和环境限制；Factory
      Droid 若阻断，停止扩展并提交准确失败证据供决策。
- [ ] 加载 `trellis-update-spec`
      判断是否有需沉淀的非显然知识；没有则记录判断，不为了流程添加文档。
- [ ] 按 Phase
      3.4 汇总提交方案和文件清单，请用户一次确认；已有个人工作区文件单独列出并默认排除，不 amend、不 push。
- [ ] 获准后提交；验证原 HEAD 与固定正式版均为 HEAD 祖先，`5ceced5` 不为祖先。
- [ ] 报告提交和所有实际检查结果；归档/日志按 finish-work 的独立后续步骤处理。

## 本会话规划验证结果

- 已运行变更 Markdown 的 Prettier `3.6.2` 格式化与 markdownlint-cli `0.45.0`
  修复。首轮长行诊断已修正，两者复查通过；`task.json`
  的 Prettier 格式化与复查通过。
- `task.py validate .trellis/tasks/10-10-sync-upstream-latest`
  通过：implement 清单 6 条、check 清单 5 条，引用路径均有效。
- `git diff --check`、`git diff --cached --check`
  通过，未修改跟踪中的产品文件。任务文件尚未跟踪，已按显式路径运行文档检查，不以空 diff 替代文档验证。
- 上述为激活前的规划验证，当时未运行产品测试或实际合并。当前实施状态见下节。

## 第一轮实施与实际验证

### 已实施

- 固定正式版非快进合并已开始，五处冲突均解决并仅对这些路径执行了手动
  `git add`。自动合并文件由 Git 暂存；任务目录、个人工作区和新增产品文件未暂存。
  `HEAD` 仍为 `7165c746`，`MERGE_HEAD` 为 `78c97c40`，没有提交、推送或发布。
- `main.go` 与正式版逻辑一致。新增 `persistent_identity.go`
  在控制端复制 gateway 依赖之前接入同一个 capture registry、profile
  store 与 profile-aware discovery。新增组合测试覆盖真实 desktop
  production/server
  console 路径、确认绑定、保存及草稿模型发现、聚合发现、连接测试、重启、独立 consent、初始化错误与 key 清除。
- token 创建保留 write-first 锁及有界整事务重试。
  - 测试改用锁前竞争，覆盖最后名额、名称变更、取消和重试上限。
  - 验证 secret 原子性及不确定提交不重放；其他 read-first 测试保留。
- Go/TS 请求规则保留 actor 本地标记；共享最终清理点再次去掉该标记。新增真实 HTTP/WS 回归同时断言本地控制头不转发、provider
  credential 与用户正文不受影响。
- 自动合并复查补出 Web 接入遗漏：已有兼容界面调用 `service_identity`，新增 Web
  transport 原本未分派。复用现有
  `route`/ETag/consoleRequest 接入八类闭合操作，新增
  `web-service-identity.test.ts`
  验证真实 bridge 的作用域、ETag、确认与 capture 请求。
- 保留两项构建配置、operator 路由并集、preview 默认及 release
  repository 间接层； `Calcium-Ion`
  搜索在所审查生产路径中只命中双仓库测试。README 与迁移相对原 HEAD 不变。构建自动重写的无关
  `icon.icns` 已恢复至原基线，没有保留该副作用。

### 通过的检查

- Go 自动工具链首次下载超时后成功使用 `go1.26.9 windows/amd64`；Bun 使用临时
  `bun@1.3.14`，没有全局安装或改版本声明。可复用路径为
  `C:/Users/admin/AppData/Local/Temp/bunx-2598106984-bun@1.3.14/node_modules/bun/bin`。
- 96 个受影响 Go 文件执行 gofmt 写入及空输出复查；50 个原受影响 JS/TS 文件及新增 Web 测试执行 Oxlint
  1.19.0 修复/复查、Prettier
  3.6.2 写入/复查；9 个 JSON/CSS/HTML/YAML 文件执行 Prettier 写入/复查。清单展开为带引号路径，临时日志保存实际命令。
- 三个模块均执行并通过 `go vet ./...`（末轮再次通过）及
  `ASTRLINK_CI_NO_REMOTE_MODELS=1 go test ./... -timeout 10m`。
  - Core 包全通过；SQLite 全包 215.535 秒。
  - 之前人为设为 120 秒的两轮全包检查超时，栈位于 Windows
    `FlushFileBuffers`；未改数据库行为，延长检查时限后通过。
- 定向 contract 回归（`-count=1`）通过：请求头分类、请求规则校验及
  `TestDefaultCapabilitiesMatchesFrozenFixture`。token/config-write 与 identity
  composition 定向测试也通过。未观察到 Droid 导致的产品测试失败；Ruby 验证仍缺工具，跨层风险尚未排除。
- `bun install --frozen-lockfile`、`bun run typecheck`、`bun run test`、
  `bun run build`、`bun run build:web` 通过，末轮包含 Web
  identity 修复。末轮 Vitest 为 **143 files passed，1581 passed / 1
  skipped**。单独发布脚本回归为 18 passed / 1
  skipped；跳过的是 macOS 原生 adhoc 签名实测，不算已验证。
- `bun apps/desktop/scripts/stage-web.mjs`、
  `go test -tags webui ./internal/console ./cmd/astrlink-core -timeout 10m`、
  `go build -tags webui -o bin/astrlink-core.exe ./cmd/astrlink-core`
  通过；Windows可执行文件使用 `.exe`
  后缀。`bun scripts/check-no-production-models.mjs` 两次通过。
- `docker compose config --quiet` 通过；没有启动或发布容器。
  `git diff --check`、`git diff --cached --check` 通过，`git ls-files -u`
  为空；主线独有 `5ceced5` 不是 `MERGE_HEAD` 祖先。

### 环境阻断及下一轮工作

- Rust
  1.96.0 与已有 1.95.0 都没有 rustfmt/Clippy。格式/Clippy 修复及复查命令实际返回
  `cargo-fmt.exe` / `cargo-clippy.exe is not installed`，未安装系统组件。
- `bun run sidecar:build` 首轮 420 秒超时；重试完成到 privacy
  worker 链接时失败：MSVC `14.39.33519` 的 `link.exe`
  返回 1120，ORT 静态库找不到 `__std_find_last_of_trivial_pos_1`
  等 STL 符号。Core/CLI 与运行库文件已暂存到忽略的 binaries 目录，但 privacy/classifier
  sidecar 未完成，不可声称桌面原生构建成功。
- `CARGO_NET_OFFLINE=true cargo test --locked --all-targets` 及带
  `ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink` 的
  `cargo test --locked --lib updates::` 均在依赖解析失败：本地缓存没有
  `jsonc-parser`。完整 Rust 测试未运行。
- `go test -race ./...` 报 `-race requires cgo`；再以 `CGO_ENABLED=1`
  对 transport 尝试，报
  `C compiler "gcc" not found`。普通 Go 测试不替代 race 验证。
- `ruby contracts/validate.rb` 报
  `ruby: command not found`；Make 也不在 PATH。未安装系统包。Playwright 模块解析报
  `Cannot find module 'playwright'`，未执行浏览器 smoke、原生壳布局高度或 macOS/Unix 专项。运行 Web
  smoke 前还须保留子进程的模型隔离环境并核对脚本 Windows 二进制路径；本轮没有为此更改上游 smoke 脚本。
- 全范围自动合并评审、server 监听错误/取消时资源释放的新增证据、原生布局及上述缺项交主会话/check
  agent；不将整个任务标记验收通过。当前相对原 HEAD 有 169 个跟踪产品路径变更及 4 个未跟踪新增产品文件，其中包含要求执行的格式修复。
- 详细临时检查日志位于
  `C:/Users/admin/AppData/Local/Temp/astrlink-*.log`，重点为
  `astrlink-core-tests.log`、`astrlink-desktop-tests-final.log`、`astrlink-format.log`、
  `astrlink-oxlint.log`、`astrlink-sidecar-retry.log`、`astrlink-rust-tests.log`。

## 第二轮（本会话）复核与实际验证

主会话承接上一轮遗留项，未改动产品代码，未提交、未推送、未 abort 合并。

### 合并状态与提交门

- `HEAD` 仍为 `7165c746`，`MERGE_HEAD` 为 `78c97c40`，`git ls-files -u`
  为空，合并仍未提交。
- `git diff --check`、`git diff --cached --check` 退出码均为 0。
- `core/internal/storage/migrate/`
  与 README 相对原 HEAD 的 diff 为空，确认未改迁移与 README。
- `git merge-base --is-ancestor` 确认主线独有 `5ceced5` **不是** `MERGE_HEAD`
  祖先；原 HEAD 不是目标祖先，为真实双亲合并。
- 清理了上一轮检查命令在 Windows 下重定向产生的仓库根目录空壳文件 `NUL`
  （非用户内容，且会导致 ripgrep 扫描报错）。未跟踪条目回到预期 6 项。

### 本会话实际通过的检查

- `go vet ./...` 在 `core`、`convo`、`contracts` 三模块退出码均为 0。
- `ASTRLINK_CI_NO_REMOTE_MODELS=1 go test -count=1`
  定向 12 个关键包全部 ok：transport 3.6s、ingress 37.3s、controlapi
  137.0s、console 33.6s、coreapp 4.3s、cmd/astrlink-core 29.4s、storage/migrate
  51.3s、contract 1.5s、identitycapture 1.2s、servicemodel 5.0s、subscription
  5.7s、accountauth 3.7s。
- `bun run typecheck`（tsc --noEmit）退出码 0；`bun run test` 为 **143 files
  passed，1581 passed / 1 skipped**，与上一轮一致，无回归。

### 代码层面复核结论

- 本地 actor 头隔离成立：fork 的 `gateway_reserved` 分类在
  `core/contract/request_rules.go:113` 与
  `apps/desktop/src/request-compatibility-model.ts:133` 两侧同时覆盖
  `X-Astrlink-` 前缀与 `X-Openai-Actor-Authorization`。
- 最终出站清理点齐备：`removeInboundCredentials` 额外折叠删除 actor 头，
  `removeGatewayHeaders` 在 target overlay 之后同时清理 actor 头与 `x-astrlink-`
  命名空间；HTTP (`forwarder.go:146/161/162`) 与 WS
  (`responses_websocket.go:99/102/103`) 两条路径均调用三个清理函数。
- 身份接入时序正确：`persistent.go:251` 先经 `configurePersistentIdentity` 改写
  `gatewayDependencies`，其后该结构体才按值复制进 `servicetest`、`ingress` 与
  `persistentCore.gateway`；错误走既有
  `fail(...)`。符合 design 「不得事后仅修改 gateway」的要求。

### 本会话仍未解除的环境阻断

- Rust 格式/Clippy 仍不可用。`~/.cargo/bin/cargo-fmt` 只是 rustup 代理壳，
  `cargo fmt --all -- --check` 与 `cargo clippy --version` 实际返回
  `'cargo-fmt.exe' / 'cargo-clippy.exe' is not installed for the toolchain '1.96.0-x86_64-pc-windows-msvc'`。补装需联网下载并改动全局工具链，未执行。
- `ruby`、`make`、`gcc` 仍不在 PATH：契约 Ruby 验证、`make desktop-sidecar` 与
  `-race`（需 cgo）均无法运行。
- 本机 `go version` 为 `go1.26.0 windows/amd64`；上一轮经自动工具链使用
  `go1.26.9`。本轮定向测试在 1.26.0 下通过。
- 未重跑：完整 `go test ./...`（含 sqlite 全包约 215s）、Rust
  `cargo test`、sidecar 构建、Playwright Web
  smoke、原生壳布局高度测量。这些仍以上一轮记录为准，其中 Rust 测试、sidecar、Web
  smoke、原生布局**从未成功执行**，不可计为已验证。

## 第三轮（本会话）工具链补装与剩余检查

用户选择「先补装工具链再提交」。本轮未改动任何产品代码，未提交、未推送、未 abort 合并。

### 已解除：Rust 格式与 Clippy 工具链

- `rustup component add rustfmt clippy` 成功；现为 `rustfmt 1.9.0-stable`、
  `clippy 0.1.96`。上两轮记录的「`cargo-fmt` 仅为 rustup 代理壳」已不再成立。
- **格式门全通过**：`cargo fmt --all -- --check` 在
  `apps/desktop/src-tauri`、`apps/privacy-worker`、`apps/classifier-worker`
  三个 crate 退出码均为 0，无需改动任何文件。
- **Clippy 部分通过**：`cargo clippy --locked --all-targets -- -D warnings` 在
  `apps/privacy-worker`、`apps/classifier-worker`
  退出码均为 0。这证实工具链本身可用，且 ORT 问题仅影响链接、不影响类型检查。
- 本次合并仅改动 `apps/desktop/src-tauri` 下 7 个 `.rs`（`cc_switch`、
  `client_config`、`client_updates/process`、`client_updates/tests`、`lib`、
  `preferences`、`sidecar`）；privacy-worker 与 classifier-worker 未触及。

### 未解除 1：桌面 crate Clippy 被 sidecar 闸门挡住

- `cargo clippy` 在 `apps/desktop/src-tauri` 以 101 失败，原因是 `build.rs:45`
  的 `tauri_build::try_build` panic，报
  `resource path ... doesn't exist`，缺的是
  `binaries\astrlink-privacy-worker-x86_64-pc-windows-msvc.exe`。该校验来自
  `tauri.conf.json` 的 externalBin，没有合法跳过开关。

- `binaries/` 已有 `astrlink-core`、`astrlink-cli` 与 vc_redist /
  ORT 许可文件，缺两个 worker 可执行文件。
- `bun run sidecar:build` 失败于 privacy-worker 链接：`libort_sys`
  引用 19 个 MSVC STL 向量化算法符号（`__std_find_end_1`、`__std_mismatch_1`、
  `__std_max_element_f`、`__std_max_8u` 等），本机 STL 未提供，终于
  `LNK1120: 19 个无法解析的外部命令`。
- 经 `vswhere` 核实：本机唯一 MSVC 为 **14.39.33519**（Visual Studio
  Professional
  2022，17.9.34622.214）。预编译 ORT 静态库的工具集比本机新，属版本不匹配，不是代码缺陷。仓库未记录所需 MSVC 最低版本（`desktop-toolchain`
  只检查 bun/go/rustc/cargo），故未断言具体升级目标版本。
- `ASTRLINK_REUSE_WINDOWS_WORKERS=1`
  不可用：它要求已有完整的 worker 缓存（CI 在缓存命中后才启用），本地从未构建成功，`windowsWorkerCacheGaps()`
  会回落到完整构建。
- 未采取的边路：不伪造占位二进制文件麦过 `build.rs`
  校验，也不从 Release 下载未核实的 worker 二进制入暂。

### 未解除 2：D 盘空间耗尽

- `cargo test --manifest-path apps/privacy-worker/Cargo.toml` 失败于
  `failed to build archive ... libort_sys-*.rlib: 磁盘空间不足。(os error 112)`。
- `df -h`：**D: 245G / 已用 245G / 剩 164M / 100%**；C: 剩 14G；E: 剩 357G。
- Rust target 目录在 D 盘占用：privacy-worker 1.2G、desktop/src-tauri
  1.1G、classifier-worker 267M（合计约 2.6G）。
- Go 缓存在 E 盘（`GOCACHE=E:\97-SoftCache\Go\BuildCache`、
  `GOMODCACHE=E:\97-SoftCache\Go\GoPath\pkg\mod`），所以 Go 测试不受此限制。
- `.git` 仅 14M，`git status` 正常；但 164M 剩余对 162 个文件的提交偏緊。
- 未执行任何清理：删除 target 目录虽可释放空间，但会强制长时间重建，属用户环境决策，待授权。

### 第三轮结论

格式门（Go + TS + Markdown + Rust）与静态检查（go vet、tsc、worker
Clippy）已全部通过。仍未验证且**不得计为已验证**：桌面 crate
Clippy、Rust 测试、sidecar 构建、Playwright Web
smoke、原生壳布局高度测量、契约 Ruby 验证（缺 ruby）、`-race`（缺 gcc/cgo）。前两项分别被 MSVC
STL 版本不匹配和 D 盘空间耗尽阻断，均为环境问题，与本次合并的代码改动无关。

## 回滚点

合并前保护原 HEAD 和未跟踪文件清单；不以硬重置、清理工作区或删除数据库回退。未提交失败可在确认后
`git merge --abort`；保留任务研究和验证记录。已提交后的历史回退须另行批准；失败检查不构成自动推送、改 tag 或删除 Release 的授权。
