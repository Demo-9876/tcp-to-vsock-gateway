package bridge

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/metrics"
)

type fakeDialer struct {
	server func(net.Conn)
}

func (d fakeDialer) Dial(context.Context, uint32, uint32) (net.Conn, error) {
	client, server := net.Pipe()
	go d.server(server)
	return client, nil
}

type errorDialer struct{}

func (errorDialer) Dial(context.Context, uint32, uint32) (net.Conn, error) {
	return nil, errors.New("dial failed")
}

type blockingDialer struct {
	once    sync.Once
	started chan struct{}
}

func (d *blockingDialer) Dial(ctx context.Context, _, _ uint32) (net.Conn, error) {
	d.once.Do(func() { close(d.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBridgeForwardsBytesBothWays(t *testing.T) {
	d := fakeDialer{server: func(c net.Conn) {
		defer c.Close()
		buf := make([]byte, len("hello"))
		_, err := io.ReadFull(c, buf)
		if err != nil {
			t.Errorf("fake server read: %v", err)
			return
		}
		_, _ = c.Write([]byte("echo:" + string(buf)))
	}}
	s := New(Config{
		VsockCID:        4,
		VsockPort:       5005,
		ConnectTimeout:  time.Second,
		IdleTimeout:     time.Second,
		MaxConns:        4,
		CopyBufferSize:  1024,
		ShutdownTimeout: time.Second,
	}, d, metrics.New(), slog.Default())

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_, _ = conn.Write([]byte("hello"))
	if cw, ok := conn.(*net.TCPConn); ok {
		_ = cw.CloseWrite()
	}
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "echo:hello" {
		t.Fatalf("response = %q", got)
	}
}

func TestServerCountsAcceptedConnectionWhenVsockDialFails(t *testing.T) {
	m := metrics.New()
	s := New(Config{
		VsockCID:        4,
		VsockPort:       5005,
		ConnectTimeout:  time.Second,
		IdleTimeout:     time.Second,
		MaxConns:        1,
		CopyBufferSize:  1024,
		ShutdownTimeout: time.Second,
	}, errorDialer{}, m, slog.Default())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()

	eventually(t, time.Second, func() bool {
		got := m.Prometheus()
		return strings.Contains(got, `ttvg_connections_total 1`) &&
			strings.Contains(got, `ttvg_connections_active 0`) &&
			strings.Contains(got, `ttvg_vsock_dial_errors_total 1`)
	})
}

func TestServerRejectsWhenAtCapacity(t *testing.T) {
	block := make(chan struct{})
	ready := make(chan struct{})
	var once sync.Once
	d := fakeDialer{server: func(c net.Conn) {
		defer c.Close()
		once.Do(func() { close(ready) })
		<-block
	}}
	m := metrics.New()
	s := New(Config{
		VsockCID:        4,
		VsockPort:       5005,
		ConnectTimeout:  time.Second,
		IdleTimeout:     time.Second,
		MaxConns:        1,
		CopyBufferSize:  1024,
		ShutdownTimeout: time.Second,
	}, d, m, slog.Default())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	defer close(block)
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	c1, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("first connection was not established")
	}
	c2, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	_ = c2.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	b := make([]byte, 1)
	_, _ = c2.Read(b)
	if got := m.Prometheus(); !strings.Contains(got, `ttvg_connections_rejected_total{reason="limit"} 1`) {
		t.Fatalf("metrics missing rejection: %s", got)
	}
}

func TestShutdownCancelsInFlightVsockDial(t *testing.T) {
	d := &blockingDialer{started: make(chan struct{})}
	s := New(Config{
		VsockCID:        4,
		VsockPort:       5005,
		ConnectTimeout:  30 * time.Second,
		IdleTimeout:     time.Second,
		MaxConns:        1,
		CopyBufferSize:  1024,
		ShutdownTimeout: time.Second,
	}, d, metrics.New(), slog.Default())
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case <-d.started:
	case <-time.After(time.Second):
		t.Fatal("vsock dial did not start")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("Shutdown() took %s, want in-flight dial canceled promptly", elapsed)
	}
}

func TestServeExitsWhenShutdownAlreadyStarted(t *testing.T) {
	s := New(Config{
		VsockCID:        4,
		VsockPort:       5005,
		ConnectTimeout:  time.Second,
		IdleTimeout:     time.Second,
		MaxConns:        1,
		CopyBufferSize:  1024,
		ShutdownTimeout: time.Second,
	}, errorDialer{}, metrics.New(), slog.Default())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(l) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Serve() did not exit after shutdown")
	}
}

func eventually(t *testing.T, timeout time.Duration, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if f() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not satisfied before timeout")
}
