package egressroute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/egressproxy"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/metrics"
)

var (
	ErrRateLimited       = errors.New("route concurrency limit reached")
	ErrCapacityExhausted = errors.New("egress capacity exhausted")
	ErrTargetNotAllowed  = errors.New("target not allowed")
	ErrDraining          = errors.New("egress route manager is draining")
)

type ListenerFactory interface {
	Listen(port uint32) (net.Listener, error)
}

type TCPListenerFactory struct {
	Host string
}

func (f TCPListenerFactory) Listen(port uint32) (net.Listener, error) {
	host := f.Host
	if host == "" {
		host = "127.0.0.1"
	}
	return net.Listen("tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
}

type Config struct {
	PortStart               uint32
	PortEnd                 uint32
	PortCooldown            time.Duration
	RouteIdleTTL            time.Duration
	LeaseTTL                time.Duration
	ConnectTimeout          time.Duration
	IdleTimeout             time.Duration
	MaxActiveRoutes         int
	MaxActiveLeases         int
	DefaultRouteConcurrency int
	MaxRouteConcurrency     int
	AllowedTargets          []string
	CopyBufferSize          int
	Metrics                 *metrics.Metrics
}

type RouteRequest struct {
	ClientScope      string
	TenantID         string
	AccountID        string
	RequestID        string
	Nonce            string
	TargetHost       string
	TargetPort       uint16
	Proxy            egressproxy.Config
	AllowedTargets   []string
	RouteConcurrency int
}

type Manager struct {
	cfg       Config
	factory   ListenerFactory
	connector egressproxy.Connector
	m         *metrics.Metrics
	log       *slog.Logger

	mu           sync.Mutex
	draining     bool
	routes       map[string]*routeEntry
	ports        map[uint32]portState
	activeLeases int
	nextPort     uint32
	allowed      map[string]struct{}
}

type portState struct {
	active        bool
	cooldownUntil time.Time
}

type routeEntry struct {
	key         string
	proxy       egressproxy.Config
	targetHost  string
	targetPort  uint16
	active      int
	lastUsed    time.Time
	concurrency int
	draining    bool
}

type Lease struct {
	manager *Manager
	route   *routeEntry
	port    uint32
	lane    *lane
	once    sync.Once
}

type lane struct {
	listener net.Listener
	cancel   context.CancelFunc
	enclave  atomic.Value
	upstream atomic.Value
}

func New(cfg Config, factory ListenerFactory, log *slog.Logger) *Manager {
	if cfg.CopyBufferSize <= 0 {
		cfg.CopyBufferSize = 64 * 1024
	}
	if cfg.DefaultRouteConcurrency <= 0 {
		cfg.DefaultRouteConcurrency = 1
	}
	if cfg.MaxRouteConcurrency <= 0 {
		cfg.MaxRouteConcurrency = cfg.DefaultRouteConcurrency
	}
	if log == nil {
		log = slog.Default()
	}
	allowed := make(map[string]struct{}, len(cfg.AllowedTargets))
	for _, target := range cfg.AllowedTargets {
		allowed[strings.ToLower(strings.TrimSpace(target))] = struct{}{}
	}
	return &Manager{
		cfg:       cfg,
		factory:   factory,
		connector: egressproxy.Connector{Timeout: cfg.ConnectTimeout},
		m:         cfg.Metrics,
		log:       log,
		routes:    make(map[string]*routeEntry),
		ports:     make(map[uint32]portState),
		nextPort:  cfg.PortStart,
		allowed:   allowed,
	}
}

func (m *Manager) Acquire(ctx context.Context, req RouteRequest) (*Lease, error) {
	target := strings.ToLower(net.JoinHostPort(req.TargetHost, strconv.Itoa(int(req.TargetPort))))
	if _, ok := m.allowed[target]; !ok {
		m.m.IncEgressRouteFailure("target_not_allowed")
		return nil, ErrTargetNotAllowed
	}
	if len(req.AllowedTargets) > 0 && !targetAllowed(target, req.AllowedTargets) {
		m.m.IncEgressRouteFailure("target_not_allowed")
		return nil, ErrTargetNotAllowed
	}
	key := routeKey(req)
	now := time.Now()

	m.mu.Lock()
	if m.draining {
		m.observeGaugesLocked()
		m.mu.Unlock()
		m.m.IncEgressRouteFailure("draining")
		return nil, ErrDraining
	}
	m.purgeIdleLocked(now)
	if m.activeLeases >= m.cfg.MaxActiveLeases {
		m.observeGaugesLocked()
		m.mu.Unlock()
		m.m.IncEgressRouteFailure("capacity_exhausted")
		return nil, ErrCapacityExhausted
	}
	route := m.routes[key]
	if route == nil {
		if len(m.routes) >= m.cfg.MaxActiveRoutes && !m.evictIdleLocked(now) {
			m.observeGaugesLocked()
			m.mu.Unlock()
			m.m.IncEgressRouteFailure("capacity_exhausted")
			return nil, ErrCapacityExhausted
		}
		route = &routeEntry{
			key:         key,
			proxy:       req.Proxy,
			targetHost:  req.TargetHost,
			targetPort:  req.TargetPort,
			lastUsed:    now,
			concurrency: routeConcurrency(req.RouteConcurrency, m.cfg.DefaultRouteConcurrency, m.cfg.MaxRouteConcurrency),
		}
		m.routes[key] = route
	}
	if route.draining {
		m.observeGaugesLocked()
		m.mu.Unlock()
		m.m.IncEgressRouteFailure("draining")
		return nil, ErrDraining
	}
	if route.active >= route.concurrency {
		m.observeGaugesLocked()
		m.mu.Unlock()
		m.m.IncEgressRouteFailure("rate_limited")
		return nil, ErrRateLimited
	}
	port, ok := m.allocatePortLocked(now)
	if !ok {
		m.observeGaugesLocked()
		m.mu.Unlock()
		m.m.IncEgressRouteFailure("capacity_exhausted")
		return nil, ErrCapacityExhausted
	}
	route.active++
	route.lastUsed = now
	m.activeLeases++
	m.observeGaugesLocked()
	m.mu.Unlock()

	listener, err := m.factory.Listen(port)
	if err != nil {
		m.m.IncEgressRouteFailure("listen_failed")
		m.releaseCounts(route, port)
		return nil, fmt.Errorf("listen lane port %d: %w", port, err)
	}
	laneCtx, cancel := context.WithCancel(ctx)
	ln := &lane{listener: listener, cancel: cancel}
	lease := &Lease{manager: m, route: route, port: port, lane: ln}
	go m.serveLane(laneCtx, lease, req)
	return lease, nil
}

func (m *Manager) Shutdown() {
	m.mu.Lock()
	m.draining = true
	for _, route := range m.routes {
		route.draining = true
	}
	m.mu.Unlock()
}

func (l *Lease) Port() uint32 {
	if l == nil {
		return 0
	}
	return l.port
}

func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		if l.lane.cancel != nil {
			l.lane.cancel()
		}
		if l.lane.listener != nil {
			_ = l.lane.listener.Close()
		}
		closeConnValue(&l.lane.enclave)
		closeConnValue(&l.lane.upstream)
		l.manager.releaseCounts(l.route, l.port)
	})
}

func (m *Manager) serveLane(ctx context.Context, lease *Lease, req RouteRequest) {
	timer := time.NewTimer(m.cfg.LeaseTTL)
	defer timer.Stop()
	accepted := make(chan net.Conn)
	errCh := make(chan error, 1)
	go func() {
		c, err := lease.lane.listener.Accept()
		if err != nil {
			errCh <- err
			return
		}
		select {
		case accepted <- c:
		case <-ctx.Done():
			m.m.IncEgressLateConnection()
			_ = c.Close()
		}
	}()
	var enclave net.Conn
	select {
	case <-ctx.Done():
		lease.Release()
		return
	case <-timer.C:
		lease.Release()
		return
	case err := <-errCh:
		if !errors.Is(err, net.ErrClosed) {
			m.log.Warn("lane accept failed", "port", lease.port, "request_id", req.RequestID, "error", err)
		}
		lease.Release()
		return
	case enclave = <-accepted:
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	_ = lease.lane.listener.Close()
	lease.lane.enclave.Store(enclave)
	targetCtx, cancel := context.WithTimeout(ctx, m.cfg.ConnectTimeout)
	upstream, err := m.connector.Connect(targetCtx, req.TargetHost, req.TargetPort, req.Proxy)
	cancel()
	if err != nil {
		m.log.Warn("egress connect failed", "target", net.JoinHostPort(req.TargetHost, strconv.Itoa(int(req.TargetPort))), "proxy", req.Proxy.RedactedKey, "request_id", req.RequestID, "error", err)
		m.m.IncEgressProxyFailure(proxyFailureReason(err))
		lease.Release()
		return
	}
	lease.lane.upstream.Store(upstream)
	m.copyTunnel(lease, enclave, upstream)
}

func (m *Manager) copyTunnel(lease *Lease, enclave, upstream net.Conn) {
	done := make(chan copyResult, 2)
	copyOne := func(direction string, dst, src net.Conn) {
		buf := make([]byte, m.cfg.CopyBufferSize)
		n, _ := io.CopyBuffer(
			idleWriteConn{Conn: dst, timeout: m.cfg.IdleTimeout},
			idleReadConn{Conn: src, timeout: m.cfg.IdleTimeout},
			buf,
		)
		_ = dst.Close()
		_ = src.Close()
		done <- copyResult{direction: direction, bytes: uint64(n)}
	}
	go copyOne("enclave_to_upstream", upstream, enclave)
	go copyOne("upstream_to_enclave", enclave, upstream)
	first := <-done
	m.m.AddEgressBytes(first.direction, first.bytes)
	lease.Release()
	second := <-done
	m.m.AddEgressBytes(second.direction, second.bytes)
}

type copyResult struct {
	direction string
	bytes     uint64
}

func (m *Manager) releaseCounts(route *routeEntry, port uint32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if route.active > 0 {
		route.active--
	}
	route.lastUsed = time.Now()
	if m.activeLeases > 0 {
		m.activeLeases--
	}
	m.ports[port] = portState{cooldownUntil: time.Now().Add(m.cfg.PortCooldown)}
	m.observeGaugesLocked()
}

func (m *Manager) allocatePortLocked(now time.Time) (uint32, bool) {
	total := int(m.cfg.PortEnd-m.cfg.PortStart) + 1
	for i := 0; i < total; i++ {
		port := m.nextPort
		if port < m.cfg.PortStart || port > m.cfg.PortEnd {
			port = m.cfg.PortStart
		}
		m.nextPort = port + 1
		if state, used := m.ports[port]; used && (state.active || now.Before(state.cooldownUntil)) {
			continue
		}
		m.ports[port] = portState{active: true}
		return port, true
	}
	return 0, false
}

func (m *Manager) purgeIdleLocked(now time.Time) {
	if m.cfg.RouteIdleTTL <= 0 {
		return
	}
	for key, route := range m.routes {
		if route.active == 0 && now.Sub(route.lastUsed) > m.cfg.RouteIdleTTL {
			delete(m.routes, key)
		}
	}
	for port, state := range m.ports {
		if !state.active && !state.cooldownUntil.IsZero() && now.After(state.cooldownUntil) {
			delete(m.ports, port)
		}
	}
}

func (m *Manager) evictIdleLocked(now time.Time) bool {
	var oldest *routeEntry
	for _, route := range m.routes {
		if route.active != 0 {
			continue
		}
		if oldest == nil || route.lastUsed.Before(oldest.lastUsed) {
			oldest = route
		}
	}
	if oldest == nil {
		return false
	}
	delete(m.routes, oldest.key)
	oldest.draining = true
	oldest.lastUsed = now
	return true
}

func (m *Manager) observeGaugesLocked() {
	if m.m == nil {
		return
	}
	var activeRoutes, idleRoutes, activePorts int
	for _, route := range m.routes {
		if route.active > 0 {
			activeRoutes++
		} else {
			idleRoutes++
		}
	}
	for _, state := range m.ports {
		if state.active {
			activePorts++
		}
	}
	m.m.SetEgressRouteGauges(activeRoutes, idleRoutes, m.activeLeases, activePorts)
}

func proxyFailureReason(err error) string {
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "returned") ||
		strings.Contains(msg, "authentication") ||
		strings.Contains(msg, "did not accept") ||
		strings.Contains(msg, "failed with reply code") {
		return "rejected"
	}
	return "connect"
}

func routeKey(req RouteRequest) string {
	parts := []string{
		normalizeScope(req.ClientScope),
		normalizeScope(req.TenantID),
		normalizeScope(req.AccountID),
		strings.ToLower(req.TargetHost),
		strconv.Itoa(int(req.TargetPort)),
		req.Proxy.RedactedKey,
		req.Proxy.SecretHash,
	}
	return strings.Join(parts, "\x00")
}

func normalizeScope(v string) string {
	return strings.TrimSpace(v)
}

func minPositive(a, b int) int {
	if a <= 0 {
		return b
	}
	if b <= 0 || a < b {
		return a
	}
	return b
}

func routeConcurrency(requested, def, max int) int {
	if requested <= 0 {
		requested = def
	}
	return minPositive(requested, max)
}

func targetAllowed(target string, allowed []string) bool {
	for _, item := range allowed {
		if strings.EqualFold(strings.TrimSpace(item), target) {
			return true
		}
	}
	return false
}

func closeConnValue(v *atomic.Value) {
	if c, ok := v.Load().(net.Conn); ok && c != nil {
		_ = c.Close()
	}
}

type idleReadConn struct {
	net.Conn
	timeout time.Duration
}

func (c idleReadConn) Read(p []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(p)
}

type idleWriteConn struct {
	net.Conn
	timeout time.Duration
}

func (c idleWriteConn) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Write(p)
}
