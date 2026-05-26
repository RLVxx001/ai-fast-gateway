#!/bin/sh
set -eu

DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
PID_FILE="$DIR/ai-fast-gateway.pid"

if [ ! -f "$PID_FILE" ]; then
  echo "ai-fast-gateway is not running: missing pid file"
  exit 0
fi

PID=$(cat "$PID_FILE" 2>/dev/null || true)
if [ -z "$PID" ]; then
  rm -f "$PID_FILE"
  echo "ai-fast-gateway is not running: empty pid file"
  exit 0
fi

if kill -0 "$PID" 2>/dev/null; then
  kill "$PID"
  echo "ai-fast-gateway stopped: pid=$PID"
else
  echo "ai-fast-gateway is not running: stale pid=$PID"
fi

rm -f "$PID_FILE"
