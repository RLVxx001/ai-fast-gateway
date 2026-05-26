package main

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestWebSocketProxyInjectsFastIntoOpenAIFrame(t *testing.T) {
	upstreamPayload := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWebSocketUpgrade(r) {
			t.Errorf("expected websocket upgrade")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if got := r.Header.Get("Sec-WebSocket-Extensions"); got != "" {
			t.Errorf("unexpected websocket extensions: %q", got)
		}

		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack upstream: %v", err)
			return
		}
		defer conn.Close()

		accept := websocketAccept(r.Header.Get("Sec-WebSocket-Key"))
		_, _ = fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Accept: %s\r\n\r\n", accept)

		frame, err := readWSFrame(rw.Reader)
		if err != nil {
			t.Errorf("read upstream frame: %v", err)
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(frame.payload, &payload); err != nil {
			t.Errorf("decode upstream payload: %v", err)
			return
		}
		upstreamPayload <- payload

		err = writeWSFrame(conn, wsFrame{
			fin:     true,
			opcode:  1,
			payload: []byte(`{"ok":true}`),
		}, false)
		if err != nil {
			t.Errorf("write upstream frame: %v", err)
		}
	}))
	defer upstream.Close()

	upstreamURL, err := url.Parse(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewServer(&proxyServer{cfg: config{upstream: upstreamURL}})
	defer proxy.Close()

	conn, reader := openTestWebSocket(t, proxy.URL, "/responses")
	defer conn.Close()

	err = writeWSFrame(conn, wsFrame{
		fin:     true,
		opcode:  1,
		payload: []byte(`{"model":"gpt-5.5","stream":true}`),
	}, true)
	if err != nil {
		t.Fatalf("write client frame: %v", err)
	}

	select {
	case payload := <-upstreamPayload:
		if payload["service_tier"] != openAITier {
			t.Fatalf("service_tier = %v, want %q", payload["service_tier"], openAITier)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for upstream payload")
	}

	frame, err := readWSFrame(reader)
	if err != nil {
		t.Fatalf("read response frame: %v", err)
	}
	if string(frame.payload) != `{"ok":true}` {
		t.Fatalf("response payload = %s", frame.payload)
	}
}

func TestInjectFastIntoWebSocketResponseCreateWrapper(t *testing.T) {
	payload := []byte(`{"type":"response.create","response":{"model":"gpt-5.5","stream":true}}`)
	out, changed, err := injectFastIntoWebSocketTextPayload(payload, "openai")
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("expected websocket payload to change")
	}
	var event map[string]any
	if err := json.Unmarshal(out, &event); err != nil {
		t.Fatal(err)
	}
	response := event["response"].(map[string]any)
	if response["service_tier"] != openAITier {
		t.Fatalf("response.service_tier = %v, want %q", response["service_tier"], openAITier)
	}
}

func openTestWebSocket(t *testing.T, serverURL string, path string) (net.Conn, *bufio.Reader) {
	t.Helper()
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", parsed.Host)
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	_, err = fmt.Fprintf(conn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: %s\r\nSec-WebSocket-Extensions: permessage-deflate\r\n\r\n", path, parsed.Host, key)
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}

	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		_ = conn.Close()
		t.Fatalf("status = %d, want 101", resp.StatusCode)
	}
	return conn, reader
}

func websocketAccept(key string) string {
	sum := sha1.Sum([]byte(strings.TrimSpace(key) + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(sum[:])
}
