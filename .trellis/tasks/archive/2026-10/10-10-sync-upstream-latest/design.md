# 正式版同步技术方案

## 目标、边界与证据

本方案仅适用于 `v0.2.0`： `78c97c4065ec5ba2263d6d32f9f53355567ea585`。合并前本地
`HEAD` 为 `7165c7465eec555490da5b1689fa10e0f2d022af`。不合入上游 `main` 独有提交
`5ceced517b34c57329eee075069f8fd46e22c71e`。

这是一个需要统一验证的同步交付，不拆分独立产品子任务。保留 Git 双亲历史，执行获批后使用非快进、暂不提交的合并；提交和推送权限相互独立。

证据来源：

- [正式版冲突研究](research/stable-conflicts.md)：固定目标、5 个冲突、迁移及正式版契约风险。
- [共享启动与身份研究](research/persistent-core-identity.md)：对象级文件/行号、依赖流、生命周期与回归边界。
- `.trellis/spec/guides/upstream-sync-thinking-guide.md`：保留上游行为、适配 fork 接入点。

本会话重新获取远程和 GitHub API 后，固定目标未变化。
`git merge-tree --write-tree HEAD v0.2.0` 返回冲突树
`59b02f52cc3cb4be8ed6ebfa956d026b03112fc8`，退出码为预期的 1；未改变工作区或索引。

## 文本冲突解决

### 桌面脚本与构建常量

- `apps/desktop/package.json`：同时保留 fork 的
  `macos:notarize = bun scripts/macos-release.mjs verify` 和上游 `build:web`。
- `apps/desktop/rsbuild.config.ts`：同时注入 fork 的
  `ASTRLINK_RELEASE_REPOSITORY` 与上游
  `PUBLIC_ASTRLINK_EDITION`，保留 Web 输出配置。
- 不改 host 驱动的开发重载方式，不恢复已替换的 macOS 公证脚本调用。
- 自动合并处复查更新仓库间接层、默认 preview 频道和新增版本检查入口，不将 fork 更新指向原始上游。

### 契约权限

`contracts/validate.rb`
合并 operator-only 路由集合：上游网络地址读取与 fork 身份档案、捕获路由均保留，并保留理由注释。`core/internal/controlapi/handler.go`
在正式版目标下没有文本冲突，但仍需审核控制台会话与身份依赖的语义兼容。

### 主进程组装

`core/cmd/astrlink-core/main.go`
保留上游精简后的 desktop/server 共用持久化组装，不恢复旧版整块初始化代码。具体身份适配见下节。

### SQLite 写入

`core/internal/storage/sqlite/store.go`
保留上游先取得写锁再统计 token 的逻辑。在 fork 现有 `retryConfigWrite`
回调内先执行上游无行更新，再调用
`insertAccessTokenTx`；不引入嵌套事务、全局 PRAGMA 或无限重试。

保持整次短事务重试、回滚和提交不确定性语义。

- 旧 token 测试“count 之后同步启动另一个写入”不再适用于 write-first 锁定。
- 改为锁获取前竞争、最后名额/名称竞争、取消及原子性回归。
- 不能为通过旧测试删除上游锁，也不改其他仍是 read-first 的配置写入测试。

## 身份能力接入与数据流

拟新增 fork 自有 `core/cmd/astrlink-core/persistent_identity.go`，在上游
`persistent.go` 仅保留小范围调用接入：

1. 使用已经打开的同一 SQLite store 创建一个 `identitycapture.Registry`。
2. 在构造控制 API 及其连接测试器前，设置 gateway 依赖的 `IdentityCapture`、
   `IdentityProfiles`。依赖按值传递，不能事后仅修改 `core.gateway`。
3. 控制 API 使用同一捕获实例；模型发现经 `servicemodel.NewWithDependencies`
   接收相同 store、订阅管理器和 profile reader，沿用原有网络客户端默认行为。
4. 捕获初始化失败走已有 `fail(...)`
   清理路径，不能忽略错误或返回 nil 依赖继续启动。

保留上游共享的已学习订阅身份 registry、代理配置、server 的无 loopback
OAuth、控制台会话、raw 密码退避及 `NewInferenceHandler(address, networkExposed)`
分流。无持久化目录的 headless 路径不强行增加数据库依赖。

数据边界：控制 API 显式授权捕获 →
ingress 观察原始调用者请求 → 候选档案持久化 → 用户确认并绑定服务 → 普通推理/模型发现选择规则并读取档案 →
transport 最终转发。捕获不是授权，不能修改当前请求身份；未确认档案不能用于转发。

捕获 registry 的生命周期：

- 只持有内存 consent，不拥有 goroutine、外部连接或独立 Close 资源。
- 每个 core 独立创建，重启不恢复 armed 状态，持久化档案仍保留。
- 复用 store/worker/monitor 的既有清理，不另建生命周期框架。

## 转发与访问控制

- 保留上游在 target
  overlay 之后清理代理链请求头的行为；fork 规则不能反向覆盖该隔离。
- `X-AstrLink-*`、本地 cookie/token 和控制台标记不能成为转发身份。
- 通过实际 HTTP 请求及 WebSocket 握手断言，而非仅断言中间 header map。
- 上游新增本地 `X-Openai-Actor-Authorization`
  开关，而 fork 规则分类尚未保留它。在 fork 自有请求头分类策略中将其保留，添加配置拒绝与最终转发回归；若其他 target
  overlay 来源也能重引入该本地标记，在共享最终清理点做最小补强，不撤销上游功能或更改真实 provider
  credential。
- 全量身份档案/捕获与网络地址路由继续 operator-only。可信 console
  session 的 operator 身份来自上下文；不能由调用者自行提供 header、cookie 或同网段位置获得。
- 仅持有 inference token 的调用者、observer/control
  socket 不得读取敏感指纹或授权捕获。
- 保留用户正文和工具 schema，不能因提及 AstrLink 而删除用户内容。

## 兼容性与明确延期项

### 数据库与发布

上游本轮未修改迁移目录，不追加、重排或改名迁移。保留 fork 既有身份迁移和已知历史碰撞协调逻辑，测试未知历史继续 fail
closed。若执行阶段出现新证据，停止并重新规划，不操作真实数据库。

同步只改变本地代码，不执行部署、容器发布、打标签或推送。Docker/Web 新代码随正式版进入仓库，并不等于授权启动生产服务或发布镜像。

### 不借同步扩展既有行为

- 上游正式版有 Factory Droid
  Go 契约与其余层可能不匹配的静态风险。原样纳入选定版本并验证；若失败，记录精确证据后请用户决定，不私自换成主线或补齐 provider。
- 既有直接图片后端路径不保证 HTTP profile/capture；上游新增 standalone
  Images 入口复用该路径。本次不扩展为完整图片档案支持；保留新路径并检查安全边界，不声称给 gateway 接入 reader 就完成了所有特殊工具的档案支持。
- 上游 desktop 晚期错误分支调用
  `os.Exit`，不会运行 defer；本次不做通用启动生命周期重构。新增 helper 错误必须复用现有返回式清理，但不声称已修复上游所有退出分支。
- 原生 macOS 与 Unix
  socket 验证不能由 Windows 上的通过/skip 替代；Web 浏览器布局检查也不能替代 Tauri 原生标题栏空间检查。

## 回归结构与回滚

新增 fork 自有组合测试使用临时目录、离线 pricing/worker stub 和 loopback
upstream。

- 通过真实控制 API 和 production
  ingress 验证共享捕获、确认绑定、模型发现及连接测试。
- 复用已有规则、转换、重试、WebSocket、权限和存储并发测试，避免只测依赖字段存在。
- server 模式增加可移植 HTTP/console 覆盖；Unix socket 专项保留平台限制记录。

未提交合并可在用户确认后
`git merge --abort`；不得删除已存在的任务/个人记录或重置用户改动。提交后回退须另行确认
`git revert -m 1`，不使用强制推送或重写历史。
