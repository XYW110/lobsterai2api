# Logging Guidelines

> stdlib `log` only — no logging framework, no log levels, no structured format.

---

## Conventions

- Server code uses `log.Printf` with default flags (`2009/11/10 23:00:00 message`, stderr). There is no logger configuration anywhere.
- **Message shape**: `log.Printf("<operation> <id>: <detail>", ...)` — operation is a snake_case verb (`chat_stream`, `checkin`, `keepalive`, `quota`), id is the UID/account, detail is the concrete cause. Real examples:
  - `log.Printf("chat_stream uid=%s: upstream %d %s body=%s", ...)` (`internal/upstream/client.go`)
  - `log.Printf("checkin %s: ok +%.0f credits", a.UID, gained)` (`internal/upstream/checkin.go`)
  - `log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)` (`cmd/server/main.go`)
- `log.Fatalf` is reserved for unrecoverable **startup** failures: config load, auth dir load, listen failure. Everything after startup uses `Printf` and keeps running.
- Lifecycle: log a start line (`listening on %s (api_key=%v)`) and a shutdown line (`bye`); the scheduler logs each per-account job outcome.

---

## What to log

- Startup summary: account count, listen address, whether an API key is set, and a warning when the upstream base URL is empty.
- Per-account state transitions with their reason: messages passed to `pool.Cooldown` / `pool.Disable`, check-in result, refresh and quota failures.
- Upstream anomalies with **truncated** bodies: `truncate(string(raw), 200)` — enough to classify, not enough to leak a payload.
- "Nothing happened" outcomes operators will ask about: `already claimed today`, `no available activity`, `check_in action not offered` — logged like success, because they answer "did the cron run?".

---

## What NOT to log

- Tokens, refresh tokens, auth codes, full auth files — never. The login binary prints only the URL to stdout and `auth saved: <path>` to stderr.
- Full request/response bodies or prompt content — always `truncate` first.
- Per-request success noise: successful chat requests are counted in the pool (`NoteSuccess`), not logged.
- PII beyond UID/nickname (`pool.Status` is explicitly 脱敏).

---

## CLI output conventions

- `cmd/credit`: machine-readable JSON on **stdout** (scripts consume it); `-pretty` switches to a human report; diagnostics on stderr.
- `cmd/login`: the login URL is the only stdout output (scripts capture it); all diagnostics/errors go to stderr.
- Follow the same split for new binaries: stdout = data contract, stderr = diagnostics.
