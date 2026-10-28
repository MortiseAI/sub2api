#!/bin/sh
set -eu

ROOT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BACKEND_DIR="$ROOT_DIR/backend"
PORT=${SERVER_PORT:-8080}
BUILD_FILE="$BACKEND_DIR/sub2api.build.$$"
trap 'rm -f "$BUILD_FILE"' EXIT HUP INT TERM

if [ -t 1 ] && [ "${TERM:-}" != "dumb" ] && [ -z "${NO_COLOR:-}" ]; then
  GREEN=$(printf '\033[32m')
  RED=$(printf '\033[31m')
  RESET=$(printf '\033[0m')
else
  GREEN= RED= RESET=
fi
success() { printf '%s%s%s\n' "$GREEN" "$1" "$RESET"; }
error() { printf '%s%s%s\n' "$RED" "$1" "$RESET" >&2; }

for command in pnpm go lsof; do
  if ! command -v "$command" >/dev/null 2>&1; then
    error "[CMS -> 错误] 缺少必要命令：${command}。"
    exit 1
  fi
done

echo "[CMS -> 前端] 开始安装依赖并编译..."
cd "$ROOT_DIR/frontend"
if ! pnpm install --frozen-lockfile; then
  error "[CMS -> 前端] 依赖安装失败。"
  exit 1
fi
if ! pnpm run build; then
  error "[CMS -> 前端] 编译失败。"
  exit 1
fi
success "[CMS -> 前端] 编译完成。"

echo "[CMS -> 后端] 开始编译..."
cd "$BACKEND_DIR"
if ! VERSION=$(./scripts/resolve-version.sh); then
  error "[CMS -> 后端] 获取版本号失败。"
  exit 1
fi
if ! go build -tags embed -ldflags="-X main.Version=${VERSION}" -o "$BUILD_FILE" ./cmd/server; then
  error "[CMS -> 后端] 编译失败。"
  exit 1
fi
success "[CMS -> 后端] 编译完成（版本：${VERSION}）。"

PIDS=$(lsof -nP -t -iTCP:"$PORT" -sTCP:LISTEN || true)
for pid in $PIDS; do
  process=$(ps -p "$pid" -o comm= | xargs basename)
  if [ "$process" != "sub2api" ]; then
    error "[CMS -> 启动] 端口 ${PORT} 被 ${process}（PID ${pid}）占用，不会停止该进程。"
    exit 1
  fi
done
for pid in $PIDS; do
  echo "[CMS -> 启动] 正在停止 sub2api（PID ${pid}）..."
  kill "$pid"
done

if [ -n "$PIDS" ]; then
  attempts=0
  while lsof -nP -t -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; do
    attempts=$((attempts + 1))
    if [ "$attempts" -ge 30 ]; then
      error "[CMS -> 启动] 端口 ${PORT} 未能释放。"
      exit 1
    fi
    sleep 1
  done
fi

mv -f "$BUILD_FILE" "$BACKEND_DIR/sub2api"
trap - EXIT HUP INT TERM
echo "[CMS -> 启动] 正在启动 sub2api，检查端口 ${PORT}；按 Ctrl+C 停止。"
"$BACKEND_DIR/sub2api" &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; exit 130' INT
trap 'kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true; exit 143' TERM HUP
attempts=0
while [ "$attempts" -lt 30 ]; do
  if lsof -nP -a -p "$server_pid" -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1; then
    success "[CMS -> 启动] sub2api 启动成功，正在监听端口 ${PORT}。"
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    error "[CMS -> 启动] sub2api 已退出，请检查上方日志。"
    wait "$server_pid" || exit $?
    exit 1
  fi
  attempts=$((attempts + 1))
  sleep 1
done
if [ "$attempts" -ge 30 ]; then
  error "[CMS -> 启动] 30 秒内未检测到端口 ${PORT} 监听，请检查服务日志及端口配置。"
fi
wait "$server_pid"
