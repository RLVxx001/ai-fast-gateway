# AI Fast Gateway

本文档中文在前，英文版本在后。

## 中文说明

AI Fast Gateway 是一个放在 Codex、Claude Code、ccswitch 等客户端和上游 OpenAI 兼容服务之间的 HTTP/WebSocket 中转网关。它的目标不是替代上游服务，而是在请求进入上游前做一层可控的运行策略处理：补齐 fast 参数、桥接 `/responses` WebSocket、规避大请求 WS EOF、自动轮换容易失败的 session、提供日志和连接池控制台。

典型链路：

```text
Codex / Claude Code / ccswitch
  -> ai-fast-gateway
  -> sub2api / OpenAI-compatible upstream
```

Claude Code bridge 链路：

```text
Claude Code /v1/messages
  -> ai-fast-gateway
  -> upstream /responses WebSocket
  -> Anthropic SSE response
```

### 能做什么

- HTTP/SSE 代理：把所有普通 HTTP 请求转发到 `UPSTREAM_URL`。
- Fast 参数注入：开启 `FAST_MODE_ENABLED=true` 后，OpenAI 风格请求补 `service_tier=fast`，Anthropic 风格 `/v1/messages` 补 `speed=fast`、`service_tier=fast` 和 fast beta header。
- 普通 WebSocket 代理：转发 WS Upgrade 请求，去掉压缩扩展，并可对客户端发往上游的 JSON 文本帧补 fast 参数。
- OpenAI `/responses` WS bridge：把客户端的 HTTP `POST /responses` 转成上游 `/responses` WebSocket 请求，再把上游事件转回 SSE 或 JSON 响应。
- Claude Code bridge：把 Claude Code `/v1/messages` 转成上游 `/responses` WebSocket，请求失败时可回退原始 HTTP `/v1/messages`。
- 大请求绕过：当编码后的 `response.create` 超过阈值时跳过 WS bridge，直接走普通 HTTP，减少 101 后立刻 EOF。
- Session 轮换：连续首帧 EOF/超时后替换 sticky session id，并持久化映射。
- WebSocket 连接池：按 client 和 session 复用上游 WS，减少重复握手。
- Admin 控制台：浏览器页面查看状态、日志、策略、session 轮换和连接池。
- 日志查询接口：支持 tail、grep、下载日志，方便 AI 或人工排查。

### Fast 注入规则

OpenAI / Codex 风格 JSON `POST` 请求：

```json
{"service_tier":"fast"}
```

Anthropic / Claude 风格 `/v1/messages` JSON `POST` 请求：

```json
{"speed":"fast","service_tier":"fast"}
```

Anthropic 请求还会合并：

```text
anthropic-beta: fast-mode-2026-02-01
```

关闭方式：

```text
FAST_MODE_ENABLED=false
```

如果配置了 `ADMIN_POLICY_STATE_FILE`，管理台保存的运行时策略会覆盖环境变量。部署后应以 `/admin/` 策略页或 `/admin/api/status` 返回的 `fast_mode_enabled` 为准。

### 本地开发

依赖：

- Go 1.22+
- Node.js，仅用于检查 `admin/app.js` 语法
- Docker，可选，用于打包镜像

测试：

```bash
go test -count=1 ./...
node --check admin/app.js
```

本地构建二进制：

```bash
go build -o ai-fast-gateway .
```

运行：

```bash
./start.sh
```

停止：

```bash
./stop.sh
```

`start.sh` 默认使用：

```text
LISTEN_ADDR=127.0.0.1:18317
UPSTREAM_URL=http://127.0.0.1:8080
LOG_FILE=./ai-fast-gateway.log
LOG_MAX_SIZE_MB=20
LOG_MAX_BACKUPS=5
LOG_ROTATE_INTERVAL_MINUTES=0
```

自定义上游：

```bash
LISTEN_ADDR=127.0.0.1:18317 \
UPSTREAM_URL=http://127.0.0.1:8080 \
FAST_MODE_ENABLED=true \
./start.sh
```

健康检查：

```bash
curl http://127.0.0.1:18317/healthz
```

客户端 base URL 指向：

```text
http://127.0.0.1:18317/
```

### 打包说明

项目打包产物主要有两种。

第一种是单个 Go 二进制：

```bash
go build -trimpath -ldflags="-s -w" -o ai-fast-gateway .
```

Admin 页面在代码里通过 `go:embed admin/*` 嵌入，打包后的二进制不需要额外复制 `admin/` 目录也可以访问 `/admin/`。

第二种是 Docker 镜像：

```bash
docker build -t registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle .
```

推送镜像：

```bash
docker push registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

当前已推送过的镜像：

```text
registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

注意镜像名必须完整，不能写成 `registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast`，否则 Docker 会去拉不存在的 `latest`。

### Docker 部署

如果和 sub2api 在同一个 Docker network 中，可以直接使用 sub2api 容器服务名：

```bash
mkdir -p /opt/ai-fast-gateway/logs && chmod 777 /opt/ai-fast-gateway/logs

docker rm -f codex-fast-proxy-cc-ws 2>/dev/null || true

docker pull registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle

docker run -d \
  --name codex-fast-proxy-cc-ws \
  --network sub2api_sub2api-network \
  -p 8318:8317 \
  -v /opt/ai-fast-gateway/logs:/logs \
  -e LISTEN_ADDR=:8317 \
  -e UPSTREAM_URL=http://sub2api:8080 \
  -e FAST_MODE_ENABLED=true \
  -e LOG_FILE=/logs/ai-fast-gateway.log \
  -e LOG_MAX_SIZE_MB=20 \
  -e LOG_MAX_BACKUPS=5 \
  -e LOG_ROTATE_INTERVAL_MINUTES=0 \
  -e MAX_IDLE_CONNS=100 \
  -e MAX_IDLE_CONNS_PER_HOST=100 \
  -e CC_WS_BRIDGE_ENABLED=true \
  -e CC_WS_BRIDGE_UPSTREAM_PATH=/responses \
  -e CC_WS_BRIDGE_FALLBACK_HTTP=true \
  -e CC_WS_BRIDGE_DEBUG_FRAMES=false \
  -e CC_WS_BRIDGE_FIRST_EVENT_TIMEOUT_MS=15000 \
  -e CC_WS_BRIDGE_MAX_ATTEMPTS=1 \
  -e CC_WS_BRIDGE_MAX_REQUEST_BYTES=900000 \
  -e OPENAI_RESPONSES_WS_BRIDGE_ENABLED=true \
  -e OPENAI_RESPONSES_WS_BRIDGE_FALLBACK_HTTP=true \
  -e OPENAI_RESPONSES_WS_BRIDGE_MAX_ATTEMPTS=4 \
  -e OPENAI_RESPONSES_WS_BRIDGE_MAX_REQUEST_BYTES=900000 \
  -e CC_WS_SESSION_ROTATE_ENABLED=true \
  -e CC_WS_SESSION_ROTATE_THRESHOLD=2 \
  -e CC_WS_SESSION_ROTATE_STATE_FILE=/logs/ws-session-rotate.json \
  -e ADMIN_ENABLED=true \
  -e ADMIN_TOKEN='adminmima' \
  -e ADMIN_POLICY_STATE_FILE=/logs/runtime-policy.json \
  -e WS_DEBUG_PAYLOAD_BYTES=0 \
  -e CC_WS_POOL_MODE=client \
  -e CC_WS_POOL_MAX_CONNS_PER_CLIENT=20 \
  -e CC_WS_POOL_MAX_IDLE_PER_CLIENT=20 \
  -e CC_WS_POOL_IDLE_TTL_SECONDS=600 \
  -e CC_WS_POOL_ACQUIRE_TIMEOUT_MS=3000 \
  -e TZ=Asia/Shanghai \
  registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

检查：

```bash
curl -sS http://127.0.0.1:8318/healthz

curl -sS http://127.0.0.1:8318/admin/api/status \
  -H 'Authorization: Bearer adminmima'
```

管理台：

```text
http://服务器IP:8318/admin/
```

如果端口被占用：

```bash
docker ps --format 'table {{.ID}}\t{{.Names}}\t{{.Ports}}\t{{.Image}}'
ss -lntp | grep ':8318'
```

可以删除旧容器，或者换宿主机端口：

```bash
docker rm -f codex-fast-proxy-cc-ws
# 或把 -p 8318:8317 改成 -p 8319:8317
```

### 常用环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8317` | 网关监听地址。 |
| `UPSTREAM_URL` | `http://127.0.0.1:8080` | 上游服务地址。 |
| `FAST_MODE_ENABLED` | `true` | 是否注入 fast 参数和 Anthropic fast beta header。 |
| `LOG_FILE` | 二进制旁日志文件 | `stdout` 表示只写标准输出；文件路径会同时写 Docker stdout 和文件。 |
| `LOG_MAX_SIZE_MB` | `20` | 单个日志文件大小轮转阈值，`0` 关闭。 |
| `LOG_MAX_BACKUPS` | `5` | 最多保留多少个历史日志，`0` 不限制。 |
| `LOG_ROTATE_INTERVAL_MINUTES` | `0` | 定时轮转分钟数，`0` 关闭。 |
| `MAX_IDLE_CONNS` | `100` | HTTP transport 全局最大空闲连接。 |
| `MAX_IDLE_CONNS_PER_HOST` | `100` | 每个上游 host 最大空闲连接。 |
| `CC_WS_BRIDGE_ENABLED` | `false` | 是否把 Claude Code `/v1/messages` 转成 `/responses` WS。 |
| `CC_WS_BRIDGE_UPSTREAM_PATH` | `/responses` | Claude Code bridge 连接上游 WS 的路径。 |
| `CC_WS_BRIDGE_FALLBACK_HTTP` | `true` | WS bridge 失败且未写出客户端响应时是否回退 HTTP。 |
| `CC_WS_BRIDGE_DEBUG_FRAMES` | `false` | 是否记录上游 WS frame 类型。 |
| `CC_WS_BRIDGE_FIRST_EVENT_TIMEOUT_MS` | `15000` | 上游 WS 握手后等待第一条事件的超时。 |
| `CC_WS_BRIDGE_MAX_ATTEMPTS` | `5` | Claude Code bridge 最大尝试次数。 |
| `CC_WS_BRIDGE_MAX_REQUEST_BYTES` | `0` | Claude Code bridge 大请求绕过阈值，`0` 关闭。 |
| `OPENAI_RESPONSES_WS_BRIDGE_ENABLED` | `false` | 是否把 HTTP `/responses` 转成上游 `/responses` WS。 |
| `OPENAI_RESPONSES_WS_BRIDGE_FALLBACK_HTTP` | `true` | OpenAI `/responses` WS bridge 失败时是否回退 HTTP。 |
| `OPENAI_RESPONSES_WS_BRIDGE_MAX_ATTEMPTS` | `1` | OpenAI `/responses` WS bridge 最大尝试次数。 |
| `OPENAI_RESPONSES_WS_BRIDGE_MAX_REQUEST_BYTES` | `900000` | OpenAI `/responses` 大请求绕过阈值。 |
| `CC_WS_SESSION_ROTATE_ENABLED` | `false` | 是否开启首帧失败 session 自动轮换。 |
| `CC_WS_SESSION_ROTATE_THRESHOLD` | `2` | 连续失败多少次后轮换 session。 |
| `CC_WS_SESSION_ROTATE_STATE_FILE` | 空 | session 轮换映射持久化文件。 |
| `WS_DEBUG_PAYLOAD_BYTES` | `0` | WS JSON 摘要预览字节数，`0` 关闭。 |
| `CC_WS_POOL_MODE` | `client` | `client` 复用连接池，`request` 每请求新建 WS。 |
| `CC_WS_POOL_MAX_CONNS_PER_CLIENT` | `20` | 每个 client 最多上游 WS 连接数。 |
| `CC_WS_POOL_MAX_IDLE_PER_CLIENT` | `20` | 每个 client 最多保留 idle WS。 |
| `CC_WS_POOL_IDLE_TTL_SECONDS` | `600` | idle WS 保留时间。 |
| `CC_WS_POOL_ACQUIRE_TIMEOUT_MS` | `3000` | 等待可用 WS 连接的最长时间。 |
| `ADMIN_ENABLED` | `false` | 是否开启 `/admin/` 控制台。 |
| `ADMIN_TOKEN` | 空 | 管理台和管理 API token。 |
| `ADMIN_POLICY_STATE_FILE` | 空 | 运行时策略持久化文件。 |

### WebSocket 策略

普通 WS 代理会转发 WebSocket 帧，开启 fast 后会尝试对客户端发往上游的 JSON 文本帧注入 fast 参数。

Claude Code bridge 开启后：

```text
CC_WS_BRIDGE_ENABLED=true
```

`/v1/messages` 会被转换成上游 `/responses` WebSocket，再把上游 WS 事件转换回 Anthropic SSE。该模式适合让原本不支持 Responses WebSocket 的 Claude Code 链路复用上游 WS 能力。

重试只发生在还没有向客户端写出响应头和响应内容之前。例如上游 WS 握手成功但第一条事件前 EOF，可以安全重试；如果已经向客户端写出内容，就不会再切换链路。

大请求绕过：

```text
OPENAI_RESPONSES_WS_BRIDGE_MAX_REQUEST_BYTES=900000
CC_WS_BRIDGE_MAX_REQUEST_BYTES=900000
```

编码后的 `response.create` 超过阈值时跳过 WS bridge，直接走普通 HTTP，避免大包在上游 101 后立刻 EOF。

Session 轮换：

```text
CC_WS_SESSION_ROTATE_ENABLED=true
CC_WS_SESSION_ROTATE_THRESHOLD=2
CC_WS_SESSION_ROTATE_STATE_FILE=/logs/ws-session-rotate.json
```

网关会按“客户端身份 + 原始 session”记录连续首帧失败，达到阈值后生成替代 session，并替换 `prompt_cache_key`、`x-claude-code-session-id`、`x-codex-session-id`、`x-codex-window-id` 和 turn metadata。成功收到第一条上游事件后会清空失败计数。

连接池：

```text
CC_WS_POOL_MODE=client
CC_WS_POOL_MAX_CONNS_PER_CLIENT=20
CC_WS_POOL_MAX_IDLE_PER_CLIENT=20
```

同一个客户端身份共享上游 WS 池。每条 WS 同一时间只跑一个请求，结束后回池。不同 session 不混用同一条 WS，但共享 client 级连接上限。要退回旧逻辑：

```text
CC_WS_POOL_MODE=request
```

### Admin 控制台和日志接口

开启：

```text
ADMIN_ENABLED=true
ADMIN_TOKEN=change-me
ADMIN_POLICY_STATE_FILE=/logs/runtime-policy.json
```

页面：

```text
/admin/
```

主要 API：

```bash
curl -sS http://127.0.0.1:8318/admin/api/status \
  -H 'Authorization: Bearer change-me'

curl -sS 'http://127.0.0.1:8318/admin/api/logs?tail=200&grep=EOF' \
  -H 'Authorization: Bearer change-me'

curl -sS 'http://127.0.0.1:8318/admin/api/logs/download?tail=500&grep=bridge' \
  -H 'Authorization: Bearer change-me'

curl -sS http://127.0.0.1:8318/admin/api/pool \
  -H 'Authorization: Bearer change-me'
```

运行时修改策略：

```bash
curl -sS -X PATCH http://127.0.0.1:8318/admin/api/policy \
  -H 'Authorization: Bearer change-me' \
  -H 'Content-Type: application/json' \
  -d '{"fast_mode_enabled":true,"openai_responses_ws_enabled":true}'
```

注意：`ADMIN_POLICY_STATE_FILE` 存在时，文件里的运行时策略会覆盖环境变量。需要完全按环境变量启动时，可以先备份或删除该文件。

### 常见排查

端口占用：

```bash
docker ps --format 'table {{.ID}}\t{{.Names}}\t{{.Ports}}\t{{.Image}}'
ss -lntp | grep ':8318'
```

镜像名错误：

```text
错误：registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast:latest
正确：registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

WS 101 后 EOF：

- 看日志是否有 `responses ws bridge first event wait failed ... err=EOF`。
- 如果随后有 `upstream response ... status=200`，说明 WS 失败后 HTTP fallback 成功。
- 如果请求体很大并出现 `reason=large_request`，说明大请求绕过生效。

502：

- `responses websocket bridge upstream failed` 是网关 WS bridge 失败且没有 fallback。
- `upstream response ... status=502` 是上游服务返回的 502，需要查 sub2api 或上游账号状态。

Fast 开关不符合预期：

```bash
curl -sS http://127.0.0.1:8318/admin/api/status \
  -H 'Authorization: Bearer change-me' | grep fast_mode_enabled
```

如果环境变量是 `FAST_MODE_ENABLED=true` 但状态里是 `false`，通常是 `ADMIN_POLICY_STATE_FILE` 里的历史策略覆盖了环境变量。

### 注意事项

- 网关只负责中转、补参数、桥接和运行时策略，不负责账号额度、账号池调度或上游换号。
- 上游返回 usage limit、quota exhausted、429、502 等错误时，是否自动换号取决于 sub2api 或上游服务。
- 不要把 API key、OAuth token、真实上游密钥写进仓库。
- 文件日志建议映射 `/logs`，并设置 `LOG_MAX_SIZE_MB` 和 `LOG_MAX_BACKUPS`。

---

## English Version

AI Fast Gateway is an HTTP/WebSocket proxy placed between clients such as Codex, Claude Code, or ccswitch and an OpenAI-compatible upstream service. It does not replace the upstream service. It adds a controllable policy layer for fast-mode injection, `/responses` WebSocket bridging, large-request bypass, session rotation, WebSocket pooling, and operational diagnostics.

Typical path:

```text
Codex / Claude Code / ccswitch
  -> ai-fast-gateway
  -> sub2api / OpenAI-compatible upstream
```

Claude Code bridge path:

```text
Claude Code /v1/messages
  -> ai-fast-gateway
  -> upstream /responses WebSocket
  -> Anthropic SSE response
```

### Capabilities

- HTTP/SSE proxying to `UPSTREAM_URL`.
- Fast-mode injection when `FAST_MODE_ENABLED=true`.
- WebSocket proxying with compression extensions stripped.
- OpenAI HTTP `/responses` to upstream `/responses` WebSocket bridging.
- Claude Code `/v1/messages` to upstream `/responses` WebSocket bridging.
- Large request bypass to avoid repeated 101-then-EOF failures.
- Session rotation after repeated first-event EOF or timeout.
- Client/session scoped upstream WebSocket pooling.
- Built-in admin console for status, logs, runtime policy, session rotation, and pool snapshots.
- Log query and download APIs for troubleshooting.

### Fast Injection

OpenAI/Codex-style JSON `POST` bodies receive:

```json
{"service_tier":"fast"}
```

Anthropic/Claude-style `/v1/messages` JSON `POST` bodies receive:

```json
{"speed":"fast","service_tier":"fast"}
```

Anthropic requests also receive:

```text
anthropic-beta: fast-mode-2026-02-01
```

Disable it with:

```text
FAST_MODE_ENABLED=false
```

If `ADMIN_POLICY_STATE_FILE` is configured, saved runtime policy overrides environment variables. Check `/admin/` or `/admin/api/status` for the effective `fast_mode_enabled` value.

### Local Development

Requirements:

- Go 1.22+
- Node.js for `admin/app.js` syntax checks
- Docker for image packaging

Run checks:

```bash
go test -count=1 ./...
node --check admin/app.js
```

Build a local binary:

```bash
go build -o ai-fast-gateway .
```

Run locally:

```bash
./start.sh
```

Stop:

```bash
./stop.sh
```

Health check:

```bash
curl http://127.0.0.1:18317/healthz
```

Point your client base URL to:

```text
http://127.0.0.1:18317/
```

### Packaging

Binary package:

```bash
go build -trimpath -ldflags="-s -w" -o ai-fast-gateway .
```

The admin UI is embedded through `go:embed admin/*`, so the binary can serve `/admin/` without copying the `admin/` directory next to it.

Docker image:

```bash
docker build -t registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle .
docker push registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

Use the complete image name. `registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast` is not the same repository and will fail unless that repository exists.

### Docker Deployment

```bash
mkdir -p /opt/ai-fast-gateway/logs && chmod 777 /opt/ai-fast-gateway/logs

docker rm -f codex-fast-proxy-cc-ws 2>/dev/null || true

docker pull registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle

docker run -d \
  --name codex-fast-proxy-cc-ws \
  --network sub2api_sub2api-network \
  -p 8318:8317 \
  -v /opt/ai-fast-gateway/logs:/logs \
  -e LISTEN_ADDR=:8317 \
  -e UPSTREAM_URL=http://sub2api:8080 \
  -e FAST_MODE_ENABLED=true \
  -e LOG_FILE=/logs/ai-fast-gateway.log \
  -e LOG_MAX_SIZE_MB=20 \
  -e LOG_MAX_BACKUPS=5 \
  -e LOG_ROTATE_INTERVAL_MINUTES=0 \
  -e MAX_IDLE_CONNS=100 \
  -e MAX_IDLE_CONNS_PER_HOST=100 \
  -e CC_WS_BRIDGE_ENABLED=true \
  -e CC_WS_BRIDGE_UPSTREAM_PATH=/responses \
  -e CC_WS_BRIDGE_FALLBACK_HTTP=true \
  -e CC_WS_BRIDGE_DEBUG_FRAMES=false \
  -e CC_WS_BRIDGE_FIRST_EVENT_TIMEOUT_MS=15000 \
  -e CC_WS_BRIDGE_MAX_ATTEMPTS=1 \
  -e CC_WS_BRIDGE_MAX_REQUEST_BYTES=900000 \
  -e OPENAI_RESPONSES_WS_BRIDGE_ENABLED=true \
  -e OPENAI_RESPONSES_WS_BRIDGE_FALLBACK_HTTP=true \
  -e OPENAI_RESPONSES_WS_BRIDGE_MAX_ATTEMPTS=4 \
  -e OPENAI_RESPONSES_WS_BRIDGE_MAX_REQUEST_BYTES=900000 \
  -e CC_WS_SESSION_ROTATE_ENABLED=true \
  -e CC_WS_SESSION_ROTATE_THRESHOLD=2 \
  -e CC_WS_SESSION_ROTATE_STATE_FILE=/logs/ws-session-rotate.json \
  -e ADMIN_ENABLED=true \
  -e ADMIN_TOKEN='adminmima' \
  -e ADMIN_POLICY_STATE_FILE=/logs/runtime-policy.json \
  -e WS_DEBUG_PAYLOAD_BYTES=0 \
  -e CC_WS_POOL_MODE=client \
  -e CC_WS_POOL_MAX_CONNS_PER_CLIENT=20 \
  -e CC_WS_POOL_MAX_IDLE_PER_CLIENT=20 \
  -e CC_WS_POOL_IDLE_TTL_SECONDS=600 \
  -e CC_WS_POOL_ACQUIRE_TIMEOUT_MS=3000 \
  -e TZ=Asia/Shanghai \
  registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

Verify:

```bash
curl -sS http://127.0.0.1:8318/healthz

curl -sS http://127.0.0.1:8318/admin/api/status \
  -H 'Authorization: Bearer adminmima'
```

Admin console:

```text
http://SERVER_IP:8318/admin/
```

### Environment Variables

| Variable | Default | Description |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8317` | Gateway listen address. |
| `UPSTREAM_URL` | `http://127.0.0.1:8080` | Upstream base URL. |
| `FAST_MODE_ENABLED` | `true` | Enables fast field and Anthropic fast beta injection. |
| `LOG_FILE` | executable-local log file | Use `stdout` to disable file logging. |
| `LOG_MAX_SIZE_MB` | `20` | Size-based log rotation threshold. |
| `LOG_MAX_BACKUPS` | `5` | Maximum rotated log files to keep. |
| `LOG_ROTATE_INTERVAL_MINUTES` | `0` | Time-based log rotation interval. |
| `CC_WS_BRIDGE_ENABLED` | `false` | Enables Claude Code `/v1/messages` to `/responses` WS bridge. |
| `CC_WS_BRIDGE_FALLBACK_HTTP` | `true` | Falls back to normal HTTP before client response bytes are written. |
| `CC_WS_BRIDGE_MAX_REQUEST_BYTES` | `0` | Large-request bypass for Claude Code bridge. |
| `OPENAI_RESPONSES_WS_BRIDGE_ENABLED` | `false` | Enables HTTP `/responses` to upstream `/responses` WS bridge. |
| `OPENAI_RESPONSES_WS_BRIDGE_MAX_REQUEST_BYTES` | `900000` | Large-request bypass for OpenAI `/responses` bridge. |
| `CC_WS_SESSION_ROTATE_ENABLED` | `false` | Enables automatic session rotation after first-event failures. |
| `CC_WS_POOL_MODE` | `client` | `client` reuses upstream WS connections; `request` opens one WS per request. |
| `ADMIN_ENABLED` | `false` | Enables `/admin/`. |
| `ADMIN_TOKEN` | empty | Bearer token for admin APIs. |
| `ADMIN_POLICY_STATE_FILE` | empty | Persists runtime policy across restarts. |

See the Chinese table above for the full variable list and exact defaults.

### Admin APIs

```bash
curl -sS http://127.0.0.1:8318/admin/api/status \
  -H 'Authorization: Bearer change-me'

curl -sS 'http://127.0.0.1:8318/admin/api/logs?tail=200&grep=EOF' \
  -H 'Authorization: Bearer change-me'

curl -sS http://127.0.0.1:8318/admin/api/pool \
  -H 'Authorization: Bearer change-me'

curl -sS -X PATCH http://127.0.0.1:8318/admin/api/policy \
  -H 'Authorization: Bearer change-me' \
  -H 'Content-Type: application/json' \
  -d '{"fast_mode_enabled":true}'
```

### Troubleshooting

- Port already allocated: check `docker ps` and `ss -lntp | grep ':8318'`.
- Image pull denied: verify the full image name and tag.
- 101 then EOF: check `responses ws bridge first event wait failed`; if a later `status=200` exists, HTTP fallback succeeded.
- Large request bypass: `reason=large_request` means the WS bridge was intentionally skipped.
- Upstream 502: `upstream response ... status=502` means the upstream service returned 502.
- Fast mode mismatch: `ADMIN_POLICY_STATE_FILE` can override `FAST_MODE_ENABLED`.
