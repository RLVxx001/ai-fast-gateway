package main

import (
	"embed"
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

//go:embed admin/*
var adminAssets embed.FS

type adminStatusResponse struct {
	Now           string                 `json:"now"`
	StartedAt     string                 `json:"started_at"`
	Uptime        string                 `json:"uptime"`
	Admin         adminInfo              `json:"admin"`
	Upstream      string                 `json:"upstream"`
	LogFile       string                 `json:"log_file"`
	Bridge        adminBridge            `json:"bridge"`
	Responses     adminResponses         `json:"responses"`
	Policy        adminPolicyResponse    `json:"policy"`
	Pool          bridgeWSPoolSnapshot   `json:"pool"`
	SessionRotate adminSessionRotateInfo `json:"session_rotate"`
}

type adminInfo struct {
	Enabled      bool `json:"enabled"`
	TokenSet     bool `json:"token_set"`
	AuthRequired bool `json:"auth_required"`
}

type adminBridge struct {
	Enabled         bool   `json:"enabled"`
	Path            string `json:"path"`
	FallbackHTTP    bool   `json:"fallback_http"`
	DebugFrames     bool   `json:"debug_frames"`
	MaxAttempts     int    `json:"max_attempts"`
	MaxRequestBytes int    `json:"max_request_bytes"`
	FirstEventWait  string `json:"first_event_wait"`
}

type adminResponses struct {
	Enabled         bool `json:"enabled"`
	FallbackHTTP    bool `json:"fallback_http"`
	MaxAttempts     int  `json:"max_attempts"`
	MaxRequestBytes int  `json:"max_request_bytes"`
}

type adminPolicyItem struct {
	Name        string `json:"name"`
	Status      string `json:"status"`
	Description string `json:"description"`
}

type adminPolicyResponse struct {
	RuntimeOnly bool                `json:"runtime_only"`
	StateFile   string              `json:"state_file"`
	Fields      adminPolicyFields   `json:"fields"`
	Items       []adminPolicyItem   `json:"items"`
	ReadOnly    adminPolicyReadOnly `json:"read_only"`
}

type adminPolicyFields struct {
	UpstreamURL                      string `json:"upstream_url"`
	CCWSBridgePath                   string `json:"cc_ws_bridge_path"`
	CCWSFirstEventTimeoutMS          int    `json:"cc_ws_first_event_timeout_ms"`
	OpenAIResponsesWSEnabled         bool   `json:"openai_responses_ws_enabled"`
	OpenAIResponsesWSMaxRequestBytes int    `json:"openai_responses_ws_max_request_bytes"`
	OpenAIResponsesWSMaxAttempts     int    `json:"openai_responses_ws_max_attempts"`
	OpenAIResponsesWSFallback        bool   `json:"openai_responses_ws_fallback"`
	CCWSBridgeEnabled                bool   `json:"cc_ws_bridge_enabled"`
	CCWSBridgeMaxRequestBytes        int    `json:"cc_ws_bridge_max_request_bytes"`
	CCWSBridgeMaxAttempts            int    `json:"cc_ws_bridge_max_attempts"`
	CCWSBridgeFallback               bool   `json:"cc_ws_bridge_fallback"`
	CCWSSessionRotateEnabled         bool   `json:"cc_ws_session_rotate_enabled"`
	CCWSSessionRotateThreshold       int    `json:"cc_ws_session_rotate_threshold"`
	CCWSBridgeDebugFrames            bool   `json:"cc_ws_bridge_debug_frames"`
	WSDebugPayloadBytes              int    `json:"ws_debug_payload_bytes"`
}

type adminPolicyPatch struct {
	UpstreamURL                      *string `json:"upstream_url"`
	CCWSBridgePath                   *string `json:"cc_ws_bridge_path"`
	CCWSFirstEventTimeoutMS          *int    `json:"cc_ws_first_event_timeout_ms"`
	OpenAIResponsesWSEnabled         *bool   `json:"openai_responses_ws_enabled"`
	OpenAIResponsesWSMaxRequestBytes *int    `json:"openai_responses_ws_max_request_bytes"`
	OpenAIResponsesWSMaxAttempts     *int    `json:"openai_responses_ws_max_attempts"`
	OpenAIResponsesWSFallback        *bool   `json:"openai_responses_ws_fallback"`
	CCWSBridgeEnabled                *bool   `json:"cc_ws_bridge_enabled"`
	CCWSBridgeMaxRequestBytes        *int    `json:"cc_ws_bridge_max_request_bytes"`
	CCWSBridgeMaxAttempts            *int    `json:"cc_ws_bridge_max_attempts"`
	CCWSBridgeFallback               *bool   `json:"cc_ws_bridge_fallback"`
	CCWSSessionRotateEnabled         *bool   `json:"cc_ws_session_rotate_enabled"`
	CCWSSessionRotateThreshold       *int    `json:"cc_ws_session_rotate_threshold"`
	CCWSBridgeDebugFrames            *bool   `json:"cc_ws_bridge_debug_frames"`
	WSDebugPayloadBytes              *int    `json:"ws_debug_payload_bytes"`
}

type adminPolicyReadOnly struct {
	PoolMode        string `json:"pool_mode"`
	PoolMaxConns    int    `json:"pool_max_conns"`
	PoolMaxIdle     int    `json:"pool_max_idle"`
	PoolIdleTTL     string `json:"pool_idle_ttl"`
	PoolAcquireWait string `json:"pool_acquire_timeout"`
}

type adminSessionRotateInfo struct {
	Enabled   bool   `json:"enabled"`
	Threshold int    `json:"threshold"`
	StateFile string `json:"state_file"`
	Entries   int    `json:"entries"`
}

func (p *proxyServer) serveAdmin(w http.ResponseWriter, r *http.Request) {
	cfg := p.currentConfig()
	if !cfg.adminEnabled {
		http.NotFound(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/admin/api/") {
		p.serveAdminAPI(w, r)
		return
	}
	p.serveAdminStatic(w, r)
}

func (p *proxyServer) serveAdminStatic(w http.ResponseWriter, r *http.Request) {
	assets, err := fs.Sub(adminAssets, "admin")
	if err != nil {
		http.Error(w, "admin assets unavailable", http.StatusInternalServerError)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin")
	if path == "" || path == "/" {
		path = "/index.html"
	}
	name := strings.TrimPrefix(path, "/")
	data, err := fs.ReadFile(assets, name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	switch {
	case strings.HasSuffix(name, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(name, ".js"):
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case strings.HasSuffix(name, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	}
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

func (p *proxyServer) serveAdminAPI(w http.ResponseWriter, r *http.Request) {
	if !p.authorizeAdminAPI(w, r) {
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/admin/api")
	switch {
	case path == "/status" && r.Method == http.MethodGet:
		writeAdminJSON(w, p.adminStatus())
	case path == "/logs" && r.Method == http.MethodGet:
		p.serveAdminLogs(w, r)
	case path == "/logs/download" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		p.serveAdminLogDownload(w, r)
	case path == "/logs/query" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
		p.serveAdminLogCommand(w, r)
	case path == "/pool" && r.Method == http.MethodGet:
		writeAdminJSON(w, p.adminPool())
	case path == "/policy" && r.Method == http.MethodGet:
		writeAdminJSON(w, p.adminPolicy())
	case path == "/policy" && r.Method == http.MethodPatch:
		p.serveUpdateAdminPolicy(w, r)
	case path == "/session-rotations" && r.Method == http.MethodGet:
		writeAdminJSON(w, p.adminSessionRotations())
	case strings.HasPrefix(path, "/session-rotations/") && r.Method == http.MethodDelete:
		p.serveDeleteSessionRotation(w, path)
	case strings.HasPrefix(path, "/session-rotations/") && strings.HasSuffix(path, "/rotate") && r.Method == http.MethodPost:
		p.serveForceRotateSession(w, path)
	default:
		http.NotFound(w, r)
	}
}

func (p *proxyServer) authorizeAdminAPI(w http.ResponseWriter, r *http.Request) bool {
	cfg := p.currentConfig()
	if strings.TrimSpace(cfg.adminToken) == "" {
		return true
	}
	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" {
		token = strings.TrimSpace(r.URL.Query().Get("token"))
	}
	if token == cfg.adminToken {
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "admin token required"})
	return false
}

func (p *proxyServer) adminStatus() adminStatusResponse {
	cfg := p.currentConfig()
	now := time.Now()
	entries := 0
	if p.sessionRotator != nil {
		entries = len(p.sessionRotator.list())
	}
	startedAt := p.startedAt
	if startedAt.IsZero() {
		startedAt = now
	}
	return adminStatusResponse{
		Now:       now.Format(time.RFC3339),
		StartedAt: startedAt.Format(time.RFC3339),
		Uptime:    now.Sub(startedAt).Round(time.Second).String(),
		Admin: adminInfo{
			Enabled:      cfg.adminEnabled,
			TokenSet:     cfg.adminToken != "",
			AuthRequired: cfg.adminToken != "",
		},
		Upstream: cfg.upstream.String(),
		LogFile:  cfg.logFile,
		Bridge: adminBridge{
			Enabled:         cfg.ccWSBridgeEnabled,
			Path:            cfg.ccWSBridgePath,
			FallbackHTTP:    cfg.ccWSBridgeFallback,
			DebugFrames:     cfg.ccWSBridgeDebug,
			MaxAttempts:     cfg.ccWSBridgeMaxAttempts,
			MaxRequestBytes: cfg.ccWSBridgeMaxRequestBytes,
			FirstEventWait:  cfg.ccWSFirstEventWait.String(),
		},
		Responses: adminResponses{
			Enabled:         cfg.openAIResponsesWS,
			FallbackHTTP:    cfg.openAIResponsesWSFallback,
			MaxAttempts:     cfg.openAIResponsesWSMaxAttempts,
			MaxRequestBytes: cfg.openAIResponsesWSMaxRequestBytes,
		},
		Policy: p.adminPolicy(),
		Pool:   p.adminPool(),
		SessionRotate: adminSessionRotateInfo{
			Enabled:   cfg.ccWSSessionRotateEnabled,
			Threshold: cfg.ccWSSessionRotateThreshold,
			StateFile: cfg.ccWSSessionRotateStateFile,
			Entries:   entries,
		},
	}
}

func (p *proxyServer) adminPolicy() adminPolicyResponse {
	cfg := p.currentConfig()
	items := []adminPolicyItem{
		{
			Name:        "OpenAI /responses WS bridge",
			Status:      enabledStatus(cfg.openAIResponsesWS),
			Description: "HTTP /responses 请求会优先转换为上游 /responses WebSocket。",
		},
		{
			Name:        "OpenAI /responses 大请求绕过",
			Status:      byteLimitStatus(cfg.openAIResponsesWSMaxRequestBytes),
			Description: "编码后的 response.create 超过阈值时跳过 WS bridge，直接走普通 HTTP 代理，避免上游 101 后 EOF。",
		},
		{
			Name:        "Claude Code WS bridge",
			Status:      enabledStatus(cfg.ccWSBridgeEnabled),
			Description: "Claude Code /v1/messages 转换到上游 /responses WebSocket。",
		},
		{
			Name:        "Claude Code 大请求绕过",
			Status:      byteLimitStatus(cfg.ccWSBridgeMaxRequestBytes),
			Description: "转换后的 response.create 超过阈值时跳过 WS bridge，直接走原始 HTTP /v1/messages 代理。",
		},
		{
			Name:        "首帧失败 session 轮换",
			Status:      enabledStatus(cfg.ccWSSessionRotateEnabled) + ", threshold " + strconv.Itoa(cfg.ccWSSessionRotateThreshold),
			Description: "连续首帧 EOF/超时后替换 prompt_cache_key 和相关 session metadata。",
		},
		{
			Name:        "WS 连接池",
			Status:      cfg.ccWSPoolMode + ", max " + strconv.Itoa(cfg.ccWSPoolMaxConns),
			Description: "按客户端和 session 复用上游 WebSocket，连接同一时间只承载一个请求。",
		},
	}
	return adminPolicyResponse{
		RuntimeOnly: cfg.adminPolicyStateFile == "",
		StateFile:   cfg.adminPolicyStateFile,
		Fields:      adminPolicyFieldsFromConfig(cfg),
		Items:       items,
		ReadOnly: adminPolicyReadOnly{
			PoolMode:        cfg.ccWSPoolMode,
			PoolMaxConns:    cfg.ccWSPoolMaxConns,
			PoolMaxIdle:     cfg.ccWSPoolMaxIdle,
			PoolIdleTTL:     cfg.ccWSPoolIdleTTL.String(),
			PoolAcquireWait: cfg.ccWSPoolAcquireWait.String(),
		},
	}
}

func (p *proxyServer) serveUpdateAdminPolicy(w http.ResponseWriter, r *http.Request) {
	var patch adminPolicyPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		http.Error(w, "invalid policy json", http.StatusBadRequest)
		return
	}
	cfg := p.currentConfig()
	if err := applyAdminPolicyPatch(&cfg, patch); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	p.setConfig(cfg)
	if err := saveAdminRuntimePolicyOverrides(cfg); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("admin runtime policy updated: openai_responses_ws=%v openai_max_bytes=%d openai_attempts=%d cc_ws=%v cc_max_bytes=%d cc_attempts=%d session_rotate=%v session_threshold=%d debug_payload_bytes=%d",
		cfg.openAIResponsesWS, cfg.openAIResponsesWSMaxRequestBytes, cfg.openAIResponsesWSMaxAttempts,
		cfg.ccWSBridgeEnabled, cfg.ccWSBridgeMaxRequestBytes, cfg.ccWSBridgeMaxAttempts,
		cfg.ccWSSessionRotateEnabled, cfg.ccWSSessionRotateThreshold, cfg.wsDebugPayloadBytes)
	writeAdminJSON(w, p.adminPolicy())
}

func adminPolicyFieldsFromConfig(cfg config) adminPolicyFields {
	return adminPolicyFields{
		UpstreamURL:                      cfg.upstream.String(),
		CCWSBridgePath:                   cfg.ccWSBridgePath,
		CCWSFirstEventTimeoutMS:          int(cfg.ccWSFirstEventWait / time.Millisecond),
		OpenAIResponsesWSEnabled:         cfg.openAIResponsesWS,
		OpenAIResponsesWSMaxRequestBytes: cfg.openAIResponsesWSMaxRequestBytes,
		OpenAIResponsesWSMaxAttempts:     cfg.openAIResponsesWSMaxAttempts,
		OpenAIResponsesWSFallback:        cfg.openAIResponsesWSFallback,
		CCWSBridgeEnabled:                cfg.ccWSBridgeEnabled,
		CCWSBridgeMaxRequestBytes:        cfg.ccWSBridgeMaxRequestBytes,
		CCWSBridgeMaxAttempts:            cfg.ccWSBridgeMaxAttempts,
		CCWSBridgeFallback:               cfg.ccWSBridgeFallback,
		CCWSSessionRotateEnabled:         cfg.ccWSSessionRotateEnabled,
		CCWSSessionRotateThreshold:       cfg.ccWSSessionRotateThreshold,
		CCWSBridgeDebugFrames:            cfg.ccWSBridgeDebug,
		WSDebugPayloadBytes:              cfg.wsDebugPayloadBytes,
	}
}

func applyAdminPolicyPatch(cfg *config, patch adminPolicyPatch) error {
	if patch.UpstreamURL != nil {
		upstream, err := parseAdminUpstreamURL(*patch.UpstreamURL)
		if err != nil {
			return err
		}
		cfg.upstream = upstream
	}
	if patch.CCWSBridgePath != nil {
		cfg.ccWSBridgePath = normalizePath(*patch.CCWSBridgePath, "/responses")
	}
	if patch.CCWSFirstEventTimeoutMS != nil {
		cfg.ccWSFirstEventWait = time.Duration(boundedInt(*patch.CCWSFirstEventTimeoutMS, 1000, 120000)) * time.Millisecond
	}
	if patch.OpenAIResponsesWSEnabled != nil {
		cfg.openAIResponsesWS = *patch.OpenAIResponsesWSEnabled
	}
	if patch.OpenAIResponsesWSMaxRequestBytes != nil {
		cfg.openAIResponsesWSMaxRequestBytes = maxInt(*patch.OpenAIResponsesWSMaxRequestBytes, 0)
	}
	if patch.OpenAIResponsesWSMaxAttempts != nil {
		cfg.openAIResponsesWSMaxAttempts = boundedInt(*patch.OpenAIResponsesWSMaxAttempts, 1, 10)
	}
	if patch.OpenAIResponsesWSFallback != nil {
		cfg.openAIResponsesWSFallback = *patch.OpenAIResponsesWSFallback
	}
	if patch.CCWSBridgeEnabled != nil {
		cfg.ccWSBridgeEnabled = *patch.CCWSBridgeEnabled
	}
	if patch.CCWSBridgeMaxRequestBytes != nil {
		cfg.ccWSBridgeMaxRequestBytes = maxInt(*patch.CCWSBridgeMaxRequestBytes, 0)
	}
	if patch.CCWSBridgeMaxAttempts != nil {
		cfg.ccWSBridgeMaxAttempts = boundedInt(*patch.CCWSBridgeMaxAttempts, 1, 10)
	}
	if patch.CCWSBridgeFallback != nil {
		cfg.ccWSBridgeFallback = *patch.CCWSBridgeFallback
	}
	if patch.CCWSSessionRotateEnabled != nil {
		cfg.ccWSSessionRotateEnabled = *patch.CCWSSessionRotateEnabled
	}
	if patch.CCWSSessionRotateThreshold != nil {
		cfg.ccWSSessionRotateThreshold = boundedInt(*patch.CCWSSessionRotateThreshold, 1, 20)
	}
	if patch.CCWSBridgeDebugFrames != nil {
		cfg.ccWSBridgeDebug = *patch.CCWSBridgeDebugFrames
	}
	if patch.WSDebugPayloadBytes != nil {
		cfg.wsDebugPayloadBytes = boundedInt(*patch.WSDebugPayloadBytes, 0, 65536)
	}
	return nil
}

func loadAdminRuntimePolicyOverrides(cfg config) config {
	if strings.TrimSpace(cfg.adminPolicyStateFile) == "" {
		return cfg
	}
	data, err := os.ReadFile(cfg.adminPolicyStateFile)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("admin runtime policy load failed: file=%s err=%v", cfg.adminPolicyStateFile, err)
		}
		return cfg
	}
	var fields adminPolicyFields
	if err := json.Unmarshal(data, &fields); err != nil {
		log.Printf("admin runtime policy decode failed: file=%s err=%v", cfg.adminPolicyStateFile, err)
		return cfg
	}
	patch := adminPolicyPatch{
		OpenAIResponsesWSEnabled:         &fields.OpenAIResponsesWSEnabled,
		OpenAIResponsesWSMaxRequestBytes: &fields.OpenAIResponsesWSMaxRequestBytes,
		OpenAIResponsesWSMaxAttempts:     &fields.OpenAIResponsesWSMaxAttempts,
		OpenAIResponsesWSFallback:        &fields.OpenAIResponsesWSFallback,
		CCWSBridgeEnabled:                &fields.CCWSBridgeEnabled,
		CCWSBridgeMaxRequestBytes:        &fields.CCWSBridgeMaxRequestBytes,
		CCWSBridgeMaxAttempts:            &fields.CCWSBridgeMaxAttempts,
		CCWSBridgeFallback:               &fields.CCWSBridgeFallback,
		CCWSSessionRotateEnabled:         &fields.CCWSSessionRotateEnabled,
		CCWSSessionRotateThreshold:       &fields.CCWSSessionRotateThreshold,
		CCWSBridgeDebugFrames:            &fields.CCWSBridgeDebugFrames,
		WSDebugPayloadBytes:              &fields.WSDebugPayloadBytes,
	}
	if fields.UpstreamURL != "" {
		patch.UpstreamURL = &fields.UpstreamURL
	}
	if fields.CCWSBridgePath != "" {
		patch.CCWSBridgePath = &fields.CCWSBridgePath
	}
	if fields.CCWSFirstEventTimeoutMS > 0 {
		patch.CCWSFirstEventTimeoutMS = &fields.CCWSFirstEventTimeoutMS
	}
	if err := applyAdminPolicyPatch(&cfg, patch); err != nil {
		log.Printf("admin runtime policy apply failed: file=%s err=%v", cfg.adminPolicyStateFile, err)
		return cfg
	}
	log.Printf("admin runtime policy loaded: file=%s", cfg.adminPolicyStateFile)
	return cfg
}

func parseAdminUpstreamURL(value string) (*url.URL, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, errors.New("upstream_url is required")
	}
	upstream, err := url.Parse(value)
	if err != nil {
		return nil, err
	}
	if upstream.Scheme == "" || upstream.Host == "" {
		return nil, errors.New("upstream_url must include scheme and host")
	}
	return upstream, nil
}

func saveAdminRuntimePolicyOverrides(cfg config) error {
	if strings.TrimSpace(cfg.adminPolicyStateFile) == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(cfg.adminPolicyStateFile), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(adminPolicyFieldsFromConfig(cfg), "", "  ")
	if err != nil {
		return err
	}
	tmp := cfg.adminPolicyStateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	if err := os.Rename(tmp, cfg.adminPolicyStateFile); err != nil {
		return err
	}
	return nil
}

func (p *proxyServer) adminPool() bridgeWSPoolSnapshot {
	if p.bridgePool == nil {
		return bridgeWSPoolSnapshot{}
	}
	return p.bridgePool.snapshot()
}

func (p *proxyServer) adminSessionRotations() []bridgeSessionRotationView {
	if p.sessionRotator == nil {
		return nil
	}
	items := p.sessionRotator.list()
	sort.SliceStable(items, func(i, j int) bool {
		return items[i].UpdatedUnix > items[j].UpdatedUnix
	})
	return items
}

func (p *proxyServer) serveDeleteSessionRotation(w http.ResponseWriter, path string) {
	key := strings.Trim(strings.TrimPrefix(path, "/session-rotations/"), "/")
	if strings.Contains(key, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	ok := p.sessionRotator != nil && p.sessionRotator.delete(key)
	writeAdminJSON(w, map[string]any{"deleted": ok})
}

func (p *proxyServer) serveForceRotateSession(w http.ResponseWriter, path string) {
	key := strings.TrimSuffix(strings.TrimPrefix(path, "/session-rotations/"), "/rotate")
	key = strings.Trim(key, "/")
	view, ok := bridgeSessionRotationView{}, false
	if p.sessionRotator != nil {
		view, ok = p.sessionRotator.forceRotate(key)
	}
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeAdminJSON(w, view)
}

func (p *proxyServer) serveAdminLogs(w http.ResponseWriter, r *http.Request) {
	cfg := p.currentConfig()
	query := adminLogQueryFromRequest(r, 2000)
	result, err := readLogLines(cfg.logFile, query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.EqualFold(r.URL.Query().Get("format"), "text") {
		writeAdminText(w, strings.Join(result.Lines, "\n")+"\n", false)
		return
	}
	writeAdminJSON(w, map[string]any{
		"file":    cfg.logFile,
		"grep":    query.Grep,
		"tail":    query.Tail,
		"all":     query.All,
		"matched": result.Matched,
		"total":   result.Total,
		"lines":   result.Lines,
	})
}

func (p *proxyServer) serveAdminLogDownload(w http.ResponseWriter, r *http.Request) {
	cfg := p.currentConfig()
	query := adminLogQueryFromRequest(r, 20000)
	result, err := readLogLines(cfg.logFile, query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	name := "ai-fast-gateway"
	if query.Grep != "" {
		name += "-grep"
	}
	name += "-" + time.Now().Format("20060102-150405") + ".log"
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		return
	}
	writeAdminText(w, strings.Join(result.Lines, "\n")+"\n", true)
}

func (p *proxyServer) serveAdminLogCommand(w http.ResponseWriter, r *http.Request) {
	cfg := p.currentConfig()
	command, err := adminLogCommandFromRequest(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	query, output, err := parseAdminLogCommand(command)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	result, err := readLogLines(cfg.logFile, query)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if output == "text" || strings.EqualFold(r.URL.Query().Get("format"), "text") {
		writeAdminText(w, strings.Join(result.Lines, "\n")+"\n", false)
		return
	}
	writeAdminJSON(w, map[string]any{
		"command": command,
		"file":    cfg.logFile,
		"grep":    query.Grep,
		"tail":    query.Tail,
		"all":     query.All,
		"matched": result.Matched,
		"total":   result.Total,
		"lines":   result.Lines,
	})
}

type adminLogQuery struct {
	Grep string
	Tail int
	All  bool
}

type adminLogResult struct {
	Lines   []string
	Total   int
	Matched int
}

func adminLogQueryFromRequest(r *http.Request, maxTail int) adminLogQuery {
	tail := positiveOrDefault(queryInt(r, "tail", queryInt(r, "lines", 300)), 300)
	if tail > maxTail {
		tail = maxTail
	}
	return adminLogQuery{
		Grep: strings.TrimSpace(r.URL.Query().Get("grep")),
		Tail: tail,
		All:  queryBool(r, "all", false),
	}
}

func adminLogCommandFromRequest(r *http.Request) (string, error) {
	if r.Method == http.MethodGet {
		return strings.TrimSpace(firstNonEmpty(r.URL.Query().Get("q"), r.URL.Query().Get("cmd"))), nil
	}
	var payload struct {
		Query   string `json:"query"`
		Command string `json:"command"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		return "", err
	}
	return strings.TrimSpace(firstNonEmpty(payload.Query, payload.Command)), nil
}

func parseAdminLogCommand(command string) (adminLogQuery, string, error) {
	if command == "" {
		return adminLogQuery{}, "", errors.New("query command is required")
	}
	if strings.ContainsAny(command, ";&`$><") || strings.Contains(command, "$(") {
		return adminLogQuery{}, "", errors.New("unsupported shell operator in query")
	}
	tokens, err := splitAdminLogCommand(command)
	if err != nil {
		return adminLogQuery{}, "", err
	}
	if len(tokens) == 0 {
		return adminLogQuery{}, "", errors.New("query command is required")
	}
	query := adminLogQuery{Tail: 200}
	output := "json"
	for i := 0; i < len(tokens); i++ {
		token := tokens[i]
		switch token {
		case "tail":
			if i+1 < len(tokens) && (tokens[i+1] == "-n" || tokens[i+1] == "-lines") {
				i += 2
				if i >= len(tokens) {
					return query, output, errors.New("tail requires a line count")
				}
				n, err := strconv.Atoi(strings.TrimPrefix(tokens[i], "+"))
				if err != nil {
					return query, output, errors.New("invalid tail line count")
				}
				query.Tail = boundedInt(n, 1, 20000)
				continue
			}
			if i+1 < len(tokens) {
				n, err := strconv.Atoi(strings.TrimPrefix(tokens[i+1], "-n="))
				if err == nil {
					query.Tail = boundedInt(n, 1, 20000)
					i++
					continue
				}
			}
		case "head":
			return query, output, errors.New("head is not supported; use tail -n N")
		case "grep", "rg":
			if i+1 >= len(tokens) {
				return query, output, errors.New("grep requires a pattern")
			}
			i++
			query.Grep = tokens[i]
		case "-n", "--lines", "--tail":
			if i+1 >= len(tokens) {
				return query, output, errors.New(token + " requires a line count")
			}
			i++
			n, err := strconv.Atoi(tokens[i])
			if err != nil {
				return query, output, errors.New("invalid line count")
			}
			query.Tail = boundedInt(n, 1, 20000)
		case "--grep":
			if i+1 >= len(tokens) {
				return query, output, errors.New("--grep requires a pattern")
			}
			i++
			query.Grep = tokens[i]
		case "--all", "all":
			query.All = true
		case "--text", "text":
			output = "text"
		case "--json", "json":
			output = "json"
		case "|":
			continue
		default:
			if strings.HasPrefix(token, "-n") && len(token) > 2 {
				n, err := strconv.Atoi(strings.TrimPrefix(token, "-n"))
				if err != nil {
					return query, output, errors.New("invalid tail line count")
				}
				query.Tail = boundedInt(n, 1, 20000)
				continue
			}
			if strings.HasPrefix(token, "--grep=") {
				query.Grep = strings.TrimPrefix(token, "--grep=")
				continue
			}
			if strings.HasPrefix(token, "--tail=") || strings.HasPrefix(token, "--lines=") {
				raw := strings.TrimPrefix(strings.TrimPrefix(token, "--tail="), "--lines=")
				n, err := strconv.Atoi(raw)
				if err != nil {
					return query, output, errors.New("invalid line count")
				}
				query.Tail = boundedInt(n, 1, 20000)
				continue
			}
			if isLogFileToken(token) {
				continue
			}
			return query, output, errors.New("unsupported token: " + token)
		}
	}
	return query, output, nil
}

func splitAdminLogCommand(command string) ([]string, error) {
	re := regexp.MustCompile(`"([^"\\]*(?:\\.[^"\\]*)*)"|'([^']*)'|(\S+)`)
	matches := re.FindAllStringSubmatch(command, -1)
	tokens := make([]string, 0, len(matches))
	for _, match := range matches {
		switch {
		case match[1] != "":
			value := strings.ReplaceAll(match[1], `\"`, `"`)
			tokens = append(tokens, value)
		case match[2] != "":
			tokens = append(tokens, match[2])
		default:
			tokens = append(tokens, match[3])
		}
	}
	return tokens, nil
}

func isLogFileToken(token string) bool {
	token = strings.TrimSpace(token)
	return token == "" || token == "-" || token == "." || strings.HasSuffix(token, ".log") || strings.Contains(token, "/")
}

func readLogLines(file string, query adminLogQuery) (adminLogResult, error) {
	if file == "" || file == "-" || strings.EqualFold(file, "stdout") {
		return adminLogResult{}, nil
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return adminLogResult{}, err
	}
	raw := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(raw) == 1 && raw[0] == "" {
		raw = nil
	}
	limit := query.Tail
	if query.All {
		limit = len(raw)
	}
	out := make([]string, 0, minInt(limit, len(raw)))
	matched := 0
	for i := len(raw) - 1; i >= 0 && (query.All || len(out) < limit); i-- {
		line := raw[i]
		if query.Grep != "" && !strings.Contains(strings.ToLower(line), strings.ToLower(query.Grep)) {
			continue
		}
		matched++
		out = append(out, line)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return adminLogResult{Lines: out, Total: len(raw), Matched: matched}, nil
}

func queryInt(r *http.Request, key string, fallback int) int {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func queryBool(r *http.Request, key string, fallback bool) bool {
	value := strings.TrimSpace(r.URL.Query().Get(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		switch strings.ToLower(value) {
		case "1", "yes", "y", "on":
			return true
		case "0", "no", "n", "off":
			return false
		default:
			return fallback
		}
	}
	return parsed
}

func writeAdminJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(value)
}

func writeAdminText(w http.ResponseWriter, value string, download bool) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if !download {
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = w.Write([]byte(value))
}

func minInt(a int, b int) int {
	if a < b {
		return a
	}
	return b
}

func boundedInt(value int, minimum int, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func enabledStatus(enabled bool) string {
	if enabled {
		return "enabled"
	}
	return "disabled"
}

func byteLimitStatus(limit int) string {
	if limit <= 0 {
		return "disabled"
	}
	return strconv.Itoa(limit) + " bytes"
}
