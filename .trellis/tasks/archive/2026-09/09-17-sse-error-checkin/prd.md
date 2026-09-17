# PRD — 移植上游 SSE 错误帧识别 + 实现每日签到

## Goal

让本项目正确识别并处理两种上游行为，提升多账号池的健壮性与积分运营自动化：

1. **HTTP 200 流内业务错误帧**：LobsterAI 会把部分业务错误（典型：额度不足 `code=40201`「免费额度已用完，请升级套餐」）藏在 HTTP 200 SSE 流首（`event:error` 帧）。当前实现只看状态码，导致错误被当作成功处理。
2. **每日签到**：实现 `DailyCheckin`（当前为 no-op），按 `temp.txt` 已验证的签到流程，为每个账号每日领取 +100 积分。

## Confirmed Facts（仓库/证据已确认）

- **端口错误**（`internal/upstream/client.go:233`）：ChatStream 对 200 直接 `return resp.Body`，无任意探测；`internal/server/handler.go:218` 错误分支条件为 `status >= 400`，200 携带错误帧落入正常分支。
- **分类缺词**（`internal/upstream/classify.go:55-60`）：`hardMarkers` 缺失 `额度已用完` / `免费额度` / `升级套餐` / `"code":40201`。
- **后果**（对照 `internal/upstream/sse.go:17-149` `Aggregate`）：收到 `data:{"type":"error","error":{...}}` 时无 `choices` → `content=""` → 返回 200 假成功。不触发冷却换号。
- **修复已有效**（上游 PR #3，原始提交 `xinxinshuhao-create/lobsterai2api`，作者 `xhzqw`，2026-09-15）：无损探测 + `peekCloser` + 分类词，含 2 个回归测试。上游 PR #2 实现会吞掉流首 4KB，已替换。当前上游 HEAD 未合并任何 PR。
- **签到流程已验证**（`temp.txt`，2026-09-17）：`clientVersion` 从 `UPDATE_API`（地址由 `LB2A_UPDATE_API` 环境变量提供，仓库内不内置厂商域名）获取。签到头带 `placement=desktop_sidebar&clientVersion={V}`。要走 GET slot / GET context / POST check_in + `configRevision` + `idempotencyKey`。
- **go.mod 为纯 stdlib**：生产实现不得引入第三方依赖。

## Requirements

### R1：透明地识别 200 流内错误帧
- InputStream 分支 `status >= 400` 改为 `rc == nil`（统一覆盖非 2xx 与 200 带错误帧）。
- Behavior：错误帧返回 `rc = nil`，并把首块放入 `LastBody`，按非 2xx 相同路径分类处置。
- 无损：探测必须用同一个 `bufio.NewReader` 交给下游读，流首不丢。

### R2：分类词扩展
- 向 `hardMarkers` 追加 `额度已用完` / `免费额度` / `升级套餐` / `"code":4022`。

### R3：DailyCheckin 接入
- 暴露 `DailyCheckin`（当前 `internal/upstream/client.go:329` 为 no-op）。
- 注册调度（`internal/scheduler` 用每天 9/21 点已调用 `RunCheckinNow()` → `DailyCheckin`，无需改调度器）。
- 任务用 `temp.txt` 验证的流程：自动解析 `clientVersion`（失败则本次跳过），对每个账号执行 GET slot → GET context → POST check_in。
- 幂等防护：claimToday 判定与本地上一次结果一致。
- 成功/失败均记录输出（stdout + 状态统计）。

### R4：测试
- `internal/upstream/client_test.go`：新增回归测试（透明吐 50KB 流全量等于原请求流头、以及对 `code:4022` 错误帧的拦截、`Classify` 归类 `hard_credit`）。
- 签到逻辑函数可见性可测（output/返回值）。

## Acceptance Criteria
- [ ] A1：正常 200 流首次裸读取后仍能从流首读到完整 `data:` 帧（`TestChatStreamPassthruByteExact` 得出）。
- [ ] A2：200 内错误帧（`event:error` / `"error":{`）被拦截，`rc==nil`，且 `Classify(last)` 得 `ErrHardCredit`。
- [ ] A3：handler 错误分支用 `rc == nil`，不再依赖 `status >= 400`。
- [ ] A4：`hardMarkers` 含 4 个新词。
- [ ] A5 `DailyCheckin` 不再 no-op：能解析 targetTime、对每账号执行签到并把结果写入日志；Go 构建通过、`go test ./...`（新增测试）通过。

## Out of Scope
- 上游 PR 追赶（`ne\'e`）不做 —— 只做 `映射`/`签到`两件事。
- 不改为前端管理台。
- 不做签到历史入库或 WebUI。

## Open Questions（待用户决策）
（当前无阻塞问题；申请键/解析键在临时实现中已经存在）