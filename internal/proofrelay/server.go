package proofrelay

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/clientpolicy"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/egressproxy"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/egressroute"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/metrics"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/protocol"
	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/vsockdial"
)

const ContentTypeFrames = "application/vnd.poo.frames"

type Config struct {
	Addr                    string
	VsockCID                uint32
	VsockPort               uint32
	MaxMetadataBytes        int64
	MaxReqHeadBytes         int64
	MaxFrameBytes           int64
	MaxRequestBytes         int64
	MaxBufferedBytes        int64
	SpoolDir                string
	MaxSpoolBytes           int64
	IOTimeout               time.Duration
	ReadHeaderTimeout       time.Duration
	ShutdownTimeout         time.Duration
	AuthenticatedClientName string
	TLSConfig               *tls.Config
	ClientPolicies          *clientpolicy.PolicySet
	Metrics                 *metrics.Metrics
}

type Server struct {
	cfg       Config
	dialer    vsockdial.Dialer
	routes    *egressroute.Manager
	m         *metrics.Metrics
	log       *slog.Logger
	http      *http.Server
	errCh     chan error
	spoolUsed atomic.Int64
}

type problem struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
	Retryable bool   `json:"retryable"`
}

type requestFrames struct {
	headPayload []byte
	body        bodyStore
	targetHost  string
	targetPort  uint16
	nonce       string
}

type bodyStore interface {
	Len() int64
	WriteFrameTo(io.Writer) error
	Close() error
}

func New(cfg Config, dialer vsockdial.Dialer, routes *egressroute.Manager, log *slog.Logger) *Server {
	if log == nil {
		log = slog.Default()
	}
	mux := http.NewServeMux()
	s := &Server{cfg: cfg, dialer: dialer, routes: routes, m: cfg.Metrics, log: log}
	mux.HandleFunc("/v1/proof/relay", s.handleRelay)
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}
	return s
}

func (s *Server) Start() error {
	l, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return err
	}
	if s.cfg.TLSConfig != nil {
		l = tls.NewListener(l, s.cfg.TLSConfig)
	}
	s.errCh = make(chan error, 1)
	go func() {
		if err := s.http.Serve(l); err != nil && err != http.ErrServerClosed {
			s.log.Error("proof relay server stopped", "error", err)
			s.errCh <- err
		}
		close(s.errCh)
	}()
	return nil
}

func (s *Server) ErrorC() <-chan error {
	if s == nil {
		return nil
	}
	return s.errCh
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.http.Shutdown(ctx)
}

func (s *Server) handleRelay(w http.ResponseWriter, r *http.Request) {
	s.m.IncProofRelayRequest()
	requestID := cleanHeader(r.Header.Get("X-PoO-Request-ID"))
	rc := http.NewResponseController(w)
	if r.Method != http.MethodPost {
		s.writeProblem(w, http.StatusMethodNotAllowed, problem{Code: "bad_request", Message: "method must be POST", RequestID: requestID}, "bad_request")
		return
	}
	if ct := strings.ToLower(strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])); ct != ContentTypeFrames {
		s.writeProblem(w, http.StatusUnsupportedMediaType, problem{Code: "bad_request", Message: "content-type must be application/vnd.poo.frames", RequestID: requestID}, "bad_request")
		return
	}
	if headerBytes(r.Header) > s.cfg.MaxMetadataBytes {
		s.writeProblem(w, http.StatusRequestEntityTooLarge, problem{Code: "request_too_large", Message: "relay metadata exceeds limit", RequestID: requestID}, "request_too_large")
		return
	}
	proxyURL := r.Header.Get("X-PoO-Proxy-URL")
	proxy, err := egressproxy.Parse(proxyURL)
	if err != nil {
		s.writeProblem(w, http.StatusBadRequest, problem{Code: "invalid_proxy_url", Message: "invalid proxy URL", RequestID: requestID}, "bad_request")
		return
	}
	frames, err := s.readRequest(idleReader{
		r:       r.Body,
		timeout: s.cfg.IOTimeout,
		setDeadline: func(t time.Time) error {
			return rc.SetReadDeadline(t)
		},
	})
	if err != nil {
		status, code := mapPreflightError(err)
		s.writeProblem(w, status, problem{Code: code, Message: err.Error(), RequestID: requestID}, code)
		return
	}
	defer frames.body.Close()

	tenantID := cleanHeader(r.Header.Get("X-PoO-Tenant-ID"))
	accountID := cleanHeader(r.Header.Get("X-PoO-Account-ID"))
	clientScope, policy, err := s.clientPolicy(r)
	if err != nil {
		s.writeProblem(w, http.StatusForbidden, problem{Code: "forbidden", Message: err.Error(), RequestID: requestID}, "forbidden")
		return
	}
	lease, err := s.routes.Acquire(r.Context(), egressroute.RouteRequest{
		ClientScope:      clientScope,
		TenantID:         tenantID,
		AccountID:        accountID,
		RequestID:        requestID,
		Nonce:            frames.nonce,
		TargetHost:       frames.targetHost,
		TargetPort:       frames.targetPort,
		Proxy:            proxy,
		AllowedTargets:   policy.AllowedTargets,
		RouteConcurrency: policy.MaxConcurrency,
	})
	if err != nil {
		s.writeRouteError(w, err, requestID)
		return
	}
	defer lease.Release()

	rewrittenHead, err := rewriteEgressPort(frames.headPayload, lease.Port())
	if err != nil {
		s.writeProblem(w, http.StatusBadRequest, problem{Code: "bad_request", Message: err.Error(), RequestID: requestID}, "bad_request")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.IOTimeout)
	defer cancel()
	enclave, err := s.dialer.Dial(ctx, s.cfg.VsockCID, s.cfg.VsockPort)
	if err != nil {
		s.writeProblem(w, http.StatusServiceUnavailable, problem{Code: "enclave_unavailable", Message: "enclave control port unavailable", RequestID: requestID, Retryable: true}, "unavailable")
		return
	}
	defer enclave.Close()
	enclaveWriter := idleWriteConn{Conn: enclave, timeout: s.cfg.IOTimeout}
	enclaveReader := idleReadConn{Conn: enclave, timeout: s.cfg.IOTimeout}
	if err := protocol.WriteFrame(enclaveWriter, protocol.REQHead, rewrittenHead); err != nil {
		s.writeProblem(w, http.StatusServiceUnavailable, problem{Code: "enclave_unavailable", Message: "write REQ_HEAD to enclave failed", RequestID: requestID, Retryable: true}, "unavailable")
		return
	}
	if err := frames.body.WriteFrameTo(enclaveWriter); err != nil {
		s.log.Warn("write REQ_BODY to enclave failed after relay commit boundary", "request_id", requestID, "error", err)
		s.m.IncProofRelayAbort("enclave_write")
		closeHTTPConnection(w)
		return
	}

	first := make([]byte, 32*1024)
	n, err := enclaveReader.Read(first)
	if err != nil {
		s.log.Warn("read first enclave response bytes failed after relay commit boundary", "request_id", requestID, "error", err)
		s.m.IncProofRelayAbort("enclave_read")
		closeHTTPConnection(w)
		return
	}
	w.Header().Set("Content-Type", ContentTypeFrames)
	w.WriteHeader(http.StatusOK)
	responseWriter := flushWriter{w: idleResponseWriter{
		w:       w,
		timeout: s.cfg.IOTimeout,
		setDeadline: func(t time.Time) error {
			return rc.SetWriteDeadline(t)
		},
	}}
	if _, err := responseWriter.Write(first[:n]); err != nil {
		s.log.Warn("write first relay response bytes failed", "request_id", requestID, "error", err)
		s.m.IncProofRelayAbort("client_write")
		return
	}
	_, err = io.Copy(responseWriter, enclaveReader)
	if err != nil && !errors.Is(err, net.ErrClosed) && !errors.Is(err, context.Canceled) {
		s.log.Warn("relay response copy failed", "request_id", requestID, "error", err)
		s.m.IncProofRelayAbort("client_write")
	}
}

func (s *Server) readRequest(r io.Reader) (requestFrames, error) {
	maxHead := uint32(s.cfg.MaxReqHeadBytes)
	maxFrame := uint32(s.cfg.MaxFrameBytes)
	h1, err := protocol.ReadHeader(r, maxHead)
	if err != nil {
		return requestFrames{}, fmt.Errorf("read REQ_HEAD: %w", err)
	}
	if h1.Type != protocol.REQHead {
		return requestFrames{}, fmt.Errorf("expected REQ_HEAD, got 0x%02x", h1.Type)
	}
	head, err := protocol.ReadPayload(r, h1.Length)
	if err != nil {
		return requestFrames{}, fmt.Errorf("read REQ_HEAD payload: %w", err)
	}
	h2, err := protocol.ReadHeader(r, maxFrame)
	if err != nil {
		return requestFrames{}, fmt.Errorf("read REQ_BODY: %w", err)
	}
	if h2.Type != protocol.REQBody {
		return requestFrames{}, fmt.Errorf("expected REQ_BODY, got 0x%02x", h2.Type)
	}
	total := int64(protocol.HeaderSize*2) + int64(h1.Length) + int64(h2.Length)
	if total > s.cfg.MaxRequestBytes {
		return requestFrames{}, fmt.Errorf("relay request exceeds limit")
	}
	body, err := s.readBodyStore(r, h2.Length)
	if err != nil {
		return requestFrames{}, err
	}
	var tail [1]byte
	n, err := r.Read(tail[:])
	if n != 0 {
		_ = body.Close()
		return requestFrames{}, fmt.Errorf("trailing bytes after REQ_BODY")
	}
	if err != io.EOF {
		_ = body.Close()
		if err == nil {
			return requestFrames{}, fmt.Errorf("missing EOF after REQ_BODY")
		}
		return requestFrames{}, fmt.Errorf("confirm EOF: %w", err)
	}
	targetHost, targetPort, nonce, err := parseHead(head)
	if err != nil {
		_ = body.Close()
		return requestFrames{}, err
	}
	return requestFrames{headPayload: head, body: body, targetHost: targetHost, targetPort: targetPort, nonce: nonce}, nil
}

func (s *Server) readBodyStore(r io.Reader, n uint32) (bodyStore, error) {
	if int64(n) <= s.cfg.MaxBufferedBytes {
		payload, err := protocol.ReadPayload(r, n)
		if err != nil {
			return nil, fmt.Errorf("read REQ_BODY payload: %w", err)
		}
		return memoryBody(payload), nil
	}
	if int64(n) > s.cfg.MaxFrameBytes || int64(n) > s.cfg.MaxSpoolBytes {
		s.m.IncProofRelaySpoolFailure()
		return nil, fmt.Errorf("REQ_BODY exceeds spool limit")
	}
	reserved := s.spoolUsed.Add(int64(n))
	if reserved > s.cfg.MaxSpoolBytes {
		s.spoolUsed.Add(-int64(n))
		s.m.IncProofRelaySpoolFailure()
		return nil, fmt.Errorf("REQ_BODY exceeds global spool capacity")
	}
	s.m.AddProofRelaySpoolBytes(int64(n))
	releaseReserve := true
	defer func() {
		if releaseReserve {
			s.spoolUsed.Add(-int64(n))
			s.m.AddProofRelaySpoolBytes(-int64(n))
		}
	}()
	if err := os.MkdirAll(s.cfg.SpoolDir, 0o700); err != nil {
		s.m.IncProofRelaySpoolFailure()
		return nil, fmt.Errorf("create spool dir: %w", err)
	}
	f, err := os.CreateTemp(s.cfg.SpoolDir, "poo-relay-body-*")
	if err != nil {
		s.m.IncProofRelaySpoolFailure()
		return nil, fmt.Errorf("create spool file: %w", err)
	}
	path := f.Name()
	removeNow := os.Remove(path)
	if removeNow != nil {
		_ = f.Close()
		s.m.IncProofRelaySpoolFailure()
		return nil, fmt.Errorf("unlink spool file: %w", removeNow)
	}
	if _, err := io.CopyN(f, r, int64(n)); err != nil {
		_ = f.Close()
		s.m.IncProofRelaySpoolFailure()
		return nil, fmt.Errorf("spool REQ_BODY payload: %w", err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		_ = f.Close()
		s.m.IncProofRelaySpoolFailure()
		return nil, fmt.Errorf("seek spool file: %w", err)
	}
	releaseReserve = false
	return &fileBody{f: f, n: int64(n), release: func() {
		s.spoolUsed.Add(-int64(n))
		s.m.AddProofRelaySpoolBytes(-int64(n))
	}}, nil
}

func parseHead(payload []byte) (string, uint16, string, error) {
	var obj map[string]any
	if err := json.Unmarshal(payload, &obj); err != nil {
		return "", 0, "", fmt.Errorf("REQ_HEAD JSON: %w", err)
	}
	upstreamObj, ok := obj["upstream"].(map[string]any)
	if !ok {
		return "", 0, "", fmt.Errorf("REQ_HEAD upstream is required")
	}
	hostRaw, ok := upstreamObj["host"].(string)
	if !ok || hostRaw == "" {
		return "", 0, "", fmt.Errorf("REQ_HEAD upstream.host is required")
	}
	host, port, err := normalizeTargetHost(hostRaw)
	if err != nil {
		return "", 0, "", err
	}
	nonce, _ := obj["nonce"].(string)
	return host, port, nonce, nil
}

func normalizeTargetHost(raw string) (string, uint16, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.ContainsAny(raw, "\r\n\t/") {
		return "", 0, fmt.Errorf("invalid upstream.host")
	}
	host := raw
	port := "443"
	if h, p, err := net.SplitHostPort(raw); err == nil {
		host, port = h, p
	} else if strings.Count(raw, ":") > 0 {
		return "", 0, fmt.Errorf("upstream.host must not include non-443 port")
	}
	if port != "443" {
		return "", 0, fmt.Errorf("only upstream port 443 is supported in v1")
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return "", 0, fmt.Errorf("invalid upstream.host")
	}
	return host, 443, nil
}

func rewriteEgressPort(payload []byte, port uint32) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil {
		return nil, fmt.Errorf("REQ_HEAD JSON: %w", err)
	}
	portJSON, err := json.Marshal(port)
	if err != nil {
		return nil, err
	}
	obj["egress_port"] = portJSON
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Server) writeRouteError(w http.ResponseWriter, err error, requestID string) {
	switch {
	case errors.Is(err, egressroute.ErrTargetNotAllowed):
		s.writeProblem(w, http.StatusForbidden, problem{Code: "target_not_allowed", Message: "target is not allowed", RequestID: requestID}, "forbidden")
	case errors.Is(err, egressroute.ErrRateLimited):
		s.writeProblem(w, http.StatusTooManyRequests, problem{Code: "rate_limited", Message: "route concurrency limit reached", RequestID: requestID, Retryable: true}, "unavailable")
	case errors.Is(err, egressroute.ErrCapacityExhausted):
		s.writeProblem(w, http.StatusServiceUnavailable, problem{Code: "egress_capacity_exhausted", Message: "egress capacity exhausted", RequestID: requestID, Retryable: true}, "unavailable")
	default:
		s.writeProblem(w, http.StatusServiceUnavailable, problem{Code: "egress_capacity_exhausted", Message: "egress route unavailable", RequestID: requestID, Retryable: true}, "unavailable")
	}
}

func (s *Server) writeProblem(w http.ResponseWriter, status int, p problem, reason string) {
	s.m.IncProofRelayPreflightFailure(reason)
	writeProblem(w, status, p)
}

func mapPreflightError(err error) (int, string) {
	if isTimeoutError(err) {
		return http.StatusRequestTimeout, "relay_timeout"
	}
	msg := err.Error()
	if strings.Contains(msg, "exceeds") || strings.Contains(msg, "limit") {
		return http.StatusRequestEntityTooLarge, "request_too_large"
	}
	return http.StatusBadRequest, "bad_request"
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, os.ErrDeadlineExceeded) ||
		(errors.As(err, &netErr) && netErr.Timeout())
}

func writeProblem(w http.ResponseWriter, status int, p problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

func headerBytes(h http.Header) int64 {
	var n int64
	for k, values := range h {
		n += int64(len(k))
		for _, v := range values {
			n += int64(len(v))
		}
	}
	return n
}

func cleanHeader(v string) string {
	v = strings.TrimSpace(v)
	if strings.ContainsAny(v, "\r\n\t") {
		return ""
	}
	return v
}

func (s *Server) clientScope(r *http.Request) string {
	if s.cfg.AuthenticatedClientName != "" {
		return s.cfg.AuthenticatedClientName
	}
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		cert := r.TLS.PeerCertificates[0]
		if len(cert.URIs) > 0 {
			return cert.URIs[0].String()
		}
		if len(cert.DNSNames) > 0 {
			return cert.DNSNames[0]
		}
		if cert.Subject.String() != "" {
			return cert.Subject.String()
		}
	}
	return "local"
}

func (s *Server) clientPolicy(r *http.Request) (string, clientpolicy.ClientPolicy, error) {
	if s.cfg.ClientPolicies == nil {
		return s.clientScope(r), clientpolicy.ClientPolicy{}, nil
	}
	if s.cfg.AuthenticatedClientName != "" {
		if p, ok := s.cfg.ClientPolicies.Lookup(s.cfg.AuthenticatedClientName); ok {
			return s.cfg.AuthenticatedClientName, p, nil
		}
		return "", clientpolicy.ClientPolicy{}, fmt.Errorf("client identity is not allowed")
	}
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return "", clientpolicy.ClientPolicy{}, fmt.Errorf("client certificate is required")
	}
	cert := r.TLS.PeerCertificates[0]
	if id, p, ok, ambiguous := lookupPolicyURIs(s.cfg.ClientPolicies, cert.URIs); ok || ambiguous {
		if ambiguous {
			return "", clientpolicy.ClientPolicy{}, fmt.Errorf("ambiguous client identity")
		}
		return id, p, nil
	}
	if id, p, ok, ambiguous := lookupPolicyStrings(s.cfg.ClientPolicies, cert.DNSNames); ok || ambiguous {
		if ambiguous {
			return "", clientpolicy.ClientPolicy{}, fmt.Errorf("ambiguous client identity")
		}
		return id, p, nil
	}
	if subjectRaw := cert.Subject.String(); subjectRaw != "" {
		subject, err := clientpolicy.CanonicalSubject(subjectRaw)
		if err != nil {
			return "", clientpolicy.ClientPolicy{}, fmt.Errorf("client certificate subject is invalid")
		}
		if p, ok := s.cfg.ClientPolicies.Lookup(subject); ok {
			return subject, p, nil
		}
	}
	return "", clientpolicy.ClientPolicy{}, fmt.Errorf("client identity is not allowed")
}

func lookupPolicyURIs(policies *clientpolicy.PolicySet, values []*url.URL) (string, clientpolicy.ClientPolicy, bool, bool) {
	var raw []string
	for _, value := range values {
		if value != nil {
			raw = append(raw, value.String())
		}
	}
	return lookupPolicyStrings(policies, raw)
}

func lookupPolicyStrings(policies *clientpolicy.PolicySet, values []string) (string, clientpolicy.ClientPolicy, bool, bool) {
	var foundID string
	var found clientpolicy.ClientPolicy
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		policy, ok := policies.Lookup(value)
		if !ok {
			continue
		}
		if foundID != "" {
			return "", clientpolicy.ClientPolicy{}, false, true
		}
		foundID = value
		found = policy
	}
	return foundID, found, foundID != "", false
}

type flushWriter struct {
	w io.Writer
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if flusher, ok := f.w.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

type idleReader struct {
	r           io.Reader
	timeout     time.Duration
	setDeadline func(time.Time) error
}

func (r idleReader) Read(p []byte) (int, error) {
	if r.timeout > 0 && r.setDeadline != nil {
		_ = r.setDeadline(time.Now().Add(r.timeout))
	}
	return r.r.Read(p)
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

type idleResponseWriter struct {
	w           io.Writer
	timeout     time.Duration
	setDeadline func(time.Time) error
}

func (w idleResponseWriter) Write(p []byte) (int, error) {
	if w.timeout > 0 && w.setDeadline != nil {
		_ = w.setDeadline(time.Now().Add(w.timeout))
	}
	return w.w.Write(p)
}

func closeHTTPConnection(w http.ResponseWriter) {
	if c, _, err := http.NewResponseController(w).Hijack(); err == nil {
		_ = c.Close()
		return
	}
	panic(http.ErrAbortHandler)
}

type memoryBody []byte

func (b memoryBody) Len() int64 { return int64(len(b)) }

func (b memoryBody) WriteFrameTo(w io.Writer) error {
	return protocol.WriteFrame(w, protocol.REQBody, b)
}

func (b memoryBody) Close() error { return nil }

type fileBody struct {
	f       *os.File
	n       int64
	release func()
	once    sync.Once
}

func (b *fileBody) Len() int64 { return b.n }

func (b *fileBody) WriteFrameTo(w io.Writer) error {
	var hdr [protocol.HeaderSize]byte
	hdr[0] = protocol.REQBody
	n := uint32(b.n)
	hdr[1] = byte(n >> 24)
	hdr[2] = byte(n >> 16)
	hdr[3] = byte(n >> 8)
	hdr[4] = byte(n)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := b.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err := io.CopyN(w, b.f, b.n)
	return err
}

func (b *fileBody) Close() error {
	if b.f == nil {
		return nil
	}
	var err error
	b.once.Do(func() {
		err = b.f.Close()
		if b.release != nil {
			b.release()
		}
	})
	return err
}
