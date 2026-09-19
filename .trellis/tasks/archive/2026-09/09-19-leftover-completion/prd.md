# PRD — 遗留项收口（父任务）

## Goal

把 2026-09-19 全仓盘点出的三项遗留按 Trellis 流程收口到「已完成」，让 README 的 Known limitations 与实际行为一致：

1. **部署配置遗漏**：Docker 部署下每日签到永远不会执行（`LB2A_UPDATE_API` 未透传进容器）。
2. **失效账号无法恢复**：账号 session 失效被禁用后，重新登录/替换 auth 文件也不会恢复，必须重启进程。
3. **核心包无单测**：`auth` / `pool` / `server` / `scheduler` 四个包没有任何测试。

本父任务只做需求收口、子任务映射与最终集成复查，不直接改代码。

## Confirmed Facts（2026-09-19 盘点证据）

- `docker-compose.yml:23-32` 的 `lobsterai2api` 服务环境块列出了 `LB2A_UPSTREAM_BASE` 等变量，唯独缺 `LB2A_UPDATE_API`；而 `.env.example:18`、`README.md:183`、`.trellis/spec/backend/upstream-integration.md:11` 都把该变量描述为启用签到的唯一开关，`internal/upstream/checkin.go:27` 只从环境变量读取，`config.json` 无对应字段（`cmd/server/config.go:32-35`）。
- `internal/pool/pool.go:95-123`（`Add` / `SyncToDir`）在账号已存在时只更新 `e.a`，`disabled` 状态原样保留 → 重登写回新凭证后 rescan 依旧禁用。README `:210` 声称「until re-login or file replacement」与实际行为不符。
- `go test ./...` 输出显示 `internal/auth`、`internal/pool`、`internal/server`、`internal/scheduler`、`cmd/*` 均为 `[no test files]`；README `:211` 已把该缺口列为已知限制。
- 本机 `core.autocrlf=true`，工作区 `.go` 为 CRLF，仓库 blob 为 LF（`git show HEAD:internal/upstream/client.go` 验证）→ `gofmt -l` 列出 11 个文件，属本地检出噪音；`.gitattributes` 目前只锁定了 `*.sh` / `Dockerfile`。

## Requirements（来源需求集）

- **R1 部署配置遗漏**：容器化部署（compose）必须能把 `LB2A_UPDATE_API` 透传给 server 进程，签到在 Docker 下可正常工作；仓库内仍不得内置任何厂商域名。
- **R2 失效账号恢复**：账号因 session 失效被禁用后，重新登录（凭证变更）必须能在进程不重启的情况下自动恢复可用；同时提供受鉴权的手动启用能力。
- **R3 测试覆盖**：`auth` / `pool` / `server` / `scheduler` 四个包具备 stdlib 单测，覆盖各自的状态机、持久化、路由与调度行为；不引入第三方依赖。
- **R4 文档一致性**：README（Known limitations / 部署说明 / API 示例）在 R1–R3 完成后不再陈述与实际不符的内容。
- **R5 换行规格**：仓库层面锁定 Go 源码为 LF，消除 Windows 检出时的 `gofmt -l` 全仓噪音。

## 子任务映射

| 子任务 | 覆盖需求 | 交付物（可独立验收） |
|---|---|---|
| `09-19-deploy-config-fix` | R1、R5、R4（部署段） | compose 透传 + `.gitattributes` 规格 + README 部署文档补充 |
| `09-19-account-reenable` | R2、R4（限制段） | `pool` 凭证变更自动启用 + `POST /admin/accounts/{uid}/enable` + 相应测试与文档 |
| `09-19-core-package-tests` | R3、R4（限制段） | 四个包的 `*_test.go` + README 限制条目移除 |

## 跨子任务验收标准

- [x] A1：`docker compose config` 渲染出 `LB2A_UPDATE_API`（未设 = 空串，容器内等价于未设置 = 跳过签到）；`LB2A_UPDATE_API=https://example.invalid/update docker compose config` 证明值来自 `.env` 透传；`login` 服务不含该变量。
- [x] A2：`TestSyncToDirReenablesOnTokenChange` / `TestAddReenablesOnTokenChange`（新令牌 → `Disabled==false`、可被 `Pick()` 选中、且跨重启持久化）与 `TestSyncToDirKeepsDisabledWhenTokenUnchanged` / `TestSyncToDirRefreshKeepsDisabled`（令牌未变 → 保持禁用，进程内 refresh 不会误解锁）全部通过。
- [x] A3：`TestEnableAccountRequiresAPIKey`（无/错 Bearer 401、正确 200）、`TestEnableAccountEndpoint`（200 + `disabled:false`）、`TestEnableAccountNotFound`（404 + `account_not_found`）、`TestEnableAccountRouteMatrix`（7 条路由/方法不被遮蔽）全部通过。
- [x] A4：提交后全新 clone 中 `go build ./... && go vet ./... && go test ./...` 全绿；`internal/auth|pool|server|scheduler` 均有测试文件（覆盖率 88.2% / 94.2% / 76.6% / 73.2%）。
- [x] A5：全新 clone（`core.autocrlf=true` 下）`git ls-files --eol -- '*.go'` 显示 `crlf_count=0`，`gofmt -l .` 无输出。
- [x] A6：README 的 Known limitations 现为两条如实描述——会话失效账号的两条恢复路径（凭证变更自动恢复 / `POST /admin/accounts/{uid}/enable`）与 `/status` 无鉴权的披露；「无单测」条目改为只披露 `cmd/*`。
- [x] A7：子任务 1/2/3 依次归档并各自提交（`34d31e6`…`9597937`，共 11 个提交），工作区仅剩父任务目录未跟踪；`git log f780899..HEAD` 的主题逐条对应 R1/R2/R3/R5 与各自的 spec 更新。

## 集成复查记录（父任务自身工作，2026-09-19）

- **全新 clone 端到端**：`git clone` 到临时目录后 `crlf_count=0`、`gofmt -l .` 空、build/vet/test 全绿（临时目录已删除）。
- **compose 渲染**：`docker compose config` 中 `lobsterai2api` 服务含 `LB2A_UPDATE_API`（未设时为空串，容器内等价于未设置 = 跳过签到）；`login` 服务不含该变量。
- **三处行为修复的落地验证**：`SyncToDir` 令牌变更自动恢复（含重启持久化）、`Pool.Enable` + 鉴权路由、`SyncToDir` 增删落盘，均由单测覆盖并通过变异抽查（7 项反向变异全部被捕获）。
- **提交序列**（`f780899` → `9597937`）：每个子任务都是「工作提交 → spec 更新 → archive 记账提交」，无交叉混装。

## 子任务顺序（写入子任务产物，而非靠树结构推断）

1. `09-19-deploy-config-fix`（独立，无前置）
2. `09-19-account-reenable`（独立，改动 `internal/pool`、`internal/server`）
3. `09-19-core-package-tests`（**必须在 2 之后**：其测试文件与 2 改动的 `pool`/`server` 行为重叠，先做 2 可避免同文件冲突与返工）

## Out of Scope

- 上游原仓库 PR 的跟进与合并。
- 为 `cmd/*` 三个 binary 补测试（属 R3 之外的后续工作）。
- 把 `/status` 改为鉴权接口、引入 WebUI 或账号管理台。
- CI 中新增测试/覆盖率流水线（现有 workflow 只负责镜像发布）。
