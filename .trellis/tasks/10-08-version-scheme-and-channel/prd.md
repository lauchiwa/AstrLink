# 确立上游血缘版本命名与默认更新频道

## 背景

本仓库是 `Calcium-Ion/AstrLink` 的 fork，需要一套既能看出同步自哪个上游版本、
又能表达我方二开打包次数的版本号。

现有 tag 是 `v0.1.8-lauchiwa.1/.2/.3`，以及本轮推出的 `v0.1.8-lauchiwa.4`。后者
选错了基数：它声称自己是 0.1.8 的预发布，而实际内容是上游 v0.1.9 合并后的代码。

## 命名方案

`X.Y.Z-rc.N`

- `X.Y.Z` 取**同步到的上游版本号**。
- `N` 是我方在该上游版本之上的二开修订号，从 0 起。`rc.0` 即「同步完成、尚无二开
  改动」。
- **纯 `X.Y.Z` 永不发布。**

```text
同步上游 0.1.9 后打包   v0.1.9-rc.0
改了逻辑                v0.1.9-rc.1
又改                    v0.1.9-rc.2
第十次                  v0.1.9-rc.10
上游发 0.1.10，同步后    v0.1.10-rc.0
```

### 为什么「纯 X.Y.Z 永不发布」是必要条件

SemVer 规定 prerelease 低于同号正式版。若先发一个纯 `0.1.9`，它立刻成为天花板，
后续全部 `0.1.9-rc.N` 都排在它之下，客户端会把用户钉死在同步版上，二开包永远推不
出去。实测：

```text
0.1.9-rc.1 vs 0.1.9 → Less
0.1.9-rc.9 vs 0.1.9 → Less
客户端在 [0.1.9, 0.1.9-rc.1, 0.1.9-rc.2] 中：两个频道都选中 0.1.9
```

不发布纯 `X.Y.Z` 则天花板不存在，序列单调递增。实测：

```text
0.1.9-rc.0 → rc.1 → rc.2 → rc.10 → 0.1.10-rc.0 → 0.1.10-rc.1
全程单调递增 = true
0.1.10-rc.0 vs 0.1.9-rc.99 → Greater
```

跨上游版本安全：数字段不同时 prerelease 不参与比较，只在同号内部排序。纯数字的
prerelease 分量按**数值**比较而非字典序，故 `rc.10 > rc.2`，修订号过 9 不会翻车。

### 已排除的替代方案

| 方案                     | 否决原因（均经实测）                                             |
| ------------------------ | ------------------------------------------------------------- |
| 四段 `v0.1.9.0`          | `semver` crate 拒绝；`Cargo.toml` 置入后 `cargo metadata` exit=101，应用编译不出来。版本号经 Cargo 编入二进制（`updates.rs:26`），这是外部约束 |
| `0.1.9+lauchiwa.N`       | build metadata 不参与 SemVer 优先级，`cmp_precedence` 返回 `Equal`，客户端永远发现不了更新 |
| `0.1.10-rc.N`（基数取下一 patch） | 可行但更难读，需心算「基数是下面那个 patch」。其唯一优势是「相对上游 tag 排序也如实」，而客户端只读本仓库 release 列表，从不与上游 tag 比较，该优势为空 |
| 先发纯 `0.1.9` 再发 rc   | 方向反转，见上 |

### 与 new-api 的关系

`Calcium-Ion/new-api` 的 Latest 是 `v1.0.0-rc.42`，且 `v1.0.0` 从未发布
（`gh release view v1.0.0` → `release not found`）。**「基数永不单独发布、靠后缀
计数」这个形状来自它，可以借。**

但它另有 19 个四段版本 tag，且 `v1.0.0-rc.1` 标为 `prerelease=false`、
`v1.0.0-alpha.1` 标为 `prerelease=true`、`v0.2.7.2-alpha.7` 标为
`prerelease=false` —— 这套混用在 SemVer 下自相矛盾，**只因 new-api 是 Docker 部署
的服务端、没有基于 SemVer 比较的 updater 才能成立，这部分借不了。**

## 需要同时解决的缺陷

本仓库**稳定版 release 数为 0**（两个 release 都是 Pre-release）。而：

- 新装机默认 `UpdateChannel::Stable`（`updates.rs:32–36` 的 `#[default]`）。
- `select_release` 在 Stable 下过滤：`updates.rs:126`
  `if channel == UpdateChannel::Stable && (r.prerelease || !v.pre.is_empty())`。

两个条件是**或**关系：只要 SemVer 带 `-rc.N`，Stable 就看不见，**即使把 GitHub 的
prerelease 标记改成 false 也无效**。因此全新安装的用户得到 `no_releases`，永远收
不到更新。

该缺陷**当前就存在**，不是新命名引入的；但新命名使其成为永久状态，必须一并解决。

### 取向

把默认频道改为 Preview（或首次运行显式让用户选），**不放宽 `:126` 的过滤器**。
放宽会让「频道」概念失去意义，以后真要发预览版就没有位置可放。

## 范围

1. 把命名约定写入 `.trellis/spec/` 并在 `index.md` 注册，含「纯 X.Y.Z 永不发布」
   这条硬约束及其原因。
2. 默认更新频道由 Stable 改为 Preview，含既有 `desktop-preferences.json` 的迁移。
3. 不改 `updates.rs:126` 的过滤逻辑。
4. 不改发布脚本：`releaseVersion` 已原样接受 `v0.1.9-rc.0` / `rc.10` /
   `v0.1.10-rc.0`（实测），`lauchiwa` 仅出现在仓库名中，非版本后缀硬编码。

## 验收标准

1. 默认频道为 Preview；新建偏好文件时写入的 `channel` 为 `preview`。
2. 既有偏好文件的迁移有测试覆盖：已显式设为 `stable` 的用户**不被静默改写**，
   未设置过的走新默认。
3. `updates.rs:126` 的过滤逻辑未被放宽（由测试守护 Stable 仍拒绝 prerelease）。
4. 命名约定落入 spec 并在 `index.md` 注册。
5. 突变验证：至少证明「默认频道改回 Stable」与「迁移覆盖了显式 stable 设置」两种
   情形会被测试抓到，并保留一行未突变的绿色基线。
6. `cargo fmt --all -- --check` 干净；`cargo clippy --locked --all-targets -- -D
   warnings` 零告警；`git diff --check` 干净。

## 不在范围

- **不删除任何远端 tag。** 实测 `0.1.9-rc.0` 与 `rc.1` 均 `Greater` 于
  `0.1.8-lauchiwa.3` 和 `.4`，新旧序列可共存于同一列表且排序正确（客户端会选中
  `0.1.9-rc.1`）。孤立的 `v0.1.8-lauchiwa.4`（release 创建失败，无产物）留着不用
  即可，无需破坏性操作。
- 不发布新 tag。本任务只确立规则与修复频道缺陷，实际发版另行决定。
- 不改 `release-updates.mjs` 与三个平台打包 workflow。
- 签到验收基线（任务 `10-08-checkin-baseline-rebuild`）与本任务无关，各自独立。

## 风险

- **改默认频道会让所有未显式设置过频道的现有用户开始收到 prerelease。** 对本 fork
  而言这是期望行为（否则他们收不到任何更新），但必须确认不会覆盖显式选择。
- 若将来确实要发正式版，本方案要求改基数（如上游 1.0.0 → 我方
  `1.0.0-rc.N`），而不是发布纯 `1.0.0`。这条约束必须写进 spec，否则后来者会
  「顺手」发一个纯版本号而把天花板装上。
