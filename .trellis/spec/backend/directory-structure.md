# Directory Structure

> Where code lives in lobsterai2api and where new code goes.

---

## Layout

```text
.
├── cmd/                          # three binaries, one directory each
│   ├── server/
│   │   ├── main.go               # wiring: config → auth → pool → upstream → scheduler → handler
│   │   └── config.go             # Config struct + Default() + applyEnv() + normalize()
│   ├── login/main.go             # OAuth helper (`login url` / `login poll`)
│   └── credit/main.go            # credit report (stdout JSON, `-pretty` for humans)
├── internal/
│   ├── auth/auth.go              # credential parse (nested/flat) + atomic save; no internal deps
│   ├── pool/pool.go              # account state machine (credits/cooling/disabled) + state.json
│   ├── upstream/                 # every LobsterAI HTTP call
│   │   ├── client.go             # Client, doJSON, ChatStream, RefreshToken, FetchModels, QuotaUsage
│   │   ├── sse.go                # Aggregate (non-stream clients) / Stream (passthrough)
│   │   ├── classify.go           # ErrKind, Classify, keyword tables
│   │   └── checkin.go            # daily check-in flow + clientVersion resolution
│   ├── scheduler/scheduler.go    # daily loop: check-in at 9/21, keepalive at 22
│   └── server/handler.go         # OpenAI-compatible HTTP layer (routes, auth, pick, rotate)
├── docker/                       # entrypoint.sh, compose login helper
├── Dockerfile, docker-compose.yml, .env.example, config.example.json
├── README.md
└── go.mod                        # module lobsterai2api, go 1.22, zero requires
```

---

## Where new code goes

| Change | Location |
|---|---|
| New upstream endpoint or call | `internal/upstream` — add a method to `Client` in `client.go`; a whole flow gets its own file (precedent: `checkin.go`) |
| New HTTP route | `internal/server/handler.go`, registered in `NewHandler` with Go 1.22 method patterns (`"POST /v1/chat/completions"`) |
| New pool behavior (cooldown/disable semantics) | `internal/pool/pool.go` |
| New scheduled job | `internal/scheduler/scheduler.go` — add hours to `Config`, branch in `Run`, expose a `RunXxxNow` method |
| New config knob | `cmd/server/config.go` (`Config` + `Default` + `applyEnv` + `normalize`), then sync README table + `config.example.json` + `.env.example` |
| New binary | `cmd/<name>/main.go` (`package main`); split a second file only for a real unit, precedent `cmd/server/config.go` |
| Container/deploy plumbing | `docker/`, root `Dockerfile` / `docker-compose.yml` |

---

## Dependency direction

```text
cmd/*  →  internal/*
internal/server, internal/scheduler  →  internal/pool, internal/upstream
internal/pool, internal/upstream      →  internal/auth
internal/auth                         →  (nothing)
```

- `internal/*` never imports `cmd/*`; no cycles between `internal` packages.
- `internal/auth` stays dependency-free so every binary can reuse it.
- There is no `pkg/`, no `utils/`, no vendoring: this module is an application, not a library.

---

## Naming and file conventions

- Package name = directory name, lowercase single word (`upstream`, `scheduler`).
- Single-concern packages keep one file (`auth/auth.go`, `pool/pool.go`); larger packages split files by **concern**, not by layer: `client.go`, `sse.go`, `classify.go`, `checkin.go`.
- Tests sit next to the code they test and use the same package: `internal/upstream/client_test.go`, `checkin_test.go`.
- Every package has a one-line Chinese doc comment on its first file, e.g. `// Package upstream 封装对 LobsterAI 上游的全部 HTTP 调用。`
- Files in `package main` start with a header comment naming the file and its job, e.g. `// config.go 加载 JSON 配置 + 环境变量覆盖。`
- Binaries are built with `go build -o <name>.exe ./cmd/<name>` (see README "Build").

---

## Reference examples

- Split-by-concern package: `internal/upstream/` (client / sse / classify / checkin).
- Wiring, background goroutines and graceful shutdown without a framework: `cmd/server/main.go`.
- File header plus env-var documentation block: `cmd/login/main.go`.
