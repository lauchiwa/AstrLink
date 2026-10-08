# 执行计划

工作目录 `core/`。Go 命令无需 `MISE_RUST_VERSION`。两个验收测试都很重
（单个约 80s 以上），长命令放后台并留日志。

## 步骤

### 1. 先确认未经验证的两处假设

实施前必须读代码确认，**不得假定**：

- 读 `prepareAcceptanceStorageControl`，确认 `storage-fixed-core` 从去扩展的树构建
  不破坏它的用途（它针对 service-delete 存储缺陷，与扩展无关）。
- 确认迁移版本的读取路径：Core 是否暴露该值，还是需要直接打开库文件读
  `MAX(version) FROM schema_migrations`。

任一假设不成立 → 回到 `design.md` 调整，不要硬改。

### 2. 实现 `stripCheckinExtension`

在 `acceptance_process_test.go` 新增。按 `design.md` 的删除清单与 fail closed
规则：每项匹配数为 0 即 `t.Fatalf`；删除后全树扫描仍有 `fork_checkin` /
`/forkcheckin/` 的 `.go` 文件即 `t.Fatalf`。

### 3. 写 stub

由 `stripCheckinExtension` 写入 `cmd/astrlink-core/fork_checkin_absent.go`。
只 import `context` / `net/http` / `sqlite`。**绝不 import
`internal/forkcheckin`。**

### 4. 改构建流程

`git archive` 的对象从 `acceptanceBaseline` 改为当前提交；解包后调用
`stripCheckinExtension`。处理好 `acceptanceBaseline` 常量与 `:185` 日志行
（删除或改写，不留悬空提交号）。

保留 `-overlay=` 诊断分支与 `-race` 分支可用。

### 5. 加符号断言与迁移版本断言

按 `design.md`：`nm(baseline)==0`、`nm(current)>0`，不写死 435；`go tool nm`
不可用时 `t.Fatal` 不跳过。迁移版本相等断言按步骤 1 的确认结果落地。

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

性能门槛按既有规则：**仅当差值同时超过 1ms 与 5% 才算失败**。不放宽阈值，不重排或
丢弃样本，不让后续通过的运行覆盖先前的失败。繁忙机器导致的超时不算通过，需安静
窗口复跑。

### 7. 突变验证（验收标准 6）

每次改完**必须还原并 diff 确认**。vitest 不涉及，这里是 Go，但同样注意：
**编译失败不算有效突变** —— 若删分支导致变量未使用，补 `_ = x` 让它编译通过，由
测试而非编译器报错。

| 突变                                    | 期望 |
| --------------------------------------- | ---- |
| stub 改为 import 并返回真实 facade（C 退化为 B） | 红   |
| 删除清单去掉 `internal/forkcheckin/` 一项      | 红   |
| 基线改回从 `e7ebad2` 构建（基线≠当前源码）           | 红   |
| 未突变基线行                                 | 绿   |

最后一行是必需的：没有绿色基线行，无法证明检测器本身有效。

### 8. 收尾

```bash
cd core && gofmt -l internal/forkcheckin/ && go vet ./internal/forkcheckin/
cd .. && git diff --check
```

## 评审门

步骤 7 的突变矩阵全部符合期望后再报告完成。**若「stub 退化为 B」那条突变没有转
红，说明符号断言无效，必须先修断言，不能以测试通过为由放过。**

## 回滚点

- 步骤 2–5 的改动都在一个测试文件内，`git checkout -- <file>` 即可回退。
- 步骤 1 发现假设不成立 → 回 `design.md`，不在实施中临时改设计。

## 不做

- 不改生产代码。
- 不动发布与 tag。
- 不优化性能数值本身。
- 不碰 `storage/sqlite` 的 compat 门槛（已修完）。
