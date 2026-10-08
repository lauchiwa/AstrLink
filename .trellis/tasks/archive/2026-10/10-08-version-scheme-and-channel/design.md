# 技术设计：版本命名约定与默认更新频道

## 边界

两块互相独立的改动，可分别验证：

1. **命名约定** —— 纯文档，落入 `.trellis/spec/`。零代码。
2. **默认频道** —— `apps/desktop/src-tauri/src/updates.rs` 的默认值，加
   `apps/desktop/src/update-model.ts` 的前端默认值对齐。

发布脚本与三个平台打包 workflow **一行都不动**：`releaseVersion` 已实测接受
`v0.1.9-rc.0` / `v0.1.9-rc.10` / `v0.1.10-rc.0`，`lauchiwa` 仅出现在仓库名中。

## 一、命名约定

写入 `.trellis/spec/guides/` 并在 `index.md` 注册（与
`upstream-sync-thinking-guide.md` 同级）。必须写进去的硬约束：

- `X.Y.Z` 取同步到的上游版本号，`N` 为我方修订号，`rc.0` 表示「同步完成、无二开」。
- **纯 `X.Y.Z` 永不发布。** 连带写明原因：SemVer 下 prerelease 低于同号正式版，
  一旦发布纯版本号就装上天花板，后续 rc 全部排在它之下，客户端会把用户钉在同步版
  上。
- 后缀必须是**纯数字**分量（`rc.10` 而非 `rc.10a`），否则失去数值序。
- 不要用四段版本号、不要把修订号放进 build metadata —— 两条都附实测结论，否则后来
  者会重新尝试。

这一条的要点是**防「顺手发个正式版」**。没有写下原因的约束会被下一个人善意地
破坏。

## 二、默认频道

### 现状

```rust
// updates.rs:30-36
#[derive(..., Default, ...)]
pub enum UpdateChannel {
    #[default]
    Stable,
    Preview,
}

// updates.rs:38-44
#[serde(default, deny_unknown_fields)]
pub struct UpdatePreferences {
    pub auto_check: bool,
    pub auto_download: bool,
    pub channel: UpdateChannel,
}
impl Default for UpdatePreferences { /* 手写，auto_check/auto_download = true */ }
```

### 关键语义：`#[serde(default)]` 只在字段**缺失**时生效

字段存在且写着 `"stable"` → 解析为 `Stable`，不受默认值影响。因此把默认值改为
Preview 的效果是：

| 偏好文件状态                | 改动后的结果              |
| --------------------------- | ------------------------- |
| 文件不存在                  | Preview（缺陷修复）       |
| 文件存在、无 `channel` 键   | Preview                   |
| 文件存在、`channel: stable` | 仍为 Stable（不覆盖）     |

验收标准 2「不静默改写显式选择」由此**自动满足**，不需要引入 `Option<UpdateChannel>`
或迁移标记字段。后者还会与 `deny_unknown_fields` 冲突（新增键会让旧版本读取失败）。

### 全新安装确实会被修复

`preferences.rs:330–340`：文件 `NotFound` 且 `has_existing_data` 为假时立即
`persist_atomic` 落盘默认值。注释写明「Save new-install defaults before Core
creates its database」。所以全新安装会写出 `"channel": "preview"`，`select_release`
不再过滤掉我们的 release。

`has_existing_data` 来自 `data_directory.join("astrlink.db").exists()`
（`preferences.rs:298`），已被用于区分「老装机从未存过偏好」并走
`LEGACY_INFERENCE_PORT`。**本设计沿用这个既有判据，不新增机制。**

### 一个无法修复的情形，必须如实记录

前端在 `About.tsx:410` 有频道切换入口，所以持久化的 `"channel": "stable"`
**可能**是显式选择，也可能只是旧默认值被写出。两者在反序列化后无法区分。

受影响人群：文件已存在且写着 `stable`、但从未真正点过那个开关的用户。他们仍收不到
更新，需手动切换。

**不要为了「修全」而去猜测意图并覆盖它** —— 覆盖显式选择的代价高于少修一部分人。
该限制写入 spec 与发布说明，不要掩盖。

### 前端默认值对齐

`update-model.ts:12` 的 `channel: "stable"` 需同步改为 `"preview"`，否则前端在后端
返回之前会短暂显示错误的频道。

注意 `update-model.ts:60–63` 的校验要求键集合**恰好**是
`auto_check,auto_download,channel` 且 `channel` 属于 `["stable","preview"]` ——
只改默认值不触碰这个校验。

### 明确不改

`updates.rs:126` 的过滤器保持原样：

```rust
if channel == UpdateChannel::Stable && (r.prerelease || !v.pre.is_empty()) {
    return None;
}
```

两个条件是**或**关系，所以即使把 GitHub 的 prerelease 标记改成 false 也无法让
Stable 看见 `-rc.N`。放宽它会让「频道」概念失去意义，以后真要发预览版无处可放。
由测试守护这一行的语义不被后续改动放宽。

## 三、不需要破坏性操作

实测新旧序列可共存且排序正确：

```text
0.1.8-lauchiwa.1 < .2 < .3 < .4 < 0.1.9-rc.0 < 0.1.9-rc.1
客户端会选中：0.1.9-rc.1
```

`0.1.9-rc.0` 与 `rc.1` 都 `Greater` 于 `0.1.8-lauchiwa.3`（已装机版本）和 `.4`。
孤立的 `v0.1.8-lauchiwa.4`（release 创建失败，无产物）**留着不用即可**，不删远端
tag。

## 兼容性

- 改默认值不改变序列化结构，`deny_unknown_fields` 不受影响。
- 旧版本客户端读新偏好文件：键集合未变，正常解析。
- 降级场景：旧版本读到 `"channel": "preview"` 能正常解析（该枚举值早已存在）。

## 回滚

两块都是小改动。频道默认值回滚即改回 `Stable`；spec 回滚即删文件与 `index.md`
条目。无数据迁移，无发布层面的回滚需求。
