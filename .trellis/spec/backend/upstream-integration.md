# Upstream Integration

> How lobsterai2api talks to the LobsterAI backend. Every outbound call goes through `internal/upstream`.

---

## Base URL

- The single accessor is `upstream.ServerBase()` (`internal/upstream/client.go`). Precedence: `SetServerBase()` (from `config.json → upstream.base_url`, applied in `cmd/server/main.go`) → `LB2A_UPSTREAM_BASE` env → `""`.
- **Never hardcode an upstream domain.** Commit `fe299f1` removed one on purpose. An empty base URL must degrade to a warning (`cmd/server/main.go`) or a clear error (`cmd/login`, `cmd/credit`).
- The check-in's vendor version endpoint follows the same rule — its URL comes **only** from `LB2A_UPDATE_API` (no built-in default). When unset, `resolveClientVersion` fails and the check-in is skipped with a log line (`internal/upstream/checkin.go`).

---

## Request conventions

- Chat: `chatHeaders()` — `Authorization: Bearer <accessToken>`, `User-Agent: LobsterAI/<clientVersion>`, `X-LobsterAI-Client-Capabilities`, JSON content type, `Accept: text/event-stream, application/json`.
- Check-in: `checkinJSON()` — Bearer + `LobsterAI/<resolved version>` UA; slot/context/check_in all go through it.
- Other calls set the three standard headers inline (`Authorization`, `Accept`, `User-Agent`) — see `FetchModels`, `QuotaUsage`.
- The upstream only serves SSE for chat: `prepareChatBody` forces `stream: true` and normalizes `tool_choice` (measured: `stream:false` returns 500 — keep that comment when refactoring).

---

## Response envelope

- Upstream wraps everything as `{code, msg|message, data}`. Parse with `apiEnvelope` + `envMsg()` (`client.go`) — some endpoints use `msg`, others `message`; never read a single field directly.
- `code != 0` → build `*upstream.Error` via `Classify`. Empty/null `data` on token flows means the access token is dead (`checkinJSON`).
- `doJSON` is the standard helper: request → envelope → `json.RawMessage` of `data`. Bodies are read through `io.LimitReader(..., 1<<20)`.

---

## Endpoints in use

| Path | Method | Used by | Notes |
|---|---|---|---|
| `/api/auth/refresh` | POST | `RefreshToken` | keyfrom body + refreshToken; `ExpiresAt` from `expiresIn` or JWT `exp` |
| `/api/proxy/v1/chat/completions` | POST | `ChatStream` | SSE only; see the three-state contract in `error-handling.md` |
| `/api/models/available` | GET | `FetchModels` | keyfrom query params; handler caches 1h, falls back to `staticModels` |
| `/api/user/profile-summary` | GET | `QuotaUsage`, `cmd/credit` | `totalCreditsRemaining`; `/api/user/quota` only shows free tier (misleading) |
| `/api/client-activities/slot` | GET | `DailyCheckin` | `placement=desktop_sidebar&clientVersion=…&containerApiVersion=2&platform=win32` |
| `/api/client-activities/{code}/context` | GET | `DailyCheckin` | `configRevision` query param |
| `/api/client-activities/{code}/actions/check_in` | POST | `DailyCheckin` | body `{configRevision, idempotencyKey, payload}`; UUIDv4 idempotency key |
| vendor update API | GET | `resolveClientVersion` | URL from `LB2A_UPDATE_API` (required); `data.value.version`; 1h cache |

---

## SSE handling

- `Aggregate(r)` — for non-streaming clients: reads the whole SSE stream, merges `delta.content` / `reasoning_content` / tool_calls (by index) into one `chat.completion` object. Tolerates both `data: {...}` and `data:{...}`.
- `Stream(w, r)` — passthrough for `stream:true` clients; flushes per line and appends `data: [DONE]` if the upstream omitted it.
- `ChatStream` peeks the first 4KB **losslessly** (`bufio.Reader.Peek` + `peekCloser`) to catch business error frames hidden in HTTP 200. The peek must never consume bytes — the same reader goes downstream. Contract details in `error-handling.md`.
- Timeout: `Client.HTTP.Timeout` comes from `upstream.timeout_seconds` (default 180s), set in `cmd/server/main.go`.

---

## clientVersion

- `client.go` sends the static `clientVersion = "0.1.0"` on chat calls, while check-in resolves the current version dynamically (`resolveClientVersion`) because activity endpoints validate it. Do not unify these without re-testing against upstream.

---

## Testing upstream changes

- Point the client at an `httptest.NewServer` with `SetServerBase(ts.URL)` and restore in `t.Cleanup` — precedents: `newTestClient` (`client_test.go`), `newCheckinEnv` (`checkin_test.go`, which also pins `LB2A_UPDATE_API` and resets `versionCache`).
- Assert on what the test server received (query string, headers, JSON body) — that is how `TestDailyCheckinSuccess` pins the protocol.
