//go:build linux

package vsockdial

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestConnReadUsesUpdatedDeadline(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer syscall.Close(fds[1])

	c, err := newConn(fds[0], 4, 5005)
	if err != nil {
		t.Fatalf("newConn: %v", err)
	}
	defer c.Close()

	if err := c.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := c.Read(b[:])
		done <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if err := c.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline update: %v", err)
	}

	select {
	case err := <-done:
		if !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("Read error = %v, want deadline exceeded", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Read did not observe updated deadline")
	}
}

func TestConnDeadlineSettersRaceWithClose(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("socketpair: %v", err)
	}
	defer syscall.Close(fds[1])

	c, err := newConn(fds[0], 4, 5005)
	if err != nil {
		t.Fatalf("newConn: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 100; j++ {
				_ = c.SetDeadline(time.Now().Add(time.Second))
			}
		}()
	}
	close(start)
	_ = c.Close()
	wg.Wait()
}
