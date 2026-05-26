# AI Fast Gateway 中文说明

AI Fast Gateway 是一个轻量级 HTTP/WebSocket 中转网关，用来把 Codex、Claude Code、ccswitch 等客户端请求转发到上游服务，同时强制补齐 fast mode 相关参数。它还可以把 Claude Code 的 `/v1/messages` 请求转换成上游 `/responses` WebSocket 请求，用于让原本走 Anthropic Messages/SSE 的 Claude Code 链路接入支持 Responses WebSocket 的上游。

它适合放在客户端和 sub2api / OpenAI 兼容服务之间，既可以做普通 HTTP/SSE fast 参数注入，也可以在需要时做 Claude Code 到 Responses WebSocket 的桥接：

```text
Codex / Claude Code / ccswitch -> ai-fast-gateway -> sub2api / upstream
Claude Code /v1/messages -> ai-fast-gateway -> upstream /responses WebSocket
```

## 功能

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

WebSocket Upgrade 请求也支持。网关会去掉 WebSocket 压缩扩展，并在客户端发往上游的 JSON 文本帧中补齐 `service_tier:"fast"`，例如 `response.create` 这类消息。

Claude Code 的 `/v1/messages` 还可以可选桥接到上游 `/responses` WebSocket。该能力默认关闭；如果 WS 连接在写回客户端前失败，会回退到普通 HTTP 代理路径。

## 构建

```bash
go build -o ai-fast-gateway .
```

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
LOG_FILE=./ai-fast-gateway.log
LOG_MAX_SIZE_MB=20
LOG_MAX_BACKUPS=5
LOG_ROTATE_INTERVAL_MINUTES=0
```

自定义上游：

```bash
LISTEN_ADDR=127.0.0.1:18317 \
UPSTREAM_URL=http://127.0.0.1:8080 \
./start.sh
```

健康检查：

```bash
curl http://127.0.0.1:18317/healthz
```

然后把 Codex、ccswitch 或 Claude Code 的 base URL 指向：

```text
http://127.0.0.1:18317/
```

## Docker 部署

如果和 sub2api 在同一个 Docker network 中，可以直接使用 sub2api 容器服务名：

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
  ghcr.io/your-org/ai-fast-gateway:latest
```

这里的 `http://sub2api:8080` 是 Docker 内部网络地址，不是宿主机暴露出来的外部端口。

日志会同时输出到 Docker stdout 和 `LOG_FILE` 指定的文件。上面的 `-v /opt/ai-fast-gateway/logs:/logs` 会把容器内日志目录映射到服务器 `/opt/ai-fast-gateway/logs`，便于下载和排查。

日志轮转规则：

```text
LOG_MAX_SIZE_MB=20              # 单个日志超过 20MB 后切分；设为 0 可关闭按大小切分
LOG_MAX_BACKUPS=5               # 最多保留 5 个历史日志；设为 0 表示不自动删除历史日志
LOG_ROTATE_INTERVAL_MINUTES=0   # 按分钟定时切分；0 表示关闭。比如 60 表示每小时切一次
```

切分后的文件名类似：

```text
ai-fast-gateway.log
ai-fast-gateway-20260526-173538.log
ai-fast-gateway-20260526-180000.log
```

## 常用环境变量

```text
LISTEN_ADDR=:8317
UPSTREAM_URL=http://127.0.0.1:8080
LOG_FILE=stdout
LOG_MAX_SIZE_MB=20
LOG_MAX_BACKUPS=5
LOG_ROTATE_INTERVAL_MINUTES=0
MAX_IDLE_CONNS=100
MAX_IDLE_CONNS_PER_HOST=100
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

## WebSocket 说明

普通 WS 代理模式下，网关会转发 WebSocket 帧，并尝试对客户端发往上游的 JSON 文本帧注入 fast 参数。

Claude Code WS bridge 是额外能力：

```text
CC_WS_BRIDGE_ENABLED=true
```

开启后，Claude Code 的 `/v1/messages` 请求会被转换成上游 `/responses` WebSocket 请求，再把上游 WS 事件转换回 Anthropic SSE 响应。这个模式适合实验和调试，生产使用前建议先压测和观察日志。

默认 WS 复用策略是 `client`：

```text
CC_WS_POOL_MODE=client
CC_WS_POOL_MAX_CONNS_PER_CLIENT=20
CC_WS_POOL_MAX_IDLE_PER_CLIENT=20
CC_WS_POOL_IDLE_TTL_SECONDS=600
CC_WS_POOL_ACQUIRE_TIMEOUT_MS=3000
```

含义是：同一个客户端身份共享一个最多 20 条上游 WS 的连接池；每条 WS 同一时间只跑一个请求，收到终止事件后回池，后续同 session 请求可以串行复用这条 WS。客户端身份由请求 IP、认证头哈希和 User-Agent 哈希组成；session 由 `x-claude-code-session-id` 或 `prompt_cache_key` 识别。不同 session 不会混用同一条 WS，但会共享这个客户端池的 20 条连接上限。

如果要退回旧逻辑，每个请求新建并关闭一条 WS：

```text
CC_WS_POOL_MODE=request
```

## 注意事项

- 网关只负责中转和补参数，不负责账号额度、账号池调度或上游重试。
- 如果上游账号返回 usage limit、quota exhausted、429 等错误，是否自动换号取决于 sub2api 或上游服务本身。
- 不建议把真实上游地址、API key、OAuth token 写死进仓库，使用环境变量注入。
- 如果用文件日志，建议把容器内 `/logs` 映射到服务器目录，并配置 `LOG_MAX_SIZE_MB` 和 `LOG_MAX_BACKUPS` 控制大小。
