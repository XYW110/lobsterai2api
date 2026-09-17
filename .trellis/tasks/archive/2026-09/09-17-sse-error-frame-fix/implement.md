# implement — 子任务A：SSE 200 流内错误帧识别修复

## 背景
透明化移植上游已验证的 PR#3（`xinxinshuhao-create/lobsterai2api`，作者 `xhzqw`），修复 HTTP 200 SSE 流内错误帧穿透。本仓库是其 fork，当前落后于该 PR。

## 实施清单（顺序）
1. `internal/upstream/client.go`
   - 导入 `bufio`。
   - `ChatStream`：200 分支改为窥探窗口 `peekBufSize = 4<<10` 用 `bufio.NewReader.Peek`；命中错误帧即关 body、填 `c.LastBody`、返回 `rc=nil`；否则返回 `&peekCloser{r: br, c: resp.Body}`（**同一个 Reader** 递给下游）。
   - 新增 `peekCloser` 类型与 `isSSEErrorFrame(head)` 判定。
   - 更新函数注释说明 200 流内错误路径。
2. `internal/server/handler.go`
   - 错误判定 `if status >= 400` → `if rc == nil`（覆盖非 2xx 与 200 带错误帧）。
   - 把 `terr` 分支的 `upstream.Error` 判断合并进来（供 handler 直接返回 503/401 等），并修正做冷却处理。
3. `internal/upstream/classify.go`
   - `hardMarkers` 追加：`额度已用完`, `免费额度`, `升级套餐`, `"code":40201`。
4. `internal/upstream/client_test.go`（新增，仅 stdlib）
   - `TestChatStreamPassthroughByteExact`：50KB 多帧流字节级等于源。
   - `TestChatStreamErrorFrameIn200`：`event:error` + `code:40201` → `rc==nil status==200` + `Classify→hard_credit`。

## 验证命令
```bash
go build ./...
go test ./internal/upstream/ ./internal/server/
```

## 风险文件 / 回滚
- 风险文件：`internal/upstream/client.go`，`sse.go`、`internal/server/handler.go`。
- 回滚点：`5b24d6e`（当前 HEAD）。若异常可直接回退本任务改动。

## 里程碑 / 提交流
- 实现完成 → 本地 build+test 全绿。
- 提交信息建议：`fix(upstream): detect business error frames hidden in HTTP 200 SSE streams (lossless peek)`
- 本子任务合并后，再进行子任务B（每日签到）独立规划与实现，两者互不阻塞。