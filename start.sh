#!/bin/sh
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
BIN="$DIR/ai-fast-gateway"
PID_FILE="$DIR/ai-fast-gateway.pid"
STDOUT_FILE="$DIR/ai-fast-gateway.stdout"

LISTEN="${LISTEN_ADDR:-127.0.0.1:18317}"
UPSTREAM="${UPSTREAM_URL:-http://127.0.0.1:8080}"
LOG_FILE="${LOG_FILE:-$DIR/ai-fast-gateway.log}"
LOG_MAX_SIZE_MB="${LOG_MAX_SIZE_MB:-20}"
LOG_MAX_BACKUPS="${LOG_MAX_BACKUPS:-5}"
LOG_ROTATE_INTERVAL_MINUTES="${LOG_ROTATE_INTERVAL_MINUTES:-0}"

if [ -f "$PID_FILE" ]; then
  OLD_PID=$(cat "$PID_FILE" 2>/dev/null || true)
  if [ -n "$OLD_PID" ] && kill -0 "$OLD_PID" 2>/dev/null; then
    echo "ai-fast-gateway already running: pid=$OLD_PID"
    exit 0
  fi
fi

nohup "$BIN" \
  -listen "$LISTEN" \
  -upstream "$UPSTREAM" \
  -log-file "$LOG_FILE" \
  -log-max-size-mb "$LOG_MAX_SIZE_MB" \
  -log-max-backups "$LOG_MAX_BACKUPS" \
  -log-rotate-interval-minutes "$LOG_ROTATE_INTERVAL_MINUTES" \
  >"$STDOUT_FILE" 2>&1 &
PID=$!
echo "$PID" >"$PID_FILE"
echo "ai-fast-gateway started: pid=$PID listen=$LISTEN upstream=$UPSTREAM log=$LOG_FILE max_size_mb=$LOG_MAX_SIZE_MB max_backups=$LOG_MAX_BACKUPS rotate_minutes=$LOG_ROTATE_INTERVAL_MINUTES"
