#!/bin/sh
# login.sh — 容器内登录助手
#
# 流程：
#   1. 后台起 lb2a-login url（本地回调服务器，监听 LB2A_LOGIN_BIND:LB2A_LOGIN_PORT）
#   2. 打印授权链接 → 浏览器打开 → 回调落到本容器
#   3. lb2a-login poll 读取结果 → 落盘 auths/lobsterai-<uid>.json
#
# 用法（宿主机）：
#   docker compose --profile login run --rm --service-ports login
set -eu

AUTH_DIR="${LB2A_AUTH_DIR:-/app/auths}"
BIND="${LB2A_LOGIN_BIND:-0.0.0.0}"
PORT="${LB2A_LOGIN_PORT:-1455}"

URL_FILE=/tmp/lb2api-login-url.txt
ERR_FILE=/tmp/lb2api-login.err
RESULT_FILE=/tmp/lb2api-login-result.json

rm -f "$URL_FILE" "$ERR_FILE" "$RESULT_FILE"
mkdir -p "$AUTH_DIR"

echo "============================================================"
echo "  LobsterAI 登录"
echo "============================================================"
echo "  回调地址 : http://127.0.0.1:${PORT}/auth/callback"
echo "  auth 目录: ${AUTH_DIR}"
echo ""
echo "  提示：浏览器不在本机时，先做端口转发再打开链接："
echo "    ssh -L ${PORT}:127.0.0.1:${PORT} <user>@<server>"
echo ""

LB2A_LOGIN_BIND="$BIND" LB2A_LOGIN_PORT="$PORT" /app/lb2a-login url >"$URL_FILE" 2>"$ERR_FILE" &
URL_PID=$!

# 等待授权链接输出（最多 20s）
i=0
while [ "$i" -lt 100 ]; do
    [ -s "$URL_FILE" ] && break
    if ! kill -0 "$URL_PID" 2>/dev/null; then
        break
    fi
    i=$((i + 1))
    sleep 0.2
done

if [ ! -s "$URL_FILE" ]; then
    echo "启动登录回调服务器失败：" >&2
    cat "$ERR_FILE" >&2 2>/dev/null || true
    wait "$URL_PID" 2>/dev/null || true
    exit 1
fi

echo "请在浏览器打开以下链接完成登录："
echo ""
echo "  $(cat "$URL_FILE")"
echo ""
echo "等待登录回调（最长 10 分钟）..."

OK=1
if /app/lb2a-login poll >"$RESULT_FILE" 2>>"$ERR_FILE"; then
    echo ""
    echo "登录成功："
    cat "$RESULT_FILE"
    echo ""
    echo "auth 文件已写入 ${AUTH_DIR}，服务会在 30 秒内自动加载新账号。"
else
    OK=0
    echo "" >&2
    echo "登录失败：" >&2
    tail -n 20 "$ERR_FILE" >&2 2>/dev/null || true
fi

# 回调已结束，直接收掉后台回调服务器，避免等满 10 分钟超时
kill "$URL_PID" 2>/dev/null || true
wait "$URL_PID" 2>/dev/null || true

[ "$OK" = "1" ] || exit 1
