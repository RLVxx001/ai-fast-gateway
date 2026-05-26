package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata"
)

const (
	defaultListenAddr  = ":8317"
	defaultUpstreamURL = "http://127.0.0.1:8080"
	openAITier         = "fast"
	anthropicSpeed     = "fast"
	anthropicFastBeta  = "fast-mode-2026-02-01"
	openAIWSBeta       = "responses_websockets=2026-02-06"
	maxWSFramePayload  = 64 << 20
)

type config struct {
	listenAddr          string
	upstream            *url.URL
	logFile             string
	maxIdleConns        int
	maxIdleConnsPerHost int
	ccWSBridgeEnabled   bool
	ccWSBridgePath      string
	ccWSBridgeFallback  bool
	ccWSBridgeDebug     bool
	ccWSFirstEventWait  time.Duration
	wsDebugPayloadBytes int
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config error: %v", err)
	}
	if err := configureLogger(cfg); err != nil {
		log.Fatalf("log setup error: %v", err)
	}

	proxy := &proxyServer{
		cfg: cfg,
		client: &http.Client{
			Timeout: 0,
			Transport: &http.Transport{
				Proxy: http.ProxyFromEnvironment,
				DialContext: (&net.Dialer{
					Timeout:   30 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				ForceAttemptHTTP2:     true,
				MaxIdleConns:          cfg.maxIdleConns,
				MaxIdleConnsPerHost:   cfg.maxIdleConnsPerHost,
				IdleConnTimeout:       90 * time.Second,
				TLSHandshakeTimeout:   10 * time.Second,
				ExpectContinueTimeout: 1 * time.Second,
				DisableCompression:    true,
			},
		},
	}

	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           proxy,
		ReadHeaderTimeout: 30 * time.Second,
	}

	log.Printf("ai-fast-gateway listening on %s, upstream=%s, openai_service_tier=%s, anthropic_speed=%s, anthropic_beta=%s",
		cfg.listenAddr, cfg.upstream.String(), openAITier, anthropicSpeed, anthropicFastBeta)
	log.Printf("transport config: max_idle_conns=%d, max_idle_conns_per_host=%d, disable_compression=true",
		cfg.maxIdleConns, cfg.maxIdleConnsPerHost)
	log.Printf("cc websocket bridge: enabled=%v, path=%s, fallback_http=%v, debug_frames=%v",
		cfg.ccWSBridgeEnabled, cfg.ccWSBridgePath, cfg.ccWSBridgeFallback, cfg.ccWSBridgeDebug)
	log.Printf("cc websocket bridge timeout: first_event_wait=%s", cfg.ccWSFirstEventWait)
	log.Printf("websocket debug: payload_preview_bytes=%d", cfg.wsDebugPayloadBytes)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server error: %v", err)
	}
}

func loadConfig() (config, error) {
	listenAddr := flag.String("listen", envOrDefault("LISTEN_ADDR", defaultListenAddr), "listen address, for example :8317 or 127.0.0.1:8317")
	upstreamRaw := flag.String("upstream", envOrDefault("UPSTREAM_URL", defaultUpstreamURL), "upstream base URL")
	logFile := flag.String("log-file", envOrDefault("LOG_FILE", defaultLogFile()), "log file path")
	maxIdleConns := flag.Int("max-idle-conns", envIntOrDefault("MAX_IDLE_CONNS", 100), "maximum idle upstream connections across all hosts")
	maxIdleConnsPerHost := flag.Int("max-idle-conns-per-host", envIntOrDefault("MAX_IDLE_CONNS_PER_HOST", 100), "maximum idle upstream connections per host")
	ccWSBridgeEnabled := flag.Bool("cc-ws-bridge-enabled", envBoolOrDefault("CC_WS_BRIDGE_ENABLED", false), "bridge Claude Code /v1/messages to upstream /responses WebSocket")
	ccWSBridgePath := flag.String("cc-ws-bridge-path", envOrDefault("CC_WS_BRIDGE_UPSTREAM_PATH", "/responses"), "upstream WebSocket path used by Claude Code bridge")
	ccWSBridgeFallback := flag.Bool("cc-ws-bridge-fallback-http", envBoolOrDefault("CC_WS_BRIDGE_FALLBACK_HTTP", true), "fall back to normal HTTP proxy when WS bridge fails before streaming")
	ccWSBridgeDebug := flag.Bool("cc-ws-bridge-debug-frames", envBoolOrDefault("CC_WS_BRIDGE_DEBUG_FRAMES", false), "log upstream WS frame event types for bridge debugging")
	ccWSFirstEventWaitMS := flag.Int("cc-ws-bridge-first-event-timeout-ms", envIntOrDefault("CC_WS_BRIDGE_FIRST_EVENT_TIMEOUT_MS", 15000), "maximum time to wait for first upstream WS event before fallback")
	wsDebugPayloadBytes := flag.Int("ws-debug-payload-bytes", envIntOrDefault("WS_DEBUG_PAYLOAD_BYTES", 0), "optional redacted JSON payload preview bytes for websocket debugging")
	flag.Parse()

	upstream, err := url.Parse(strings.TrimSpace(*upstreamRaw))
	if err != nil {
		return config{}, err
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return config{}, errors.New("UPSTREAM_URL/upstream must include scheme and host")
	}
	return config{
		listenAddr:          strings.TrimSpace(*listenAddr),
		upstream:            upstream,
		logFile:             strings.TrimSpace(*logFile),
		maxIdleConns:        positiveOrDefault(*maxIdleConns, 100),
		maxIdleConnsPerHost: positiveOrDefault(*maxIdleConnsPerHost, 100),
		ccWSBridgeEnabled:   *ccWSBridgeEnabled,
		ccWSBridgePath:      normalizePath(*ccWSBridgePath, "/responses"),
		ccWSBridgeFallback:  *ccWSBridgeFallback,
		ccWSBridgeDebug:     *ccWSBridgeDebug,
		ccWSFirstEventWait:  time.Duration(positiveOrDefault(*ccWSFirstEventWaitMS, 15000)) * time.Millisecond,
		wsDebugPayloadBytes: maxInt(*wsDebugPayloadBytes, 0),
	}, nil
}

func defaultLogFile() string {
	executable, err := os.Executable()
	if err != nil {
		return "ai-fast-gateway.log"
	}
	return filepath.Join(filepath.Dir(executable), "ai-fast-gateway.log")
}

func configureLogger(cfg config) error {
	if cfg.logFile == "-" || strings.EqualFold(cfg.logFile, "stdout") {
		log.SetOutput(os.Stdout)
		log.SetFlags(log.LstdFlags)
		log.Printf("log_file=stdout")
		return nil
	}
	if cfg.logFile == "" {
		cfg.logFile = defaultLogFile()
	}
	if err := os.MkdirAll(filepath.Dir(cfg.logFile), 0755); err != nil {
		return err
	}
	file, err := os.OpenFile(cfg.logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	log.SetOutput(io.MultiWriter(os.Stdout, file))
	log.SetFlags(log.LstdFlags)
	log.Printf("log_file=%s", cfg.logFile)
	return nil
}

func envOrDefault(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func envIntOrDefault(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return positiveOrDefault(parsed, fallback)
}

func envBoolOrDefault(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		switch strings.ToLower(value) {
		case "1", "yes", "y", "on", "enabled":
			return true
		case "0", "no", "n", "off", "disabled":
			return false
		default:
			return fallback
		}
	}
	return parsed
}

func normalizePath(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	return "/" + strings.TrimLeft(value, "/")
}

func positiveOrDefault(value int, fallback int) int {
	if value <= 0 {
		return fallback
	}
	return value
}

func maxInt(value int, minimum int) int {
	if value < minimum {
		return minimum
	}
	return value
}

type proxyServer struct {
	cfg    config
	client *http.Client
}

func (p *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}

	if isWebSocketUpgrade(r) {
		p.serveWebSocket(w, r)
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	_ = r.Body.Close()

	outBody := body
	pathKind := requestPathKind(r.URL.Path)
	if p.cfg.ccWSBridgeEnabled && pathKind == "anthropic" && r.Method == http.MethodPost {
		if p.serveClaudeMessagesViaResponsesWS(w, r, body) {
			return
		}
	}
	if shouldInjectTier(r, body) {
		hadFastBefore := bodyHasFastField(body, pathKind)
		outBody, err = injectFastField(body, pathKind)
		if err != nil {
			http.Error(w, "failed to inject fast field", http.StatusBadRequest)
			return
		}
		log.Printf("fast field processed: method=%s path=%s kind=%s had_fast_field=%v changed=%v",
			r.Method, r.URL.RequestURI(), pathKind, hadFastBefore, !bytes.Equal(outBody, body))
	}

	upstreamURL := p.buildUpstreamURL(r)
	req, err := http.NewRequestWithContext(contextWithClientCancel(r), r.Method, upstreamURL, bytes.NewReader(outBody))
	if err != nil {
		http.Error(w, "failed to create upstream request", http.StatusInternalServerError)
		return
	}
	copyHeaders(req.Header, r.Header)
	if pathKind == "anthropic" {
		ensureAnthropicBeta(req.Header)
	}
	req.Host = p.cfg.upstream.Host
	req.ContentLength = int64(len(outBody))
	req.Header.Del("Content-Length")
	req.Header.Set("Accept-Encoding", "identity")
	logOutboundRequest(r, req, outBody, pathKind)

	resp, err := p.client.Do(req)
	if err != nil {
		log.Printf("upstream request failed: method=%s path=%s err=%v", r.Method, r.URL.Path, err)
		http.Error(w, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	log.Printf("upstream response: method=%s path=%s kind=%s status=%d fast_headers=%q",
		r.Method, r.URL.RequestURI(), pathKind, resp.StatusCode, fastRelatedHeaders(resp.Header))

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	writer := responseStreamWriter(w)
	if _, err := io.Copy(writer, resp.Body); err != nil {
		log.Printf("copy response failed: method=%s path=%s err=%v", r.Method, r.URL.Path, err)
	}
}

func responseStreamWriter(w http.ResponseWriter) io.Writer {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return w
	}
	return flushWriter{writer: w, flusher: flusher}
}

type flushWriter struct {
	writer  io.Writer
	flusher http.Flusher
}

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.writer.Write(p)
	if n > 0 {
		w.flusher.Flush()
	}
	return n, err
}

func contextWithClientCancel(r *http.Request) context.Context {
	return r.Context()
}

func (p *proxyServer) buildUpstreamURL(r *http.Request) string {
	target := *p.cfg.upstream
	basePath := strings.TrimRight(target.Path, "/")
	reqPath := "/" + strings.TrimLeft(r.URL.Path, "/")
	if basePath == "" || basePath == "/" {
		target.Path = reqPath
	} else {
		target.Path = basePath + reqPath
	}
	target.RawQuery = r.URL.RawQuery
	return target.String()
}

func isWebSocketUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket") && headerHasToken(r.Header, "Connection", "upgrade")
}

func headerHasToken(header http.Header, key string, token string) bool {
	for _, value := range header.Values(key) {
		for _, part := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

func (p *proxyServer) serveWebSocket(w http.ResponseWriter, r *http.Request) {
	pathKind := requestPathKind(r.URL.Path)
	upstreamURL, err := url.Parse(p.buildUpstreamURL(r))
	if err != nil {
		http.Error(w, "failed to build upstream websocket URL", http.StatusInternalServerError)
		return
	}

	upstreamConn, upstreamReader, resp, err := p.openUpstreamWebSocket(r, upstreamURL, pathKind)
	if err != nil {
		log.Printf("websocket upstream handshake failed: method=%s path=%s err=%v", r.Method, r.URL.RequestURI(), err)
		http.Error(w, "websocket upstream handshake failed", http.StatusBadGateway)
		return
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer upstreamConn.Close()
		copyHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		if _, err := io.Copy(responseStreamWriter(w), resp.Body); err != nil {
			log.Printf("websocket non-101 response copy failed: method=%s path=%s err=%v", r.Method, r.URL.Path, err)
		}
		return
	}

	hijacker, ok := w.(http.Hijacker)
	if !ok {
		_ = upstreamConn.Close()
		http.Error(w, "websocket hijack unsupported", http.StatusInternalServerError)
		return
	}
	clientConn, clientRW, err := hijacker.Hijack()
	if err != nil {
		_ = upstreamConn.Close()
		log.Printf("websocket client hijack failed: method=%s path=%s err=%v", r.Method, r.URL.RequestURI(), err)
		return
	}

	if err := writeHTTPResponseHead(clientConn, resp); err != nil {
		_ = upstreamConn.Close()
		_ = clientConn.Close()
		log.Printf("websocket handshake response write failed: method=%s path=%s err=%v", r.Method, r.URL.RequestURI(), err)
		return
	}

	log.Printf("websocket tunnel established: method=%s path=%s kind=%s upstream=%s",
		r.Method, r.URL.RequestURI(), pathKind, upstreamURL.String())

	done := make(chan struct{}, 2)
	go func() {
		if err := p.pipeWebSocketFrames("client_to_upstream", clientRW.Reader, upstreamConn, true, true, r.URL.RequestURI(), pathKind); err != nil {
			log.Printf("websocket client_to_upstream ended: path=%s err=%v", r.URL.RequestURI(), err)
		}
		_ = upstreamConn.Close()
		_ = clientConn.Close()
		done <- struct{}{}
	}()
	go func() {
		if err := p.pipeWebSocketFrames("upstream_to_client", upstreamReader, clientConn, false, false, r.URL.RequestURI(), pathKind); err != nil {
			log.Printf("websocket upstream_to_client ended: path=%s err=%v", r.URL.RequestURI(), err)
		}
		_ = upstreamConn.Close()
		_ = clientConn.Close()
		done <- struct{}{}
	}()
	<-done
}

func (p *proxyServer) openUpstreamWebSocket(in *http.Request, upstreamURL *url.URL, pathKind string) (net.Conn, *bufio.Reader, *http.Response, error) {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	hostPort := hostWithDefaultPort(upstreamURL)
	var conn net.Conn
	var err error
	switch upstreamURL.Scheme {
	case "https", "wss":
		serverName := upstreamURL.Hostname()
		conn, err = tls.DialWithDialer(dialer, "tcp", hostPort, &tls.Config{ServerName: serverName})
	default:
		conn, err = dialer.DialContext(in.Context(), "tcp", hostPort)
	}
	if err != nil {
		return nil, nil, nil, err
	}

	outReq := &http.Request{
		Method:     in.Method,
		URL:        upstreamURL,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Host:       p.cfg.upstream.Host,
		Header:     make(http.Header),
	}
	copyWebSocketHeaders(outReq.Header, in.Header)
	outReq.Header.Set("Connection", "Upgrade")
	outReq.Header.Set("Upgrade", "websocket")
	outReq.Header.Set("Accept-Encoding", "identity")
	outReq.Header.Del("Sec-Websocket-Extensions")
	outReq.Header.Del("Sec-WebSocket-Extensions")
	if pathKind == "anthropic" {
		ensureAnthropicBeta(outReq.Header)
	}

	if err := outReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, outReq)
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	log.Printf("websocket upstream handshake response: method=%s path=%s kind=%s status=%d",
		in.Method, in.URL.RequestURI(), pathKind, resp.StatusCode)
	return conn, reader, resp, nil
}

func hostWithDefaultPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	switch u.Scheme {
	case "https", "wss":
		return net.JoinHostPort(u.Hostname(), "443")
	default:
		return net.JoinHostPort(u.Hostname(), "80")
	}
}

func copyWebSocketHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "host", "content-length", "accept-encoding", "sec-websocket-extensions":
			continue
		default:
			dst.Del(key)
			for _, value := range values {
				dst.Add(key, value)
			}
		}
	}
}

func writeHTTPResponseHead(writer io.Writer, resp *http.Response) error {
	if _, err := fmt.Fprintf(writer, "HTTP/%d.%d %s\r\n", resp.ProtoMajor, resp.ProtoMinor, resp.Status); err != nil {
		return err
	}
	if err := resp.Header.Write(writer); err != nil {
		return err
	}
	_, err := io.WriteString(writer, "\r\n")
	return err
}

func (p *proxyServer) pipeWebSocketFrames(direction string, reader *bufio.Reader, writer io.Writer, expectMasked bool, writeMasked bool, requestURI string, pathKind string) error {
	for {
		frame, err := readWSFrame(reader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if frame.masked != expectMasked {
			return errors.New("unexpected websocket mask state")
		}

		if direction == "client_to_upstream" && frame.opcode == 1 {
			original := frame.payload
			injected, changed, err := injectFastIntoWebSocketTextPayload(frame.payload, pathKind)
			if err != nil {
				log.Printf("websocket fast injection skipped: path=%s payload_bytes=%d err=%v", requestURI, len(frame.payload), err)
			} else if changed {
				frame.payload = injected
			}
			log.Printf("websocket text frame processed: path=%s kind=%s payload_bytes=%d changed=%v", requestURI, pathKind, len(original), changed)
			if p.cfg.ccWSBridgeDebug || p.cfg.wsDebugPayloadBytes > 0 {
				logWebSocketJSONSummary("websocket tunneled client frame", requestURI, frame.payload, p.cfg.wsDebugPayloadBytes)
			}
		}

		if err := writeWSFrame(writer, frame, writeMasked); err != nil {
			return err
		}
	}
}

func injectFastIntoWebSocketTextPayload(payload []byte, pathKind string) ([]byte, bool, error) {
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(payload), &event); err != nil {
		return payload, false, err
	}

	changed := false
	if response, ok := event["response"].(map[string]any); ok && shouldInjectFastIntoWSObject(event, response) {
		changed = injectFastIntoMap(response, pathKind) || changed
	}
	if shouldInjectFastIntoWSObject(event, event) {
		changed = injectFastIntoMap(event, pathKind) || changed
	}
	if !changed {
		return payload, false, nil
	}
	out, err := json.Marshal(event)
	if err != nil {
		return payload, false, err
	}
	return out, !bytes.Equal(out, payload), nil
}

func shouldInjectFastIntoWSObject(event map[string]any, body map[string]any) bool {
	eventType := stringOrDefault(event, "type")
	if eventType == "response.create" {
		return true
	}
	for _, key := range []string{"model", "input", "messages", "stream"} {
		if _, ok := body[key]; ok {
			return true
		}
	}
	return false
}

func injectFastIntoMap(payload map[string]any, pathKind string) bool {
	changed := false
	if pathKind == "anthropic" {
		if payload["speed"] != anthropicSpeed {
			payload["speed"] = anthropicSpeed
			changed = true
		}
	}
	if payload["service_tier"] != openAITier {
		payload["service_tier"] = openAITier
		changed = true
	}
	return changed
}

type wsFrame struct {
	fin     bool
	rsv     byte
	opcode  byte
	masked  bool
	payload []byte
}

func readWSFrame(reader *bufio.Reader) (wsFrame, error) {
	header, err := readExact(reader, 2)
	if err != nil {
		return wsFrame{}, err
	}
	first := header[0]
	second := header[1]
	length := uint64(second & 0x7f)
	switch length {
	case 126:
		extended, err := readExact(reader, 2)
		if err != nil {
			return wsFrame{}, err
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended, err := readExact(reader, 8)
		if err != nil {
			return wsFrame{}, err
		}
		length = binary.BigEndian.Uint64(extended)
	}
	if length > maxWSFramePayload {
		return wsFrame{}, errors.New("websocket frame too large")
	}

	masked := second&0x80 != 0
	var maskKey []byte
	if masked {
		maskKey, err = readExact(reader, 4)
		if err != nil {
			return wsFrame{}, err
		}
	}

	payload, err := readExact(reader, int(length))
	if err != nil {
		return wsFrame{}, err
	}
	if masked {
		applyWSMask(payload, maskKey)
	}
	return wsFrame{
		fin:     first&0x80 != 0,
		rsv:     first & 0x70,
		opcode:  first & 0x0f,
		masked:  masked,
		payload: payload,
	}, nil
}

func readExact(reader *bufio.Reader, size int) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	buf := make([]byte, size)
	_, err := io.ReadFull(reader, buf)
	return buf, err
}

func writeWSFrame(writer io.Writer, frame wsFrame, masked bool) error {
	header := []byte{frame.opcode | frame.rsv}
	if frame.fin {
		header[0] |= 0x80
	}
	length := len(frame.payload)
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch {
	case length < 126:
		header = append(header, maskBit|byte(length))
	case length <= 65535:
		header = append(header, maskBit|126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(length))
	default:
		header = append(header, maskBit|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(length))
	}

	payload := frame.payload
	if masked {
		maskKey := make([]byte, 4)
		if _, err := rand.Read(maskKey); err != nil {
			return err
		}
		header = append(header, maskKey...)
		payload = append([]byte(nil), frame.payload...)
		applyWSMask(payload, maskKey)
	}
	if err := writeAll(writer, header); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	return writeAll(writer, payload)
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}

func applyWSMask(payload []byte, maskKey []byte) {
	for i := range payload {
		payload[i] ^= maskKey[i%4]
	}
}

func shouldInjectTier(r *http.Request, body []byte) bool {
	if r.Method != http.MethodPost {
		return false
	}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	return strings.Contains(contentType, "application/json") && len(bytes.TrimSpace(body)) > 0
}

func requestPathKind(path string) string {
	switch strings.TrimRight(path, "/") {
	case "/v1/messages", "/messages":
		return "anthropic"
	default:
		return "openai"
	}
}

func injectFastField(body []byte, pathKind string) ([]byte, error) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	injectFastIntoMap(payload, pathKind)
	return json.Marshal(payload)
}

func bodyHasFastField(body []byte, pathKind string) bool {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return false
	}
	if pathKind == "anthropic" {
		return hasNonEmptyString(payload, "speed") && hasNonEmptyString(payload, "service_tier")
	}
	return hasNonEmptyString(payload, "service_tier")
}

func hasNonEmptyString(payload map[string]any, key string) bool {
	value, ok := payload[key]
	if !ok {
		return false
	}
	text, ok := value.(string)
	return ok && strings.TrimSpace(text) != ""
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		dst.Del(key)
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}

func ensureAnthropicBeta(header http.Header) {
	ensureCommaHeaderValue(header, "anthropic-beta", anthropicFastBeta)
}

func ensureOpenAIWebSocketBeta(header http.Header) {
	ensureCommaHeaderValue(header, "OpenAI-Beta", openAIWSBeta)
}

func ensureCommaHeaderValue(header http.Header, key string, required string) {
	current := header.Values(key)
	var parts []string
	hasRequired := false
	for _, value := range current {
		for _, part := range strings.Split(value, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			if part == required {
				hasRequired = true
			}
			parts = append(parts, part)
		}
	}
	if !hasRequired {
		parts = append(parts, required)
	}
	header.Set(key, strings.Join(parts, ","))
}

func logOutboundRequest(in *http.Request, out *http.Request, body []byte, pathKind string) {
	if pathKind != "anthropic" {
		return
	}
	model, speed, serviceTier, effort := inspectAnthropicBody(body)
	log.Printf("outbound anthropic request: method=%s path=%s model=%q speed=%q service_tier=%q effort=%q anthropic_beta=%q",
		in.Method, in.URL.RequestURI(), model, speed, serviceTier, effort, strings.Join(out.Header.Values("anthropic-beta"), ", "))
}

func inspectAnthropicBody(body []byte) (model string, speed string, serviceTier string, effort string) {
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", "", ""
	}
	model = stringField(payload, "model")
	speed = stringField(payload, "speed")
	serviceTier = stringField(payload, "service_tier")
	if outputConfig, ok := payload["output_config"].(map[string]any); ok {
		effort = stringField(outputConfig, "effort")
	}
	return model, speed, serviceTier, effort
}

func stringField(payload map[string]any, key string) string {
	value, ok := payload[key]
	if !ok {
		return ""
	}
	text, ok := value.(string)
	if !ok {
		return ""
	}
	return text
}

func fastRelatedHeaders(header http.Header) string {
	var parts []string
	for key, values := range header {
		lowerKey := strings.ToLower(key)
		if !strings.Contains(lowerKey, "fast") && !strings.Contains(lowerKey, "service-tier") {
			continue
		}
		parts = append(parts, key+"="+strings.Join(values, ","))
	}
	return strings.Join(parts, "; ")
}
