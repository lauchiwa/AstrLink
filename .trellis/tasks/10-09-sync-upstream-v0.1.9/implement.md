# 执行计划

Rust 命令需 `export MISE_RUST_VERSION=stable`；bun 经 mise
shims（`export PATH="$HOME/.local/share/mise/shims:$PATH"`）。

## 步骤

### 0. 前置确认

- `git fetch upstream main --tags`，**单独检查 fetch 的退出码**，不经过管道。
- 确认 `upstream/main` 等于 `v0.1.9`（`78cc0dc`），工作区干净。
- 记录合并前 `git diff upstream/main...main --stat` 作为 fork 差异基准。

### 1. 合并与冲突解决

1. `git merge --no-ff --no-commit upstream/main`。
2. 按 `design.md` 解决 3 处文本冲突。
3. 实现 `manifest_tag_for_repository`，改写对应单测为双仓库参数化。
4. `rg -n 'Calcium-Ion' apps/desktop/src apps/desktop/src-tauri/src apps/desktop/scripts`
   逐条确认：生产代码不新增写死上游地址；测试中出现的都在双仓库用例里。

### 2. 本地验证

```bash
(cd apps/desktop/src-tauri && cargo fmt --all -- --check && cargo clippy --locked --all-targets -- -D warnings && cargo test --lib)
(cd apps/desktop && bun run typecheck && node_modules/.bin/vitest run)
(cd apps/desktop && bunx oxlint@1.19.0 --deny-warnings <changed> && bunx prettier@3.6.2 --check <changed>)
(cd core && gofmt -l . && go vet ./... && go test ./...)
git diff --check
```

Rust 测试另用 `ASTRLINK_RELEASE_REPOSITORY=Calcium-Ion/AstrLink` 跑一遍
`cargo test --lib updates::`，确认上游仓库构建下同样通过。

### 3. fork 差异复核

合并后 `git diff upstream/main...HEAD`
与步骤 0 的基准对比：fork 部分不缺失，新增的只有 `LATEST_MANIFEST` 的 `concat!`
与 `manifest_tag_for_repository` 及其测试。

### 4. 评审门

提交前向用户报告冲突解决与验证结果。

### 5. 提交、推送、CI

- 提交 `merge(upstream): 同步上游 v0.1.9`，推送 `main`，等 CI 全绿。

### 6. 规范更新

在 `upstream-sync-thinking-guide.md` 的同步检查清单中补两条：

- fetch 必须单独检查退出码，并核对 `upstream/main` 与上游最新 release tag 一致。
- 合并后 grep 上游写死的仓库名，新代码可能绕过 fork 的仓库间接层。

### 7. 发布

- `bun apps/desktop/scripts/release-updates.mjs version v0.1.9-rc.0` 预检。
- 打 lightweight tag `v0.1.9-rc.0` 并推送，跟踪 Release workflow 到完成。
- 确认 Release 为 Pre-release，含 `latest.json`。

## 回滚点

- 步骤 1–3 任一失败：`git merge --abort`，回到干净的 `main`。
- 步骤 5 后 CI 失败：先修复；无法修复则 `git revert -m 1`，并在打 tag 前停下。
- 步骤 7 失败：Release 未发布时不影响用户；删 tag 需用户同意。
