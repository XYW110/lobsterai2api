# design — 子任务：失效账号恢复

父任务：`09-19-leftover-completion`。PRD：[`prd.md`](./prd.md)。

## 问题本质

`disabled` 是「需要人工介入」的终态（session 死亡、refresh 无有效凭证），目前只有三处 `Disable` 写入、零处清除——`pool.Enable` 不存在，`ReenableIfCredits`（`pool.go:187-199`）显式跳过 disabled 账号。于是重新登录这条本该是"人工介入"的路径，因为 `Add` / `SyncToDir` 只覆盖凭证、不动状态标志而失效。

## 边界

| 层 | 改动 | 不动 |
|---|---|---|
| `internal/pool` | `entry` 凭证变更判定 + `Pool.Enable` | 冷却/错误的既有语义、`state.json` 字段 |
| `internal/server` | 一条新路由 + handler | `withAuth`、既有路由、错误体形态 |
| `internal/upstream` / `internal/scheduler` | 无 | 三处 `Disable` 触发条件 |

## 契约 1：凭证变更即恢复（pool 内部）

在 `entry` 上加一个判定方法，供 `Add` 与 `SyncToDir` 的「账号已存在」分支共用：

```go
// rotateCredentials 用新凭证覆盖旧凭证；旧 accessToken 非空且发生变化时，
// 视为重新登录，清空禁用/冷却/错误计数并返回 true（调用方记日志）。
func (e *entry) rotateCredentials(next *auth.Auth) bool {
	changed := e.a.AccessToken != "" && e.a.AccessToken != next.AccessToken
	e.a = next
	if !changed {
		return false
	}
	e.disabled = false
	e.until = time.Time{}
	e.reason = ""
	e.errCount = 0
	return true
}
```

**为什么必须判 `e.a.AccessToken != ""`**：`pool.New` 载入 `state.json` 时给每个账号塞的是 `&auth.Auth{UID: uid}` 占位（`pool.go:279-280`），accessToken 为空。启动后 `main.go` 立刻 `Add` 真实凭证——若不排除空旧值，每次重启都会把 `state.json` 里的 `disabled` 清掉，禁用状态就再也持久不住（违反 `persistence-guidelines.md` 的既有语义）。

**为什么用 accessToken 作指纹**：它是 session 存活的直接凭据，登录工具每次 exchange 都会写入新值；而服务端自身的 `RefreshToken` 刷新虽然会改内存里的 `a` 与磁盘文件，但 rescan 读回的字符串与内存一致（`SyncToDir` 比较的是同一值），不会触发误恢复。`ExpiresAt` / `LastKeyfrom` 不适合做指纹——前者会随刷新变化，后者是活动时间戳。

日志（遵循 `logging-guidelines.md` 的 `<operation> <id>: <detail>` 形态，绝不含令牌）：

```go
log.Printf("auth_rotate uid=%s: access token changed, account re-enabled", a.UID)
```

调用点：
- `Add`（`pool.go:98-101` 的已存在分支）→ `if e.rotateCredentials(a) { log...; p.saveLocked() }`
- `SyncToDir`（`pool.go:112-114` 的已存在分支）→ 同上

两个调用点都在持写锁的临界区内，日志在锁内输出（与 `saveLocked` 的既有做法一致，避免为日志引入额外的锁次序）。

**check 阶段补充（2026-09-19）：恢复必须同时 `saveLocked()` 落盘。** 原设计只改内存，探针证明这会在重启时静默回退：`SyncToDir` 恢复后 `state.json` 仍是 `disabled:true`，重启时 `load` 读回禁用态，而占位保护（契约 1）又拦住了新凭证，账号永久停用——正是本任务要消除的故障换个入口重现。`persistence-guidelines.md` 也要求 `saveLocked()` 在每次状态变更时写盘。回归测试 `TestSyncToDirReenablePersistsAcrossRestart` 锁住该行为；写盘只发生在令牌真变化的路径（重登事件），rescan 的常规同步不写盘。

## 契约 2：手动启用（pool 导出 API）

```go
// Enable 手动清除账号的禁用与冷却状态（人工重登后无法自动判定时使用）。
// 返回是否命中账号；credits 不受影响。
func (p *Pool) Enable(uid string) bool
```

- 命中：清 `disabled` / `until` / `reason` / `errCount` → `saveLocked()` → `true`。
- 未命中：不改任何状态、**不写盘**、返回 `false`（避免为不存在的账号刷盘）。

## 契约 3：HTTP 接口（server 层）

```
POST /admin/accounts/{uid}/enable
```

- 注册方式沿用 Go 1.22 方法模式（`handler.go:57-60`），**套用 `withAuth`**：设置了 `LB2A_API_KEY` 时未带 Bearer 直接 401（`invalid_api_key`），与 `/v1/*` 一致。
- `uid` 取自 `r.PathValue("uid")`（Go 1.22 自动解码路径段），不做额外反转义。
- 命中：`200` + 该账号的脱敏 `pool.Status`（JSON，字段与 `/status` 里的一致；用 `Pool.List()` 扫描取回，避免为一次读引入新导出方法；若扫描不到——例如同时被 rescan 移除——按未命中处理）。
- 未命中：`404` + `writeOpenAIError(w, 404, "account_not_found", ...)`，保持既有 OpenAI 错误体形态。
- 语义上这是**运维接口**，不是 OpenAI 兼容面，所以放在 `/admin` 前缀下，不污染 `/v1`。

## 兼容性 / 回滚

- `state.json` 结构不变，旧文件照常可读；`Enable` 只是把已有字段置零。
- `Add` / `SyncToDir` 的行为变化仅限「旧令牌非空且变化」这一条路径，其余路径逐字不变。
- 回滚 = 回退本子任务提交；无迁移、无残留文件。

## 已知取舍

- 令牌未变时无法自动恢复：运维若只是把同一份文件重写一遍，仍需要显式调用 `enable`（这是有意的——避免误判把 session 已死的账号放回池里反复失败）。
- `/status` 仍未鉴权（保持现状，本子任务不动），因此只把**写操作**放进 `withAuth`，读接口的鉴权缺口记录在 PRD 的 Out of Scope 中。
