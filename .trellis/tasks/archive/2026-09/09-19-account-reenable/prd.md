# PRD — 子任务：失效账号恢复（凭证变更自动启用 + 手动 enable 接口）

父任务：`09-19-leftover-completion`（本子任务拥有父 PRD 的 R2 与 R4 限制段）。
类型：复杂任务（需要 `design.md` + `implement.md`）。

## Goal

账号因 session 失效被 `Disable` 后，重新登录（auth 文件里出现新 accessToken）必须能在不重启进程的情况下自动恢复；同时给运维一个受鉴权的手动启用接口。

## Confirmed Facts

- `internal/pool/pool.go:95-103`（`Add`）与 `:106-123`（`SyncToDir`）在账号已存在时只覆盖 `e.a`，`disabled` / `until` / `errCount` 原样保留。
- 禁用触发点共三处：`internal/server/handler.go:203`（refresh 遇 `ErrSessionDead`）、`:232`（200 错误帧分类为 `ErrSessionDead`）、`internal/scheduler/scheduler.go:123`（keepalive 刷新失败）。
- `disabled` 会写进 `state.json`（`pool.go:69-76`、`:289-324`），所以重启也不会自动恢复。
- 启动流程 `cmd/server/main.go:56-59`：`pool.New(stateFile)` 先载入状态（此时 `entry.a` 是只有 UID 的占位对象，`pool.go:279-280`），随后 `p.Add(a)` 用真实凭证覆盖。
- `auths/` 每 30s 重扫一次并调用 `SyncToDir`（`cmd/server/main.go:112-127`），登录工具写新文件后无需重启即可进入池。
- 路由用 Go 1.22 方法模式（`handler.go:57-60`），`withAuth` 只在设置 `LB2A_API_KEY` 时校验 Bearer（`handler.go:68-79`）；`/status` 目前无鉴权并直接输出 `pool.List()`。
- README `:210` 的现描述「Session-dead accounts stay disabled until re-login or file replacement (no re-enable endpoint)」前半句与实际行为不符（重登也不会恢复），后半句属实。

## Requirements

- **R-E1**：凭证变更即恢复。`Add` / `SyncToDir` 在覆盖凭证时，若**旧 accessToken 非空且与新值不同**，清除该账号的 `disabled`、`until`、`reason`、`errCount`，并输出一行日志说明账号被自动启用（含 uid，不含令牌内容）。
- **R-E2**：占位保护。启动时 `pool.New` 载入的占位账号（旧 accessToken 为空）不得被判定为「凭证变更」，否则 `state.json` 里的禁用状态会在每次重启时被清掉。
- **R-E3**：令牌未变时不得恢复。旧值非空且等于新值时，`disabled` 保持原样（运维只是重写了同一文件不应解锁）。
- **R-E4**：新增 `Pool.Enable(uid string) bool`：清除 `disabled` / `until` / `reason` / `errCount`，不改动 `credits`，返回是否命中账号；命中后写盘（与其它变更方法一致）。
- **R-E5**：新增路由 `POST /admin/accounts/{uid}/enable`，走 `withAuth`（与 `/v1/*` 同一鉴权）。命中返回 200 与该账号的脱敏 `Status`；uid 不存在返回 404，错误体沿用 `writeOpenAIError` 的形态（code=`account_not_found`）。
- **R-E6**：文档同步。README 调整三处：Known limitations 里「无法恢复」的表述改为描述真实的两种恢复路径（凭证变更自动恢复 / `enable` 接口）；`/status` 区块附近补 `enable` 的 `curl` 示例；若涉及鉴权说明则与 `LB2A_API_KEY` 保持一致。
- **R-E7**：测试（stdlib + httptest，与本仓库既有测试风格一致）：pool 层覆盖「令牌变更恢复 / 令牌未变不恢复 / 占位不算变更 / Enable 命中与未命中的返回值与状态」；server 层覆盖「enable 成功 200 + 状态体 / 未知 uid 404 / 设置 APIKey 后无凭证 401 与正确凭证 200」。`dynamicModelsCache` 等包级状态需在用例中重置。
- **R-E8**：不改动禁用判定逻辑本身（三处 `Disable` 触发条件不变），不引入第三方依赖，新增导出符号带中文 doc comment、标识符/日志英文，符合 `.trellis/spec/backend/` 规范。

## Acceptance Criteria

- [x] A-E1：`TestSyncToDirReenablesOnTokenChange` 断言 `Disabled==false`、`Cooling==false` 且 `Pick()` 返回该账号。
- [x] A-E2：`TestAddPlaceholderDoesNotReenable` 断言由 `state.json` 载入的 `disabled=true` 在 `Add` 真实凭证后仍为禁用（`until`/`reason`/`credits` 均保留）。
- [x] A-E3：`TestSyncToDirKeepsDisabledWhenTokenUnchanged` 断言令牌未变时仍禁用且 `reason` 不变、不可被 `Pick()`；另有 `TestSyncToDirRefreshKeepsDisabled` 覆盖「进程内 refresh 落盘后重扫」这一真实路径不会误解锁。
- [x] A-E4：`TestEnableClearsStateAndReportsHit` 断言命中返回 true、`Disabled/Cooling/ErrCount` 清零、`credits` 保持 1500、落盘生效；未命中返回 false 且先删除的 `state.json` 不会被重建（证明未写盘）。
- [x] A-E5：`TestEnableAccountEndpoint`（200 + `disabled:false`）、`TestEnableAccountNotFound`（404 + 完整 `writeOpenAIError` 形状）、`TestEnableAccountRequiresAPIKey`（无/错凭证 401、正确 200）、`TestEnableAccountRouteMatrix`（7 条路由/方法不被遮蔽，错误方法 405，未注册的 `/disable` 404）。
- [x] A-E6：README 已无「重登也无法恢复」表述；限制条目写明两条真实恢复路径；`enable` 的 `curl` 示例已加在 `/status` 示例旁。
- [x] A-E7：`go build ./... && go vet ./... && go test ./...` 全绿；`go test ./internal/pool/ ./internal/server/ -count=2 -shuffle=on -count=3` 均通过（无顺序/全局态泄漏）。

## 检查阶段发现（已在本次修复）

- **[高] 自动恢复未落盘**：`rotateCredentials` 起初只在内存清 `disabled`，未调用 `saveLocked()` → 重启后 `state.json` 把 `disabled:true` 还原，而占位保护又会挡住凭证变更路径，账号永久死亡（正是本任务要消除的故障，只是被推到了重启之后）。已修复：两个调用点在 rotate 命中时 `saveLocked()`，并新增回归测试 `TestSyncToDirReenablePersistsAcrossRestart`；`design.md` 契约 1 已补注该为何必需。
- **[低] 文档与注释失真**：`Disable` / `Add` / `SyncToDir` 的 doc comment 与 README 的限制条目已按新行为修正，README 未再夸大测试覆盖（明确 `auth` / `scheduler` / `cmd/*` 仍无测试）。
- **[信息] 已知良性竞态**：重扫若读到 refresh 落盘前的旧令牌，会在健康账号上走一次 rotate 分支，仅重置未达阈值的 `errCount`，下一次重扫即收敛；未做改动，记录于此备查。

## Out of Scope

- 自动探测失效账号并尝试恢复（没有新凭证就无法唤醒 session，属上游约束）。
- 给 `/status` 加鉴权、新增账号删除/禁用接口、WebUI。
- 修改禁用触发条件或冷却时长语义。
