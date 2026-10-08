# 执行计划

Rust 命令需 `export MISE_RUST_VERSION=stable`；bun 经 mise
shims 可用（`export PATH="$HOME/.local/share/mise/shims:$PATH"`）。证据放
`/tmp/astrlink-remove-checkin/`，不进仓库。

## 步骤

### 0. 采集移除前基准

- 六个共享文件的 `git diff -U0 upstream/main -- <file>` 改动行，存入证据目录。
- 依赖文件的
  `sha256`（`core/go.mod`、`core/go.sum`、`convo/go.mod`、`convo/go.sum`、
  `apps/desktop/src-tauri/Cargo.toml`、`apps/desktop/src-tauri/Cargo.lock`、
  `apps/desktop/package.json`、`bun.lock`）。
- 构建移除前的 Core 二进制，存到证据目录，供步骤 5 使用。

### 1. Core

1. 删 `main.go` 中 4 处 `checkin` 接线及相关注释。
2. 删 `handler.go` 中常量、依赖字段、挂载块。
3. 删 `cmd/astrlink-core/fork_checkin.go`、`internal/forkcheckin/`、
   `internal/storage/sqlite/fork_checkin_*`、`internal/controlapi/checkin_extension_test.go`。
4. 删 `contracts/extensions/`。
5. `gofmt -l`、`go vet ./...`、`go build ./...` 通过后再进入下一步。

### 2. 桌面 Rust

1. 删 `lib.rs` 的 `mod fork_checkin`、注释与命令注册。
2. 删 `sidecar.rs` 的 `mod fork_checkin` 与注释。
3. 删 `src/fork_checkin/`、`src/sidecar/fork_checkin.rs`、`examples/`。
4. `cargo fmt --all -- --check`、`cargo clippy --locked --all-targets -- -D warnings`。

### 3. 前端

1. 删 `App.tsx` 中签到导入、页面种类、导航、图标、页面分支、`CircleCheck`。
2. 删 `App.navigation.test.tsx` 中签到 mock、重置与签到测试。
3. 删 `src/features/fork-checkin/`。
4. `bun run typecheck`、oxlint、prettier。

### 4. 残留引用扫描（验收标准 1）

```bash
git grep -n -i -E 'forkcheckin|fork_checkin|fork-checkin|CheckinExtension|checkin_probe|ForkCheckin|checkinNavLabel|CheckinWorkspaceEntry' -- . ':!.trellis/tasks/archive/*'
```

必须为空。再用 `git grep -n -i checkin` 人工过一遍，排除 `checking` 之类误报。

### 5. 残留数据实测（验收标准 4）

用步骤 0 的移除前 Core 在临时目录建库并产生签到表与
`fork-checkin.json`，停止后用移除后的 Core 在同一目录启动，断言
`"event":"ready"`。用 `sqlite3` 确认签到表确实存在于该库中，否则测试无效。

### 6. 差集与依赖校验（验收标准 2、3）

按 `design.md` 的差集规则比较步骤 0 的基准；依赖文件 `sha256` 必须一致。

### 7. 全量验证（验收标准 5）

```bash
(cd core && gofmt -l . && go vet ./... && go test ./...)
(cd convo && go vet ./... && go test ./...)
(cd contracts && go vet ./... && go test ./...)
(cd apps/desktop/src-tauri && cargo fmt --all -- --check && cargo clippy --locked --all-targets -- -D warnings && cargo test --lib)
(cd apps/desktop && bun run typecheck && bunx oxlint@1.19.0 --deny-warnings <changed> && bunx prettier@3.6.2 --check <changed> && node_modules/.bin/vitest run)
git diff --check
```

## 评审门

步骤 6 的「移除后行集 − 移除前行集」必须为空才能报告完成。若 gofmt 重新对齐导致多出纯空白差异，如实列出，由用户判断。

## 回滚点

- 每一层（Core / Rust / 前端）完成并通过本层检查后再进入下一层；任一层失败，
  `git checkout -- <files>` 与 `git restore` 恢复该层即可。

## 不做

- 不改上游逻辑、不重构、不删依赖。
- 不动发布与 tag。
- 不碰已安装客户端与其数据目录。
