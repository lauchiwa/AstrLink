# 签到验收基线改为当前源码去扩展构建

> **已废弃（未实施）**：签到功能整体移除，本任务的前提不复存在。保留规划与第 1 步
> 的核实结论（`storage-fixed-core` 控制组在方案 C 下退化为空操作、`git archive
> HEAD` 与工作区构建不同源）仅作记录。后续见任务 `10-08-remove-checkin`。

## 背景

`core/internal/forkcheckin` 的两个验收测试 `TestCheckinCoreAcceptance` 与
`TestCheckinCoreIsolationAcceptance`
需要一个「没有签到扩展的真实 Core」作为对照。当前做法是把 `acceptanceBaseline`
钉在提交 `e7ebad2` （`acceptance_process_test.go:34`），用 `git archive`
解包后构建基线二进制。

上游同步（合并
`69fce40`，即上游 v0.1.9）之后这个做法失效，并暴露出它一直存在的缺陷。

### 失效的直接原因

播种目录用的是**当前**存储包：`seedAcceptanceDirectory`
（`acceptance_process_test.go:203`）调用
`sqlite.Open`，把库迁移到 50。基线 Core 只认 49，启动即退出：

```text
astrlink-core: open persistent store: migrate sqlite database:
database schema is newer than this core: database=50 core=49
```

测试侧表现为 ready 握手 EOF，0.04–0.10s 失败：

```text
acceptance_test.go:341: Core failed its bounded ready handshake: EOF
acceptance_test.go:430: missing complete baseline for on_success
```

该结论已用手工复现证实：当前 Core 建库后 `MAX(version)=50`，以 `e7ebad2`
构建的基线 Core 打开同一目录即报上述错误并 exit=1。

### 更重要的缺陷

基线二进制与当前的差异**现在是 17 个上游提交加上签到扩展**。性能对照已不再隔离扩展，而是在比较「当前 vs 一个旧快照」，上游的任何性能变化都会被归到扩展账上。

历史钉子只在 `e7ebad2`
恰好等于「当前减去扩展」的那一瞬间成立。迁移 50 的拒绝启动不是病根，是这个混淆因子长到藏不住了的第一个症状。

### 已排除的两条路

- **把钉子前移**：不存在「迁移集合等于当前、且不含扩展」的提交。`ed99df9` 引入
  `service_identity_profiles` 时它是版本 48，当前是 50（中间有上游的 48
  `request_first_answer_timing` 与 49
  `privacy_token_kinds_audit_chunks`）。迁移 50 是这次合并才产生的。
- **让基线自己建库**（原方案 A）：只修「跑不起来」，不修「测的不是那个东西」。且会让基线在迁移 49、当前在 50 上跑，两侧连 schema 都不同，产出一个无法归因的 p95 数字。**跑得起来但测错东西，比跑不起来更危险**，故否决。

## 目标

基线改为**从当前源码构建、但不含签到扩展**的 Core，使对照两侧共享同一份上游代码与同一套迁移，恢复「扩展编入并启用」对比「扩展根本不在二进制里」这一原本要证明的命题。

这同时满足 `.trellis/spec/guides/upstream-sync-thinking-guide.md`
的第 5 条：我方门槛适配上游，而不是把上游钉住。下次上游再推进迁移，该门槛不应再因
`database=N+1 core=N` 倒掉。

## 范围

### 1. 构建无扩展基线

`buildAcceptanceExecutables`（`acceptance_process_test.go:105–111`）改为归档当前提交而非
`acceptanceBaseline`，在解包出的副本中删除扩展文件，再补一个不引入
`internal/forkcheckin` 的 stub 以满足 `main.go` 的调用点。

可行性已实测验证，不是推断：

- 扩展在生产侧的接线只有 `cmd/astrlink-core/fork_checkin.go` 与 `main.go`
  的三处调用（310 建、366 start、375 stop）加一处依赖赋值（343
  `CheckinExtension: checkin.handler`）。
- `internal/storage/sqlite` 下 29 个 `fork_checkin_*.go`
  是叶子文件：非扩展文件对
  `EnsureForkCheckinSchema`、`ForkCheckinModuleFactory`、
  `ForkCheckinSchemaPresent` 的引用数为 0，`store.go` 与 `migrate`
  也不提扩展表。
- 删除 `internal/forkcheckin`、29 个 sqlite 扩展文件与
  `cmd/astrlink-core/fork_checkin.go` 后，**只剩 `main.go:310`
  一处未定义**，其余三处都经由 `checkin` 变量，因此补 stub 即可，`main.go`
  无需改动。
- stub 以 nil `http.Handler` 落地，`controlapi` 在 `handler.go:282` 对
  `CheckinExtension` 做 nil 判断，扩展命名空间不会挂载。
- 实测结果：stub 构建 `build_exit=0`；`go tool nm` 下无扩展基线的 `forkcheckin`
  符号为 **0**，当前二进制为 **435**；该基线打开迁移 50 的库正常输出
  `"event":"ready"`。

### 2. 保留原钉子注释的用意

原注释的意图是「基线不得等于被测实现，以免门槛变空」。新做法下基线与当前同源，必须由构建过程本身证明扩展确实不在其中，而不是靠提交号。

### 3. 覆盖两个测试

`TestCheckinCoreIsolationAcceptance` 与 `TestCheckinCoreAcceptance` 共用
`buildAcceptanceExecutables`，一处改动同时覆盖。

`acceptance_control_test.go` 的 `storage-fixed-core`
控制组随之**退役**（实施第 1 步读代码后修订，原文要求「不得被破坏」）。它的作用是把当前的配置写重试回植进历史基线树；基线改为当前源码后，回植变成空操作，该控制组与基线同源，能跑但不再证明任何东西。隔离测试的
`baseline*` 模式改用去扩展基线。

### 4. 基线与被测同源

基线树从**工作区**复制，而非
`git archive HEAD`。被测二进制从工作区构建，若基线取自提交，`core/`
的任何未提交改动都会混进两侧差异。

## 验收标准

1. 两个验收测试在安静窗口通过，且日志中基线不再出现 ready 握手 EOF。
2. 基线二进制的 `forkcheckin` 符号数为 0，当前二进制 >
   0，并由测试断言而非人工检查。
3. 基线与当前的主迁移版本相同；测试断言该相等关系，使下次上游推进迁移时该门槛不会倒掉。
4. 删除清单失效时**fail
   closed**：若预期删除的扩展文件一个都没匹配到，或删除后仍有 `fork_checkin` /
   `/forkcheckin/` 残留，测试必须失败并指出清单已过期（对照现有诊断 overlay 的
   `strings.Count(...) != 1` →
   `t.Fatalf("diagnostic overlay no longer matches %s")` 写法）。
5. 性能门槛阈值不放宽，不重排或丢弃样本，不让后续通过的运行覆盖先前的失败。
6. 突变验证：至少证明新基线仍能抓到「stub 把扩展留在了二进制里」（即 C 退化为 B）与「基线等于被测实现」两种情形。
7. `gofmt`/`go vet` 干净；`git diff --check` 干净。

## 不在范围

- 发布与推送。tag 是否改名为 `v0.1.9-lauchiwa.1` 另行决定。
- 性能数值本身的优化。本任务只恢复对照的可比性，不调优。
- 签到扩展的功能改动。
- `storage/sqlite`
  的 compat 门槛（`fork_checkin_compat_test.go`），已在上一个任务按同一原则修完。
- 那四个未跟踪的 agent 目录是否纳入版本管理。

## 风险

- **stub 漂移**：生产侧新增扩展接线点时，stub 不再满足编译。这是可接受的失败方式（构建失败而非静默跳过），但必须保证它表现为清晰的构建错误。
- **退化为 B**：若 stub 意外引入
  `forkcheckin`（例如为了复用某个类型），扩展会被编回二进制，对照悄悄变成「启用 vs 关闭」。验收标准 2 与 6 专门防这一点。
- **两个重型测试在繁忙机器上仍可能超时**。本任务不解决机器噪声；性能结论仍需安静窗口。
