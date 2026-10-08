# 移除签到功能

## 背景

签到（check-in）扩展经过 V01、P01–P03 已打通「粘贴站点 Cookie 导入」的端到端流程，但评估下来在本项目中价值不大。它还带来了持续成本：两个重型验收测试（`TestCheckinCoreAcceptance`、`TestCheckinCoreIsolationAcceptance`）在上游把主迁移推进到 50 后全平台变红，挡住了 CI 与发布；修复方案（任务
`10-08-checkin-baseline-rebuild`，已废弃归档）本身又需要维护一套去扩展构建。

签到从未进入任何已发布版本：已发布的
`v0.1.8-lauchiwa.3`（`e7ebad2`）不含任何签到文件；`v0.1.8-lauchiwa.4`
的发布失败，没有产物。

## 目标

整体移除签到功能，**只删签到代码**。已同步的上游逻辑与其他二开功能保持不变。

## 范围

### 整体删除（上游 `upstream/main` 中不存在任何签到文件）

| 路径                                                 | 文件数 |
| ---------------------------------------------------- | ------ |
| `core/internal/forkcheckin/`                         | 49     |
| `core/internal/storage/sqlite/fork_checkin_*`        | 29     |
| `core/cmd/astrlink-core/fork_checkin.go`             | 1      |
| `core/internal/controlapi/checkin_extension_test.go` | 1      |
| `contracts/extensions/`                              | 2      |
| `apps/desktop/src-tauri/src/fork_checkin/`           | 4      |
| `apps/desktop/src-tauri/src/sidecar/fork_checkin.rs` | 1      |
| `apps/desktop/src-tauri/examples/`                   | 8      |
| `apps/desktop/src/features/fork-checkin/`            | 25     |

合计 120 个文件。

### 共享文件，只删签到片段

| 文件                                       | 删除                                                                                                                      | 必须保留                                                                                                        |
| ------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `core/cmd/astrlink-core/main.go`           | `newForkCheckinExtension` 调用与错误处理、`CheckinExtension` 依赖、`checkin.start`、`checkin.stop` 及注释                 | `identitycapture` 导入与初始化、`IdentityCapture`、`IdentityProfiles`、`servicemodel.NewWithDependencies`       |
| `core/internal/controlapi/handler.go`      | `CheckinExtensionPath` 常量、`CheckinExtension` 依赖字段、挂载块                                                          | `IdentityCapture` 字段与 `identityCapture`                                                                      |
| `apps/desktop/src-tauri/src/lib.rs`        | `mod fork_checkin` 及注释、`fork_checkin::fork_checkin_operation` 注册                                                    | `mod service_identity`、`service_identity` 命令及其注册                                                         |
| `apps/desktop/src-tauri/src/sidecar.rs`    | `mod fork_checkin` 及注释                                                                                                 | `service_identity` 方法；`validate_resource_id` / `validate_etag` 的 `pub(crate)`（`service_identity.rs` 在用） |
| `apps/desktop/src/App.tsx`                 | `CircleCheck` 导入、`fork-checkin/entry` 导入、`checkin` 页面种类、导航按钮、图标映射、页面分支、导航列表中的 `"checkin"` | 其余全部                                                                                                        |
| `apps/desktop/src/App.navigation.test.tsx` | `checkinMocks` 与 `fork-checkin/bridge` 的 mock、`beforeEach` 中的重置、「loads check-in only when visited…」测试         | 路由自动保存相关 mock 与测试、`repository` 感知的更新 URL 断言                                                  |

### 保留，不属于本次范围

- `04478e4 fix(sqlite)`
  配置写入整事务重试：由签到负载测试发现，但修的是 Core 自身在负载下的
  `SQLITE_BUSY`，代码与测试都不引用签到。
- 迁移 50 `service_identity_profiles` 及 `identity_profile_history.go`
  的版本对账：属于多设备同步与身份档案，与签到无关。
- `apps/desktop/src-tauri/Cargo.toml` 的 build-dependency `serde_json`、
  `apps/desktop/package.json` 的 `macos:notarize`：属于 fork 发布功能。
- `.trellis/spec/guides/upstream-sync-thinking-guide.md`
  中「固定基线提交的准入门槛」一节：是通用经验，不依赖签到代码存在。
- 已归档任务中的签到记录：历史记录，不改。

## 验收标准

1. 上述 120 个文件全部删除；仓库内（排除 `.trellis/tasks/archive/`）不再有
   `forkcheckin`、`fork_checkin`、`fork-checkin`、`CheckinExtension`、
   `checkin_probe`、`ForkCheckin`、`checkinNavLabel`、`CheckinWorkspaceEntry`
   等引用。
2. 六个共享文件相对 `upstream/main`
   的 diff 中**不再出现任何签到行**，且**其余二开差异与移除前逐行一致**（移除前后各取一次
   `git diff upstream/main -- <file>`，差集只能是签到行）。
3. 依赖不变：`core/go.mod`、`convo/go.mod`、`Cargo.toml`、`Cargo.lock`、
   `package.json`、`bun.lock` 与移除前相同。
4. 已有本地数据不受影响：在一个含签到表（`fork_checkin_*`）与
   `fork-checkin.json` 的数据目录上，移除后的 Core 能正常启动并就绪。
5. 全量验证通过：Go `gofmt -l`、`go vet`、`go test ./...`（三个模块）；Rust
   `cargo fmt --check`、`cargo clippy --locked --all-targets -- -D warnings`、
   `cargo test --lib`；前端
   `bun run typecheck`、oxlint、prettier、`vitest run`； `git diff --check`。
6. 签到两个验收测试随文件删除；CI 中不再有签到相关步骤需要处理（已核实 CI 与构建脚本无签到引用）。

## 不在范围

- 不改上游逻辑，不顺手重构共享文件中的其他代码。
- 不删远端 tag，不发布新版本。
- 不清理已安装客户端或本地开发库中的签到残留数据（无害，见验收标准 4）。
- 不处理 shrimp 任务管理器中的签到待办（不在仓库内）。
- 不决定 `.claude/`、`.codex/`、`.pi/`、`.agents/skills/` 是否纳入版本控制。

## 风险

- **共享文件误删其他二开代码。**
  签到片段与身份采集、身份档案等改动交错在同几个文件中。由验收标准 2 的 diff 差集校验兜底。
- **残留数据导致启动失败。** 签到扩展使用独立迁移表
  `fork_checkin_schema_migrations`，主迁移校验只看
  `schema_migrations`，预期无影响；由验收标准 4 实测确认，不靠推断。
