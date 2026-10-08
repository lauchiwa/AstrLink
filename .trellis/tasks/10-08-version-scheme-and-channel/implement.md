# 执行计划

Rust 命令需 `export MISE_RUST_VERSION=stable`。bun 不可用，前端测试走
`node_modules/.bin/vitest`。

## 步骤

### 1. 写命名约定 spec

新建 `.trellis/spec/guides/version-naming-guide.md`，在
`.trellis/spec/guides/index.md` 注册（追加，**不要重排既有条目**）。

内容按 `design.md` 第一节，必须含：

- `X.Y.Z-rc.N` 的含义与 `rc.0` 约定。
- **纯 `X.Y.Z` 永不发布**及其原因（prerelease 低于同号正式版 → 天花板）。
- 后缀须为纯数字分量。
- 四段版本号与 build metadata 两条已否决方案及实测结论。

注意 `index.md` 第 3、9、24、25、56–59、62、63 行的长行是**既有的**（多为表格），
不要顺手「修」。

### 2. 改默认频道

`apps/desktop/src-tauri/src/updates.rs`：`UpdateChannel` 的 `#[default]` 由
`Stable` 移到 `Preview`。

确认 `UpdatePreferences` 的手写 `impl Default` 是否也需同步 —— 它构造的是具体值，
若其中写死了 `UpdateChannel::Stable` 就一并改；若用的是 `Default::default()` 则自
动跟随。**读代码确认，不要假定。**

`apps/desktop/src/update-model.ts:12` 的 `channel: "stable"` 改为 `"preview"`。
不碰 `:60–63` 的键集合校验。

### 3. 加测试

三条，缺一不可：

- **新装机走 Preview**：无偏好文件、无 `astrlink.db` 时落盘的 `channel` 为
  `preview`。可参照 `preferences.rs:538` 的 `load_with_fallback_locale(directory,
  directory, Locale::En)` 测试辅助。
- **显式 stable 不被覆盖**：预置含 `"channel": "stable"` 的文件，加载后仍为
  `Stable`。
- **过滤器未被放宽**：Stable 频道下，`-rc.N` 的 release 仍被 `select_release` 排除
  （守护 `updates.rs:126` 的语义）。

既有测试 `preferences_migrate_defaults_and_reject_unknown_channels`
（`updates.rs:1023`）可能断言了旧默认值，需检查并更新 —— **更新时保持至少同等严格**。

### 4. 验证

```bash
export MISE_RUST_VERSION=stable
cd apps/desktop/src-tauri
cargo fmt --all -- --check
cargo clippy --locked --all-targets -- -D warnings
cargo test --lib
cd ..
node_modules/.bin/tsc --noEmit
node_modules/.bin/vitest run src/update-model
```

### 5. 突变验证（验收标准 5）

每次改完**必须还原并 diff 确认**。注意：**编译失败不算有效突变** —— 若删分支导致
变量未使用，补 `_ = x` 让它编译通过，由测试而非编译器报错。

| 突变                                      | 期望 |
| ----------------------------------------- | ---- |
| `#[default]` 改回 `Stable`                | 红   |
| 加载时无条件覆盖 `channel` 为 Preview     | 红   |
| 放宽 `:126` 去掉 `!v.pre.is_empty()` 一项 | 红   |
| 未突变基线行                              | 绿   |

最后一行必需：没有绿色基线行，无法证明检测器本身有效。

### 6. 收尾

```bash
cd apps/desktop && node_modules/.bin/tsc --noEmit
cd ../.. && git diff --check
```

Markdown 按 AGENTS.md 过一遍：
`npx --yes prettier@3.6.2 --write --prose-wrap always <files>` 然后
`npx --yes --package markdownlint-cli@0.45.0 markdownlint --fix <files>`。
两者在 CJK 行宽上会互相拉扯，**残留的 MD013 与仓库现状一致即可**，不要为它反复
改写中文段落。

## 评审门

突变矩阵全部符合期望后再报告完成。**若「放宽 `:126`」那条没有转红，说明没有测试在
守护频道语义，必须先补测试**，不能以其他测试通过为由放过。

## 不做

- 不删任何远端 tag（实测不需要，见 `prd.md` 不在范围一节）。
- 不发布新 tag。
- 不改 `release-updates.mjs` 与平台打包 workflow。
- 不放宽 `updates.rs:126`。
- 不为「修全」而覆盖用户的显式 stable 选择。
