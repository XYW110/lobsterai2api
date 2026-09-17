# Quality Guidelines

> What "done" means in this repo: stdlib-only code, `go vet` clean, tests where behavior is non-obvious.

---

## Forbidden patterns

- **Third-party dependencies.** `go.mod` has no `require` block and must stay that way (README: "zero external dependencies, pure stdlib"). Precedent for replacing a library: UUIDv4 via `crypto/rand` in `internal/upstream/checkin.go`. If a feature seems to need a module, implement the minimal subset with stdlib.
- **Hardcoded upstream domains** — see `upstream-integration.md`; commit `fe299f1` removed one for public-repo safety.
- **Framework-style abstractions**: no `pkg/`, no `utils/`, no interfaces with a single implementation. The codebase is deliberately flat — plain structs, plain functions.
- **Untruncated upstream bodies** in logs or errors.
- **Incompatible on-disk format changes** (auth file keys, `state.json` fields) — see `persistence-guidelines.md`.

---

## Required patterns

- Exported symbols get doc comments; comments explain *why* (upstream quirks, security constraints), not *what*. Reference precedent: the `stream:false 返回 500` note in `client.go`.
- Comments are Chinese; identifiers, log messages and error strings are English. Keep that split when editing.
- Errors: wrap with `%w` and a lowercase operation prefix (`fmt.Errorf("check_in: %w", err)`); typed `*upstream.Error` is produced only by the upstream layer.
- New config knobs are wired in four places: `Config` struct, `Default()`, `applyEnv()` (`cmd/server/config.go`), and docs (`README.md`, `config.example.json`, `.env.example`).
- Concurrency: mutable shared state lives behind a mutex inside its owner (`pool.Pool`, `versionCache`); do not add package-level mutable state elsewhere.
- HTTP layer follows `cmd/server/main.go`: Go 1.22 method-pattern routes, `ReadHeaderTimeout`, `signal.NotifyContext` shutdown.

---

## Testing requirements

- Tests are stdlib-only: `testing` + `net/http/httptest`. No assertion libraries — plain `t.Fatalf` with got/want values.
- Test files sit next to the code, same package (`package upstream`), named `<file>_test.go`.
- Upstream-facing behavior is tested by pointing the client at an `httptest.Server` and restoring state in `t.Cleanup` — copy `newTestClient` (`client_test.go`) or `newCheckinEnv` (`checkin_test.go`).
- Environment-dependent tests use `t.Setenv` (auto-restores); global caches are reset explicitly when they affect the test (`versionCache` in `checkin_test.go`).
- Protocol-sensitive behavior gets **byte-exact** assertions, not "contains": the SSE passthrough test compares the whole 60KB stream with `bytes.Equal`.
- Regression tests are named for the failure they prevent (`TestChatStreamErrorFrameIn200`, `TestChatStreamPassthroughByteExact`).
- `pool`, `auth`, `server`, `scheduler` currently have no tests — add coverage when you change their behavior.

---

## Verification before commit

```bash
go build ./... && go vet ./... && go test ./...
```

`gofmt` formatting is expected. On Windows checkouts `gofmt -l` flags every file that round-tripped through CRLF — compare against a file you did not touch before reacting.

---

## Commit style

Conventional commits, English subject, scope = package or area:

- `fix(upstream): detect business error frames hidden in HTTP 200 SSE streams (lossless peek)`
- `feat(upstream): implement daily check-in (+100 credits/account)`
- `ci:`, `docs:`, `security:` prefixes as used in `git log`.

One logical change per commit; put the upstream-behavior rationale in the body when the subject cannot carry it.
