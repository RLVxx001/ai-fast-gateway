package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
)

func (p *proxyServer) serveOpenAIResponsesViaWS(w http.ResponseWriter, r *http.Request, body []byte) bool {
	cfg := p.currentConfig()
	createBytes, stream, model, sessionKey, err := buildOpenAIResponsesCreateEvent(r, body, cfg.fastModeEnabled)
	if err != nil {
		http.Error(w, "invalid responses json", http.StatusBadRequest)
		return true
	}
	if cfg.openAIResponsesWSMaxRequestBytes > 0 && len(createBytes) > cfg.openAIResponsesWSMaxRequestBytes {
		log.Printf("responses ws bridge skipped: path=%s model=%q request_bytes=%d max_request_bytes=%d reason=large_request",
			r.URL.RequestURI(), model, len(createBytes), cfg.openAIResponsesWSMaxRequestBytes)
		return false
	}

	upstream, firstFrame, err := p.startOpenAIResponsesWSBridge(r, model, createBytes, sessionKey)
	if err != nil {
		log.Printf("responses ws bridge attempts exhausted: path=%s model=%q attempts=%d err=%v",
			r.URL.RequestURI(), model, positiveOrDefault(cfg.openAIResponsesWSMaxAttempts, 1), err)
		if cfg.openAIResponsesWSFallback {
			return false
		}
		http.Error(w, "responses websocket bridge upstream failed", http.StatusBadGateway)
		return true
	}
	defer upstream.Release()

	if stream {
		header := w.Header()
		header.Set("Content-Type", "text/event-stream")
		header.Set("Cache-Control", "no-cache")
		header.Set("Connection", "keep-alive")
		header.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		if err := relayResponsesWebSocketAsSSE(w, upstream.conn, upstream.reader, cfg.ccWSBridgeDebug, firstFrame); err != nil {
			upstream.MarkBroken()
			log.Printf("responses ws bridge stream relay failed: path=%s model=%q err=%v", r.URL.RequestURI(), model, err)
		}
		return true
	}

	response, err := collectResponsesWebSocketResponse(upstream.conn, upstream.reader, cfg.ccWSBridgeDebug, firstFrame)
	if err != nil {
		upstream.MarkBroken()
		log.Printf("responses ws bridge collect failed: path=%s model=%q err=%v", r.URL.RequestURI(), model, err)
		http.Error(w, "responses websocket bridge collect failed", http.StatusBadGateway)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(response); err != nil {
		log.Printf("responses ws bridge response write failed: path=%s model=%q err=%v", r.URL.RequestURI(), model, err)
	}
	return true
}

func buildOpenAIResponsesCreateEvent(r *http.Request, body []byte, fastModeEnabled bool) ([]byte, bool, string, string, error) {
	var payload map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(body), &payload); err != nil {
		return nil, false, "", "", err
	}
	payload["type"] = "response.create"
	normalizeResponsesWebSocketCreate(payload, fastModeEnabled)
	sessionKey := firstNonEmpty(stringOrDefault(payload["prompt_cache_key"], ""), r.Header.Get("x-codex-window-id"), r.Header.Get("x-codex-session-id"), "responses")
	if _, ok := payload["client_metadata"]; !ok {
		if clientMetadata := bridgeClientMetadataForSession(r, sessionKey); len(clientMetadata) > 0 {
			payload["client_metadata"] = clientMetadata
		}
	}
	stream, _ := payload["stream"].(bool)
	model := stringOrDefault(payload["model"], "")
	createBytes, err := json.Marshal(payload)
	return createBytes, stream, model, sessionKey, err
}

func (p *proxyServer) startOpenAIResponsesWSBridge(r *http.Request, model string, createBytes []byte, sessionKey string) (*bridgeWSLease, []byte, error) {
	cfg := p.currentConfig()
	maxAttempts := positiveOrDefault(cfg.openAIResponsesWSMaxAttempts, 1)
	clientKey := bridgeWSClientKey(r)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		effectiveSessionKey := p.effectiveBridgeSession(clientKey, sessionKey)
		attemptCreateBytes, err := rewriteBridgeCreateSession(createBytes, effectiveSessionKey)
		if err != nil {
			return nil, nil, err
		}
		upstream, firstFrame, err := p.tryStartOpenAIResponsesWSBridge(r, model, attemptCreateBytes, effectiveSessionKey, attempt, maxAttempts)
		if err == nil {
			p.markBridgeSessionSuccess(clientKey, sessionKey)
			return upstream, firstFrame, nil
		}
		lastErr = err
		if isBridgeFirstEventFailure(err) {
			p.markBridgeSessionFirstEventFailure(clientKey, sessionKey, err)
		}
		if attempt < maxAttempts {
			log.Printf("responses ws bridge retrying: path=%s model=%q attempt=%d/%d err=%v",
				r.URL.RequestURI(), model, attempt+1, maxAttempts, err)
		}
	}
	return nil, nil, lastErr
}

func (p *proxyServer) tryStartOpenAIResponsesWSBridge(r *http.Request, model string, createBytes []byte, sessionKey string, attempt int, maxAttempts int) (*bridgeWSLease, []byte, error) {
	cfg := p.currentConfig()
	upstream, err := p.acquireBridgeWebSocket(bridgeRequestWithSession(r, sessionKey), sessionKey)
	if err != nil {
		log.Printf("responses ws bridge upstream connect failed: path=%s model=%q attempt=%d/%d err=%v",
			r.URL.RequestURI(), model, attempt, maxAttempts, err)
		return nil, nil, err
	}
	if err := writeWSFrame(upstream.conn, wsFrame{fin: true, opcode: 1, payload: createBytes}, true); err != nil {
		upstream.MarkBroken()
		upstream.Release()
		log.Printf("responses ws bridge write response.create failed: path=%s model=%q attempt=%d/%d err=%v",
			r.URL.RequestURI(), model, attempt, maxAttempts, err)
		return nil, nil, err
	}
	if cfg.ccWSBridgeDebug || cfg.wsDebugPayloadBytes > 0 {
		logWebSocketJSONSummary("responses ws bridge outbound response.create", r.URL.RequestURI(), createBytes, cfg.wsDebugPayloadBytes)
	}
	log.Printf("responses ws bridge started: path=%s upstream_path=%s model=%q request_bytes=%d conn_id=%s conn_reused=%v pool_mode=%s session_key=%s attempt=%d/%d",
		r.URL.RequestURI(), cfg.ccWSBridgePath, model, len(createBytes), upstream.id, upstream.reused, cfg.ccWSPoolMode, truncateLogValue(sessionKey, 16), attempt, maxAttempts)

	firstFrame, err := readFirstBridgeTextFrame(upstream.conn, upstream.reader, cfg.ccWSFirstEventWait, cfg.ccWSBridgeDebug)
	if err != nil {
		upstream.MarkBroken()
		upstream.Release()
		log.Printf("responses ws bridge first event wait failed: path=%s model=%q wait=%s attempt=%d/%d err=%v",
			r.URL.RequestURI(), model, cfg.ccWSFirstEventWait, attempt, maxAttempts, err)
		return nil, nil, bridgeFirstEventError{err: err}
	}
	return upstream, firstFrame, nil
}

func relayResponsesWebSocketAsSSE(w http.ResponseWriter, upstreamConn net.Conn, upstreamReader *bufio.Reader, debug bool, firstFrame []byte) error {
	flusher, _ := w.(http.Flusher)
	emitFrame := func(payload []byte) (bool, error) {
		eventType, terminal := responsesWSEventType(payload)
		if eventType == "" {
			return false, nil
		}
		if debug {
			log.Printf("responses ws bridge upstream event: type=%q bytes=%d", eventType, len(payload))
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, bytes.TrimSpace(payload)); err != nil {
			return false, err
		}
		if flusher != nil {
			flusher.Flush()
		}
		return terminal, nil
	}
	if len(firstFrame) > 0 {
		done, err := emitFrame(firstFrame)
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
			return err
		}
		switch frame.opcode {
		case 8:
			return nil
		case 9:
			_ = writeWSFrame(upstreamConn, wsFrame{fin: true, opcode: 10, payload: frame.payload}, true)
		case 1:
			done, err := emitFrame(frame.payload)
			if err != nil || done {
				return err
			}
		}
	}
}

func collectResponsesWebSocketResponse(upstreamConn net.Conn, upstreamReader *bufio.Reader, debug bool, firstFrame []byte) (map[string]any, error) {
	var finalResponse map[string]any
	processFrame := func(payload []byte) (bool, error) {
		var event map[string]any
		if err := json.Unmarshal(bytes.TrimSpace(payload), &event); err != nil {
			return false, nil
		}
		eventType := stringOrDefault(event["type"], "")
		if eventType == "" {
			eventType = stringOrDefault(event["event"], "")
		}
		if debug {
			log.Printf("responses ws bridge upstream event: type=%q bytes=%d", eventType, len(payload))
		}
		switch eventType {
		case "response.completed", "response.incomplete":
			if response, _ := event["response"].(map[string]any); response != nil {
				finalResponse = response
				return true, nil
			}
		case "response.failed", "response.error", "error":
			return true, errors.New(upstreamErrorMessage(event))
		}
		return false, nil
	}
	if len(firstFrame) > 0 {
		done, err := processFrame(firstFrame)
		if err != nil {
			return nil, err
		}
		if done {
			return finalResponse, nil
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
			if finalResponse != nil {
				return finalResponse, nil
			}
			return nil, io.EOF
		case 9:
			_ = writeWSFrame(upstreamConn, wsFrame{fin: true, opcode: 10, payload: frame.payload}, true)
		case 1:
			done, err := processFrame(frame.payload)
			if err != nil {
				return nil, err
			}
			if done {
				return finalResponse, nil
			}
		}
	}
	if finalResponse == nil {
		return nil, io.EOF
	}
	return finalResponse, nil
}

func responsesWSEventType(payload []byte) (string, bool) {
	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(payload), &event); err != nil {
		return "", false
	}
	eventType := stringOrDefault(event["type"], "")
	if eventType == "" {
		eventType = stringOrDefault(event["event"], "")
	}
	switch eventType {
	case "response.completed", "response.incomplete", "response.failed", "response.error", "error":
		return eventType, true
	default:
		return eventType, false
	}
}
