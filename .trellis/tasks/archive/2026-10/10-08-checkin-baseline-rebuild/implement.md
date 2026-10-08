# 执行计划

工作目录 `core/`。Go 命令无需
`MISE_RUST_VERSION`。两个验收测试都很重（单个约 80s 以上），长命令放后台并留日志。

## 步骤

### 1. 先确认未经验证的两处假设（已完成）

- **假设一不成立。** `prepareAcceptanceStorageControl`
  把当前的配置写函数回植进历史基线树；基线改为当前源码后回植为空操作，`storage-fixed-core`
  与基线同源。已回 `design.md` 改为退役该控制组。
- **假设二成立。** Core 不暴露迁移版本；`internal/forkcheckin` 已有
  `sql.Open("sqlite", …)` 先例，驱动已导入，可直接读 `schema_migrations`。
- **追加发现。** 原计划 `git archive HEAD`
  与被测的工作区构建不同源，改为复制工作区。

### 2. 实现 `stripCheckinExtension`

在 `acceptance_process_test.go` 新增。按 `design.md` 的删除清单与 fail
closed 规则：每项匹配数为 0 即 `t.Fatalf`；删除后全树扫描仍有 `fork_checkin` /
`/forkcheckin/` 的 `.go` 文件即 `t.Fatalf`。

### 3. 写 stub

由 `stripCheckinExtension` 写入
`cmd/astrlink-core/fork_checkin_absent.go`。只 import `context` / `net/http` /
`sqlite`。**绝不 import `internal/forkcheckin`。**

### 4. 改构建流程

- 新增 `copyAcceptanceSource`：按
  `git ls-files -z --cached --others --exclude-standard -- core convo`
  复制工作区文件到临时目录；`ENOENT` 跳过，非常规文件 `t.Fatalf`。取代
  `git archive`。
- 复制后调用 `stripCheckinExtension`。
- 删除 `acceptanceBaseline` 常量与 `git ls-tree` 检查；改写 `:185` 日志行。
- 删除 `acceptance_control_test.go` 整个文件；删除
  `acceptanceExecutables.control` 与 `withControl` 参数，两个调用方改为
  `buildAcceptanceExecutables(t)`。
- 隔离测试 `baseline*` 模式改取 `binaries.baseline`；模式名不变，在列表旁注释
  `_fixed` 的新含义。
- 清理因此不再使用的导入（`go vet` 会报）。

保留 `-overlay=` 诊断分支与 `-race` 分支可用。

### 5. 加符号断言与迁移版本断言

按 `design.md`：`nm(baseline)==0`、`nm(current)>0`，不写死 435；`go tool nm`
不可用时 `t.Fatal` 不跳过。

迁移版本：基线二进制以空目录启停一次后 `sql.Open` 读
`MAX(version)`；当前侧用进程内 `sqlite.Open` 在另一空目录建库后读取。不等即
`t.Fatalf` 并打印两值。

### 6. 验证

```bash
cd core
gofmt -l internal/forkcheckin/
go vet ./internal/forkcheckin/
# 两个重型测试，后台跑留日志
go test ./internal/forkcheckin/ -run 'TestCheckinCoreAcceptance$' -v
go test ./internal/forkcheckin/ -run 'TestCheckinCoreIsolationAcceptance$' -v
```

基线子用例不得再出现 ready 握手 EOF；不得出现
`missing complete baseline for ...`。

性能门槛按既有规则：**仅当差值同时超过 1ms 与 5% 才算失败**。不放宽阈值，不重排或丢弃样本，不让后续通过的运行覆盖先前的失败。繁忙机器导致的超时不算通过，需安静窗口复跑。

### 7. 突变验证（验收标准 6）

每次改完**必须还原并 diff 确认**。vitest 不涉及，这里是 Go，但同样注意：
**编译失败不算有效突变** —— 若删分支导致变量未使用，补 `_ = x`
让它编译通过，由测试而非编译器报错。

| 突变                                             | 期望                         |
| ------------------------------------------------ | ---------------------------- |
| stub 改为 import 并返回真实 facade（C 退化为 B） | 红                           |
| 删除清单去掉 `internal/forkcheckin/` 一项        | 红                           |
| 基线改回从 `e7ebad2` 复制源码（基线≠当前源码）   | 红（由删除清单零匹配报出）   |
| 基线树中删去最后一条主迁移（模拟落后的基线）     | 红（必须由迁移版本断言报出） |
| 删除清单去掉 `fork_checkin_*.go` glob 一项       | 红                           |
| 未突变基线行                                     | 绿                           |

最后一行是必需的：没有绿色基线行，无法证明检测器本身有效。

`e7ebad2`
那行会先被删除清单的零匹配拦下，测不到迁移版本断言，所以另设「删去最后一条主迁移」一行专门检验它。每行都要确认**是预期的那个检测器**报的错，而不只是变红。

### 8. 收尾

```bash
cd core && gofmt -l internal/forkcheckin/ && go vet ./internal/forkcheckin/
cd .. && git diff --check
```

## 评审门

步骤 7 的突变矩阵全部符合期望后再报告完成。**若「stub 退化为 B」那条突变没有转红，说明符号断言无效，必须先修断言，不能以测试通过为由放过。**

## 回滚点

- 步骤 2–5 的改动都在 `internal/forkcheckin` 的测试文件内（含删除
  `acceptance_control_test.go`），`git checkout -- <files>` 即可回退。
- 步骤 1 发现假设不成立 → 回 `design.md`，不在实施中临时改设计。

## 不做

- 不改生产代码。
- 不动发布与 tag。
- 不优化性能数值本身。
- 不碰 `storage/sqlite` 的 compat 门槛（已修完）。
