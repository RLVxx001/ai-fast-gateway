package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestTranslateClaudeToResponsesTextAndToolResult(t *testing.T) {
	req := claudeBridgeRequest{
		Model:  "gpt-5.5",
		System: "You are concise.",
		Messages: []claudeMsg{
			{Role: "user", Content: "hello"},
			{
				Role: "assistant",
				Content: []any{
					map[string]any{"type": "text", "text": "I will check."},
					map[string]any{"type": "tool_use", "id": "toolu_1", "name": "Read", "input": map[string]any{"file_path": "/tmp/a.txt"}},
				},
			},
			{
				Role: "user",
				Content: []any{
					map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "ok"},
					map[string]any{"type": "text", "text": "continue"},
				},
			},
		},
		Tools: []claudeTool{
			{Name: "Read", Description: "Read a file", InputSchema: map[string]any{"type": "object"}},
		},
		ToolChoice:   map[string]any{"type": "auto"},
		OutputConfig: map[string]any{"effort": "max"},
		Stream:       true,
	}

	out, err := translateClaudeToResponses(req, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	if out["instructions"] != "You are concise." {
		t.Fatalf("instructions = %v", out["instructions"])
	}
	if out["prompt_cache_key"] != "session-1" {
		t.Fatalf("prompt_cache_key = %v", out["prompt_cache_key"])
	}
	reasoning := out["reasoning"].(map[string]any)
	if reasoning["effort"] != "xhigh" {
		t.Fatalf("effort = %v", reasoning["effort"])
	}
	input := out["input"].([]map[string]any)
	if len(input) != 5 {
		b, _ := json.Marshal(input)
		t.Fatalf("input len = %d, input=%s", len(input), b)
	}
	if input[1]["type"] != "message" || input[1]["role"] != "assistant" {
		t.Fatalf("assistant text item = %#v", input[1])
	}
	if input[2]["type"] != "function_call" || input[2]["call_id"] != "toolu_1" {
		t.Fatalf("function call item = %#v", input[2])
	}
	if input[3]["type"] != "function_call_output" || input[3]["output"] != "ok" {
		t.Fatalf("tool output item = %#v", input[3])
	}
}

func TestSanitizeBridgeToolArgs(t *testing.T) {
	got := sanitizeBridgeToolArgs("Read", `{"file_path":"/tmp/a.txt","pages":""}`)
	if got != `{"file_path":"/tmp/a.txt"}` {
		t.Fatalf("got %s", got)
	}
	unchanged := sanitizeBridgeToolArgs("Bash", `{"cmd":"ls","pages":""}`)
	if unchanged != `{"cmd":"ls","pages":""}` {
		t.Fatalf("unchanged = %s", unchanged)
	}
}

func TestClaudeWSBridgeStreamsAnthropicSSE(t *testing.T) {
	upstreamCreate := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"))

		frame, err := readWSFrame(rw.Reader)
		if err != nil {
			t.Errorf("read create frame: %v", err)
			return
		}
		var create map[string]any
		if err := json.Unmarshal(frame.payload, &create); err != nil {
			t.Errorf("decode create frame: %v", err)
			return
		}
		upstreamCreate <- create

		events := []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"hello"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":2}}}`,
		}
		for _, event := range events {
			if err := writeWSFrame(conn, wsFrame{fin: true, opcode: 1, payload: []byte(event)}, false); err != nil {
				t.Errorf("write event: %v", err)
				return
			}
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(&proxyServer{cfg: config{
		upstream:           upstreamURL,
		ccWSBridgeEnabled:  true,
		ccWSBridgePath:     "/responses",
		ccWSBridgeFallback: false,
	}})
	defer proxy.Close()

	body := `{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(proxy.URL+"/v1/messages?beta=true", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	select {
	case create := <-upstreamCreate:
		if create["type"] != "response.create" {
			t.Fatalf("create type = %v", create["type"])
		}
		if create["service_tier"] != openAITier {
			t.Fatalf("service_tier = %v, want %q", create["service_tier"], openAITier)
		}
		if create["model"] != "gpt-5.5" {
			t.Fatalf("create model = %v", create["model"])
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for response.create")
	}

	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	got := string(buf)
	for _, want := range []string{"event: message_start", "event: content_block_delta", `"text":"hello"`, "event: message_stop"} {
		if !strings.Contains(got, want) {
			t.Fatalf("response did not contain %q:\n%s", want, got)
		}
	}
}

func TestClaudeWSBridgeHandlesNonStreamRequest(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"))
		if _, err := readWSFrame(rw.Reader); err != nil {
			t.Errorf("read create frame: %v", err)
			return
		}
		events := []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
			`{"type":"response.output_text.delta","output_index":0,"delta":"hello"}`,
			`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
			`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":2}}}`,
		}
		for _, event := range events {
			if err := writeWSFrame(conn, wsFrame{fin: true, opcode: 1, payload: []byte(event)}, false); err != nil {
				t.Errorf("write event: %v", err)
				return
			}
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(&proxyServer{cfg: config{
		upstream:           upstreamURL,
		ccWSBridgeEnabled:  true,
		ccWSBridgePath:     "/responses",
		ccWSBridgeFallback: false,
	}})
	defer proxy.Close()

	body := `{"model":"gpt-5.5","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post(proxy.URL+"/v1/messages?beta=true", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var message map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&message); err != nil {
		t.Fatal(err)
	}
	content := message["content"].([]any)
	text := content[0].(map[string]any)
	if text["text"] != "hello" {
		t.Fatalf("text = %v", text["text"])
	}
}

func TestClaudeWSBridgeClientPoolReusesConnectionSerially(t *testing.T) {
	handshakes := make(chan struct{}, 2)
	upstreamCreates := make(chan map[string]any, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %s", r.URL.Path)
		}
		handshakes <- struct{}{}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"))

		for i := 0; i < 2; i++ {
			frame, err := readWSFrame(rw.Reader)
			if err != nil {
				t.Errorf("read create frame %d: %v", i, err)
				return
			}
			var create map[string]any
			if err := json.Unmarshal(frame.payload, &create); err != nil {
				t.Errorf("decode create frame %d: %v", i, err)
				return
			}
			upstreamCreates <- create
			events := []string{
				`{"type":"response.output_item.added","output_index":0,"item":{"id":"msg_1","type":"message"}}`,
				`{"type":"response.output_text.delta","output_index":0,"delta":"hello"}`,
				`{"type":"response.output_item.done","output_index":0,"item":{"type":"message"}}`,
				`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":2}}}`,
			}
			for _, event := range events {
				if err := writeWSFrame(conn, wsFrame{fin: true, opcode: 1, payload: []byte(event)}, false); err != nil {
					t.Errorf("write event %d: %v", i, err)
					return
				}
			}
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(&proxyServer{cfg: config{
		upstream:           upstreamURL,
		ccWSBridgeEnabled:  true,
		ccWSBridgePath:     "/responses",
		ccWSBridgeFallback: false,
		ccWSPoolMode:       "client",
		ccWSPoolMaxConns:   20,
		ccWSPoolMaxIdle:    20,
		ccWSPoolIdleTTL:    time.Minute,
	}})
	defer proxy.Close()

	for i := 0; i < 2; i++ {
		body := `{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post(proxy.URL+"/v1/messages?beta=true", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		buf, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d body=%s", resp.StatusCode, buf)
		}
		if !strings.Contains(string(buf), `"text":"hello"`) {
			t.Fatalf("response did not contain text: %s", buf)
		}
		select {
		case <-upstreamCreates:
		case <-time.After(3 * time.Second):
			t.Fatal("timed out waiting for upstream create")
		}
	}

	select {
	case <-handshakes:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for first handshake")
	}
	select {
	case <-handshakes:
		t.Fatal("expected pooled websocket to be reused without second handshake")
	default:
	}
}
