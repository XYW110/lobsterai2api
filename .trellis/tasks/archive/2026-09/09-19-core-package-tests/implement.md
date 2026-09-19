# implement — 子任务：核心包补充单元测试

父任务：`09-19-leftover-completion`。PRD：[`prd.md`](./prd.md)。设计：[`design.md`](./design.md)。

**前置**：`09-19-account-reenable` 必须已归档（它会新建 `internal/pool/pool_test.go` 与 `internal/server/handler_test.go`，本子任务在其基础上扩展，而不是另起文件名）。

## 实施清单（顺序）

1. **`internal/auth/auth_test.go`（新建）**
   - `TestParseNestedForm` / `TestParseFlatForm`：字段逐一比对（含 `UID` / `UserId` / `Nickname`）。
   - `TestParseRejects`：空字节、非法 JSON、缺 `accessToken`（含纯空白）→ 断言错误消息前缀（`storage_parse_error` / `parse_error`）。
   - `TestNeedsRefresh`：`ExpiresAt == 0`、窗口内、窗口外三种。
   - `TestKeyfromBodyOptionalFields`：`Uuid`/`UserId` 为空时不出现在 map 里。
   - `TestSaveAtomicRoundTrip`：`t.TempDir()` 下写入 → 重新 `Parse` 相等；`os.Stat` 权限 `0600`；目录里无 `*.tmp` 残留。
   - `TestLoadDirSkipsBrokenFiles`：两个合法文件 + 一个坏 JSON + 一个不匹配 glob 的文件 → 返回 2 个且 `FilePath` 正确。
2. **`internal/pool/pool_test.go`（扩展子任务 2 新建的文件）**
   - `Pick` 相关：`TestPickHighestCredits`、`TestPickSkipsCoolingAndDisabled`、`TestPickExcludingSkipsTried`、`TestPickReturnsNilWhenEmpty`。
   - 状态机：`TestCooldownExpiryRestoresHealth`（负时长）、`TestNoteErrorThresholdCoolDown`、`TestNoteSuccessResetsErrCount`、`TestReenableIfCreditsSkipsDisabled`、`TestDisableSticky`。
   - 观测与持久化：`TestListSortedByUID`、`TestStateRoundTrip`（credits/disabled/until 保留、占位被 `Add` 替换）、`TestSyncToDirAddsAndRemoves`；`SaveAtomic` 式原子写断言（有 `.tmp` 则失败）。
3. **`internal/server/handler_test.go`（扩展子任务 2 新建的文件）**
   - 夹具：`newTestHandler(t, opts)` 返回 handler + 假上游 env（`httptest`、基址覆盖、`dynamicModelsCache` 清空、`t.Cleanup` 还原）。
   - `TestHealthz`、`TestStatusShape`（账号列表来自 pool）。
   - `TestWithAuthRejectsMissingKey` / `TestWithAuthAcceptsKey`。
   - `TestChatCompletionsNonStreamingAggregates`：SSE 内容含 `choices[0].delta.content` 两片 → 聚合后 `content` 拼接、`finish_reason` 正确。
   - `TestChatCompletionsStreamByteExact`：`stream:true`，断言响应体与假上游输出字节级相等（含末尾 `[DONE]` 保证逻辑）。
   - `TestChatCompletionsRotatesOnErrorFrame`：第一个账号（credits 最高）收到 200 + `event:error`/40201 → 响应成功且第一账号 `cooling=true`、`reason` 非空。
   - `TestChatCompletionsNoHealthyAccount`：唯一账号 disabled → 503 + `code=no_healthy_account`。
   - `TestModelsFallsBackToStaticOnUpstreamFailure`：假上游 500 → 19 条静态模型。
4. **`internal/scheduler/scheduler_test.go`（新建）**
   - `TestNextFirePicksEarliestHour` / `TestNextFireRollsToNextDay`（固定 `time.Now()` 的日期构造输入，不依赖真实时钟跨天）。
   - `TestRunCheckinNowUnfreezesAfterCredits`：冷却账号 + 假签到与 profile-summary 返回正积分 → `ReenableIfCredits` 生效（`Cooling==false`、`credits>0`）；`t.Setenv("LB2A_UPDATE_API", ...)` + 重置 `versionCache`。
   - `TestRunCheckinNowSkipsDisabled`：禁用账号 → 假上游零请求（用计数器断言）。
   - `TestRunKeepaliveNowPersistsRefreshedToken`：刷新成功 → 磁盘 auth 文件里的 accessToken 更新（`SaveAtomic` 生效）。
   - `TestRunKeepaliveNowDisablesOnSessionDead`：假上游返回 session 失效（500 + 分类命中 `ErrSessionDead` 的 body）→ 账号 `disabled=true`。
   - 可选：`TestRunStopsOnContextCancel`（`ctx` 取消后 `Run` 返回，不阻塞）。
5. **`README.md`**
   - 从 Known limitations 删除「No unit tests yet for the `pool`, `auth`, `server` and `scheduler` packages」这一条；同段其它条目保持原样（不由本子任务处理）。
   - 若 `## Test` 段需要说明，补一句「`go test ./...` 覆盖 auth/pool/server/scheduler/upstream，均为 stdlib + httptest，无外部依赖」。
   - 删除后该段仍需如实披露 `cmd/*` 三个 binary 无测试（不得夸大覆盖范围）。
6. **`internal/pool/pool.go` — R-T9 缺陷修复（2026-09-19 修订新增）**
   - 缺陷：`SyncToDir` 的新增分支（`p.byUID[a.UID] = &entry{a: a}`）与移除分支（`delete(p.byUID, uid)`）都不调用 `saveLocked()`，而 `persistence-guidelines.md` 要求 state 的每次变更都落盘。
   - 修复：在这两个分支上置一个局部 `changed` 标记，循环结束后 `if changed { p.saveLocked() }`；**不要无条件落盘**（否则 30s 一次的重扫会持续写盘）。
   - 回归测试 `TestSyncToDirPersistsAddAndRemove`（写进 `internal/pool/pool_test.go`）：新增/移除后 `state.json` 与内存一致；`New(fp)` 重载后被移除账号不出现、新增账号存在；无变化的重扫不改变 `state.json`（可用删除文件后重扫不被重建来断言「未写盘」）。
   - 不得改动 `healthy()` / `Pick()` / 移除时的状态保留语义。

## 验证命令

```bash
gofmt -l internal/auth internal/pool internal/server internal/scheduler
go build ./... && go vet ./... && go test ./...
go test ./... -count=2        # 确认无顺序依赖 / 无缓存假通过
```

## 风险文件 / 回滚

- 本子任务只新增/扩展 `*_test.go` 与 README 一行，生产代码零改动。
- 主要风险：夹具泄漏包级状态（`serverBaseOverride`、`dynamicModelsCache`、`versionCache`）导致用例顺序敏感 → 由设计文档的「显式重置」清单兜住，`-count=2` 用于验证。
- 次要风险：用例断言写成依赖实现细节（私有函数、日志文本）→ 复核时改为只断言导出行为与 HTTP 响应。
- 回滚点：本子任务提交前 HEAD；删除新增测试文件即可完全回退。

## 里程碑 / 提交流

- 里程碑 1：`auth` + `pool` 测试绿。
- 里程碑 2：`server` + `scheduler` 测试绿，README 限制条目移除。
- 建议提交信息（按包拆两条，便于 review）：
  - `test(auth,pool): cover auth parsing/save and pool state machine`
  - `test(server,scheduler): cover routing, rotation fallback and scheduled jobs`
  - README 的限制条目删除并入第二条。
