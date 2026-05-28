package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

type bridgeWSLease struct {
	conn       net.Conn
	reader     *bufio.Reader
	id         string
	reused     bool
	clientKey  string
	sessionKey string
	pooled     *bridgeWSConn
	broken     bool
}

func (l *bridgeWSLease) MarkBroken() {
	if l != nil {
		l.broken = true
	}
}

func (l *bridgeWSLease) Release() {
	if l == nil {
		return
	}
	if l.pooled == nil {
		_ = l.conn.Close()
		return
	}
	l.pooled.pool.release(l.pooled, l.broken)
}

type bridgeWSConn struct {
	id         string
	conn       net.Conn
	reader     *bufio.Reader
	pool       *bridgeWSClientPool
	sessionKey string
	avail      chan struct{}
	lastUsed   time.Time
}

type bridgeWSPoolManager struct {
	cfg     config
	mu      sync.Mutex
	clients map[string]*bridgeWSClientPool
}

type bridgeWSClientPool struct {
	manager   *bridgeWSPoolManager
	clientKey string

	mu       sync.Mutex
	conns    map[string]*bridgeWSConn
	creating int
}

type bridgeWSPoolSnapshot struct {
	Mode        string                   `json:"mode"`
	MaxConns    int                      `json:"max_conns_per_client"`
	MaxIdle     int                      `json:"max_idle_per_client"`
	IdleTTL     string                   `json:"idle_ttl"`
	AcquireWait string                   `json:"acquire_timeout"`
	ClientCount int                      `json:"client_count"`
	TotalConns  int                      `json:"total_conns"`
	IdleConns   int                      `json:"idle_conns"`
	BusyConns   int                      `json:"busy_conns"`
	Clients     []bridgeWSClientSnapshot `json:"clients"`
}

type bridgeWSClientSnapshot struct {
	ClientKey string                    `json:"client_key"`
	Conns     int                       `json:"conns"`
	Idle      int                       `json:"idle"`
	Busy      int                       `json:"busy"`
	Creating  int                       `json:"creating"`
	Sessions  []bridgeWSSessionSnapshot `json:"sessions"`
}

type bridgeWSSessionSnapshot struct {
	SessionKey string   `json:"session_key"`
	Conns      int      `json:"conns"`
	Idle       int      `json:"idle"`
	Busy       int      `json:"busy"`
	LastUsed   string   `json:"last_used"`
	ConnIDs    []string `json:"conn_ids"`
}

func newBridgeWSPoolManager(cfg config) *bridgeWSPoolManager {
	return &bridgeWSPoolManager{
		cfg:     cfg,
		clients: map[string]*bridgeWSClientPool{},
	}
}

func (m *bridgeWSPoolManager) updateConfig(cfg config) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.cfg = cfg
	m.mu.Unlock()
}

func (m *bridgeWSPoolManager) reset() {
	if m == nil {
		return
	}
	m.mu.Lock()
	pools := make([]*bridgeWSClientPool, 0, len(m.clients))
	for _, pool := range m.clients {
		pools = append(pools, pool)
	}
	m.clients = map[string]*bridgeWSClientPool{}
	m.mu.Unlock()
	for _, pool := range pools {
		pool.closeAll()
	}
}

func (m *bridgeWSPoolManager) currentConfig() config {
	if m == nil {
		return config{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cfg
}

func (p *proxyServer) acquireBridgeWebSocket(r *http.Request, sessionKey string) (*bridgeWSLease, error) {
	cfg := p.currentConfig()
	if cfg.ccWSPoolMode == "request" {
		conn, reader, err := p.openBridgeWebSocket(r)
		if err != nil {
			return nil, err
		}
		return &bridgeWSLease{conn: conn, reader: reader, id: "request_" + randomHex(6), sessionKey: sessionKey}, nil
	}
	if p.bridgePool == nil {
		p.bridgePool = newBridgeWSPoolManager(cfg)
	}
	return p.bridgePool.acquire(r.Context(), p, r, sessionKey)
}

func (m *bridgeWSPoolManager) acquire(ctx context.Context, proxy *proxyServer, r *http.Request, sessionKey string) (*bridgeWSLease, error) {
	cfg := m.currentConfig()
	clientKey := bridgeWSClientKey(r)
	sessionKey = firstNonEmpty(sessionKey, "default")
	pool := m.clientPool(clientKey)
	timeout := cfg.ccWSPoolAcquireWait
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	acquireCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return pool.acquire(acquireCtx, proxy, r, sessionKey)
}

func (m *bridgeWSPoolManager) clientPool(clientKey string) *bridgeWSClientPool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if pool := m.clients[clientKey]; pool != nil {
		return pool
	}
	pool := &bridgeWSClientPool{
		manager:   m,
		clientKey: clientKey,
		conns:     map[string]*bridgeWSConn{},
	}
	m.clients[clientKey] = pool
	return pool
}

func (m *bridgeWSPoolManager) snapshot() bridgeWSPoolSnapshot {
	if m == nil {
		return bridgeWSPoolSnapshot{}
	}
	m.mu.Lock()
	pools := make([]*bridgeWSClientPool, 0, len(m.clients))
	for _, pool := range m.clients {
		pools = append(pools, pool)
	}
	m.mu.Unlock()
	cfg := m.currentConfig()

	snapshot := bridgeWSPoolSnapshot{
		Mode:        cfg.ccWSPoolMode,
		MaxConns:    cfg.ccWSPoolMaxConns,
		MaxIdle:     cfg.ccWSPoolMaxIdle,
		IdleTTL:     cfg.ccWSPoolIdleTTL.String(),
		AcquireWait: cfg.ccWSPoolAcquireWait.String(),
		ClientCount: len(pools),
		Clients:     make([]bridgeWSClientSnapshot, 0, len(pools)),
	}
	for _, pool := range pools {
		client := pool.snapshot()
		snapshot.TotalConns += client.Conns
		snapshot.IdleConns += client.Idle
		snapshot.BusyConns += client.Busy
		snapshot.Clients = append(snapshot.Clients, client)
	}
	sort.SliceStable(snapshot.Clients, func(i, j int) bool {
		return snapshot.Clients[i].ClientKey < snapshot.Clients[j].ClientKey
	})
	return snapshot
}

func (p *bridgeWSClientPool) snapshot() bridgeWSClientSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()

	bySession := map[string]*bridgeWSSessionSnapshot{}
	client := bridgeWSClientSnapshot{
		ClientKey: p.clientKey,
		Conns:     len(p.conns),
		Creating:  p.creating,
	}
	for _, conn := range p.conns {
		idle := len(conn.avail) > 0
		if idle {
			client.Idle++
		} else {
			client.Busy++
		}
		session := bySession[conn.sessionKey]
		if session == nil {
			session = &bridgeWSSessionSnapshot{SessionKey: conn.sessionKey}
			bySession[conn.sessionKey] = session
		}
		session.Conns++
		if idle {
			session.Idle++
		} else {
			session.Busy++
		}
		if session.LastUsed == "" {
			session.LastUsed = conn.lastUsed.Format(time.RFC3339)
		}
		session.ConnIDs = append(session.ConnIDs, conn.id)
	}
	for _, session := range bySession {
		sort.Strings(session.ConnIDs)
		client.Sessions = append(client.Sessions, *session)
	}
	sort.SliceStable(client.Sessions, func(i, j int) bool {
		return client.Sessions[i].SessionKey < client.Sessions[j].SessionKey
	})
	return client
}

func (p *bridgeWSClientPool) acquire(ctx context.Context, proxy *proxyServer, r *http.Request, sessionKey string) (*bridgeWSLease, error) {
	started := time.Now()
	for {
		p.mu.Lock()
		p.cleanupLocked(time.Now())
		if conn := p.tryAcquireIdleLocked(sessionKey); conn != nil {
			p.mu.Unlock()
			log.Printf("cc ws pool lease acquired: client_key=%s session_key=%s conn_id=%s reused=true acquire_ms=%d",
				truncateLogValue(p.clientKey, 32), truncateLogValue(sessionKey, 16), conn.id, time.Since(started).Milliseconds())
			return &bridgeWSLease{conn: conn.conn, reader: conn.reader, id: conn.id, reused: true, clientKey: p.clientKey, sessionKey: sessionKey, pooled: conn}, nil
		}
		maxConns := positiveOrDefault(p.manager.currentConfig().ccWSPoolMaxConns, 20)
		if len(p.conns)+p.creating >= maxConns {
			if evicted := p.evictOldestIdleOtherSessionLocked(sessionKey); evicted != nil {
				p.mu.Unlock()
				_ = evicted.Close()
				continue
			}
		}
		if len(p.conns)+p.creating < maxConns {
			p.creating++
			p.mu.Unlock()

			conn, reader, err := proxy.openBridgeWebSocket(r)

			p.mu.Lock()
			p.creating--
			if err != nil {
				p.mu.Unlock()
				return nil, err
			}
			pooled := &bridgeWSConn{
				id:         "ws_" + randomHex(8),
				conn:       conn,
				reader:     reader,
				pool:       p,
				sessionKey: sessionKey,
				avail:      make(chan struct{}, 1),
				lastUsed:   time.Now(),
			}
			p.conns[pooled.id] = pooled
			p.mu.Unlock()
			log.Printf("cc ws pool lease acquired: client_key=%s session_key=%s conn_id=%s reused=false acquire_ms=%d",
				truncateLogValue(p.clientKey, 32), truncateLogValue(sessionKey, 16), pooled.id, time.Since(started).Milliseconds())
			return &bridgeWSLease{conn: conn, reader: reader, id: pooled.id, clientKey: p.clientKey, sessionKey: sessionKey, pooled: pooled}, nil
		}
		p.mu.Unlock()

		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (p *bridgeWSClientPool) tryAcquireIdleLocked(sessionKey string) *bridgeWSConn {
	if len(p.conns) == 0 {
		return nil
	}
	conns := make([]*bridgeWSConn, 0, len(p.conns))
	for _, conn := range p.conns {
		conns = append(conns, conn)
	}
	sort.SliceStable(conns, func(i, j int) bool {
		return conns[i].lastUsed.Before(conns[j].lastUsed)
	})
	for _, conn := range conns {
		if conn.sessionKey != sessionKey {
			continue
		}
		select {
		case <-conn.avail:
			return conn
		default:
		}
	}
	return nil
}

func (p *bridgeWSClientPool) release(conn *bridgeWSConn, broken bool) {
	if conn == nil {
		return
	}
	var closeConn net.Conn
	p.mu.Lock()
	if broken {
		delete(p.conns, conn.id)
		closeConn = conn.conn
	} else {
		conn.lastUsed = time.Now()
		select {
		case conn.avail <- struct{}{}:
		default:
		}
		closeConn = p.cleanupLocked(time.Now())
	}
	p.mu.Unlock()
	if closeConn != nil {
		_ = closeConn.Close()
	}
	log.Printf("cc ws pool lease released: client_key=%s conn_id=%s broken=%v",
		truncateLogValue(p.clientKey, 32), conn.id, broken)
}

func (p *bridgeWSClientPool) closeAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	conns := make([]net.Conn, 0, len(p.conns))
	for _, conn := range p.conns {
		conns = append(conns, conn.conn)
	}
	p.conns = map[string]*bridgeWSConn{}
	p.creating = 0
	p.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Close()
	}
}

func (p *bridgeWSClientPool) evictOldestIdleOtherSessionLocked(sessionKey string) net.Conn {
	var victim *bridgeWSConn
	for _, conn := range p.conns {
		if conn.sessionKey == sessionKey || len(conn.avail) == 0 {
			continue
		}
		if victim == nil || conn.lastUsed.Before(victim.lastUsed) {
			victim = conn
		}
	}
	if victim == nil {
		return nil
	}
	select {
	case <-victim.avail:
		delete(p.conns, victim.id)
		log.Printf("cc ws pool evicted idle other-session conn: client_key=%s session_key=%s conn_id=%s",
			truncateLogValue(p.clientKey, 32), truncateLogValue(victim.sessionKey, 16), victim.id)
		return victim.conn
	default:
		return nil
	}
}

func (p *bridgeWSClientPool) cleanupLocked(now time.Time) net.Conn {
	var closeConn net.Conn
	cfg := p.manager.currentConfig()
	ttl := cfg.ccWSPoolIdleTTL
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	for id, conn := range p.conns {
		if closeConn != nil {
			break
		}
		if now.Sub(conn.lastUsed) < ttl {
			continue
		}
		select {
		case <-conn.avail:
			delete(p.conns, id)
			closeConn = conn.conn
		default:
		}
	}
	maxIdle := cfg.ccWSPoolMaxIdle
	if maxIdle < 0 {
		maxIdle = 0
	}
	if closeConn != nil {
		return closeConn
	}
	idle := make([]*bridgeWSConn, 0, len(p.conns))
	for _, conn := range p.conns {
		if len(conn.avail) > 0 {
			idle = append(idle, conn)
		}
	}
	if len(idle) <= maxIdle {
		return nil
	}
	sort.SliceStable(idle, func(i, j int) bool {
		return idle[i].lastUsed.Before(idle[j].lastUsed)
	})
	victim := idle[0]
	select {
	case <-victim.avail:
		delete(p.conns, victim.id)
		return victim.conn
	default:
		return nil
	}
}

func bridgeWSClientKey(r *http.Request) string {
	ip := firstNonEmpty(firstForwardedFor(r.Header.Get("X-Forwarded-For")), r.Header.Get("X-Real-IP"), remoteHost(r.RemoteAddr), "unknown")
	authHash := shortHash(firstNonEmpty(r.Header.Get("Authorization"), r.Header.Get("x-api-key"), r.Header.Get("X-Api-Key"), "no-auth"))
	uaHash := shortHash(firstNonEmpty(r.Header.Get("User-Agent"), "no-ua"))
	return "ip=" + ip + "|auth=" + authHash + "|ua=" + uaHash
}

func bridgeWSSessionKey(r *http.Request, responseReq map[string]any) string {
	if sessionID := strings.TrimSpace(r.Header.Get("x-claude-code-session-id")); sessionID != "" {
		return sessionID
	}
	if key := stringOrDefault(responseReq["prompt_cache_key"], ""); key != "" {
		return key
	}
	return "default"
}

func firstForwardedFor(value string) string {
	if value == "" {
		return ""
	}
	return strings.TrimSpace(strings.Split(value, ",")[0])
}

func remoteHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err == nil {
		return host
	}
	return strings.TrimSpace(remoteAddr)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

func truncateLogValue(value string, maxLen int) string {
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + "..."
}
