# PRD — 子任务B：实现每日签到 DailyCheckin

父任务：`09-17-sse-error-checkin`（本子任务拥有父 PRD 的 R3 + A5）。

## Goal

把 temp.txt 已验证的签到流程移植进 `upstream.Client.DailyCheckin`（当前 no-op），让调度器 9/21 点的签到任务真正为每个账号领取 +100 积分。

## Confirmed Facts（temp.txt 2026-09-17 实测验证）

- `clientVersion` 从有道公开更新接口解析：GET `UPDATE_API` → `data.value.version`（格式形如 `x.y.z`）。
- 签到流程（Bearer accessToken，UA `LobsterAI/{clientVersion}`）：
  1. GET `/api/client-activities/slot?placement=desktop_sidebar&clientVersion={V}&containerApiVersion=2&platform=win32`
  2. `slotState != "available"` 或无 `activity` → 本次无活动（非错误）。
  3. 取 `activity.activityCode` + `activity.configRevision` → GET `/api/client-activities/{code}/context?configRevision={rev}`
  4. `state.claimedToday == true` 或 `actions` 不含 `check_in` → 今日已签到（非错误）。
  5. POST `/api/client-activities/{code}/actions/check_in`，body `{"configRevision":rev,"idempotencyKey":<uuid4>,"payload":{}}` → `result` 里取 `creditsGranted`/`rewardCredits`/`credits` 之一为获得积分。
- 响应信封 `{code, message|msg, data}`：`code != 0` 或 `data` 为空 = 失败。
- 调度器 `RunCheckinNow`（`internal/scheduler/scheduler.go:87`）已逐账号调用 `DailyCheckin(a)`（跳过 disabled），随后查余额解冻 —— **无需改调度器**。
- 签到端点在 `ServerBase()` 同一基址上；更新接口是另一个公开域名。
- go.mod 纯 stdlib（uuid 用 crypto/rand 拼 UUIDv4）。

## Requirements

- **R-B1**：新增 `internal/upstream/checkin.go`：`DailyCheckin(a *auth.Auth) error` 实现完整流程；「无可用活动」「今日已签到」记日志后视为成功返回 nil；其余失败返回 error。
- **R-B2**：`clientVersion` 动态解析（带 TTL 缓存，避免每账号重复请求）；解析失败返回 error（本次跳过，不影响其他账号）；更新接口域名支持 `LB2A_UPDATE_API` 环境变量覆盖（默认值为公开的厂商更新接口 —— 非用户私有部署地址，与 fe299f1 移除的 upstream base 性质不同）。
- **R-B3**：`idempotencyKey` 每次用 crypto/rand 生成 UUIDv4；签到请求 UA 用 `LobsterAI/{解析到的版本}`。
- **R-B4**：`apiEnvelope` 兼容 `message` 字段（现有只解 `msg`），doJSON 错误信息取两者之一。
- **R-B5**：单元测试（仅 stdlib + httptest）：签到成功（断言 slot 查询参数与 check_in 请求体含 configRevision/idempotencyKey）、已签到不发起 check_in、无可用活动、版本解析失败直接报错不打签到端点。`go build ./...` 与 `go test ./...` 通过。

## Acceptance Criteria

- [ ] A-B1：`DailyCheckin` 不再 no-op，流程与 temp.txt 一致。
- [ ] A-B2：`claimedToday` → 返回 nil 且不发 check_in POST。
- [ ] A-B3：版本解析失败 → 返回 error，不请求签到端点。
- [ ] A-B4：构建与全量测试通过。

## Out of Scope

- 签到历史入库 / WebUI；调度器改造；为 disabled 账号签到。
