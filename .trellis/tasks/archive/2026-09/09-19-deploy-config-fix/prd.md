# PRD — 子任务：部署配置遗漏修复（compose 透传 LB2A_UPDATE_API + Go 换行规格）

父任务：`09-19-leftover-completion`（本子任务拥有父 PRD 的 R1、R5 与 R4 的部署段）。
类型：轻量任务（PRD-only）。

## Goal

让 Docker 部署下的每日签到真正可能生效，并让 Windows 检出不再污染 `gofmt -l`。

## Confirmed Facts

- `docker-compose.yml:23-32` 的 `lobsterai2api` 服务环境块缺 `LB2A_UPDATE_API`；同块已透传 `LB2A_UPSTREAM_BASE`，说明这是遗漏而非有意设计。
- 签到版本解析只认环境变量：`internal/upstream/checkin.go:27`（`updateAPIEnv = "LB2A_UPDATE_API"`）；`config.json` 无对应字段（`cmd/server/config.go:32-35` 只有 `timeout_seconds` / `base_url`）；`.env.example:18` 与 `README.md:183` 已声明「留空 = 跳过签到」。
- 仓库不得内置厂商域名（commit `fe299f1` 的安全约束，见 `.trellis/spec/backend/quality-guidelines.md`）：本子任务只做透传，不设默认值。
- 本机 `core.autocrlf=true`，`.go` 工作区文件为 CRLF 而仓库 blob 为 LF，`gofmt -l .` 列出 11 个文件；`.gitattributes` 目前只锁定 `*.sh` 与 `Dockerfile` 为 LF。
- README 的 Docker 段「Key `.env` knobs」（`:46-53`）未提及 `LB2A_UPDATE_API`，只在配置表（`:183`）出现。

## Requirements

- **R-D1**：`docker-compose.yml` 的 `lobsterai2api` 服务环境块增加 `LB2A_UPDATE_API: ${LB2A_UPDATE_API:-}`，并用中文注释说明「留空 = 跳过签到」（与同文件既有注释风格一致，不内置任何域名）。`login` 服务不需要该变量。
- **R-D2**：`.gitattributes` 增加 `*.go text eol=lf` 并附中文注释说明原因（Windows `core.autocrlf=true` 会让 `gofmt -l` 报全仓）。不新增会触发全仓 renormalize 的宽泛规则（`*` 级别）；仓库 blob 已是 LF，改动不应在 `git status` 中产生除 `.gitattributes` 外的文件变更。
- **R-D3**：README 的 Docker「Key `.env` knobs」列表补一条 `LB2A_UPDATE_API`（留空则跳过每日签到），与 `.env.example` 的措辞保持一致。
- **R-D4**：不改变任何 Go 源码的运行时行为；不改变签到在 `LB2A_UPDATE_API` 未设置时的既有行为（仍跳过并记录日志）。唯一允许的 Go 改动见 R-D5（纯格式对齐）。
- **R-D5**（2026-09-19 修订新增）：修复仓库里既有的 2 行 `gofmt` 对齐偏差，使 A-D3 可达——`cmd/credit/main.go` 的 `authFile.Auth.AccessToken` 标签列、`internal/auth/auth.go` 的 `Auth.ExpiresAt` 注释列各差一个空格（`gofmt -d` 对已提交的 LF blob 可复现）。只允许这两处对齐变化，不得顺带改动其它行或标识符。

## Acceptance Criteria

- [x] A-D1：`docker compose config` 输出里 `lobsterai2api` 服务包含 `LB2A_UPDATE_API`；`docker compose config`（未设）渲染为空串，`LB2A_UPDATE_API=https://example.invalid/update docker compose config` 渲染出该值；`docker compose --profile login config` 中仅出现 1 次（属 `lobsterai2api`，`login` 服务不含）。
- [x] A-D2：`git status --porcelain` 只显示 `.gitattributes`、`README.md`、`docker-compose.yml`、`cmd/credit/main.go`、`internal/auth/auth.go`（后两个是 R-D5 白名单）；`git diff --stat` 为 5 文件 / +10 / -2，无成批 `.go` 变更、无删除。
- [x] A-D3：临时 clone（`core.autocrlf=true`，复制 5 个工作区文件后在 clone 内提交）→ `gofmt -l .` 无输出，且 14 个 `.go` 全部 `w/lf`；正向对照（故意写坏的 `zz_control.go`）被列出，证明空结果有效。分离对照：只应用 `.gitattributes` 而不带 R-D5 修复时，`gofmt -l` 恰好列出那 2 个文件——证明 R-D5 既必要又充分。
- [x] A-D4：`go build ./... && go vet ./... && go test -count=1 ./...` 全绿；`git diff --ignore-all-space -- cmd/credit/main.go internal/auth/auth.go` 为空，证明 R-D5 是纯格式改动。
- [x] A-D5：`git diff -U0 -- README.md` 只有 1 个 hunk（Key `.env` knobs 列表），`## Known limitations / TODO` 段未被触碰。

## 变更记录

- **2026-09-19（Phase 2.2 检查后回退修订）**：初版 R-D4 声明「不改动任何 Go 源码」，与 A-D3「全新检出 `gofmt -l` 无输出」自相矛盾——仓库 blob 中存在 2 处既有的结构体对齐偏差（与换行无关，`gofmt -d` 对 LF blob 可复现）。现拆出 R-D5 作为白名单式的最小修订：只修这 2 行对齐，使 A-D3 真正可达，而不是把验收标准降级为「无换行导致的告警」。

## Out of Scope

- 为 `LB2A_UPDATE_API` 设置任何内置默认值或新增 `config.json` 字段（需产品决策，且触及安全约束）。
- 本机 `core.autocrlf` 的个人配置调整（属开发者机器设置，不写入仓库）。
- 其它 env 变量的巡检（本轮盘点已确认其余变量均已透传）。
