# 技术设计：无扩展基线改为当前源码构建

## 边界

只改测试侧。生产代码（`main.go`、`cmd/astrlink-core/fork_checkin.go`、
`internal/forkcheckin`、`internal/storage/sqlite/fork_checkin_*.go`、`controlapi`）
**一行都不动**。

改动集中在 `core/internal/forkcheckin/acceptance_process_test.go` 的
`buildAcceptanceExecutables` 与其 `build` 闭包；新增一个 stub 源文件由测试在临时树
中写入，不进生产树。

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
`-overlay=` 注入，和 `ASTRLINK_CHECKIN_ACCEPTANCE_RACE=1` 的 `-race`。两个分支都
必须继续可用。

## 目标形态

```text
buildAcceptanceExecutables(t, withControl)
  ├─ git archive HEAD core convo → dir/        # 当前源码，不再用历史提交
  ├─ stripCheckinExtension(dir/core)           # 删扩展 + 写 stub，fail closed
  ├─ build(dir/core,  baseline)                # 当前源码去扩展 → 基线
  ├─ build(root/core, current)                 # 当前 → 被测
  └─ [withControl] build(dir/core, storage-fixed-core)
```

`storage-fixed-core` 本来就从 `dir/core` 构建，自动继承「去扩展」属性。需确认这
不破坏它的用途：它是为验证历史基线的 service-delete 缺陷而存在的「存储已修正的
对照」，与扩展无关，去扩展对它无害 —— **此点需在实施时读
`prepareAcceptanceStorageControl` 确认，不得假定**。

## 关键契约

### `stripCheckinExtension(t, source string)`

输入解包后的 core 源码目录，就地删除扩展并写入 stub。

删除清单（相对 source）：

| 目标                                        | 性质         |
| ------------------------------------------- | ------------ |
| `internal/forkcheckin/`                     | 整个目录     |
| `internal/storage/sqlite/fork_checkin_*.go` | glob，29 文件 |
| `cmd/astrlink-core/fork_checkin.go`         | 单文件       |

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

`handler` 留零值 nil：`controlapi` 在 `handler.go:282` 对 `CheckinExtension` 做
nil 判断，扩展命名空间不挂载。

**stub 不得 import `internal/forkcheckin`** —— 这是 C 不退化为 B 的唯一保证，由
符号断言兜底。

### 符号断言

构建后对两个二进制各跑一次 `go tool nm`，断言结构性关系而非绝对数字：

```text
nm(baseline) 中含 "forkcheckin" 的符号数 == 0
nm(current)  中含 "forkcheckin" 的符号数  > 0
```

实测基准：0 对 435。断言只写 `== 0` 与 `> 0`，**不把 435 写进断言**（符号数会随
实现变化）。

若 `go tool nm` 在某平台不可用，必须 `t.Fatal` 而非跳过 —— 跳过会让验收标准 2
静默失效。

### 迁移版本相等断言

对应验收标准 3，防止这一类故障复发。基线与当前都构建自同一源码，迁移集合必然相
等，因此该断言是在**守护这个不变量**：若将来有人把基线改回历史提交，断言会立刻
失败。

实现取向：由两个二进制各自在空目录建库后读 `MAX(version) FROM schema_migrations`
并比较。读取方式需在实施时确认 —— Core 不一定暴露该值的查询入口，可能需要直接打开
各自建出的库文件读取。

## 数据流

播种保持不变：`seedAcceptanceDirectory` 继续用当前 `sqlite.Open` 迁移到 50。这现在
是安全的，因为基线也认 50。**这正是本方案相对方案 A 的核心优势**：两侧 schema 一
致，p95 可比。

## 兼容性

- `acceptanceBaseline` 常量不再用于构建。不要留着不用 —— 要么删除，要么改成记录
  「历史钉子已废弃及原因」的注释。保留一个未使用的提交号会误导后来者。
- 现有 `git ls-tree` 的「钉子不含扩展」断言由 `stripCheckinExtension` 的 fail
  closed 扫描取代，语义更强（证明构建输入确实无扩展，而非相信某个提交号）。
- 日志行 `real Core comparison against no-extension commit %s`
  （`acceptance_process_test.go:185`）需改写，否则会继续打印一个已不参与构建的提交
  号。

## 取舍

**失去**：「历史上某个真实发布过的 Core」这一层参照。新基线是一个从未发布的构建
配置。可以接受 —— 原本要证明的是「扩展不拖累基础产品」，而不是「与某个历史版本
对比」。

**保留**：扩展确实不在基线二进制里（由符号数 0 证明，强于靠提交号推断）。

**新增维护点**：删除清单与 stub 必须跟随生产侧接线变化。两者都 fail closed，表现为
构建失败或 `t.Fatalf`，不会静默放过。

## 回滚

改动局限在一个测试文件加一个测试期生成的 stub。回滚即恢复该文件。生产行为零影响，
不存在数据或发布层面的回滚需求。
