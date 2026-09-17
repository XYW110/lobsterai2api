# PRD — 子任务A：SSE 200 流内错误帧识别修复

父任务：`09-17-sse-error-checkin`（该子任务拥有 R1/R2 + A1/A2/A3/A4）。

## Goal

修复 LobsterAI 把业务错误（典型 `code=40201`「免费额度已用完，请升级套餐」）隐藏在 HTTP 200 SSE 流首导致的三种穿透：非流式假成功、流式错误透传、不触发冷却换号。

## Confirmed Facts

- 上游 PR #3（作者 `xhzqw`）已提供通过验证的修复，本仓库是其 fork、当前 HEAD 落后于该 PR。
- `chatStream` 当前对 200 直接 `return resp.Body`（`upstream/client.go`）。
- handler 错误判定用 `rc == nil`（需要改 `status >= 400` → `rc == nil`）。
- 错误分类需要扩 `hardMarkers`（`classify.go`）。
- go 纯 stdlib，无外部依赖。

## Requirements

### R-A1：查档者（无损）流内错误检测
- 200 响应先窥探首块（`bufio.NewReader.Peek`，`4<<10`），命中 `event:error` / `"error":{` / `"code":403` 时按非 2xx 路径处置（`rc=nil` + `LastBody`）。
- 窥探不消费字节：**必须把同一个 Reader 交给下游**，流首不得截断。

### R-A2：handler 错误分支切换
- `status >= 400` → `rc == nil`（覆盖 非2xx 与 200带错误帧 两条路径）。

### R-A3：分类词扩展
- `hardMarkers` 追加 `额度已用完` / `免费额度` / `升级` / `"code":402`。

## Acceptance Criteria
- A-A1：正常流 50KB 经 `ChatStream` 后字节级等于原流（`TestChatStreamPassthroughByteExact`）。
- A-A2：`event:error` 帧被拦下且 `rc==nil status==200`，`Classify` 得 `hard_credit`。
- A-A3：handler 用 `rc==nil`，不再依据 `status>=400`。
- A-A4：`go build ./...` 与 `go test ./...` 通过（含新增 2 测试）。

## Out of Scope
- 每日签到（子任务B）。
- 前端 / 发布流程。