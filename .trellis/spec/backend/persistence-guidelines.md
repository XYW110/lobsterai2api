# Persistence

> This project has **no database**: no ORM, no migrations, no SQL. All state is JSON on disk, which keeps the service a single static binary.

---

## Files on disk

| File | Written by | Read by | Shape |
|---|---|---|---|
| `auths/lobsterai-<uid>.json` | `login` binary (exchange), `auth.SaveAtomic()` after refresh | server startup + 30s rescan, `credit` binary | `{"auth": {...}, "account": {...}}`, camelCase |
| `data/state.json` | `pool.saveLocked()` on every mutation | `pool.New()` at startup | `{"accounts": {"<uid>": {credits, disabled, reason, until}}}` |
| `/tmp/lb2api-login-state.json` (+`.result`, `.failed`) | `login url` | `login poll` | tiny coordination state; transient |

Paths are configurable (`LB2A_AUTH_DIR`, `LB2A_STATE_FILE`); the container maps them onto mounted volumes (`./auths`, `./data`).

---

## Write rules

- **Atomic only**: write `path + ".tmp"` then `os.Rename` — `auth.SaveAtomic` (0600) and `pool.saveLocked` (0600, `MkdirAll` 0755 first). A crash must never leave a half-written credential or state file.
- Whole-file snapshot: `state.json` is rewritten in full on each change; there is no append or partial update.
- `MarshalIndent(doc, "", "  ")` — files stay human-inspectable and diff-friendly.
- Credential/state files are `0600`; directories `0755`.

---

## Read rules

- Loading is **best-effort and never fatal**: missing file, corrupt JSON or an unreadable entry is skipped silently (`pool.load`, `auth.LoadDir`).
- `auth.Parse` accepts both disk shapes — nested (`{auth, account}`, written by the login tool) and flat (`{accessToken, uid, ...}`, hand-written). Saving always writes the nested shape so the login tool can still read it.
- `SaveAtomic` requires `FilePath` to be set (it is, when the auth came from `LoadDir`).

---

## Naming conventions per file

- Auth files: camelCase keys (`accessToken`, `firstKeyfrom`) — wire compatibility with the surrounding LobsterAI tooling; do not rename.
- `state.json`: lowercase single words (`credits`, `disabled`, `reason`, `until`).
- `config.json`: snake_case (`api_key`, `auth_dir`, `hard_credit`).
- Optional fields use `omitempty`, and readers must tolerate absent fields so old state files stay loadable.

---

## When adding a field or a new file

1. Reuse the tmp + rename pattern — never `os.WriteFile` directly onto a live state/credential path.
2. Keep old files readable: new fields default to zero values, never required.
3. If a tool outside the server reads the file (`login`, `credit`, dashboards), preserve its field names — the file is an interface, not a private detail.
