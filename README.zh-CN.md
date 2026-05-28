# AI Fast Gateway 中文说明

本文档中文在前，英文版本在后。完整双语主文档见 [README.md](README.md)。

## 中文说明

AI Fast Gateway 是一个轻量级 HTTP/WebSocket 中转网关，用来把 Codex、Claude Code、ccswitch 等客户端请求转发到上游服务，同时按配置补齐 fast mode 相关参数。`FAST_MODE_ENABLED` 默认开启，关闭后不会再写入 fast 参数和 Anthropic fast beta header。它还可以把 Claude Code 的 `/v1/messages` 请求转换成上游 `/responses` WebSocket 请求，用于让原本走 Anthropic Messages/SSE 的 Claude Code 链路接入支持 Responses WebSocket 的上游。

典型链路：

```text
Codex / Claude Code / ccswitch -> ai-fast-gateway -> sub2api / upstream
Claude Code /v1/messages -> ai-fast-gateway -> upstream /responses WebSocket
```

## 能做什么

- 普通 HTTP/SSE 代理：把请求转发到 `UPSTREAM_URL`。
- Fast 参数注入：OpenAI 请求补 `service_tier=fast`，Anthropic 请求补 `speed=fast`、`service_tier=fast` 和 fast beta header。
- 普通 WebSocket 代理：转发 WS Upgrade 请求，并可对客户端发往上游的 JSON 文本帧补 fast 参数。
- OpenAI `/responses` WS bridge：把 HTTP `POST /responses` 转成上游 `/responses` WebSocket。
- Claude Code bridge：把 `/v1/messages` 转成上游 `/responses` WebSocket，再转换回 Anthropic SSE。
- 大请求绕过：超过阈值时跳过 WS bridge，直接走 HTTP，降低 101 后 EOF 的概率。
- Session 轮换：连续首帧 EOF/超时后替换 sticky session id。
- WS 连接池：按 client 和 session 复用上游 WebSocket。
- Admin 控制台：查看状态、策略、日志、session 轮换和连接池。
- 日志查询接口：支持 tail、grep 和下载，方便人工或 AI 排查。

## Fast 注入

OpenAI / Codex 风格的 JSON `POST` 请求会自动写入：

```json
{"service_tier":"fast"}
```

Anthropic / Claude 风格的 `/v1/messages` JSON `POST` 请求会自动写入：

```json
{"speed":"fast","service_tier":"fast"}
```

Claude 风格请求还会合并写入 beta header：

```text
anthropic-beta: fast-mode-2026-02-01
```

关闭：

```text
FAST_MODE_ENABLED=false
```

如果配置了 `ADMIN_POLICY_STATE_FILE`，管理台保存的运行时策略会覆盖环境变量。部署后应以 `/admin/` 策略页或 `/admin/api/status` 返回的 `fast_mode_enabled` 为准。

## 构建和检查

检查：

```bash
go test -count=1 ./...
node --check admin/app.js
```

构建本地二进制：

```bash
go build -o ai-fast-gateway .
```

构建发布二进制：

```bash
go build -trimpath -ldflags="-s -w" -o ai-fast-gateway .
```

Admin 页面通过 `go:embed admin/*` 嵌入到二进制中，发布单个二进制即可访问 `/admin/`，不需要额外复制 `admin/` 目录。

构建并推送 Docker 镜像：

```bash
docker build -t registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle .
docker push registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

镜像名必须完整，例如：

```text
registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

不要写成 `registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast`，否则 Docker 会尝试拉不存在的 `latest`。

## 本地运行

```bash
./start.sh
```

停止：

```bash
./stop.sh
```

默认参数：

```text
LISTEN_ADDR=127.0.0.1:18317
UPSTREAM_URL=http://127.0.0.1:8080
FAST_MODE_ENABLED=true
LOG_FILE=./ai-fast-gateway.log
LOG_MAX_SIZE_MB=20
LOG_MAX_BACKUPS=5
LOG_ROTATE_INTERVAL_MINUTES=0
```

健康检查：

```bash
curl http://127.0.0.1:18317/healthz
```

客户端 base URL 指向：

```text
http://127.0.0.1:18317/
```

## Docker 部署

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

## 常用环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `LISTEN_ADDR` | `:8317` | 网关监听地址。 |
| `UPSTREAM_URL` | `http://127.0.0.1:8080` | 上游服务地址。 |
| `FAST_MODE_ENABLED` | `true` | 是否注入 fast 参数和 Anthropic fast beta header。 |
| `LOG_FILE` | 二进制旁日志文件 | `stdout` 表示只写标准输出。 |
| `LOG_MAX_SIZE_MB` | `20` | 单个日志文件大小轮转阈值，`0` 关闭。 |
| `LOG_MAX_BACKUPS` | `5` | 最多保留多少个历史日志，`0` 不限制。 |
| `CC_WS_BRIDGE_ENABLED` | `false` | 是否把 Claude Code `/v1/messages` 转成 `/responses` WS。 |
| `CC_WS_BRIDGE_MAX_ATTEMPTS` | `5` | Claude Code bridge 最大尝试次数。 |
| `CC_WS_BRIDGE_MAX_REQUEST_BYTES` | `0` | Claude Code bridge 大请求绕过阈值，`0` 关闭。 |
| `OPENAI_RESPONSES_WS_BRIDGE_ENABLED` | `false` | 是否把 HTTP `/responses` 转成上游 `/responses` WS。 |
| `OPENAI_RESPONSES_WS_BRIDGE_MAX_ATTEMPTS` | `1` | OpenAI `/responses` WS bridge 最大尝试次数。 |
| `OPENAI_RESPONSES_WS_BRIDGE_MAX_REQUEST_BYTES` | `900000` | OpenAI `/responses` 大请求绕过阈值。 |
| `CC_WS_SESSION_ROTATE_ENABLED` | `false` | 是否开启首帧失败 session 自动轮换。 |
| `CC_WS_SESSION_ROTATE_STATE_FILE` | 空 | session 轮换映射持久化文件。 |
| `CC_WS_POOL_MODE` | `client` | `client` 复用连接池，`request` 每请求新建 WS。 |
| `ADMIN_ENABLED` | `false` | 是否开启 `/admin/` 控制台。 |
| `ADMIN_TOKEN` | 空 | 管理台和管理 API token。 |
| `ADMIN_POLICY_STATE_FILE` | 空 | 运行时策略持久化文件。 |

完整变量表见 [README.md](README.md)。

## WebSocket 策略

普通 WS 代理模式下，网关会转发 WebSocket 帧，并尝试对客户端发往上游的 JSON 文本帧注入 fast 参数。

Claude Code bridge：

```text
CC_WS_BRIDGE_ENABLED=true
```

开启后，Claude Code 的 `/v1/messages` 请求会被转换成上游 `/responses` WebSocket 请求，再把上游 WS 事件转换回 Anthropic SSE 响应。这个模式适合实验和调试，生产使用前建议先压测和观察日志。

大请求绕过：

```text
OPENAI_RESPONSES_WS_BRIDGE_MAX_REQUEST_BYTES=900000
CC_WS_BRIDGE_MAX_REQUEST_BYTES=900000
```

编码后的 `response.create` 超过阈值时，网关会跳过 WS bridge，直接走普通 HTTP `/responses` 代理。

Session 轮换：

```text
CC_WS_SESSION_ROTATE_ENABLED=true
CC_WS_SESSION_ROTATE_THRESHOLD=2
CC_WS_SESSION_ROTATE_STATE_FILE=/logs/ws-session-rotate.json
```

开启后，网关会按“客户端身份 + 原始 session”记录连续首帧失败次数，达到阈值后生成一个替代 session，并把后续请求中的 `prompt_cache_key`、`x-claude-code-session-id`、`x-codex-session-id`、`x-codex-window-id` 和 turn metadata 一起改成替代值。

连接池：

```text
CC_WS_POOL_MODE=client
CC_WS_POOL_MAX_CONNS_PER_CLIENT=20
CC_WS_POOL_MAX_IDLE_PER_CLIENT=20
```

同一个客户端身份共享一个最多 20 条上游 WS 的连接池；每条 WS 同一时间只跑一个请求，收到终止事件后回池。要退回旧逻辑：

```text
CC_WS_POOL_MODE=request
```

## Admin 控制台和日志接口

开启：

```text
ADMIN_ENABLED=true
ADMIN_TOKEN=change-me
ADMIN_POLICY_STATE_FILE=/logs/runtime-policy.json
```

常用 API：

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

`ADMIN_POLICY_STATE_FILE` 为空时，页面保存的策略只在当前进程内生效；配置到 `/logs/runtime-policy.json` 后会写入文件，容器重启后自动恢复。注意该文件里的运行时策略会覆盖环境变量。

## 常见排查

- 端口占用：用 `docker ps` 和 `ss -lntp | grep ':8318'` 查。
- 镜像拉取失败：检查镜像名是否完整。
- WS 101 后 EOF：看日志是否有 `first event wait failed ... err=EOF`。如果随后有 `status=200`，说明 HTTP fallback 成功。
- 大请求绕过：`reason=large_request` 表示跳过 WS bridge 是预期行为。
- 502：`upstream response ... status=502` 表示上游返回 502，需要查 sub2api 或上游账号状态。
- Fast 开关不符合预期：检查 `/admin/api/status` 里的 `fast_mode_enabled`，通常是 `ADMIN_POLICY_STATE_FILE` 覆盖了环境变量。

## 注意事项

- 网关只负责中转、补参数、桥接和运行时策略，不负责账号额度、账号池调度或上游换号。
- 上游返回 usage limit、quota exhausted、429、502 等错误时，是否自动换号取决于 sub2api 或上游服务。
- 不要把 API key、OAuth token、真实上游密钥写进仓库。
- 文件日志建议映射 `/logs`，并设置 `LOG_MAX_SIZE_MB` 和 `LOG_MAX_BACKUPS`。

---

## English Version

AI Fast Gateway is an HTTP/WebSocket proxy between Codex, Claude Code, ccswitch, and an OpenAI-compatible upstream service. It adds a controllable runtime policy layer: fast-mode injection, `/responses` WebSocket bridging, large-request bypass, session rotation, WebSocket pooling, and admin diagnostics.

Main capabilities:

- Proxy normal HTTP/SSE requests to `UPSTREAM_URL`.
- Inject fast fields when `FAST_MODE_ENABLED=true`.
- Proxy WebSocket upgrade requests and optionally inject fast fields into JSON text frames.
- Bridge HTTP `POST /responses` to upstream `/responses` WebSocket.
- Bridge Claude Code `/v1/messages` to upstream `/responses` WebSocket and convert events back to Anthropic SSE.
- Skip WS bridge for large `response.create` payloads to reduce 101-then-EOF failures.
- Rotate sticky sessions after repeated first-event EOF or timeout.
- Reuse upstream WebSocket connections by client and session.
- Provide `/admin/` for status, logs, policies, session rotation, and pool snapshots.

Build and test:

```bash
go test -count=1 ./...
node --check admin/app.js
go build -trimpath -ldflags="-s -w" -o ai-fast-gateway .
```

Docker package:

```bash
docker build -t registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle .
docker push registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

Run:

```bash
docker run -d \
  --name codex-fast-proxy-cc-ws \
  --network sub2api_sub2api-network \
  -p 8318:8317 \
  -v /opt/ai-fast-gateway/logs:/logs \
  -e LISTEN_ADDR=:8317 \
  -e UPSTREAM_URL=http://sub2api:8080 \
  -e FAST_MODE_ENABLED=true \
  -e ADMIN_ENABLED=true \
  -e ADMIN_TOKEN='adminmima' \
  -e ADMIN_POLICY_STATE_FILE=/logs/runtime-policy.json \
  registry.cn-guangzhou.aliyuncs.com/x_ct/codex-fast-proxy:20260528-fast-toggle
```

Verify:

```bash
curl -sS http://127.0.0.1:8318/healthz
curl -sS http://127.0.0.1:8318/admin/api/status -H 'Authorization: Bearer adminmima'
```

Important note: when `ADMIN_POLICY_STATE_FILE` exists, saved runtime policy overrides environment variables. Check `/admin/` or `/admin/api/status` for the effective values.
