FROM golang:1.22-alpine AS builder

WORKDIR /src
COPY *.go .
RUN go build -trimpath -ldflags="-s -w" -o /out/ai-fast-gateway main.go bridge.go

FROM alpine:3.20

RUN adduser -D -H app
USER app

COPY --from=builder /out/ai-fast-gateway /usr/local/bin/ai-fast-gateway

ENV LISTEN_ADDR=:8317
ENV UPSTREAM_URL=http://127.0.0.1:8080
ENV LOG_FILE=stdout
ENV MAX_IDLE_CONNS=100
ENV MAX_IDLE_CONNS_PER_HOST=100
ENV SERVICE_TIER=fast
ENV FORCE_SERVICE_TIER=false
ENV CC_WS_BRIDGE_ENABLED=false
ENV CC_WS_BRIDGE_UPSTREAM_PATH=/responses
ENV CC_WS_BRIDGE_FALLBACK_HTTP=true
ENV CC_WS_BRIDGE_DEBUG_FRAMES=false
ENV CC_WS_BRIDGE_FIRST_EVENT_TIMEOUT_MS=15000
ENV WS_DEBUG_PAYLOAD_BYTES=0
ENV TZ=Asia/Shanghai

EXPOSE 8317
ENTRYPOINT ["ai-fast-gateway"]
