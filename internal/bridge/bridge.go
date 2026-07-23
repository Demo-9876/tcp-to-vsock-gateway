package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/metrics"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/vsockdial"
)

type Config struct {
	VsockCID          uint32
	VsockPort         uint32
	ConnectTimeout    time.Duration
	IdleTimeout       time.Duration
	MaxConnLifetime   time.Duration
	MaxConns          int
	TCPKeepAlive      time.Duration
	ShutdownTimeout   time.Duration
	CopyBufferSize    int
	ShutdownStartedFn func()
}

type Server struct {
	cfg    Config
	dialer vsockdial.Dialer
	m      *metrics.Metrics
	log    *slog.Logger

	draining       atomic.Bool
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc
	listener       net.Listener
	listenerMu     sync.Mutex
	sem            chan struct{}
	wg             sync.WaitGroup
	wgMu           sync.Mutex
	conns          sync.Map
}

func New(cfg Config, dialer vsockdial.Dialer, m *metrics.Metrics, log *slog.Logger) *Server {
	if cfg.CopyBufferSize <= 0 {
		cfg.CopyBufferSize = 64 * 1024
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 1
	}
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	return &Server{
		cfg:            cfg,
		dialer:         dialer,
		m:              m,
		log:            log,
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
		sem:            make(chan struct{}, cfg.MaxConns),
	}
}

func (s *Server) Serve(l net.Listener) error {
	s.listenerMu.Lock()
	s.listener = l
	if s.draining.Load() {
		_ = l.Close()
	}
	s.listenerMu.Unlock()

	for {
		c, err := l.Accept()
		if err != nil {
			if s.draining.Load() || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		if s.draining.Load() {
			s.m.IncRejected("shutdown")
			_ = c.Close()
			continue
		}
		select {
		case s.sem <- struct{}{}:
		default:
			s.m.IncRejected("limit")
			_ = c.Close()
			continue
		}
		s.wgMu.Lock()
		if s.draining.Load() {
			s.wgMu.Unlock()
			<-s.sem
			s.m.IncRejected("shutdown")
			_ = c.Close()
			continue
		}
		s.wg.Add(1)
		s.wgMu.Unlock()
		s.m.IncConnectionsTotal()
		go s.handle(c)
	}
}

func (s *Server) Shutdown(ctx context.Context) error {
	if s.draining.CompareAndSwap(false, true) {
		s.shutdownCancel()
		if s.cfg.ShutdownStartedFn != nil {
			s.cfg.ShutdownStartedFn()
		}
		s.listenerMu.Lock()
		if s.listener != nil {
			_ = s.listener.Close()
		}
		s.listenerMu.Unlock()
	}
	done := make(chan struct{})
	go func() {
		s.wgMu.Lock()
		s.wgMu.Unlock()
		s.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		s.closeActiveConns()
		select {
		case <-done:
			return nil
		case <-time.After(5 * time.Second):
		}
		return ctx.Err()
	}
}

func (s *Server) handle(tcp net.Conn) {
	defer s.wg.Done()
	defer func() { <-s.sem }()
	s.trackConn(tcp)
	defer s.untrackConn(tcp)
	start := time.Now()
	remote := tcp.RemoteAddr().String()
	if s.draining.Load() {
		s.m.IncRejected("shutdown")
		_ = tcp.Close()
		return
	}
	if ka := s.cfg.TCPKeepAlive; ka > 0 {
		if c, ok := tcp.(*net.TCPConn); ok {
			_ = c.SetKeepAlive(true)
			_ = c.SetKeepAlivePeriod(ka)
		}
	}

	ctx, cancel := context.WithTimeout(s.shutdownCtx, s.cfg.ConnectTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		s.m.IncRejected("shutdown")
		_ = tcp.Close()
		return
	}
	vsock, err := s.dialer.Dial(ctx, s.cfg.VsockCID, s.cfg.VsockPort)
	if err != nil {
		s.m.IncVsockDial(false)
		s.log.Warn("vsock dial failed", "remote", remote, "cid", s.cfg.VsockCID, "port", s.cfg.VsockPort, "error", err)
		_ = tcp.Close()
		return
	}
	s.m.IncVsockDial(true)
	s.trackConn(vsock)
	defer s.untrackConn(vsock)

	s.m.IncConnectionsActive()
	defer func() {
		s.m.DecConnectionsActive()
		s.m.ObserveConnectionDuration(time.Since(start))
	}()

	if s.cfg.MaxConnLifetime > 0 {
		timer := time.AfterFunc(s.cfg.MaxConnLifetime, func() {
			_ = tcp.Close()
			_ = vsock.Close()
		})
		defer timer.Stop()
	}

	errCh := make(chan copyResult, 2)
	go s.copyDirection(errCh, "tcp_to_vsock", vsock, tcp)
	go s.copyDirection(errCh, "vsock_to_tcp", tcp, vsock)

	first := <-errCh
	if first.err != nil {
		s.m.IncCopyError()
		_ = tcp.Close()
		_ = vsock.Close()
		s.log.Warn("copy failed", "remote", remote, "direction", first.direction, "bytes", first.bytes, "error", first.err)
		<-errCh
		return
	}

	if first.direction == "tcp_to_vsock" {
		closeWrite(vsock)
		second := <-errCh
		if second.err != nil {
			s.m.IncCopyError()
			s.log.Warn("copy failed", "remote", remote, "direction", second.direction, "bytes", second.bytes, "error", second.err)
		}
		_ = tcp.Close()
		_ = vsock.Close()
		return
	}

	_ = tcp.Close()
	_ = vsock.Close()
	<-errCh
}

func (s *Server) trackConn(c net.Conn) {
	s.conns.Store(c, struct{}{})
}

func (s *Server) untrackConn(c net.Conn) {
	s.conns.Delete(c)
}

func (s *Server) closeActiveConns() {
	s.conns.Range(func(key, _ any) bool {
		if c, ok := key.(net.Conn); ok {
			_ = c.Close()
		}
		return true
	})
}

type copyResult struct {
	direction string
	bytes     int64
	err       error
}

func (s *Server) copyDirection(ch chan<- copyResult, direction string, dst net.Conn, src net.Conn) {
	buf := make([]byte, s.cfg.CopyBufferSize)
	wsrc := withIdleDeadline{Conn: src, timeout: s.cfg.IdleTimeout}
	wdst := withIdleDeadline{Conn: dst, timeout: s.cfg.IdleTimeout}
	n, err := io.CopyBuffer(wdst, wsrc, buf)
	if err != nil && errors.Is(err, net.ErrClosed) {
		err = nil
	}
	if direction == "tcp_to_vsock" {
		s.m.AddBytesTCPToVsock(uint64(n))
	} else {
		s.m.AddBytesVsockToTCP(uint64(n))
	}
	ch <- copyResult{direction: direction, bytes: n, err: err}
}

type withIdleDeadline struct {
	net.Conn
	timeout time.Duration
}

func (c withIdleDeadline) Read(p []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Read(p)
}

func (c withIdleDeadline) Write(p []byte) (int, error) {
	if c.timeout > 0 {
		_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	}
	return c.Conn.Write(p)
}

type closeWriter interface {
	CloseWrite() error
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
	}
}
