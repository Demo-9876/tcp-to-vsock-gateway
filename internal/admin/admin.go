package admin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/metrics"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/vsockdial"
)

const (
	frameStats byte   = 0x20
	maxStats   uint32 = 1024 * 1024
)

type Config struct {
	Addr          string
	ReadyCacheTTL time.Duration
	ProbeTimeout  time.Duration
}

type Prober interface {
	Probe(ctx context.Context) error
}

type Server struct {
	cfg    Config
	prober Prober
	m      *metrics.Metrics
	log    *slog.Logger

	httpServer  *http.Server
	mu          sync.Mutex
	draining    bool
	drainCtx    context.Context
	drainCancel context.CancelFunc
	cache       readyCache
	inflight    *inflightProbe
}

type readyCache struct {
	validUntil time.Time
	err        error
}

type inflightProbe struct {
	done      chan struct{}
	err       error
	hasResult bool
}

func New(cfg Config, prober Prober, m *metrics.Metrics, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	drainCtx, drainCancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:         cfg,
		prober:      prober,
		m:           m,
		log:         log,
		drainCtx:    drainCtx,
		drainCancel: drainCancel,
		inflight:    nil,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.healthz)
	mux.HandleFunc("/readyz", s.readyz)
	mux.HandleFunc("/metrics", s.metrics)
	s.httpServer = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 2 * time.Second,
	}
	return s
}

func (s *Server) Start() error {
	l, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	go func() {
		if err := s.httpServer.Serve(l); err != nil && err != http.ErrServerClosed {
			s.log.Error("admin server stopped", "error", err)
		}
	}()
	return nil
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) SetDraining() {
	s.mu.Lock()
	s.draining = true
	s.cache = readyCache{}
	s.mu.Unlock()
	s.drainCancel()
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok"}`+"\n")
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	if err := s.ready(r.Context()); err != nil {
		w.Header().Set("content-type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "not_ready", "error": err.Error()})
		return
	}
	w.Header().Set("content-type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ready"}`+"\n")
}

func (s *Server) metrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "text/plain; version=0.0.4")
	_, _ = io.WriteString(w, s.m.Prometheus())
}

func (s *Server) ready(parent context.Context) error {
	now := time.Now()
	s.mu.Lock()
	if s.draining {
		s.mu.Unlock()
		return fmt.Errorf("shutting down")
	}
	if now.Before(s.cache.validUntil) {
		err := s.cache.err
		s.m.IncReadinessCacheHit()
		s.mu.Unlock()
		return err
	}
	if s.inflight != nil {
		probe := s.inflight
		s.mu.Unlock()
		return s.waitProbe(parent, probe, true)
	}
	probe := &inflightProbe{done: make(chan struct{})}
	s.inflight = probe
	s.mu.Unlock()

	go s.finishProbe(probe)

	return s.waitProbe(parent, probe, false)
}

func (s *Server) waitProbe(parent context.Context, probe *inflightProbe, cacheHit bool) error {
	select {
	case <-probe.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.draining {
			return fmt.Errorf("shutting down")
		}
		if !probe.hasResult {
			return fmt.Errorf("readiness probe finished without result")
		}
		if cacheHit {
			s.m.IncReadinessCacheHit()
		}
		return probe.err
	case <-s.drainCtx.Done():
		return fmt.Errorf("shutting down")
	case <-parent.Done():
		return parent.Err()
	}
}

func (s *Server) finishProbe(probe *inflightProbe) {
	err := s.runProbe()
	s.mu.Lock()
	probe.err = err
	probe.hasResult = true
	close(probe.done)
	s.inflight = nil
	if s.draining {
		s.mu.Unlock()
		return
	}
	s.cache = readyCache{validUntil: time.Now().Add(s.cfg.ReadyCacheTTL), err: err}
	s.mu.Unlock()
}

func (s *Server) runProbe() error {
	timeout := s.cfg.ProbeTimeout
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	ctx, cancel := context.WithTimeout(s.drainCtx, timeout)
	defer cancel()
	start := time.Now()
	err := s.prober.Probe(ctx)
	s.m.ObserveReadinessProbe(time.Since(start), err == nil)
	return err
}

type VsockMetricsProber struct {
	Dialer  vsockdial.Dialer
	CID     uint32
	Port    uint32
	Timeout time.Duration
}

func NewVsockMetricsProber(d vsockdial.Dialer, cid, port uint32, timeout time.Duration) VsockMetricsProber {
	return VsockMetricsProber{Dialer: d, CID: cid, Port: port, Timeout: timeout}
}

func (p VsockMetricsProber) Probe(ctx context.Context) error {
	conn, err := p.Dialer.Dial(ctx, p.CID, p.Port)
	if err != nil {
		return err
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	if p.Timeout > 0 {
		deadline := time.Now().Add(p.Timeout)
		_ = conn.SetReadDeadline(deadline)
		_ = conn.SetWriteDeadline(deadline)
	}
	header := make([]byte, 5)
	if _, err := io.ReadFull(conn, header); err != nil {
		return fmt.Errorf("read metrics frame header: %w", err)
	}
	if header[0] != frameStats {
		return fmt.Errorf("unexpected metrics frame type: 0x%02x", header[0])
	}
	n := binary.BigEndian.Uint32(header[1:])
	if n == 0 || n > maxStats {
		return fmt.Errorf("invalid metrics frame length: %d", n)
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(conn, payload); err != nil {
		return fmt.Errorf("read metrics frame payload: %w", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return fmt.Errorf("parse metrics JSON: %w", err)
	}
	for _, key := range []string{"n_workers", "accepted", "failed"} {
		if _, ok := obj[key]; !ok {
			return fmt.Errorf("metrics JSON missing %q", key)
		}
	}
	return nil
}
