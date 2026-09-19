# PRD — 子任务：核心包补充单元测试（auth / pool / server / scheduler）

父任务：`09-19-leftover-completion`（本子任务拥有父 PRD 的 R3 与 R4 限制段）。
类型：复杂任务（需要 `design.md` + `implement.md`）。

## 前置依赖（必须写进排期，不靠树结构推断）

**必须在 `09-19-account-reenable` 归档之后再实现。** 两个子任务都要改 `internal/pool/pool.go` 与 `internal/server/handler.go`，并都会新增同名的包内测试文件；先做 account-reenable 可让本子任务直接为新行为补测试，避免同文件写冲突与重复返工。

## Goal

为 README 声明「无单测」的四个包补上 stdlib 单测，覆盖各自的状态机、持久化、路由与调度行为，使该限制条目可以删除。

## Confirmed Facts

- `go test ./...` 现状：`internal/auth`、`internal/pool`、`internal/server`、`internal/scheduler` 与 `cmd/*` 全部 `[no test files]`；只有 `internal/upstream` 有测试（`client_test.go`、`checkin_test.go`）。
- 测试规范在 `.trellis/spec/backend/quality-guidelines.md:28-36`：仅 `testing` + `net/http/httptest`、无断言库、测试文件与被测代码同包同名规则、上游行为用 `httptest.Server` + `t.Cleanup`、环境用 `t.Setenv`、包级缓存显式重置、协议敏感行为用字节级断言、回归测试按「防止的失败」命名。
- `internal/pool` 的 `saveLocked`（`pool.go:289-324`）在 `stateFp` 为空时不落盘，便于纯内存用例；非空时 tmp+rename 写 0600，可用 `t.TempDir()` 验证。
- `internal/server` 有包级可变状态 `dynamicModelsCache`（`handler.go:117-123`）和 `upstream.serverBaseOverride`（`client.go:26`），测试之间必须重置，否则互相污染。
- `internal/scheduler` 的 `nextFire`（`scheduler.go:40-52`）是纯函数，可无副作用测试；`RunCheckinNow` / `RunKeepaliveNow`（`:87-131`）依赖 `upstream.Client` + `pool.Pool`，可通过 `httptest` 假上游驱动。
- 签到链路还需要 `LB2A_UPDATE_API`（`internal/upstream/checkin.go:27`），`checkin_test.go` 已给出 `t.Setenv` + `versionCache` 重置的范式。
- README `:211` 现描述：「No unit tests yet for the `pool`, `auth`, `server` and `scheduler` packages」。

## Requirements

- **R-T1**：`internal/auth/auth_test.go` 覆盖：嵌套形解析、扁平形解析、空输入/非法 JSON/缺 accessToken 的报错、`NeedsRefresh` 边界（无 expiry、窗口内、窗口外）、`KeyfromBody` 可选字段、`SaveAtomic` 往返与 0600/无残留 `.tmp`、`LoadDir` 跳过坏文件并按 glob 取文件。
- **R-T2**：`internal/pool/pool_test.go` 覆盖：`Pick` 取 healthy 中积分最高者、跳过冷却/禁用账号、`PickExcluding` 跳过已试 uid、冷却到期后恢复、`NoteError` 达阈值触发冷却、`NoteSuccess` 清零错误计数、`ReenableIfCredits` 的两种分支（禁用不恢复 / 冷却且余额>0 恢复）、`Disable` 粘性、`List` 按 uid 排序、`state.json` 往返（含 `disabled`/`until` 保留与占位账号被 `Add` 替换）、`SyncToDir` 增删账号时保留既有状态。
- **R-T3**：`internal/server/handler_test.go` 覆盖：`/healthz`、`/status` 结构、`withAuth` 的 401/200 两条路径、非流式聚合返回、流式字节级透传、上游 200 错误帧（40201）→ 该账号被冷却并轮换到下一账号、全部不可用 → 503 `no_healthy_account`、动态模型失败时回退静态表。
- **R-T4**：`internal/scheduler/scheduler_test.go` 覆盖：`nextFire` 取最近整点与跨天滚动、`RunCheckinNow` 对冷却账号签到后余额>0 则解冻、禁用账号被跳过（不发任何请求）、`RunKeepaliveNow` 刷新成功后落盘、刷新报 `ErrSessionDead` 时账号被禁用。
- **R-T5**：测试必须确定性：不依赖真实网络、不依赖长 sleep（冷却到期类用例用可立即过期或极短的时长）、不依赖执行顺序（包级缓存与全局 `serverBaseOverride` 在 `t.Cleanup` 恢复）。
- **R-T6**：仅使用 stdlib（`testing`、`net/http/httptest`、`encoding/json`、`bytes` 等），`go.mod` 保持无 `require`。
- **R-T7**：README 移除「No unit tests yet for the …」条目；若同一段还有其它条目（会话失效账号的限制），保留不动——它由兄弟子任务 `09-19-account-reenable` 负责。
- **R-T8**：不为 `cmd/*` 三个 binary 补测试。
- **R-T9**（2026-09-19 修订新增，由本任务测试暴露）：修复 `internal/pool` 的 `SyncToDir` 增删账号不落盘缺陷。`pool.go` 中「新账号入池」与「账号被移除」两个分支不调用 `saveLocked()`，与 `persistence-guidelines.md`「`pool.saveLocked()` on every mutation」矛盾；后果是 rescan 移除账号后 `state.json` 仍留着它，若在下一次任意其它变更前重启，`load()` 会把它复活成 `AccessToken` 为空的占位账号，而 `cmd/server/main.go` 只会 `Add` 仍在 `auths/` 里的 uid，于是该占位账号能被 `Pick()` 选中（向上游发空 Bearer），并出现在 `/status` 里。
  - 修复要求：仅在账号集合**确实发生变化**（有新增或有移除）时才 `saveLocked()`，避免 30s 一次的重扫每次都写盘。
  - 回归测试 `TestSyncToDirPersistsAddAndRemove`：新增/移除后 `state.json` 与内存一致；用 `New(fp)` 重新载入后被移除的账号不再出现（不产生空令牌占位账号），新增账号的 uid 存在。
  - 该修复不得改变 `healthy()` / `Pick()` 的既有语义，也不得改变「移除账号时保留其状态以备回归」的既有行为（状态保留在内存中由 `SyncToDir` 现有的 `delete` 语义决定，本次只补落盘）。
- **R-T10**：本任务只允许 R-T9 这一处生产代码修改；若还发现其它缺陷，记录到 notes 并另行开任务。

## Acceptance Criteria

- [x] A-T1：`internal/auth/auth_test.go`、`internal/scheduler/scheduler_test.go` 新增，`internal/pool/pool_test.go`、`internal/server/handler_test.go` 扩展；`go test ./...` 全绿，四个包均不再是 `[no test files]`（覆盖率：auth 88.2%、pool 94.2%、server 76.6%、scheduler 73.2%、upstream 33.8%）。
- [x] A-T2：四个测试文件只 import 标准库与本仓库包，全部使用 `t.Fatalf` 并携带 got/want；无断言库、无 golden 文件、无真实网络。
- [x] A-T3：`go test ./... -count=2` 与 `go test ./internal/... -shuffle=on -count=2` 均通过（48 个用例 × 重复运行，无顺序依赖）。
- [x] A-T4：`go vet ./...` 无输出、`go build ./...` 通过。
- [x] A-T5：README 已无「No unit tests yet …」；改写后的条目如实披露 `cmd/*` 三个 binary 仍无测试。
- [x] A-T6：断言与 `error-handling.md`（`rc == nil` 三态契约、hard-credit 冷却与轮换、`ErrSessionDead` → `Disable`、best-effort 签到）、`persistence-guidelines.md`（tmp+rename 原子写、每次变更落盘、占位账号语义）、`upstream-integration.md`（信封与端点形态）逐条对应；除 R-T9 外未发现其它代码与 spec 冲突。
- [x] A-T7（R-T9）：`SyncToDir` 增删落盘修复 + `TestSyncToDirPersistsAddAndRemove`；检查阶段用两次反向变异证明测试有效（去掉移除分支的 `changed` → 「state.json has 2 accounts, pool has 1」；把 `if changed` 改成恒真 → 「unchanged rescan must not rewrite state.json」）；既有 7 个 pool 测试全部保持通过。

## 检查阶段记录（最终全范围复查）

- **变异抽查（7 项，全部被测试捕获，回滚后树字节一致）**：`Pick` 的积分比较、handler 的 `rc == nil` 分支（改成旧反模式会 panic）、`nextFire` 的最近整点选择、`auth.Parse` 的 accessToken 校验、`SyncToDir` 的 `changed` 守卫（两个方向）、`RunCheckinNow` 的禁用跳过、`prepareChatBody` 的强stream。回滚前后 `git diff | sha256sum` 与 `git status --porcelain` 完全一致，无残留文件。
- **[低] 已修复**：`TestChatCompletionsNonStreamingAggregates` 的注释声称锁住「客户端 stream=false 必须被改写成 true」，但当时没有断言；已补断言并由变异 7 证明其可失败。
- **[信息]** `TestModelsFallsBackToStaticOnUpstreamFailure` 与 `staticModels` 自比较（能捕获回退被移除，不能捕获目录收缩）；`scheduler` 包无法重置 `upstream.versionCache`（未导出，跨包不可达，且不影响确定性）；`pool.go` 中「消失的账号剔除（状态保留）」注释与 `delete` 语义本就矛盾，属既有文案问题，按 R-T10 未改。

## 变更记录

- **2026-09-19（Phase 2.1 实现后回退修订）**：实现阶段写 `SyncToDir`/持久化测试时发现真实缺陷——`SyncToDir` 的增删分支不落盘（与 `persistence-guidelines.md` 的 "saveLocked on every mutation" 矛盾），重启后会把已移除账号复活成空令牌占位账号并可被 `Pick()` 选中。原 R-T8 要求「发现缺陷只记录不改」，现改为把这一处修复纳入本任务（R-T9/R-T10），因为缺陷正是本任务的测试目标区域、修复约 3 行且已有可复现路径；其余缺陷仍只记录。

## Out of Scope

- 覆盖率百分比目标、`-race` 之外的性能/压力测试、CI 集成（现有 workflow 只发布镜像）。
- 重写 `internal/upstream` 已有测试。
- 修复测试过程中发现的生产缺陷（记录后另行开任务）。
