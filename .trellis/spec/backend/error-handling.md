# Error Handling

> Error kinds, classification, and the contract between `internal/upstream` and its callers.

---

## Upstream streaming error contract (ChatStream → handler)

`upstream.Client.ChatStream` returns `(rc io.ReadCloser, status int, err error)` with **three** distinct outcomes; the handler must branch on `rc == nil`, never on `status >= 400`:

| Outcome | rc | status | err | Meaning |
|---------|----|--------|-----|---------|
| transport failure | nil | 0 | non-nil | network/TLS error |
| upstream error | nil | upstream status | nil | non-2xx, **or** HTTP 200 whose SSE head is an error frame (`event:error` / `"error":{`); body in `Client.LastBody` |
| success | non-nil | 200 | nil | caller must `Close()` |

Why the 200 path exists: LobsterAI hides business errors (e.g. `code=40201` 免费额度已用完) inside HTTP 200 SSE streams. `ChatStream` losslessly peeks the first `peekBufSize` (4KB) via `bufio.Reader.Peek` and hands the *same* reader downstream wrapped in `peekCloser` — the peeked bytes must never be dropped or re-read.

Envelope note: upstream API responses wrap as `{code, msg|message, data}` — some endpoints use `msg`, others `message`; always read via `apiEnvelope.envMsg()`, never a single field.

---

## Error kinds

`ErrKind` (`internal/upstream/classify.go`) is the vocabulary shared by upstream, handler and scheduler:

| Kind | Detected from | Action |
|---|---|---|
| `ErrHardCredit` | HTTP 402, or a `hardMarkers` hit | `Cooldown(CoolHard, 12h)` |
| `ErrSoftRate` | HTTP 429 | `Cooldown(CoolSoft, 60s)` |
| `ErrSessionDead` | 401 + `sessionDeadMarkers` | `Disable` (requires re-login) |
| `ErrNotFound` | HTTP 404 | short cooldown, `errCount` not incremented (防雪崩) |
| `ErrServer` | 5xx | rotate + `NoteError` threshold |
| `ErrClient` | other 4xx / business code | rotate + `NoteError` threshold |

Durations and thresholds come from config (`cmd/server/config.go`), not from literals in the handler.

---

## Classification

- `Classify(status, body)` is the only place that maps text → kind. Phrase lists live in `classify.go` (`hardMarkers`, `sessionDeadMarkers`); latin matching is lowercase, Chinese compared as-is.
- Adding a new upstream error phrase = add it to the relevant list, never match strings at the call site.
- Order matters: hard-credit and session-dead markers are checked before the status-code fallback, so a 200 in-stream error frame still classifies.

---

## Error values

- `*upstream.Error{Kind, Status, Msg}` is the typed error; extract with `errors.As` (see `scheduler.RunKeepaliveNow`).
- `doJSON` failures: HTTP >= 400 → `*Error` with truncated body; envelope `code != 0` → `*Error` (kind from `envMsg()`, fallback `ErrClient`); parse failure → plain wrapped `fmt.Errorf`.
- Wrap with an operation prefix and `%w`: `fmt.Errorf("check_in: %w", err)` (`checkin.go`).
- Bodies embedded in errors/logs are truncated via `truncate(s, n)` (120–200 chars) — never a full upstream body.

---

## Client-facing errors

- All local API errors go through `writeOpenAIError(w, status, code, msg)` (`internal/server/handler.go`) in OpenAI shape: `{"error": {"message", "type": "api_error", "code"}}`.
- Handler policy: rotate accounts up to `MaxRotate` (3); when all fail → 503 `no_healthy_account` with the last classified error appended. Business errors do not get their own HTTP status — they trigger rotation instead.
- Transport errors (`err != nil`) → `NoteError` + rotate. Classified errors (`rc == nil`) → cooldown/disable/rotate chosen by kind (table above).

---

## Best-effort paths (intentionally non-fatal)

- `auth.LoadDir` silently skips unreadable/unparseable files; `pool.load` ignores a missing or corrupt `state.json`; `rescanAuths` skips a failed scan. Startup must not die because one credential file is broken.
- The scheduler logs a `DailyCheckin` error and still runs the quota refresh — an upstream "already claimed" error must not block unfreezing.
- `login` / `credit` binaries fail loudly instead: `fatal()` → stderr + exit 1. CLI paths are fail-fast; server paths degrade gracefully.

---

## Anti-patterns

- ❌ Branching on `status >= 400` in the handler — use `rc == nil` (a 200 error frame would slip through).
- ❌ Matching upstream error text in the handler/scheduler instead of extending `classify.go`.
- ❌ Returning a raw upstream body (or any token) to a client or into logs.
- ❌ Treating "already claimed today" / "no available activity" as errors — check-in logs them and returns nil.
