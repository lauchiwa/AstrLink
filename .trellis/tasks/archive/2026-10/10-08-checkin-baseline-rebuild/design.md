# 技术设计：无扩展基线改为当前源码构建

## 边界

只改测试侧。生产代码（`main.go`、`cmd/astrlink-core/fork_checkin.go`、
`internal/forkcheckin`、`internal/storage/sqlite/fork_checkin_*.go`、`controlapi`）
**一行都不动**。

改动集中在 `core/internal/forkcheckin/acceptance_process_test.go` 的
`buildAcceptanceExecutables` 与其 `build`
闭包，加上隔离测试的二进制选择，以及删除
`acceptance_control_test.go`。stub 源文件由测试在临时树中写入，不进生产树。

## 现状

```text
buildAcceptanceExecutables(t, withControl)
  ├─ git ls-tree acceptanceBaseline core      # 断言钉子不含扩展
  ├─ git archive acceptanceBaseline core convo → dir/
  ├─ build(dir/core,  baseline)               # 旧提交 → 基线
  ├─ build(root/core, current)                # 当前 → 被测
  └─ [withControl] build(dir/core, storage-fixed-core)
```

`build` 闭包内已有两个可选分支：`ASTRLINK_CHECKIN_ACCEPTANCE_DIAGNOSTIC=1` 的
`-overlay=` 注入，和 `ASTRLINK_CHECKIN_ACCEPTANCE_RACE=1` 的
`-race`。两个分支都必须继续可用。

## 目标形态

```text
buildAcceptanceExecutables(t)
  ├─ copyAcceptanceSource(root → dir/)          # 工作区 core + convo，非 git archive
  ├─ stripCheckinExtension(dir/core)           # 删扩展 + 写 stub，fail closed
  ├─ build(dir/core,  baseline)                # 当前源码去扩展 → 基线
  ├─ build(root/core, current)                 # 当前 → 被测
  ├─ 符号断言 + 迁移版本相等断言
  └─ （不再构建 storage-fixed-core）
```

### 源码一致性：复制工作区，不用 `git archive HEAD`

`current` 从工作区 `root/core` 构建。若基线取自 `git archive HEAD`，`core/`
只要有未提交改动，两侧差异就不止于扩展，测量被混淆。

因此基线树从**同一工作区**复制：

```bash
git ls-files -z --cached --others --exclude-standard -- core convo
```

- 列出的文件逐个复制到 `dir/`，保持相对路径与权限。
- 已跟踪但工作区已删除的文件（`ENOENT`）跳过 —— 它们也不参与 `current` 的构建。
- 只接受常规文件；遇到符号链接或其他类型 → `t.Fatalf`（现状：`core`、`convo`
  中无此类文件，实测 `git ls-files -s` 无非 `100644`/`100755` 项）。
- 必须同时带上 `convo`：`core/go.mod` 依赖它。

### `storage-fixed-core` 控制组退役

**此点推翻了原设计中「去扩展对它无害」的推测**，实施第 1 步读代码确认：

`prepareAcceptanceStorageControl`（`acceptance_control_test.go:20`）把当前树中的
`UpdateService`、`DeleteService`、`UpdatePolicy`、`CreateAccessToken`、
`insertAccessTokenTx` 及整个 `config_write.go`
回植进**历史基线树**，用来隔离「审阅过的配置写重试」与扩展本身。方案 C 下基线树就是当前源码，这些函数逐字节相同：

- 补丁变成空操作，`storage-fixed-core` 与 `baseline` 从同一源码构建；
- 它会继续编译、运行、通过 ——
  **能跑但不再证明它名义上的东西**，正是本任务要消除的那类风险。

处理：

| 对象                                                                                                                                                    | 处置                                                 |
| ------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------- |
| `acceptance_control_test.go`（含 `prepareAcceptanceStorageControl`、`acceptancePatchFunctions`、`TestAcceptanceStorageControlRejectsUnrelatedChanges`） | 整个文件删除                                         |
| `acceptanceExecutables.control` 字段与 `withControl` 参数                                                                                               | 删除；两个调用方改为 `buildAcceptanceExecutables(t)` |
| 隔离测试中 `baseline*` 模式取 `binaries.control`                                                                                                        | 改为取 `binaries.baseline`                           |
| 模式名 `baseline_fixed` / `_repeat` / `_lock`                                                                                                           | **保持不变**                                         |

模式名不改的理由：所有分支都按前缀 `baseline` 与后缀 `_lock`
判断（`acceptance_test.go:164/338/345/386/422`，`acceptance_isolation_test.go:38/66/114`），改名只产生无功能的变动，还会让历史日志无法对照。`_fixed`
的含义改为「去扩展的当前源码」，在模式列表旁加一行注释说明。

`baseline_fixed` 与 `baseline_fixed_repeat` 构成的同二进制 A/A 对照**保留不变**
——它检验测量环境本身，与基线取自何处无关。

## 关键契约

### `stripCheckinExtension(t, source string)`

输入解包后的 core 源码目录，就地删除扩展并写入 stub。

删除清单（相对 source）：

| 目标                                        | 性质          |
| ------------------------------------------- | ------------- |
| `internal/forkcheckin/`                     | 整个目录      |
| `internal/storage/sqlite/fork_checkin_*.go` | glob，29 文件 |
| `cmd/astrlink-core/fork_checkin.go`         | 单文件        |

fail closed 规则（对应验收标准 4）：

- 每一项若匹配数为 0 → `t.Fatalf`，提示清单已过期。
- 删除后对 source 做一次全树扫描，若仍存在路径含 `fork_checkin` 或
  `/forkcheckin/` 的 `.go` 文件 → `t.Fatalf`。
- 不接受「删了但还剩」的中间状态。

### stub 文件

写入 `source/cmd/astrlink-core/fork_checkin_absent.go`。已实测可编译：

```go
package main

// 只 import context / net/http / sqlite —— 绝不 import internal/forkcheckin。
type forkCheckinExtension struct{ handler http.Handler }

func newForkCheckinExtension(*sqlite.Store, string, func(string, ...any)) (*forkCheckinExtension, error)
func (*forkCheckinExtension) start(context.Context)
func (*forkCheckinExtension) stop()
```

`handler` 留零值 nil：`controlapi` 在 `handler.go:282` 对 `CheckinExtension`
做 nil 判断，扩展命名空间不挂载。

**stub 不得 import `internal/forkcheckin`**
—— 这是 C 不退化为 B 的唯一保证，由符号断言兜底。

### 符号断言

构建后对两个二进制各跑一次 `go tool nm`，断言结构性关系而非绝对数字：

```text
nm(baseline) 中含 "forkcheckin" 的符号数 == 0
nm(current)  中含 "forkcheckin" 的符号数  > 0
```

实测基准：0 对 435。断言只写 `== 0` 与
`> 0`，**不把 435 写进断言**（符号数会随实现变化）。

若 `go tool nm` 在某平台不可用，必须 `t.Fatal`
而非跳过 —— 跳过会让验收标准 2 静默失效。

### 迁移版本相等断言

对应验收标准 3。基线与当前同源，迁移集合必然相等，该断言**守护这个不变量**：若将来有人把基线改回历史提交，它会给出直接的原因，而不是现在这种只看得到
`ready handshake: EOF` 的间接症状。

实现（第 1 步已确认读取路径）：

- 基线侧：以空目录启动基线二进制（复用
  `startAcceptanceCore`），握手 ready 后停止，
  `sql.Open("sqlite", <dir>/astrlink.db)` 读
  `SELECT MAX(version) FROM schema_migrations`。
- 当前侧：在另一个空目录用进程内 `sqlite.Open` 建库后同样读取 —— 与
  `seedAcceptanceDirectory` 的建库路径一致。
- 两值不等 → `t.Fatalf`，消息同时打印两个版本号。

Core 不对外暴露迁移版本（只在 `migrate.go:137/192`
内部查询），`internal/forkcheckin` 已有直接 `sql.Open("sqlite", …)`
的先例（`acceptance_test.go:273/289/368`），驱动 `modernc.org/sqlite` 已在
`acceptance_process_test.go:31` 导入。

代价：多一次 Core 启停，约数秒，相对单个测试 80s 以上可忽略。

## 数据流

播种保持不变：`seedAcceptanceDirectory` 继续用当前 `sqlite.Open`
迁移到 50。这现在是安全的，因为基线也认 50。**这正是本方案相对方案 A 的核心优势**：两侧 schema 一致，p95 可比。

## 兼容性

- `acceptanceBaseline` 常量删除。保留一个不参与构建的提交号会误导后来者。
- 现有 `git ls-tree` 的「钉子不含扩展」断言由 `stripCheckinExtension` 的 fail
  closed 扫描取代，语义更强（证明构建输入确实无扩展，而非相信某个提交号）。
- `:185` 日志行 `real Core comparison against no-extension commit %s`
  改写为说明基线来源（工作区去扩展构建）与 race 开关。
- `bytes` 导入若因删除 `ls-tree` 检查而不再使用，一并移除。
- `-overlay=` 诊断分支与 `-race` 分支不变；诊断分支对 `source` 内文件做
  `EvalSymlinks`，复制出的常规文件不受影响。

## 取舍

**失去**：

- 「历史上某个真实发布过的 Core」这一层参照。可以接受 —— 原本要证明的是「扩展不拖累基础产品」，而不是「与某个历史版本对比」。
- 原约定「历史无扩展基线与带标签的存储修正对照分开保留」随之不再成立：历史基线不再构建，存储修正对照失去意义。**该约定由本设计显式撤销，需评审确认。**
- 原历史基线已知的 service-delete `500 storage_unavailable`
  问题不会再出现在基线侧，因为基线已含当前存储代码。这是去掉混淆的结果，不是修复了某个缺陷。

**保留**：扩展确实不在基线二进制里（由符号数 0 证明，强于靠提交号推断）；同二进制 A/A 对照。

**新增维护点**：删除清单与 stub 必须跟随生产侧接线变化。两者都 fail
closed，表现为构建失败或 `t.Fatalf`，不会静默放过。

## 回滚

改动局限在 `internal/forkcheckin`
的测试文件内（三个修改、一个删除）加一个测试期生成的 stub。回滚即恢复这些文件。生产行为零影响，不存在数据或发布层面的回滚需求。
