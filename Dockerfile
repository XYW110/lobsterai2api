# check=skip=SecretsUsedInArgOrEnv
#
# 说明：
#   - 上面的 check 指令用于屏蔽 SecretsUsedInArgOrEnv 误报（LB2A_AUTH_DIR 只是
#     目录路径，因变量名含 "AUTH" 被判定为疑似敏感值）
#   - 不使用 `# syntax=` 指令，依赖 BuildKit 内置前端即可（$BUILDPLATFORM /
#     TARGETARCH 均支持），避免构建时额外拉取 frontend 镜像

# ---------------------------------------------------------------------------
# 构建阶段：编译 server / login / credit 三个静态二进制（CGO 关闭，纯 stdlib）
# ---------------------------------------------------------------------------
FROM --platform=$BUILDPLATFORM golang:1.22-alpine AS build

ARG TARGETOS=linux
ARG TARGETARCH=amd64
ARG TARGETVARIANT=""

WORKDIR /src

COPY go.mod ./
COPY cmd/ ./cmd/
COPY internal/ ./internal/

RUN set -eux; \
    export CGO_ENABLED=0 GOOS="$TARGETOS" GOARCH="$TARGETARCH" GOARM="${TARGETVARIANT#v}"; \
    go build -trimpath -ldflags="-s -w" -o /out/lobsterai2api ./cmd/server; \
    go build -trimpath -ldflags="-s -w" -o /out/lb2a-login    ./cmd/login; \
    go build -trimpath -ldflags="-s -w" -o /out/lb2a-credit   ./cmd/credit

# ---------------------------------------------------------------------------
# 运行阶段：alpine（glibc 无关，amd64/arm64/armv7 通用）
# ---------------------------------------------------------------------------
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata su-exec

WORKDIR /app

COPY --from=build /out/lobsterai2api /app/lobsterai2api
COPY --from=build /out/lb2a-login    /app/lb2a-login
COPY --from=build /out/lb2a-credit   /app/lb2a-credit
COPY config.example.json             /app/config.example.json

COPY docker/entrypoint.sh /usr/local/bin/entrypoint.sh
COPY docker/login.sh      /usr/local/bin/lb2a-login.sh

RUN set -eux; \
    chmod +x /app/lobsterai2api /app/lb2a-login /app/lb2a-credit \
             /usr/local/bin/entrypoint.sh /usr/local/bin/lb2a-login.sh; \
    mkdir -p /app/auths /app/data

ENV LB2A_LISTEN=":8367" \
    LB2A_AUTH_DIR="/app/auths" \
    LB2A_STATE_FILE="/app/data/state.json" \
    LB2A_CONFIG="/app/config.json" \
    TZ="Asia/Shanghai"

EXPOSE 8367

VOLUME ["/app/auths", "/app/data"]

# LB2A_LISTEN 形如 ":8367"；健康检查直接复用该端口
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O - "http://127.0.0.1${LB2A_LISTEN:-:8367}/healthz" >/dev/null 2>&1 || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
CMD ["/app/lobsterai2api"]
