package egressroute

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func TestAcquireAllocatesSingleUseLaneAndKeepsRouteCache(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	defer upstream.Close()
	upstreamDone := make(chan struct{})
	go func() {
		c, _ := upstream.Accept()
		if c != nil {
			_, _ = io.Copy(c, c)
			_ = c.Close()
		}
		close(upstreamDone)
	}()
	host, portRaw, _ := net.SplitHostPort(upstream.Addr().String())
	port, _ := strconv.ParseUint(portRaw, 10, 16)
	lanePort := freeTCPPort(t)

	m := New(Config{
		PortStart:               uint32(lanePort),
		PortEnd:                 uint32(lanePort),
		PortCooldown:            time.Millisecond,
		RouteIdleTTL:            time.Minute,
		LeaseTTL:                time.Second,
		ConnectTimeout:          time.Second,
		IdleTimeout:             time.Second,
		MaxActiveRoutes:         8,
		MaxActiveLeases:         8,
		DefaultRouteConcurrency: 1,
		MaxRouteConcurrency:     1,
		AllowedTargets:          []string{net.JoinHostPort(host, portRaw)},
	}, TCPListenerFactory{Host: "127.0.0.1"}, nil)

	lease, err := m.Acquire(context.Background(), RouteRequest{
		ClientScope: "local",
		TargetHost:  host,
		TargetPort:  uint16(port),
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(lease.Port()))))
	if err != nil {
		t.Fatalf("dial lane: %v", err)
	}
	_, _ = c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo = %q", buf)
	}
	_ = c.Close()
	lease.Release()
	<-upstreamDone

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.routes) != 1 {
		t.Fatalf("route cache entries = %d, want 1", len(m.routes))
	}
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port listen: %v", err)
	}
	defer ln.Close()
	_, portRaw, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portRaw)
	return port
}

func TestAcquireRejectsTargetOutsideAllowlist(t *testing.T) {
	m := New(Config{
		PortStart:               19000,
		PortEnd:                 19000,
		RouteIdleTTL:            time.Minute,
		LeaseTTL:                time.Second,
		ConnectTimeout:          time.Second,
		MaxActiveRoutes:         1,
		MaxActiveLeases:         1,
		DefaultRouteConcurrency: 1,
		MaxRouteConcurrency:     1,
		AllowedTargets:          []string{"api.openai.com:443"},
	}, TCPListenerFactory{Host: "127.0.0.1"}, nil)
	_, err := m.Acquire(context.Background(), RouteRequest{
		ClientScope: "local",
		TargetHost:  "api.anthropic.com",
		TargetPort:  443,
	})
	if !errors.Is(err, ErrTargetNotAllowed) {
		t.Fatalf("Acquire() error = %v, want ErrTargetNotAllowed", err)
	}
}

func TestAcquireAppliesClientAllowedTargets(t *testing.T) {
	m := New(Config{
		PortStart:               19000,
		PortEnd:                 19000,
		RouteIdleTTL:            time.Minute,
		LeaseTTL:                time.Second,
		ConnectTimeout:          time.Second,
		MaxActiveRoutes:         1,
		MaxActiveLeases:         1,
		DefaultRouteConcurrency: 1,
		MaxRouteConcurrency:     1,
		AllowedTargets:          []string{"api.openai.com:443", "api.anthropic.com:443"},
	}, TCPListenerFactory{Host: "127.0.0.1"}, nil)
	_, err := m.Acquire(context.Background(), RouteRequest{
		ClientScope:    "client-a",
		TargetHost:     "api.anthropic.com",
		TargetPort:     443,
		AllowedTargets: []string{"api.openai.com:443"},
	})
	if !errors.Is(err, ErrTargetNotAllowed) {
		t.Fatalf("Acquire() error = %v, want ErrTargetNotAllowed", err)
	}
}

func TestAcquireDoesNotReuseActivePort(t *testing.T) {
	m := New(Config{
		PortStart:               19100,
		PortEnd:                 19100,
		PortCooldown:            time.Millisecond,
		RouteIdleTTL:            time.Minute,
		LeaseTTL:                time.Second,
		ConnectTimeout:          time.Second,
		IdleTimeout:             time.Second,
		MaxActiveRoutes:         8,
		MaxActiveLeases:         8,
		DefaultRouteConcurrency: 8,
		MaxRouteConcurrency:     8,
		AllowedTargets:          []string{"api.openai.com:443"},
	}, blockingListenerFactory{}, nil)
	lease, err := m.Acquire(context.Background(), RouteRequest{
		ClientScope: "local",
		TargetHost:  "api.openai.com",
		TargetPort:  443,
	})
	if err != nil {
		t.Fatalf("first Acquire() error = %v", err)
	}
	defer lease.Release()
	_, err = m.Acquire(context.Background(), RouteRequest{
		ClientScope: "local",
		TargetHost:  "api.openai.com",
		TargetPort:  443,
	})
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("second Acquire() error = %v, want ErrCapacityExhausted", err)
	}
}

func TestLeaseTTLOnlyAppliesBeforeLaneConnect(t *testing.T) {
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("upstream listen: %v", err)
	}
	defer upstream.Close()
	go func() {
		c, _ := upstream.Accept()
		if c != nil {
			time.Sleep(80 * time.Millisecond)
			_, _ = c.Write([]byte("ok"))
			_ = c.Close()
		}
	}()
	host, portRaw, _ := net.SplitHostPort(upstream.Addr().String())
	port, _ := strconv.ParseUint(portRaw, 10, 16)
	lanePort := freeTCPPort(t)
	m := New(Config{
		PortStart:               uint32(lanePort),
		PortEnd:                 uint32(lanePort),
		PortCooldown:            time.Millisecond,
		RouteIdleTTL:            time.Minute,
		LeaseTTL:                20 * time.Millisecond,
		ConnectTimeout:          time.Second,
		IdleTimeout:             time.Second,
		MaxActiveRoutes:         8,
		MaxActiveLeases:         8,
		DefaultRouteConcurrency: 1,
		MaxRouteConcurrency:     1,
		AllowedTargets:          []string{net.JoinHostPort(host, portRaw)},
	}, TCPListenerFactory{Host: "127.0.0.1"}, nil)
	lease, err := m.Acquire(context.Background(), RouteRequest{
		ClientScope: "local",
		TargetHost:  host,
		TargetPort:  uint16(port),
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(lease.Port()))))
	if err != nil {
		t.Fatalf("dial lane: %v", err)
	}
	defer c.Close()
	var buf [2]byte
	if _, err := io.ReadFull(c, buf[:]); err != nil {
		t.Fatalf("active tunnel was cut by lease TTL: %v", err)
	}
	if string(buf[:]) != "ok" {
		t.Fatalf("read = %q, want ok", buf[:])
	}
}

func TestContextCancelReleasesLeaseBeforeConnect(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	m := New(Config{
		PortStart:               19101,
		PortEnd:                 19101,
		PortCooldown:            time.Millisecond,
		RouteIdleTTL:            time.Minute,
		LeaseTTL:                time.Second,
		ConnectTimeout:          time.Second,
		IdleTimeout:             time.Second,
		MaxActiveRoutes:         8,
		MaxActiveLeases:         8,
		DefaultRouteConcurrency: 1,
		MaxRouteConcurrency:     1,
		AllowedTargets:          []string{"api.openai.com:443"},
	}, blockingListenerFactory{}, nil)
	lease, err := m.Acquire(ctx, RouteRequest{
		ClientScope: "local",
		TargetHost:  "api.openai.com",
		TargetPort:  443,
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	cancel()
	defer lease.Release()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		active := m.activeLeases
		m.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("lease was not released after context cancellation")
}

func TestLeaseTTLReleasesWhenLaneNeverConnects(t *testing.T) {
	m := New(Config{
		PortStart:               19102,
		PortEnd:                 19102,
		PortCooldown:            time.Millisecond,
		RouteIdleTTL:            time.Minute,
		LeaseTTL:                20 * time.Millisecond,
		ConnectTimeout:          time.Second,
		IdleTimeout:             time.Second,
		MaxActiveRoutes:         8,
		MaxActiveLeases:         8,
		DefaultRouteConcurrency: 1,
		MaxRouteConcurrency:     1,
		AllowedTargets:          []string{"api.openai.com:443"},
	}, blockingListenerFactory{}, nil)
	lease, err := m.Acquire(context.Background(), RouteRequest{
		ClientScope: "local",
		TargetHost:  "api.openai.com",
		TargetPort:  443,
	})
	if err != nil {
		t.Fatalf("Acquire() error = %v", err)
	}
	defer lease.Release()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		active := m.activeLeases
		m.mu.Unlock()
		if active == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("lease was not released after lease TTL")
}

type blockingListenerFactory struct{}

func (blockingListenerFactory) Listen(uint32) (net.Listener, error) {
	return blockingListener{}, nil
}

type blockingListener struct{}

func (blockingListener) Accept() (net.Conn, error) {
	select {}
}

func (blockingListener) Close() error { return nil }

func (blockingListener) Addr() net.Addr { return dummyAddr("blocking") }

type dummyAddr string

func (a dummyAddr) Network() string { return string(a) }

func (a dummyAddr) String() string { return string(a) }
