# AI Fast Gateway

[中文说明](README.zh-CN.md)

HTTP/WebSocket proxy that forwards requests to `UPSTREAM_URL` and force-writes fast-mode fields.

```json
{"service_tier":"fast"}
```

for OpenAI/Codex-style JSON `POST` request bodies. For Anthropic/Claude-style `/v1/messages`
JSON `POST` request bodies, it writes:

```json
{"speed":"fast","service_tier":"fast"}
```

Claude-style `/v1/messages` requests also get a single merged beta header containing:

```text
anthropic-beta: fast-mode-2026-02-01
```

WebSocket Upgrade requests are also supported. The proxy strips WS compression extensions and
injects `service_tier:"fast"` into client-to-upstream JSON text frames such as `response.create`
payloads before forwarding them.

Claude Code `/v1/messages` can optionally be bridged to upstream `/responses` WebSocket.
This mode is disabled by default and falls back to the normal HTTP proxy path before any
response bytes are written if the upstream WebSocket cannot be opened.

```text
CC_WS_BRIDGE_ENABLED=false
CC_WS_BRIDGE_UPSTREAM_PATH=/responses
CC_WS_BRIDGE_FALLBACK_HTTP=true
CC_WS_BRIDGE_DEBUG_FRAMES=false
CC_WS_BRIDGE_FIRST_EVENT_TIMEOUT_MS=15000
WS_DEBUG_PAYLOAD_BYTES=0
CC_WS_POOL_MODE=client
CC_WS_POOL_MAX_CONNS_PER_CLIENT=20
CC_WS_POOL_MAX_IDLE_PER_CLIENT=20
CC_WS_POOL_IDLE_TTL_SECONDS=600
CC_WS_POOL_ACQUIRE_TIMEOUT_MS=3000
```

## Build

```bash
go build -o ai-fast-gateway .
```

## Run

```bash
./start.sh
```

Stop:

```bash
./stop.sh
```

The start script uses:

```text
LISTEN_ADDR=127.0.0.1:18317
UPSTREAM_URL=http://127.0.0.1:8080
LOG_FILE=./ai-fast-gateway.log
LOG_MAX_SIZE_MB=20
LOG_MAX_BACKUPS=5
LOG_ROTATE_INTERVAL_MINUTES=0
```

Override example:

```bash
LISTEN_ADDR=127.0.0.1:18318 \
UPSTREAM_URL=http://127.0.0.1:8080 \
./start.sh
```

Useful environment variables and flags:

```text
-listen   listen address, default :8317
-upstream upstream base URL, default http://127.0.0.1:8080
-log-file log file path, default is ai-fast-gateway.log next to the executable
LOG_MAX_SIZE_MB=20
LOG_MAX_BACKUPS=5
LOG_ROTATE_INTERVAL_MINUTES=0
MAX_IDLE_CONNS=100
MAX_IDLE_CONNS_PER_HOST=100
CC_WS_BRIDGE_ENABLED=false
```

In `client` pool mode, each client identity gets a reusable upstream WS pool. A single
WS connection runs one request at a time, then returns to the pool and can be reused
serially by later requests from the same session. Different sessions do not share the
same WS connection, but they share the client's pool limit. The default pool size is
20 upstream WS connections per client. Set `CC_WS_POOL_MODE=request` to use the old
one-request-one-WS behavior.

Health check:

```bash
curl http://127.0.0.1:18317/healthz
```

Then point Codex/ccswitch/Claude base URL to:

```text
http://127.0.0.1:18317/
```

## Docker

The upstream target is configurable with `UPSTREAM_URL`:

```bash
docker run -d \
  --name ai-fast-gateway \
  --network sub2api_sub2api-network \
  -p 8317:8317 \
  -v /opt/ai-fast-gateway/logs:/logs \
  -e LISTEN_ADDR=:8317 \
  -e UPSTREAM_URL=http://sub2api:8080 \
  -e LOG_FILE=/logs/ai-fast-gateway.log \
  -e LOG_MAX_SIZE_MB=20 \
  -e LOG_MAX_BACKUPS=5 \
  -e LOG_ROTATE_INTERVAL_MINUTES=0 \
  -e MAX_IDLE_CONNS=256 \
  -e MAX_IDLE_CONNS_PER_HOST=256 \
  -e CC_WS_BRIDGE_ENABLED=false \
  -e CC_WS_POOL_MODE=client \
  -e CC_WS_POOL_MAX_CONNS_PER_CLIENT=20 \
  ghcr.io/your-org/ai-fast-gateway:latest
```

Logs are written to Docker stdout and to `LOG_FILE` when `LOG_FILE` is not `stdout`.
Mount `/logs` to a host directory when using file logs. Rotated files are named like
`ai-fast-gateway-20260526-173538.log`.
