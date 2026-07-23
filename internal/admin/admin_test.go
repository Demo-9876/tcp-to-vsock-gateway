package admin

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Demo-9876/tcp-to-vsock-gateway/internal/metrics"
)

type countingProber struct {
	n   atomic.Int64
	err error
}

func (p *countingProber) Probe(context.Context) error {
	p.n.Add(1)
	return p.err
}

type blockingProber struct {
	once    sync.Once
	started chan struct{}
	release chan struct{}
	err     error
}

func (p *blockingProber) Probe(ctx context.Context) error {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

type cancelTrackingProber struct {
	once     sync.Once
	n        atomic.Int64
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
}

func (p *cancelTrackingProber) Probe(ctx context.Context) error {
	p.n.Add(1)
	p.once.Do(func() { close(p.started) })
	defer close(p.finished)
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type pipeDialer struct {
	server func(net.Conn)
}

func (d pipeDialer) Dial(context.Context, uint32, uint32) (net.Conn, error) {
	client, server := net.Pipe()
	go d.server(server)
	return client, nil
}

func TestReadyUsesCache(t *testing.T) {
	p := &countingProber{}
	s := New(Config{ReadyCacheTTL: time.Minute, ProbeTimeout: time.Second}, p, metrics.New(), nil)
	if err := s.ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := p.n.Load(); got != 1 {
		t.Fatalf("probe count = %d, want 1", got)
	}
}

func TestVsockMetricsProberAcceptsValidStatsFrame(t *testing.T) {
	d := pipeDialer{server: func(c net.Conn) {
		defer c.Close()
		payload := []byte(`{"n_workers":64,"accepted":1,"failed":0}`)
		header := make([]byte, 5)
		header[0] = frameStats
		binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
		_, _ = c.Write(append(header, payload...))
	}}
	p := NewVsockMetricsProber(d, 4, 5006, time.Second)
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe() error = %v", err)
	}
}

func TestVsockMetricsProberRejectsMissingStatsField(t *testing.T) {
	d := pipeDialer{server: func(c net.Conn) {
		defer c.Close()
		payload := []byte(`{"n_workers":64,"accepted":1}`)
		header := make([]byte, 5)
		header[0] = frameStats
		binary.BigEndian.PutUint32(header[1:], uint32(len(payload)))
		_, _ = c.Write(append(header, payload...))
	}}
	p := NewVsockMetricsProber(d, 4, 5006, time.Second)
	if err := p.Probe(context.Background()); err == nil {
		t.Fatal("Probe() expected error")
	}
}

func TestReadyBypassesCacheWhenDraining(t *testing.T) {
	p := &countingProber{}
	s := New(Config{ReadyCacheTTL: time.Minute, ProbeTimeout: time.Second}, p, metrics.New(), nil)
	if err := s.ready(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.SetDraining()
	if err := s.ready(context.Background()); err == nil {
		t.Fatal("ready while draining expected error")
	}
}

func TestReadyReturnsNotReadyWhenDrainingWhileProbeInFlight(t *testing.T) {
	p := &blockingProber{started: make(chan struct{}), release: make(chan struct{})}
	s := New(Config{ReadyCacheTTL: time.Minute, ProbeTimeout: time.Second}, p, metrics.New(), nil)
	done := make(chan error, 1)
	go func() { done <- s.ready(context.Background()) }()
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	s.SetDraining()
	defer close(p.release)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("ready during draining expected error")
		}
	case <-time.After(time.Second):
		t.Fatal("ready did not return")
	}
	if err := s.ready(context.Background()); err == nil {
		t.Fatal("subsequent ready while draining expected error")
	}
}

func TestReadyInflightWaiterUsesProbeResult(t *testing.T) {
	wantErr := errors.New("probe failed")
	p := &blockingProber{started: make(chan struct{}), release: make(chan struct{}), err: wantErr}
	s := New(Config{ReadyCacheTTL: time.Minute, ProbeTimeout: time.Second}, p, metrics.New(), nil)
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- s.ready(context.Background()) }()
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	waiterDone := make(chan error, 1)
	go func() { waiterDone <- s.ready(context.Background()) }()
	close(p.release)

	for name, ch := range map[string]<-chan error{"owner": ownerDone, "waiter": waiterDone} {
		select {
		case err := <-ch:
			if !errors.Is(err, wantErr) {
				t.Fatalf("%s error = %v, want %v", name, err, wantErr)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s ready did not return", name)
		}
	}
}

func TestReadyCachesErrors(t *testing.T) {
	p := &countingProber{err: errors.New("boom")}
	s := New(Config{ReadyCacheTTL: time.Minute, ProbeTimeout: time.Second}, p, metrics.New(), nil)
	if err := s.ready(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if err := s.ready(context.Background()); err == nil {
		t.Fatal("expected cached error")
	}
	if got := p.n.Load(); got != 1 {
		t.Fatalf("probe count = %d, want 1", got)
	}
}

func TestReadyOwnerCancelDoesNotCancelSharedProbe(t *testing.T) {
	p := &cancelTrackingProber{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
	s := New(Config{ReadyCacheTTL: time.Minute, ProbeTimeout: time.Second}, p, metrics.New(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	ownerDone := make(chan error, 1)
	go func() { ownerDone <- s.ready(ctx) }()
	select {
	case <-p.started:
	case <-time.After(time.Second):
		t.Fatal("probe did not start")
	}
	cancel()
	select {
	case err := <-ownerDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("owner ready error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner ready did not return after request cancel")
	}
	select {
	case <-p.finished:
		t.Fatal("shared probe was canceled by owner request")
	case <-time.After(50 * time.Millisecond):
	}

	waiterDone := make(chan error, 1)
	go func() { waiterDone <- s.ready(context.Background()) }()
	close(p.release)
	select {
	case err := <-waiterDone:
		if err != nil {
			t.Fatalf("waiter ready error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter ready did not return")
	}
	if err := s.ready(context.Background()); err != nil {
		t.Fatalf("cached ready error = %v", err)
	}
	if got := p.n.Load(); got != 1 {
		t.Fatalf("probe count = %d, want 1", got)
	}
}
