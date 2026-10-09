# 技术设计：同步上游 v0.1.9

试合并（一次性 worktree，`main` 合
`upstream/main`）结果：3 个文件文本冲突，其余 9 个上游改动文件自动合并。另有 1 处自动合并成功但语义错误。

## 文本冲突

### `apps/desktop/scripts/release-updates.test.ts`（import 列表）

双方各加了导入：fork 加 `macOSSigningMode`、`publishingRepository`、
`releaseRepository`，上游加 `newestStable`。取并集。

### `apps/desktop/src-tauri/src/updates.rs`（常量）

| 常量              | 上游                         | 合并后                                                                                                         |
| ----------------- | ---------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `REPOSITORY`      | 写死上游 URL                 | 保留 fork 的 `env!("ASTRLINK_RELEASE_REPOSITORY")`（`owner/repo`）                                             |
| `RELEASE_API`     | 写死上游 API                 | 保留 fork 的 `env!("ASTRLINK_RELEASE_API")`                                                                    |
| `LATEST_MANIFEST` | 新增，写死上游 `latest.json` | `concat!("https://github.com/", env!("ASTRLINK_RELEASE_REPOSITORY"), "/releases/latest/download/latest.json")` |

保留 fork 的 `#[cfg(test)] mod release_repository`。`LATEST_MANIFEST` 用
`concat!` 在编译期拼出，不改 `build.rs`，fork 与上游构建各自指向自己的仓库。

自动合并区域里上游的 `format!("{REPOSITORY}/releases/tag/{}")` 已被 fork 的
`https://github.com/{REPOSITORY}/releases/tag/{}`
覆盖，试合并中已确认，不需要额外处理。

### `apps/desktop/src/update-model.test.ts`（无效快照用例表）

fork 加了 4 个非法 `repository` 用例，上游加了 2 个 `retry_at` 用例。取并集。

## 语义冲突：`manifest_tag` 写死上游路径

上游新增的 `manifest_tag` 用
`strip_prefix("/Calcium-Ion/AstrLink/releases/download/")`
校验重定向地址。fork 构建中 `github_asset`
校验的是 fork 仓库，而这个前缀仍是上游：只要 fork 将来发布 stable 版本，重定向到 fork 的
`latest.json` 就会被判为「不是 release
manifest」，stable 更新失效。对应单测也写死上游 URL，在 fork 构建下会失败。

处理方式沿用 fork 已有的 `github_asset` → `github_asset_for_repository` 模式：

```text
manifest_tag_for_repository(url, repository):
    prefix = "/" + repository + "/releases/download/"
    原上游逻辑，前缀换成 prefix，github_asset 换成 github_asset_for_repository(.., repository)
manifest_tag(url) = manifest_tag_for_repository(url, REPOSITORY)
```

单测改为对 `lauchiwa/AstrLink` 与 `Calcium-Ion/AstrLink`
两个仓库都断言：正向用例解析出 tag，上游原有的反向用例（非
`latest.json`、多级 tag、空 tag、仿冒域名、http）逐条保留。上游原有断言一条不删。

## 自动合并区域的复核结论

- `release-updates.mjs`：`gh release edit` 仍带 `--repo repository`；
  `--latest=${newestStable(...)}` 对预发布 tag 恒为
  `false`，fork 发布不会被标为 Latest，与之前的 `--latest=${!prerelease}`
  行为一致。
- `update-model.ts`、`About.tsx`：fork 的 `repository` 校验、默认 `preview`
  保留，上游 `retry_at` 展示合入。
- i18n：两份 locale 各新增 `retryAt` 一个键，无 fork 键被改。
- `defaults.go`：上游原地改写迁移 49（列级 CHECK
  → 触发器）；迁移历史不做内容校验和，迁移 50 不受影响。
- `core/go.mod`：上游未改，保持 `go 1.26.9`。

## 提交形态

一个合并提交 `merge(upstream): 同步上游 v0.1.9`，冲突解决与 `manifest_tag`
适配都在其中，与上次同步 `873e98b`
一致。若适配改动较大，可拆成合并提交 + 紧随其后的
`fix(desktop)`，以便单独回滚；本次改动量小，不拆。

## 回滚

推送前：`git merge --abort` 或
`git reset --hard ORIG_HEAD`（仅本地、未推送时，且需用户同意）。推送后：`git revert -m 1 <merge>`。tag 打出前回滚不影响任何用户。
