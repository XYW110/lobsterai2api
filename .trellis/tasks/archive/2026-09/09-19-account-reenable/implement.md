# implement — 子任务：失效账号恢复

父任务：`09-19-leftover-completion`。PRD：[`prd.md`](./prd.md)。设计：[`design.md`](./design.md)。

## 实施清单（顺序）

1. **`internal/pool/pool.go`**
   - 新增 `entry.rotateCredentials(next *auth.Auth) bool`（见 design 契约 1），含中文 doc comment 说明占位保护的原因。
   - `Add` 已存在分支：`e.a = a` → `if e.rotateCredentials(a) { log.Printf(...) }`。
   - `SyncToDir` 已存在分支：同样替换。
   - 新增导出 `Enable(uid string) bool`（契约 2），放在 `Disable` 之后、`ReenableIfCredits` 之前，保持文件里「Disable → Enable → Reenable」的阅读顺序。
   - import 增加 `log`（stdlib，符合 `logging-guidelines.md`）。
2. **`internal/server/handler.go`**
   - `NewHandler` 注册 `POST /admin/accounts/{uid}/enable`，handler 名 `enableAccount`，位置放在 `status` / `healthz` 之后。
   - `enableAccount`：`PathValue` → `Pool.Enable` → 命中则在 `Pool.List()` 中取回 `Status` 并 `writeJSON(200, st)`；未命中 `writeOpenAIError(404, "account_not_found", ...)`。
3. **`internal/pool/pool_test.go`（新建）**
   - `TestSyncToDirReenablesOnTokenChange`：禁用账号 → `SyncToDir` 带新令牌 → `Disabled==false`、可被 `Pick()` 选中。
   - `TestSyncToDirKeepsDisabledWhenTokenUnchanged`：令牌相同 → 仍禁用。
   - `TestAddPlaceholderDoesNotReenable`：`New(stateFile)` 载入 `disabled=true` → `Add` 真实凭证（旧值为空占位）→ 仍禁用。
   - `TestEnableClearsStateAndReportsHit`：`Enable` 命中返回 true 且 `Disabled/Cooling/ErrCount` 清零、credits 不变；未知 uid 返回 false 且 state 文件未被改写（用 `t.TempDir()` 下 `state.json` 的 mtime/内容断言）。
4. **`internal/server/handler_test.go`（新建）**
   - 测试夹具：`httptest.NewServer` 作为上游基址（`upstream.SetServerBase` + `t.Cleanup` 恢复），`pool.New(t.TempDir()+"/state.json")` 装入 1–2 个账号。
   - `TestEnableAccountEndpoint`：禁用账号 → POST enable → 200 且响应体 `disabled==false`、`GET /status` 同步反映。
   - `TestEnableAccountNotFound`：未知 uid → 404 且错误体 `code == "account_not_found"`。
   - `TestEnableAccountRequiresAPIKey`：`APIKey:"k"` 时无 Bearer 401、错误 Bearer 401、正确 Bearer 200。
5. **`README.md`**
   - Known limitations：把「Session-dead accounts stay disabled until re-login or file replacement (no re-enable endpoint)」改写为真实机制（凭证变更自动恢复 / `POST /admin/accounts/{uid}/enable`）。
   - `## Test` 段或 `/status` 附近补 `curl` 示例：
     `curl -s -X POST http://127.0.0.1:8367/admin/accounts/<uid>/enable -H "Authorization: Bearer ***"`
   - 若 README 有接口清单，把新路由按同样风格登记。

## 验证命令

```bash
gofmt -l internal/pool internal/server
go build ./... && go vet ./... && go test ./...
```

手工冒烟（可选，需要真实凭证，默认跳过）：

```bash
# 1. 起服务 → GET /status 找一个 disabled 账号
# 2. printf 'new-token' 写入该账号 auth 文件后等待 30s 重扫，或直接调 enable
# 3. GET /status 确认 disabled=false
```

## 风险文件 / 回滚

- 风险文件：`internal/pool/pool.go`（状态机核心）、`internal/server/handler.go`（新增路由，注意不要影响既有路由的匹配）。
- 主要风险：误判凭证变更 → 把 session 已死账号放回池（由「旧值非空」+「值必须不同」两条约束兜住）；`Enable` 未写盘 → 重启后禁用状态回滚（契约要求命中即 `saveLocked`）。
- 回滚点：本子任务提交前的 master HEAD；改动集中在两个生产文件 + 两个测试文件，可直接 revert。

## 里程碑 / 提交流

- 里程碑 1：pool 行为 + 单测通过。
- 里程碑 2：endpoint + 单测通过，README 同步。
- 建议提交信息（拆分两条，按逻辑变更单元）：
  - `fix(pool): re-enable accounts when their access token changes`
  - `feat(server): add POST /admin/accounts/{uid}/enable`
  （README 属于第二条的一部分；测试文件随各自变更同行提交。）
- 归档后再进入 `09-19-core-package-tests`（其测试文件将扩展本子任务新建的两个 `_test.go`）。
