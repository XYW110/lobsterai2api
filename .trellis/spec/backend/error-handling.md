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
| `ErrSessionDead` | 401 + `sessionDeadMarkers` | `Disable` (needs a new credential — see the disable/re-enable lifecycle below) |
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
- The operator route `POST /admin/accounts/{uid}/enable` sits behind `withAuth` (same bearer check as `/v1/*`) and reuses the OpenAI error shape: 200 + the masked `pool.Status` on a hit, 404 `account_not_found` on a miss. `/status` remains unauthenticated by design — never put a mutating route there.

---

## Account disable / re-enable lifecycle

`entry.disabled` in `internal/pool` is the terminal "operator must act" state; unlike cooldowns it never expires, and it is persisted in `state.json`, so it survives restarts. It is written by three call sites only: `handler.go` (refresh → `ErrSessionDead`, and a 200 error frame classified as `ErrSessionDead`) and `scheduler.RunKeepaliveNow` (refresh failure with that kind). Do not add expiry semantics to `disabled` — use `Cooldown` for time-based recovery.

Two paths clear it, and **both must call `saveLocked()`** — clearing the flag only in memory is a real bug (found 2026-09-19): the 30s rescan re-enables the account, but on the next restart `load()` restores `disabled:true`, and the placeholder guard below then blocks the credential-change path forever, so the account is dead again with no way back short of restarting twice.

1. **Credential change (automatic)** — `entry.rotateCredentials` (used by both `Add` and `SyncToDir`): if the stored accessToken is non-empty **and** differs from the incoming one, the account is treated as re-logged-in and `disabled` / `until` / `reason` / `errCount` are cleared, logging `auth_rotate uid=%s: access token changed, account re-enabled`.
   - The `old != ""` guard is load-bearing: `pool.load` seeds each account with `&auth.Auth{UID: uid}`, so without it the first `Add` after every startup would look like a rotation and wipe the persisted flag.
   - accessToken is the fingerprint on purpose: an in-process `RefreshToken` mutates `entry.a` and the same value is written to disk, so the following rescan compares equal and does **not** unlock a healthy account by accident.
2. **Manual** — `Pool.Enable(uid) bool` clears `disabled`/`until`/`reason`/`errCount` (never `credits`), persists only on a hit, and is exposed as `POST /admin/accounts/{uid}/enable` for the "same file restored, token unchanged" case.

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
