# 技术设计：移除签到功能

## 为什么不用 `git revert`

签到由 `656aba3 feat(checkin)` 与 `09d1c48 feat(desktop)`
引入，但它们改过的六个共享文件随后又被 `873e98b merge(upstream)`
修改，且同一文件里交错着身份采集、身份档案、fork 发布等其他二开改动。反向应用这两个提交会在共享文件上冲突，冲突解决又回到手工。

所以直接做：**整体删除纯签到文件 + 共享文件中手工删除签到片段**。

## 边界

- 只删除，不新增功能代码，不重构共享文件中的其他逻辑。
- 上游逻辑零改动：签到片段全部是 fork 新增的，删除它们只会让共享文件**更接近**
  `upstream/main`，不会离上游更远。
- 保留的二开功能见 `prd.md`「保留」一节。

## 依赖方向（决定删除顺序）

```text
Core:     main.go ──► cmd/astrlink-core/fork_checkin.go ──► internal/forkcheckin
                                                  └──► storage/sqlite/fork_checkin_*.go
          controlapi/handler.go 只定义挂载点（http.Handler），不 import 扩展包

Desktop:  lib.rs ──► src/fork_checkin/ ──► sidecar/fork_checkin.rs
          App.tsx ──► features/fork-checkin/entry
```

签到的 sqlite 文件是叶子：非签到的 sqlite 代码对它们零引用（规划期已核实）。因此每一层都是「先删接线，再删目录」，任一步之后编译器都能指出漏网的引用。

## 共享文件的删除要点

- `main.go`：`checkin` 变量贯穿 4 处（初始化、`CheckinExtension` 依赖、`start`、
  `stop`），连同 `closeStore`
  里那段「扩展与 Store 共享密钥」的注释一起删。结构体字面量的对齐由
  `BuiltinToolTester`（17 字符）决定，删去
  `CheckinExtension`（16）不会触发 gofmt 重新对齐。
- `handler.go`：常量、依赖字段及其注释、`if dependencies.CheckinExtension != nil`
  挂载块。删字段后若 gofmt 重新对齐相邻字段，必须如实报告，不能把对齐变化混进「其余差异一致」的结论里。
- `sidecar.rs`：只删 `mod fork_checkin` 与其注释。`validate_resource_id` /
  `validate_etag` 的 `pub(crate)` 保留，因为 `service_identity.rs:5` 在用。
- `App.tsx`：`CircleCheck`
  仅签到使用（文件内共 2 处：导入与图标映射），一并删除。
- `App.navigation.test.tsx`：签到 mock 与那条签到测试删除；同文件中路由自动保存、
  `repository` 感知 URL 的改动属于其他二开，保留。

## 校验方法

### 差集校验（验收标准 2）

移除前对六个共享文件各存一份 `git diff -U0 upstream/main -- <file>` 的 `+`/`-`
行，移除后再取一次。要求：

```text
移除前行集 − 移除后行集 = 全部为签到行
移除后行集 − 移除前行集 = 空
```

第二条为空，证明没有误改其他行。第一条逐行人工确认属于签到。

### 依赖不变（验收标准 3）

移除前后对 `core/go.mod`、`convo/go.mod`、`go.sum`、`Cargo.toml`、`Cargo.lock`、
`package.json`、`bun.lock` 取 `sha256`，必须一致。若 `cargo` 或 `go mod tidy`
想改锁文件，说明有依赖只被签到使用，需停下报告，不自行删依赖。

### 残留数据（验收标准 4）

用移除**前**的 Core 在临时数据目录建库并启用签到（产生 `fork_checkin_*` 表、
`fork_checkin_schema_migrations` 与
`fork-checkin.json`），再用移除**后**的 Core 在同一目录启动，断言输出
`"event":"ready"`。只用临时目录与合成凭据，不碰已安装的客户端数据。

## 回滚

所有改动在一个分支提交内。回滚即 `git revert`
该提交；被删文件随之恢复。无数据迁移，无发布层面的回滚需求。
