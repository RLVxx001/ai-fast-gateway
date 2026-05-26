package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"
)

type claudeBridgeRequest struct {
	Model        string         `json:"model"`
	System       any            `json:"system,omitempty"`
	Messages     []claudeMsg    `json:"messages"`
	Tools        []claudeTool   `json:"tools,omitempty"`
	ToolChoice   map[string]any `json:"tool_choice,omitempty"`
	MaxTokens    int            `json:"max_tokens,omitempty"`
	Temperature  *float64       `json:"temperature,omitempty"`
	Stream       bool           `json:"stream,omitempty"`
	OutputConfig map[string]any `json:"output_config,omitempty"`
}

type claudeMsg struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type claudeTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema,omitempty"`
}

type bridgeBlockState struct {
	kind          string
	index         int
	callID        string
	name          string
	args          string
	emittedArgs   bool
	bufferToolArg bool
}

func (p *proxyServer) serveClaudeMessagesViaResponsesWS(w http.ResponseWriter, r *http.Request, body []byte) bool {
	var req claudeBridgeRequest
	if err := json.Unmarshal(body, &req); err != nil {
		if p.cfg.ccWSBridgeFallback {
			log.Printf("cc ws bridge fallback: invalid anthropic json: %v", err)
			return false
		}
		http.Error(w, "invalid anthropic json", http.StatusBadRequest)
		return true
	}
	upstreamConn, upstreamReader, err := p.openBridgeWebSocket(r)
	if err != nil {
		log.Printf("cc ws bridge upstream connect failed: path=%s model=%q err=%v", r.URL.RequestURI(), req.Model, err)
		if p.cfg.ccWSBridgeFallback {
			return false
		}
		http.Error(w, "cc websocket bridge upstream connect failed", http.StatusBadGateway)
		return true
	}
	defer upstreamConn.Close()

	responseReq, err := translateClaudeToResponses(req, r.Header.Get("x-claude-code-session-id"))
	if err != nil {
		log.Printf("cc ws bridge translate request failed: model=%q err=%v", req.Model, err)
		if p.cfg.ccWSBridgeFallback {
			return false
		}
		http.Error(w, "cc websocket bridge translate request failed", http.StatusBadRequest)
		return true
	}

	createEvent := make(map[string]any, len(responseReq)+2)
	for key, value := range responseReq {
		createEvent[key] = value
	}
	createEvent["type"] = "response.create"
	normalizeResponsesWebSocketCreate(createEvent)
	if clientMetadata := bridgeClientMetadata(r); len(clientMetadata) > 0 {
		createEvent["client_metadata"] = clientMetadata
	}
	createBytes, err := json.Marshal(createEvent)
	if err != nil {
		http.Error(w, "cc websocket bridge encode request failed", http.StatusInternalServerError)
		return true
	}
	if err := writeWSFrame(upstreamConn, wsFrame{fin: true, opcode: 1, payload: createBytes}, true); err != nil {
		log.Printf("cc ws bridge write response.create failed: model=%q err=%v", req.Model, err)
		if p.cfg.ccWSBridgeFallback {
			return false
		}
		http.Error(w, "cc websocket bridge write request failed", http.StatusBadGateway)
		return true
	}

	if p.cfg.ccWSBridgeDebug || p.cfg.wsDebugPayloadBytes > 0 {
		logWebSocketJSONSummary("cc ws bridge outbound response.create", r.URL.RequestURI(), createBytes, p.cfg.wsDebugPayloadBytes)
	}
	log.Printf("cc ws bridge started: path=%s upstream_path=%s model=%q request_bytes=%d",
		r.URL.RequestURI(), p.cfg.ccWSBridgePath, req.Model, len(createBytes))

	firstFrame, err := readFirstBridgeTextFrame(upstreamConn, upstreamReader, p.cfg.ccWSFirstEventWait, p.cfg.ccWSBridgeDebug)
	if err != nil {
		log.Printf("cc ws bridge first event wait failed: path=%s model=%q wait=%s err=%v",
			r.URL.RequestURI(), req.Model, p.cfg.ccWSFirstEventWait, err)
		if p.cfg.ccWSBridgeFallback {
			return false
		}
		http.Error(w, "cc websocket bridge first event timeout", http.StatusBadGateway)
		return true
	}

	if !req.Stream {
		message, err := collectResponsesWebSocketToAnthropicMessage(upstreamConn, upstreamReader, req.Model, p.cfg.ccWSBridgeDebug, firstFrame)
		if err != nil {
			log.Printf("cc ws bridge non-stream collect failed: path=%s model=%q err=%v", r.URL.RequestURI(), req.Model, err)
			http.Error(w, "cc websocket bridge collect failed", http.StatusBadGateway)
			return true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if err := json.NewEncoder(w).Encode(message); err != nil {
			log.Printf("cc ws bridge non-stream response write failed: path=%s model=%q err=%v", r.URL.RequestURI(), req.Model, err)
		}
		return true
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-cache")
	header.Set("Connection", "keep-alive")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = translateResponsesWebSocketToAnthropicSSE(w, upstreamConn, upstreamReader, req.Model, p.cfg.ccWSBridgeDebug, firstFrame)
	return true
}

func (p *proxyServer) openBridgeWebSocket(in *http.Request) (net.Conn, *bufio.Reader, error) {
	target := *p.cfg.upstream
	target.Path = joinURLPath(target.Path, p.cfg.ccWSBridgePath)
	target.RawQuery = ""

	hostPort := hostWithDefaultPort(&target)
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	var conn net.Conn
	var err error
	switch target.Scheme {
	case "https", "wss":
		conn, err = tls.DialWithDialer(dialer, "tcp", hostPort, &tls.Config{ServerName: target.Hostname()})
	default:
		conn, err = dialer.DialContext(in.Context(), "tcp", hostPort)
	}
	if err != nil {
		return nil, nil, err
	}

	key := make([]byte, 16)
	if _, err := rand.Read(key); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	outReq := &http.Request{
		Method:     http.MethodGet,
		URL:        &target,
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Host:       p.cfg.upstream.Host,
		Header:     make(http.Header),
	}
	copyBridgeHeaders(outReq.Header, in.Header)
	outReq.Header.Set("Connection", "Upgrade")
	outReq.Header.Set("Upgrade", "websocket")
	outReq.Header.Set("Sec-WebSocket-Version", "13")
	outReq.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString(key))
	outReq.Header.Set("Accept-Encoding", "identity")
	outReq.Header.Del("Sec-WebSocket-Extensions")
	ensureOpenAIWebSocketBeta(outReq.Header)

	if err := outReq.Write(conn); err != nil {
		_ = conn.Close()
		return nil, nil, err
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, outReq)
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		preview := ""
		if resp.Body != nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
			preview = string(b)
		}
		return nil, nil, fmt.Errorf("websocket upstream status %d: %s", resp.StatusCode, preview)
	}
	log.Printf("cc ws bridge upstream handshake response: path=%s status=%d", target.RequestURI(), resp.StatusCode)
	return conn, reader, nil
}

func bridgeClientMetadata(r *http.Request) map[string]string {
	metadata := map[string]string{}
	for key, values := range r.Header {
		lower := strings.ToLower(key)
		if !strings.HasPrefix(lower, "x-claude-code-") && !strings.HasPrefix(lower, "x-codex-") {
			continue
		}
		if len(values) > 0 && strings.TrimSpace(values[0]) != "" {
			metadata[strings.ToLower(key)] = values[0]
		}
	}
	if sessionID := r.Header.Get("x-claude-code-session-id"); sessionID != "" {
		metadata["x-codex-window-id"] = sessionID + ":0"
		metadata["x-codex-turn-metadata"] = `{"session_id":"` + sessionID + `","thread_id":"` + sessionID + `","thread_source":"claude-code-bridge"}`
	}
	metadata["x-codex-ws-stream-request-start-ms"] = fmt.Sprintf("%d", time.Now().UnixMilli())
	return metadata
}

func copyBridgeHeaders(dst, src http.Header) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "host", "content-length", "content-type", "accept-encoding", "connection", "upgrade",
			"sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions":
			continue
		default:
			for _, value := range values {
				dst.Add(key, value)
			}
		}
	}
}

func readFirstBridgeTextFrame(conn net.Conn, reader *bufio.Reader, wait time.Duration, debug bool) ([]byte, error) {
	if wait <= 0 {
		wait = 15 * time.Second
	}
	if err := conn.SetReadDeadline(time.Now().Add(wait)); err != nil {
		return nil, err
	}
	defer func() {
		_ = conn.SetReadDeadline(time.Time{})
	}()

	for {
		frame, err := readWSFrame(reader)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				return nil, fmt.Errorf("timeout waiting for first upstream websocket event after %s", wait)
			}
			return nil, err
		}
		switch frame.opcode {
		case 8:
			return nil, io.EOF
		case 9:
			_ = writeWSFrame(conn, wsFrame{fin: true, opcode: 10, payload: frame.payload}, true)
			continue
		case 1:
			if debug {
				log.Printf("cc ws bridge first upstream text frame: bytes=%d", len(frame.payload))
			}
			return frame.payload, nil
		default:
			continue
		}
	}
}

func joinURLPath(base string, path string) string {
	base = strings.TrimRight(base, "/")
	path = "/" + strings.TrimLeft(path, "/")
	if base == "" || base == "/" {
		return path
	}
	return base + path
}

func translateClaudeToResponses(req claudeBridgeRequest, sessionID string) (map[string]any, error) {
	out := map[string]any{
		"model":               req.Model,
		"input":               translateClaudeMessages(req.Messages),
		"store":               false,
		"stream":              true,
		"parallel_tool_calls": true,
		"tool_choice":         translateClaudeToolChoice(req.ToolChoice),
		"text": map[string]any{
			"verbosity": "low",
		},
	}
	if instructions := translateClaudeSystem(req.System); instructions != "" {
		out["instructions"] = instructions
	}
	if len(req.Tools) > 0 {
		tools := make([]map[string]any, 0, len(req.Tools))
		for _, tool := range req.Tools {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        tool.Name,
				"description": tool.Description,
				"parameters":  tool.InputSchema,
			})
		}
		out["tools"] = tools
	}
	if req.MaxTokens > 0 {
		out["max_output_tokens"] = req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if sessionID != "" {
		out["prompt_cache_key"] = sessionID
	}
	if effort := translateClaudeEffort(req.OutputConfig); effort != "" {
		out["reasoning"] = map[string]any{"effort": effort}
		out["include"] = []string{"reasoning.encrypted_content"}
	}
	if format, ok := req.OutputConfig["format"].(map[string]any); ok {
		text := map[string]any{"verbosity": "low"}
		if format["type"] == "json_schema" {
			text["format"] = map[string]any{
				"type":   "json_schema",
				"name":   stringOrDefault(format["name"], "response"),
				"schema": format["schema"],
				"strict": true,
			}
		}
		out["text"] = text
	}
	return out, nil
}

func normalizeResponsesWebSocketCreate(payload map[string]any) {
	delete(payload, "max_output_tokens")
	injectFastIntoMap(payload, "openai")
}

func translateClaudeSystem(system any) string {
	switch typed := system.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, item := range typed {
			if block, ok := item.(map[string]any); ok && block["type"] == "text" {
				if text, ok := block["text"].(string); ok && !strings.HasPrefix(text, "x-anthropic-billing-header:") {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n\n")
	default:
		return ""
	}
}

func translateClaudeMessages(messages []claudeMsg) []map[string]any {
	var out []map[string]any
	for _, msg := range messages {
		blocks := normalizeClaudeContent(msg.Content)
		if msg.Role == "user" {
			var parts []map[string]any
			flushParts := func() {
				if len(parts) == 0 {
					return
				}
				out = append(out, map[string]any{"type": "message", "role": "user", "content": parts})
				parts = nil
			}
			for _, block := range blocks {
				switch block["type"] {
				case "text":
					parts = append(parts, map[string]any{"type": "input_text", "text": stringOrDefault(block["text"], "")})
				case "image":
					if url := claudeImageURL(block); url != "" {
						parts = append(parts, map[string]any{"type": "input_image", "image_url": url})
					}
				case "tool_result":
					flushParts()
					output := toolResultToString(block["content"])
					if isError, _ := block["is_error"].(bool); isError {
						output = "[tool execution error]\n" + output
					}
					out = append(out, map[string]any{
						"type":    "function_call_output",
						"call_id": stringOrDefault(block["tool_use_id"], ""),
						"output":  output,
					})
				}
			}
			flushParts()
			continue
		}

		var textParts []map[string]any
		flushText := func() {
			if len(textParts) == 0 {
				return
			}
			out = append(out, map[string]any{"type": "message", "role": "assistant", "content": textParts})
			textParts = nil
		}
		for _, block := range blocks {
			switch block["type"] {
			case "text":
				textParts = append(textParts, map[string]any{"type": "output_text", "text": stringOrDefault(block["text"], "")})
			case "tool_use":
				flushText()
				args, _ := json.Marshal(block["input"])
				out = append(out, map[string]any{
					"type":      "function_call",
					"call_id":   stringOrDefault(block["id"], ""),
					"name":      stringOrDefault(block["name"], ""),
					"arguments": string(args),
				})
			}
		}
		flushText()
	}
	return out
}

func normalizeClaudeContent(content any) []map[string]any {
	if text, ok := content.(string); ok {
		return []map[string]any{{"type": "text", "text": text}}
	}
	items, ok := content.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if block, ok := item.(map[string]any); ok {
			out = append(out, block)
		}
	}
	return out
}

func translateClaudeToolChoice(choice map[string]any) any {
	switch stringOrDefault(choice["type"], "") {
	case "none":
		return "none"
	case "any":
		return "required"
	case "tool":
		name := stringOrDefault(choice["name"], "")
		if name == "" {
			return "required"
		}
		return map[string]any{"type": "function", "name": name}
	default:
		return "auto"
	}
}

func translateClaudeEffort(outputConfig map[string]any) string {
	effort := stringOrDefault(outputConfig["effort"], "")
	switch effort {
	case "max":
		return "xhigh"
	case "low", "medium", "high":
		return effort
	default:
		return ""
	}
}

func claudeImageURL(block map[string]any) string {
	source, ok := block["source"].(map[string]any)
	if !ok {
		return ""
	}
	if source["type"] == "url" {
		return stringOrDefault(source["url"], "")
	}
	if source["type"] == "base64" {
		mediaType := stringOrDefault(source["media_type"], "image/png")
		data := stringOrDefault(source["data"], "")
		if data == "" {
			return ""
		}
		return "data:" + mediaType + ";base64," + data
	}
	return ""
}

func toolResultToString(content any) string {
	switch typed := content.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, item := range typed {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if block["type"] == "text" {
				parts = append(parts, stringOrDefault(block["text"], ""))
			} else {
				parts = append(parts, "[unsupported content block omitted: "+stringOrDefault(block["type"], "unknown")+"]")
			}
		}
		return strings.Join(parts, "\n")
	default:
		bytes, _ := json.Marshal(content)
		return string(bytes)
	}
}

func translateResponsesWebSocketToAnthropicSSE(w http.ResponseWriter, upstreamConn net.Conn, upstreamReader *bufio.Reader, model string, debug bool, firstFrame []byte) error {
	flusher, _ := w.(http.Flusher)
	messageID := "msg_" + randomHex(12)
	messageStarted := false
	blocks := map[int]*bridgeBlockState{}
	itemToOutput := map[string]int{}
	nextAnthropicIndex := 0
	sawTool := false
	incomplete := false
	var finalUsage map[string]any

	emit := func(event string, data any) error {
		payload, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return nil
	}
	ensureMessageStart := func() error {
		if messageStarted {
			return nil
		}
		messageStarted = true
		if err := emit("message_start", map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":            messageID,
				"type":          "message",
				"role":          "assistant",
				"model":         model,
				"content":       []any{},
				"stop_reason":   nil,
				"stop_sequence": nil,
				"usage": map[string]any{
					"input_tokens":                0,
					"output_tokens":               0,
					"cache_creation_input_tokens": 0,
					"cache_read_input_tokens":     0,
				},
			},
		}); err != nil {
			return err
		}
		return emit("ping", map[string]any{"type": "ping"})
	}

	processEvent := func(payload []byte) (bool, error) {
		var event map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(payload), &event); err != nil {
			if debug {
				log.Printf("cc ws bridge ignored non-json text frame: bytes=%d err=%v", len(payload), err)
			}
			return false, nil
		}
		eventType := stringOrDefault(event["type"], "")
		if eventType == "" {
			eventType = stringOrDefault(event["event"], "")
		}
		if debug {
			log.Printf("cc ws bridge upstream event: type=%q output_index=%v item_id=%v", eventType, event["output_index"], event["item_id"])
		}

		switch eventType {
		case "codex.rate_limits":
			if rateLimits, _ := event["rate_limits"].(map[string]any); truthy(rateLimits["limit_reached"]) {
				_ = emitBridgeError(emit, ensureMessageStart, "rate_limit_error", "rate limit reached")
				return true, nil
			}
		case "response.failed", "response.error", "error":
			_ = emitBridgeError(emit, ensureMessageStart, "api_error", upstreamErrorMessage(event))
			return true, nil
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			outputIndex, ok := intField(event["output_index"])
			if !ok || item == nil {
				return false, nil
			}
			switch stringOrDefault(item["type"], "") {
			case "message":
				idx := nextAnthropicIndex
				nextAnthropicIndex++
				blocks[outputIndex] = &bridgeBlockState{kind: "text", index: idx}
				if id := stringOrDefault(item["id"], ""); id != "" {
					itemToOutput[id] = outputIndex
				}
				if err := ensureMessageStart(); err != nil {
					return false, err
				}
				if err := emit("content_block_start", map[string]any{
					"type":          "content_block_start",
					"index":         idx,
					"content_block": map[string]any{"type": "text", "text": ""},
				}); err != nil {
					return false, err
				}
			case "function_call":
				sawTool = true
				idx := nextAnthropicIndex
				nextAnthropicIndex++
				name := stringOrDefault(item["name"], "")
				callID := stringOrDefault(item["call_id"], "")
				blocks[outputIndex] = &bridgeBlockState{
					kind:          "tool",
					index:         idx,
					callID:        callID,
					name:          name,
					bufferToolArg: name == "Read",
				}
				if err := ensureMessageStart(); err != nil {
					return false, err
				}
				if err := emit("content_block_start", map[string]any{
					"type":  "content_block_start",
					"index": idx,
					"content_block": map[string]any{
						"type":  "tool_use",
						"id":    callID,
						"name":  name,
						"input": map[string]any{},
					},
				}); err != nil {
					return false, err
				}
			}
		case "response.output_text.delta":
			state := lookupBridgeState(event, blocks, itemToOutput)
			if state == nil || state.kind != "text" {
				return false, nil
			}
			delta := stringOrDefault(event["delta"], "")
			if delta == "" {
				return false, nil
			}
			if err := emit("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": state.index,
				"delta": map[string]any{"type": "text_delta", "text": delta},
			}); err != nil {
				return false, err
			}
		case "response.function_call_arguments.delta":
			state := lookupBridgeState(event, blocks, itemToOutput)
			if state == nil || state.kind != "tool" {
				return false, nil
			}
			delta := stringOrDefault(event["delta"], "")
			if delta == "" {
				return false, nil
			}
			state.args += delta
			if !state.bufferToolArg {
				state.emittedArgs = true
				if err := emit("content_block_delta", map[string]any{
					"type":  "content_block_delta",
					"index": state.index,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": delta},
				}); err != nil {
					return false, err
				}
			}
		case "response.function_call_arguments.done":
			state := lookupBridgeState(event, blocks, itemToOutput)
			if state == nil || state.kind != "tool" {
				return false, nil
			}
			if args := stringOrDefault(event["arguments"], ""); args != "" && state.args == "" {
				state.args = args
			}
		case "response.output_item.done":
			outputIndex, ok := intField(event["output_index"])
			if !ok {
				return false, nil
			}
			state := blocks[outputIndex]
			if state == nil {
				return false, nil
			}
			item, _ := event["item"].(map[string]any)
			if state.kind == "tool" {
				finalArgs := state.args
				if itemArgs := stringOrDefault(item["arguments"], ""); itemArgs != "" {
					finalArgs = itemArgs
				}
				finalArgs = sanitizeBridgeToolArgs(state.name, finalArgs)
				if finalArgs != "" && (state.bufferToolArg || !state.emittedArgs) {
					if err := emit("content_block_delta", map[string]any{
						"type":  "content_block_delta",
						"index": state.index,
						"delta": map[string]any{"type": "input_json_delta", "partial_json": finalArgs},
					}); err != nil {
						return false, err
					}
				}
			}
			if err := emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": state.index}); err != nil {
				return false, err
			}
			delete(blocks, outputIndex)
		case "response.completed", "response.incomplete":
			if response, _ := event["response"].(map[string]any); response != nil {
				if usage, _ := response["usage"].(map[string]any); usage != nil {
					finalUsage = usage
				}
				if status := stringOrDefault(response["status"], ""); status == "incomplete" {
					incomplete = true
				}
				if details, _ := response["incomplete_details"].(map[string]any); stringOrDefault(details["reason"], "") == "max_output_tokens" {
					incomplete = true
				}
			}
			stopReason := "end_turn"
			if incomplete {
				stopReason = "max_tokens"
			} else if sawTool {
				stopReason = "tool_use"
			}
			if err := ensureMessageStart(); err != nil {
				return false, err
			}
			if err := emit("message_delta", map[string]any{
				"type":  "message_delta",
				"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
				"usage": mapResponsesUsageToAnthropic(finalUsage),
			}); err != nil {
				return false, err
			}
			if err := emit("message_stop", map[string]any{"type": "message_stop"}); err != nil {
				return false, err
			}
			return true, nil
		}
		return false, nil
	}

	if len(firstFrame) > 0 {
		done, err := processEvent(firstFrame)
		if err != nil || done {
			return err
		}
	}

	for {
		frame, err := readWSFrame(upstreamReader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return nil
			}
			_ = emitBridgeError(emit, ensureMessageStart, "api_error", err.Error())
			return err
		}
		switch frame.opcode {
		case 8:
			return nil
		case 9:
			_ = writeWSFrame(upstreamConn, wsFrame{fin: true, opcode: 10, payload: frame.payload}, true)
			continue
		case 1:
			done, err := processEvent(frame.payload)
			if err != nil || done {
				return err
			}
		default:
			continue
		}
	}
}

func collectResponsesWebSocketToAnthropicMessage(upstreamConn net.Conn, upstreamReader *bufio.Reader, model string, debug bool, firstFrame []byte) (map[string]any, error) {
	messageID := "msg_" + randomHex(12)
	blocks := map[int]*bridgeBlockState{}
	itemToOutput := map[string]int{}
	contentByIndex := map[int]map[string]any{}
	nextAnthropicIndex := 0
	sawTool := false
	incomplete := false
	var finalUsage map[string]any

	processEvent := func(payload []byte) (bool, error) {
		var event map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(payload), &event); err != nil {
			if debug {
				log.Printf("cc ws bridge ignored non-json text frame: bytes=%d err=%v", len(payload), err)
			}
			return false, nil
		}
		eventType := stringOrDefault(event["type"], "")
		if eventType == "" {
			eventType = stringOrDefault(event["event"], "")
		}
		if debug {
			log.Printf("cc ws bridge upstream event: type=%q output_index=%v item_id=%v", eventType, event["output_index"], event["item_id"])
		}

		switch eventType {
		case "codex.rate_limits":
			if rateLimits, _ := event["rate_limits"].(map[string]any); truthy(rateLimits["limit_reached"]) {
				return false, errors.New("rate limit reached")
			}
		case "response.failed", "response.error", "error":
			return false, errors.New(upstreamErrorMessage(event))
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			outputIndex, ok := intField(event["output_index"])
			if !ok || item == nil {
				return false, nil
			}
			switch stringOrDefault(item["type"], "") {
			case "message":
				idx := nextAnthropicIndex
				nextAnthropicIndex++
				blocks[outputIndex] = &bridgeBlockState{kind: "text", index: idx}
				contentByIndex[idx] = map[string]any{"type": "text", "text": ""}
				if id := stringOrDefault(item["id"], ""); id != "" {
					itemToOutput[id] = outputIndex
				}
			case "function_call":
				sawTool = true
				idx := nextAnthropicIndex
				nextAnthropicIndex++
				name := stringOrDefault(item["name"], "")
				callID := stringOrDefault(item["call_id"], "")
				blocks[outputIndex] = &bridgeBlockState{kind: "tool", index: idx, callID: callID, name: name, bufferToolArg: name == "Read"}
				contentByIndex[idx] = map[string]any{"type": "tool_use", "id": callID, "name": name, "input": map[string]any{}}
			}
		case "response.output_text.delta":
			state := lookupBridgeState(event, blocks, itemToOutput)
			if state == nil || state.kind != "text" {
				return false, nil
			}
			delta := stringOrDefault(event["delta"], "")
			if delta == "" {
				return false, nil
			}
			block := contentByIndex[state.index]
			block["text"] = stringOrDefault(block["text"], "") + delta
		case "response.function_call_arguments.delta":
			state := lookupBridgeState(event, blocks, itemToOutput)
			if state == nil || state.kind != "tool" {
				return false, nil
			}
			state.args += stringOrDefault(event["delta"], "")
		case "response.function_call_arguments.done":
			state := lookupBridgeState(event, blocks, itemToOutput)
			if state == nil || state.kind != "tool" {
				return false, nil
			}
			if args := stringOrDefault(event["arguments"], ""); args != "" && state.args == "" {
				state.args = args
			}
		case "response.output_item.done":
			outputIndex, ok := intField(event["output_index"])
			if !ok {
				return false, nil
			}
			state := blocks[outputIndex]
			if state == nil || state.kind != "tool" {
				delete(blocks, outputIndex)
				return false, nil
			}
			item, _ := event["item"].(map[string]any)
			finalArgs := state.args
			if itemArgs := stringOrDefault(item["arguments"], ""); itemArgs != "" {
				finalArgs = itemArgs
			}
			finalArgs = sanitizeBridgeToolArgs(state.name, finalArgs)
			var input any = map[string]any{}
			if strings.TrimSpace(finalArgs) != "" {
				var parsed any
				if err := json.Unmarshal([]byte(finalArgs), &parsed); err == nil {
					input = parsed
				}
			}
			if block := contentByIndex[state.index]; block != nil {
				block["input"] = input
			}
			delete(blocks, outputIndex)
		case "response.completed", "response.incomplete":
			if response, _ := event["response"].(map[string]any); response != nil {
				if usage, _ := response["usage"].(map[string]any); usage != nil {
					finalUsage = usage
				}
				if status := stringOrDefault(response["status"], ""); status == "incomplete" {
					incomplete = true
				}
				if details, _ := response["incomplete_details"].(map[string]any); stringOrDefault(details["reason"], "") == "max_output_tokens" {
					incomplete = true
				}
			}
			return true, nil
		}
		return false, nil
	}

	if len(firstFrame) > 0 {
		done, err := processEvent(firstFrame)
		if err != nil {
			return nil, err
		}
		if done {
			return buildAnthropicMessage(messageID, model, contentByIndex, nextAnthropicIndex, sawTool, incomplete, finalUsage), nil
		}
	}

	for {
		frame, err := readWSFrame(upstreamReader)
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				break
			}
			return nil, err
		}
		switch frame.opcode {
		case 8:
			return buildAnthropicMessage(messageID, model, contentByIndex, nextAnthropicIndex, sawTool, incomplete, finalUsage), nil
		case 9:
			_ = writeWSFrame(upstreamConn, wsFrame{fin: true, opcode: 10, payload: frame.payload}, true)
			continue
		case 1:
			done, err := processEvent(frame.payload)
			if err != nil {
				return nil, err
			}
			if done {
				return buildAnthropicMessage(messageID, model, contentByIndex, nextAnthropicIndex, sawTool, incomplete, finalUsage), nil
			}
		default:
			continue
		}
	}
	return buildAnthropicMessage(messageID, model, contentByIndex, nextAnthropicIndex, sawTool, incomplete, finalUsage), nil
}

func buildAnthropicMessage(messageID string, model string, contentByIndex map[int]map[string]any, count int, sawTool bool, incomplete bool, finalUsage map[string]any) map[string]any {
	content := make([]any, 0, count)
	for i := 0; i < count; i++ {
		if block := contentByIndex[i]; block != nil {
			content = append(content, block)
		}
	}
	stopReason := "end_turn"
	if incomplete {
		stopReason = "max_tokens"
	} else if sawTool {
		stopReason = "tool_use"
	}
	return map[string]any{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         mapResponsesUsageToAnthropic(finalUsage),
	}
}

func emitBridgeError(emit func(string, any) error, ensureMessageStart func() error, errorType string, message string) error {
	if err := ensureMessageStart(); err != nil {
		return err
	}
	return emit("error", map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errorType, "message": message},
	})
}

func lookupBridgeState(event map[string]any, blocks map[int]*bridgeBlockState, itemToOutput map[string]int) *bridgeBlockState {
	if outputIndex, ok := intField(event["output_index"]); ok {
		return blocks[outputIndex]
	}
	if itemID := stringOrDefault(event["item_id"], ""); itemID != "" {
		if outputIndex, ok := itemToOutput[itemID]; ok {
			return blocks[outputIndex]
		}
	}
	return nil
}

func mapResponsesUsageToAnthropic(usage map[string]any) map[string]any {
	cached := 0
	if details, _ := usage["input_tokens_details"].(map[string]any); details != nil {
		cached, _ = intField(details["cached_tokens"])
	}
	input, _ := intField(usage["input_tokens"])
	output, _ := intField(usage["output_tokens"])
	if input < cached {
		input = cached
	}
	return map[string]any{
		"input_tokens":                input - cached,
		"output_tokens":               output,
		"cache_creation_input_tokens": 0,
		"cache_read_input_tokens":     cached,
	}
}

func sanitizeBridgeToolArgs(name string, args string) string {
	if name != "Read" || args == "" {
		return args
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(args), &parsed); err != nil {
		return args
	}
	if pages, ok := parsed["pages"].(string); !ok || pages != "" {
		return args
	}
	delete(parsed, "pages")
	out, err := json.Marshal(parsed)
	if err != nil {
		return args
	}
	return string(out)
}

func upstreamErrorMessage(event map[string]any) string {
	if response, _ := event["response"].(map[string]any); response != nil {
		if errObj, _ := response["error"].(map[string]any); errObj != nil {
			if msg := stringOrDefault(errObj["message"], ""); msg != "" {
				return msg
			}
		}
	}
	if errObj, _ := event["error"].(map[string]any); errObj != nil {
		if msg := stringOrDefault(errObj["message"], ""); msg != "" {
			return msg
		}
	}
	return "Upstream error"
}

func logWebSocketJSONSummary(prefix string, path string, payload []byte, previewBytes int) {
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(payload), &event); err != nil {
		log.Printf("%s summary: path=%s bytes=%d json=false err=%v", prefix, path, len(payload), err)
		return
	}

	response, _ := event["response"].(map[string]any)
	body := response
	if body == nil {
		body = event
	}
	log.Printf("%s summary: path=%s bytes=%d type=%q keys=%s response_keys=%s model=%q stream=%v generate=%v service_tier=%q priority=%q input_len=%d tools_len=%d include_len=%d",
		prefix,
		path,
		len(payload),
		stringOrDefault(event["type"], ""),
		jsonObjectKeys(event),
		jsonObjectKeys(response),
		stringOrDefault(body["model"], ""),
		body["stream"],
		body["generate"],
		stringOrDefault(body["service_tier"], ""),
		stringOrDefault(body["priority"], ""),
		jsonArrayLen(body["input"]),
		jsonArrayLen(body["tools"]),
		jsonArrayLen(body["include"]),
	)
	if previewBytes <= 0 {
		return
	}
	preview := redactJSONForLog(event)
	encoded, err := json.Marshal(preview)
	if err != nil {
		return
	}
	if len(encoded) > previewBytes {
		encoded = append(encoded[:previewBytes], []byte("...")...)
	}
	log.Printf("%s preview: path=%s json=%s", prefix, path, string(encoded))
}

func jsonObjectKeys(value map[string]any) string {
	if len(value) == 0 {
		return ""
	}
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func jsonArrayLen(value any) int {
	items, ok := value.([]any)
	if !ok {
		return 0
	}
	return len(items)
}

func redactJSONForLog(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			switch strings.ToLower(key) {
			case "input", "messages", "content", "tools", "instructions", "prompt", "metadata":
				out[key] = fmt.Sprintf("<redacted:%s>", summarizeJSONValue(item))
			default:
				out[key] = redactJSONForLog(item)
			}
		}
		return out
	case []any:
		return fmt.Sprintf("<redacted:array len=%d>", len(typed))
	case string:
		if len(typed) > 120 {
			return typed[:120] + "..."
		}
		return typed
	default:
		return typed
	}
}

func summarizeJSONValue(value any) string {
	switch typed := value.(type) {
	case []any:
		return fmt.Sprintf("array len=%d", len(typed))
	case map[string]any:
		return "object keys=" + jsonObjectKeys(typed)
	case string:
		return fmt.Sprintf("string len=%d", len(typed))
	default:
		return fmt.Sprintf("%T", value)
	}
}

func randomHex(size int) string {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(buf)
}

func stringOrDefault(value any, fallback string) string {
	if text, ok := value.(string); ok {
		return text
	}
	return fallback
}

func intField(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		i, err := typed.Int64()
		return int(i), err == nil
	default:
		return 0, false
	}
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case bool:
		return typed
	case string:
		return typed == "true" || typed == "1"
	case float64:
		return typed != 0
	default:
		return false
	}
}
