# design — 子任务：核心包补充单元测试

父任务：`09-19-leftover-completion`。PRD：[`prd.md`](./prd.md)。

## 目标形态

四个包各一个 `<file>_test.go`，同包（`package auth` / `package pool` / `package server` / `package scheduler`），只用 `testing` + `net/http/httptest`，零第三方依赖、零外部网络、零 `cmd/*` 覆盖。不引入 `internal/testutil` 之类的共享测试包——`quality-guidelines.md` 明确禁止 `utils/` 式抽象，且四个包的夹具差异大，重复二十行夹具比抽象一层更符合本仓库风格。

## 各包的夹具策略

| 包 | 被测对象 | 夹具 | 关键约束 |
|---|---|---|---|
| `auth` | `Parse` / `NeedsRefresh` / `KeyfromBody` / `SaveAtomic` / `LoadDir` | `t.TempDir()` 写 auth 文件；解析用内联 JSON 字符串 | 文件模式断言 `0600`（`os.Stat().Mode().Perm()`）；断言无 `*.tmp` 残留 |
| `pool` | `Pick*` / 冷却与禁用状态机 / `Enable` / `NoteError` / `List` / `SyncToDir` / 持久化 | 纯内存 `pool.New("")`；需要落盘时 `pool.New(filepath.Join(t.TempDir(), "state.json"))` | 冷却到期不要靠 sleep：用负时长调用 `Cooldown(uid, kind, -time.Second, ...)` 得到「已过期」的 `until` |
| `server` | `NewHandler` 路由 / `withAuth` / 聊天轮换与错误分类 / 模型回退 | `httptest.NewServer` 当上游；`upstream.SetServerBase(url)` + `t.Cleanup` 还原；账号 auth 文件的 `FilePath` 指向 `t.TempDir()`（refresh 路径会 `SaveAtomic`） | 每例开始重置 `upstream` 基址与包级 `dynamicModelsCache` |
| `scheduler` | `nextFire` / `RunCheckinNow` / `RunKeepaliveNow` | 同上（假上游）；账号必须有非空 `RefreshToken`，否则两个 Run* 会跳过它 | 签到路径需要 `t.Setenv("LB2A_UPDATE_API", 假版本接口)`，并重置 `versionCache` |

## 需要显式处理的全局状态（否则用例互相污染）

- `upstream.serverBaseOverride`：`SetServerBase` 的旧值必须在 `t.Cleanup` 恢复（`checkin_test.go:56-65` 是既有范式）。
- `upstream.versionCache`（签到版本缓存，1h TTL）：`checkin_test.go:57-60` 展示了重置写法；`scheduler` 的签到用例需要同样处理。
- `server.dynamicModelsCache`（`handler.go:117-123`）：包级 `RWMutex` + 1h 缓存。模型用例必须把它清空，否则前一个用例的缓存会让「失败回退静态表」用例假通过。
- 环境变量一律 `t.Setenv`（自动还原），不要用 `os.Setenv`。

## 确定性规则

- 不使用真实域名/网络：所有上游调用都打到 `httptest.Server`。
- 不用 `time.Sleep` 等待状态过期；需要「已冷却」时用负时长构造，需要「冷却中」时用正的大时长（如 `time.Hour`）。
- 不断言日志文本、不断言私有函数：断言走导出 API 与 HTTP 响应体。
- 协议敏感行为用字节级比较：流式透传断言 `bytes.Equal(源流, 响应体)`（沿用 `client_test.go` 的 `TestChatStreamPassthroughByteExact` 标准），非流式断言 JSON 结构与关键字段。

## server 包的行为断言要点（避免写错夹具）

- `ChatStream` 会强制 `stream=true`（`client.go:193-218`），所以假上游无论客户端传什么都要返回 SSE 文本；非流式与流式的区别只在客户端请求体的 `stream` 字段与 handler 后续分支（`handler.go:249-259`）。
- 流首窥探窗口 4KB：正常响应小于 4KB 时 `Peek` 返回 `io.EOF` + 部分数据，代码已容忍（`client.go:270-275`），夹具无需凑满 4KB。
- 40201 轮换用例：给两个账号设置不同 `credits`（`SetCredits`）以固定 `Pick` 顺序，让假上游只对第一个账号回错误帧，然后断言响应成功 + 第一个账号进入 `cooling`。
- `no_healthy_account` 用例：把唯一账号 `Disable` 后请求，断言 503 与错误体 `code`。
- 模型回退用例：假上游对 `/api/models/available` 返回 500，断言 `/v1/models` 返回 19 条静态模型（`handler.go:94-114`）。

## 不做的事

- 不给 `cmd/*` 写测试；不改生产代码（发现缺陷只记录）。
- 不追求覆盖率数字；不引入 `-race` 之外的构建标签或 CI 变更。
