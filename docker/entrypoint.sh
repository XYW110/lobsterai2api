#!/bin/sh
# entrypoint.sh — 容器入口
#
# 职责：
#   1. 准备 auths/ 与 data/ 目录
#   2. 给 lobsterai2api 子命令自动注入 -config 参数
#   3. 可选：按 PUID/PGID 降权运行（保证 bind mount 出来的文件属主 = 宿主机用户）
set -eu

CONFIG="${LB2A_CONFIG:-/app/config.json}"
AUTH_DIR="${LB2A_AUTH_DIR:-/app/auths}"
STATE_FILE="${LB2A_STATE_FILE:-/app/data/state.json}"
DATA_DIR="$(dirname "$STATE_FILE")"

mkdir -p "$AUTH_DIR" "$DATA_DIR" 2>/dev/null || true

# 拆出「命令」和「其余参数」
if [ "$#" -gt 0 ]; then
    CMD="$1"
    shift
else
    CMD="/app/lobsterai2api"
fi

# server 进程统一由 LB2A_CONFIG 指定配置文件路径；
# 文件不存在时程序会自行回退到「内置默认值 + LB2A_* 环境变量」，不影响启动
case "$CMD" in
    */lobsterai2api | lobsterai2api | server)
        set -- "$CMD" -config "$CONFIG" "$@"
        ;;
    *)
        set -- "$CMD" "$@"
        ;;
esac

# 可选降权：compose 里设置 PUID/PGID 即可
if [ "$(id -u)" = "0" ] && [ -n "${PUID:-}" ]; then
    PGID="${PGID:-$PUID}"
    chown -R "$PUID:$PGID" "$AUTH_DIR" "$DATA_DIR" 2>/dev/null || true
    exec su-exec "$PUID:$PGID" "$@"
fi

exec "$@"
