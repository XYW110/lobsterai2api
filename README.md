# lobsterai2api

OpenAI-compatible API bridge for LobsterAI — multi-account pool with credit-based load balancing, SSE streaming, and automatic token refresh.

- Language: Go (zero external dependencies, pure stdlib)
- Port: `:8367` (configurable via config or `LB2A_LISTEN`)

## Architecture

```
client (OpenAI SDK)
   │ POST /v1/chat/completions (Bearer ***)
   ▼
server: pool picks account (highest credits, healthy) → check token → forward
   ▼
upstream chat API (SSE only)
   ▼ on error → classify → cooldown/disable → rotate to next account (max 3)
```

## Docker (recommended)

Multi-arch image (`linux/amd64`, `linux/arm64`), deployable on any mainstream Linux with Docker + Compose v2.

```bash
git clone <this-repo> && cd lobsterai2api
cp .env.example .env      # fill in LB2A_UPSTREAM_BASE / LB2A_LOGIN_PORTAL / LB2A_API_KEY
docker compose up -d      # builds the image locally on first run
```

Published image: [`dockercom110/lobsterai2api`](https://hub.docker.com/r/dockercom110/lobsterai2api)
(`linux/amd64` + `linux/arm64`, tags `latest` / `0.1.0`). To use it instead of a local build:

```bash
docker compose pull && docker compose up -d
```

Data layout:

| Host path | Container path | Content |
|---|---|---|
| `./auths` | `/app/auths` | account credentials (`lobsterai-<uid>.json`) |
| `./data` | `/app/data` | pool state (`state.json`) |

Both directories are created automatically on first start. Nothing else is baked into the image.

Key `.env` knobs:

- `LB2A_IMAGE` — image reference to pull; defaults to `dockercom110/lobsterai2api:latest`
- `LB2A_UPSTREAM_BASE` — upstream API base URL (required)
- `LB2A_API_KEY` — local bearer key; **set it whenever the port is exposed**
- `LB2A_BIND_HOST` — `0.0.0.0` (default) or `127.0.0.1` for loopback-only
- `LB2A_PORT` — published port (default `8367`)
- `TZ` — local time used by the daily checkin / keepalive scheduler

### Add an account (OAuth login)

The login portal validates that `redirect_uri` is exactly `http://127.0.0.1:<port>/auth/callback`,
so the callback server has to be reachable through the host loopback:

```bash
docker compose --profile login run --rm --service-ports login
```

1. an authorization URL is printed — open it in a browser on the same machine
2. finish the login; the callback is forwarded into the container and `auths/lobsterai-<uid>.json` is written
3. the server rescans `auths/` every 30s, so **no restart is required**

Browser on a different machine? Forward the callback port first, then open the printed URL:

```bash
ssh -L 1455:127.0.0.1:1455 <user>@<server>
```

`LB2A_LOGIN_PORT` (default `1455`) must be identical on the host and in the container.

### Operations

```bash
docker compose ps
docker compose logs -f
docker compose exec lobsterai2api /app/lb2a-credit -pretty              # credit report
docker compose exec lobsterai2api wget -qO- http://127.0.0.1:8367/status # pool status
curl -s http://127.0.0.1:8367/healthz
```

### File ownership (Linux only)

The container runs as root by default, so `auths/` and `data/` end up root-owned.
To write them as your own user, set in `.env`:

```ini
PUID=<id -u>
PGID=<id -g>
```

The entrypoint then chowns the data directories and drops privileges via `su-exec`.

## Build (bare metal)

```bash
go build -o lobsterai2api.exe ./cmd/server
go build -o login.exe ./cmd/login
go build -o credit.exe ./cmd/credit
```

## Login (add account)

```bash
./login.sh
# or manually:
./login.exe url   # prints login URL (local callback server ready)
# open URL in browser → phone/WeChat login
./login.exe poll  # wait for callback → exchange → save auths/lobsterai-<uid>.json
```

## Run (bare metal)

```bash
./lobsterai2api.exe -config config.json
```

## Credit query

```bash
./credit.sh        # human-readable
./credit.exe       # JSON output (for scripts)
```

## Test

```bash
# non-streaming
curl -s http://127.0.0.1:8367/v1/chat/completions \
  -H "Authorization: Bearer ***" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好"}],"stream":false}'

# streaming
curl -s http://127.0.0.1:8367/v1/chat/completions \
  -H "Authorization: Bearer ***" \
  -H "Content-Type: application/json" \
  -d '{"model":"deepseek-v4-pro","messages":[{"role":"user","content":"你好"}],"stream":true}'

# model list
curl -s http://127.0.0.1:8367/v1/models -H "Authorization: Bearer ***"

# status
curl -s http://127.0.0.1:8367/status
```

## Publish to Docker Hub

`.github/workflows/docker-publish.yml` builds `linux/amd64` + `linux/arm64` and pushes to Docker Hub
on every push to `master`/`main` and on `v*` tags.

1. create the repository on <https://hub.docker.com> (e.g. `<user>/lobsterai2api`)
2. add two repository secrets: `DOCKERHUB_USERNAME`, `DOCKERHUB_TOKEN`
   (a Docker Hub **access token** with read & write scope, not your password)
3. push to `master`, or tag a release: `git tag v1.0.0 && git push origin v1.0.0`
   (the workflow can also be triggered manually from the Actions tab)

Manual multi-arch build from a workstation:

```bash
docker buildx build --platform linux/amd64,linux/arm64 \
  -t <user>/lobsterai2api:latest --push .
```

## Configuration

See `config.example.json`. Environment variable prefix `LB2A_*`:

| Variable | Description |
|---|---|
| `LB2A_LISTEN` | Listen address |
| `LB2A_API_KEY` | Local auth key |
| `LB2A_AUTH_DIR` | Auth file directory |
| `LB2A_STATE_FILE` | Pool state file |
| `LB2A_HARD_CREDIT` / `LB2A_SOFT_RATE` | Cooldown durations |
| `LB2A_ERR_THRESHOLD` / `LB2A_ERR_COOLDOWN` | Error threshold and cooldown |
| `LB2A_TIMEOUT_SECONDS` | Upstream timeout |
| `LB2A_UPSTREAM_BASE` | Upstream API base URL (required) |
| `LB2A_LOGIN_PORTAL` | Login portal URL for OAuth flow (required for login) |
| `LB2A_LOGIN_BIND` | Callback server bind address (`127.0.0.1` bare metal, `0.0.0.0` in Docker) |
| `LB2A_LOGIN_PORT` | Callback server port (`0`/unset = random, fixed port needed in Docker) |
| `LB2A_CONFIG` | Path of `config.json` used by the container entrypoint (default `/app/config.json`) |
| `PUID` / `PGID` | Optional, containers only: run as that uid/gid |
| `TZ` | Container timezone; drives checkin/keepalive hours |

`config.json` may also set `upstream.base_url`, which takes precedence over nothing —
env still wins if `LB2A_UPSTREAM_BASE` is set. A missing `config.json` is not an error:
the server falls back to built-in defaults plus `LB2A_*` env vars (that is how the
container starts when only `.env` is used).

## Features

- **Multi-account pool** — auto-load auth files from `auths/`, pick highest-credit healthy account per request
- **OpenAI-compatible** — `/v1/chat/completions` (streaming + non-streaming), `/v1/models`, `/status`, `/healthz`
- **OAuth login** — local callback server, browser-based login, auto-save credentials
- **Token refresh** — JWT expiry parsing, proactive refresh 10min before expiry, session death auto-disable
- **Error classification** — hard credit cooldown 12h, 429 soft cooldown 60s, consecutive errors 3→10m, refresh rejected → disable
- **Request-level rotation** — up to 3 account switches per request
- **Hot account reload** — `auths/` is rescanned every 30s; new logins take effect without a restart
- **Scheduler** — daily checkin + credit refresh, token keepalive
- **Dynamic model list** — fetched from upstream API, cached 1h, falls back to static table

## Known limitations / TODO

- Daily checkin endpoint not yet identified, `DailyCheckin` is currently a no-op
- Dynamic model list from upstream API (cached 1h, falls back to static table)

## License

MIT
