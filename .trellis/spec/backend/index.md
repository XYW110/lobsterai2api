# Backend Development Guidelines

> Coding conventions for lobsterai2api, derived from the code as it exists today. Read the file that matches your change before writing code.

---

## Project at a glance

- Go 1.22, **stdlib only** — `go.mod` has no `require` block and no file imports a third-party module.
- Three binaries under `cmd/`: `server` (the long-running HTTP bridge), `login` (OAuth helper), `credit` (credit report).
- One deployable service: an OpenAI-compatible bridge in front of the LobsterAI upstream, with a multi-account pool, SSE streaming, cooldown/rotation, and a daily scheduler.
- Developed on Windows, deployed on Linux (Docker, multi-arch).

---

## Guidelines index

| Guide | Description |
|-------|-------------|
| [Directory Structure](./directory-structure.md) | Package layout, file placement, dependency direction, naming |
| [Upstream Integration](./upstream-integration.md) | Base URL, request headers, response envelope, endpoint map, SSE handling, check-in flow |
| [Error Handling](./error-handling.md) | `ErrKind` taxonomy, classification, the ChatStream three-state contract, client-facing errors |
| [Persistence](./persistence-guidelines.md) | JSON-on-disk storage (auth files, `state.json`), atomic writes, format compatibility |
| [Logging](./logging-guidelines.md) | stdlib `log` only, message shape, what never to log, CLI stdout/stderr split |
| [Quality](./quality-guidelines.md) | Zero-dependency rule, required patterns, testing requirements, commit style |

---

## Runtime wiring (`cmd/server`)

```text
main.go
  ├─ Load(config.json + LB2A_* env)     config.go
  ├─ auth.LoadDir(cfg.AuthDir)          internal/auth
  ├─ pool.New(stateFile) + Add()        internal/pool
  ├─ upstream.New()                     internal/upstream
  ├─ scheduler.Run(ctx)                 internal/scheduler   ← 9/21 check-in, 22 keepalive
  └─ server.NewHandler(...)             internal/server      ← HTTP entry
```

Dependency direction is strictly `cmd → internal`, and inside `internal`: `server, scheduler → pool, upstream → auth`.

---

## Cross-cutting rules (apply to every change)

- **Never add a third-party dependency.** Re-implement the minimal subset with stdlib (precedent: UUIDv4 via `crypto/rand`).
- **Never hardcode the upstream domain** — it comes from config/env via `upstream.ServerBase()`.
- **Branch on `rc == nil`, not `status >= 400`** when consuming `ChatStream` results — HTTP 200 can carry a business error frame.
- Comments stay Chinese; identifiers, log messages and error strings stay English.
- On-disk formats (`auths/lobsterai-<uid>.json`, `data/state.json`) are interfaces: extend them compatibly, never rename existing keys.

---

## Verification

```bash
go build ./... && go vet ./... && go test ./...
```
