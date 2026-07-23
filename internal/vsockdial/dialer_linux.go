//go:build linux

package vsockdial

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

const (
	afVsock = 40

	pollOut  = 0x0004
	pollErr  = 0x0008
	pollHup  = 0x0010
	pollNval = 0x0020
	pollIn   = 0x0001
)

type dialer struct{}

func New() Dialer {
	return dialer{}
}

func (dialer) Dial(ctx context.Context, cid, port uint32) (net.Conn, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	if err := connect(ctx, fd, cid, port); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	return newConn(fd, cid, port)
}

type sockaddrVM struct {
	Family    uint16
	Reserved1 uint16
	Port      uint32
	CID       uint32
	Flags     uint8
	Zero      [3]uint8
}

func connect(ctx context.Context, fd int, cid, port uint32) error {
	addr := sockaddrVM{
		Family: afVsock,
		Port:   port,
		CID:    cid,
	}
	_, _, errno := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&addr)), unsafe.Sizeof(addr))
	switch errno {
	case 0:
		return nil
	case syscall.EINPROGRESS, syscall.EALREADY, syscall.EINTR:
		return waitConnect(ctx, fd)
	default:
		return errno
	}
}

type pollFD struct {
	fd      int32
	events  int16
	revents int16
}

func waitConnect(ctx context.Context, fd int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		timeout := pollTimeoutMS(ctx)
		pfds := []pollFD{{fd: int32(fd), events: pollOut}}
		n, errno := ppoll(pfds, timeout)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		if n == 0 {
			continue
		}
		if pfds[0].revents&(pollOut|pollErr|pollHup|pollNval) == 0 {
			continue
		}
		soerr, err := syscall.GetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_ERROR)
		if err != nil {
			return fmt.Errorf("getsockopt(SO_ERROR): %w", err)
		}
		if soerr != 0 {
			return syscall.Errno(soerr)
		}
		return nil
	}
}

func ppoll(pfds []pollFD, timeoutMS int) (uintptr, syscall.Errno) {
	ts := syscall.NsecToTimespec(int64(time.Duration(timeoutMS) * time.Millisecond))
	n, _, errno := syscall.Syscall6(syscall.SYS_PPOLL, uintptr(unsafe.Pointer(&pfds[0])), uintptr(len(pfds)), uintptr(unsafe.Pointer(&ts)), 0, 0, 0)
	return n, errno
}

func pollTimeoutMS(ctx context.Context) int {
	const maxPollMS = 1000
	deadline, ok := ctx.Deadline()
	if !ok {
		return maxPollMS
	}
	remaining := timeUntil(deadline)
	if remaining <= 0 {
		return 0
	}
	ms := int(remaining.Milliseconds())
	if ms <= 0 {
		return 1
	}
	if ms > maxPollMS {
		return maxPollMS
	}
	return ms
}

var timeUntil = func(t time.Time) time.Duration {
	return time.Until(t)
}

type conn struct {
	fd        int
	wakeRead  int
	wakeWrite int
	local     addr
	remote    addr

	mu            sync.Mutex
	readDeadline  time.Time
	writeDeadline time.Time
	opMu          sync.Mutex
	ops           sync.WaitGroup
	closeOnce     sync.Once
	closed        atomic.Bool
}

func newConn(fd int, cid, port uint32) (*conn, error) {
	wake := []int{-1, -1}
	if err := syscall.Pipe2(wake, syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("pipe2 wake fd: %w", err)
	}
	return &conn{
		fd:        fd,
		wakeRead:  wake[0],
		wakeWrite: wake[1],
		local:     addr{cid: 0, port: 0},
		remote:    addr{cid: cid, port: port},
	}, nil
}

func (c *conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if !c.beginOp() {
		return 0, net.ErrClosed
	}
	defer c.ops.Done()
	for {
		n, err := syscall.Read(c.fd, p)
		if n > 0 {
			return n, nil
		}
		if err == nil {
			return 0, io.EOF
		}
		if err == syscall.EINTR {
			continue
		}
		if !isAgain(err) {
			if c.closed.Load() {
				return 0, net.ErrClosed
			}
			return 0, err
		}
		if err := c.wait(pollIn, c.getReadDeadline); err != nil {
			return 0, err
		}
	}
}

func (c *conn) Write(p []byte) (int, error) {
	if !c.beginOp() {
		return 0, net.ErrClosed
	}
	defer c.ops.Done()
	written := 0
	for len(p) > 0 {
		n, err := syscall.Write(c.fd, p)
		if n > 0 {
			written += n
			p = p[n:]
		}
		if err == nil {
			if n == 0 {
				if err := c.wait(pollOut, c.getWriteDeadline); err != nil {
					return written, err
				}
			}
			continue
		}
		if err == syscall.EINTR {
			continue
		}
		if !isAgain(err) {
			if c.closed.Load() {
				err = net.ErrClosed
			}
			return written, err
		}
		if err := c.wait(pollOut, c.getWriteDeadline); err != nil {
			return written, err
		}
	}
	return written, nil
}

func (c *conn) Close() error {
	var closeErr error
	c.closeOnce.Do(func() {
		c.opMu.Lock()
		c.closed.Store(true)
		_, _ = syscall.Write(c.wakeWrite, []byte{1})
		c.opMu.Unlock()

		c.ops.Wait()
		closeErr = firstErr(
			syscall.Close(c.fd),
			syscall.Close(c.wakeRead),
			syscall.Close(c.wakeWrite),
		)
	})
	return closeErr
}

func (c *conn) CloseWrite() error {
	if !c.beginOp() {
		return net.ErrClosed
	}
	defer c.ops.Done()
	if err := syscall.Shutdown(c.fd, syscall.SHUT_WR); err != nil {
		if c.closed.Load() {
			return net.ErrClosed
		}
		return err
	}
	return nil
}

func (c *conn) LocalAddr() net.Addr {
	return c.local
}

func (c *conn) RemoteAddr() net.Addr {
	return c.remote
}

func (c *conn) SetDeadline(t time.Time) error {
	if !c.beginOp() {
		return net.ErrClosed
	}
	defer c.ops.Done()
	c.mu.Lock()
	c.readDeadline = t
	c.writeDeadline = t
	c.mu.Unlock()
	if c.closed.Load() {
		return net.ErrClosed
	}
	return c.wake()
}

func (c *conn) SetReadDeadline(t time.Time) error {
	if !c.beginOp() {
		return net.ErrClosed
	}
	defer c.ops.Done()
	c.mu.Lock()
	c.readDeadline = t
	c.mu.Unlock()
	if c.closed.Load() {
		return net.ErrClosed
	}
	return c.wake()
}

func (c *conn) SetWriteDeadline(t time.Time) error {
	if !c.beginOp() {
		return net.ErrClosed
	}
	defer c.ops.Done()
	c.mu.Lock()
	c.writeDeadline = t
	c.mu.Unlock()
	if c.closed.Load() {
		return net.ErrClosed
	}
	return c.wake()
}

func (c *conn) getReadDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.readDeadline
}

func (c *conn) getWriteDeadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeDeadline
}

func (c *conn) beginOp() bool {
	c.opMu.Lock()
	defer c.opMu.Unlock()
	if c.closed.Load() {
		return false
	}
	c.ops.Add(1)
	return true
}

func (c *conn) wake() error {
	_, err := syscall.Write(c.wakeWrite, []byte{1})
	if err == nil || isAgain(err) {
		return nil
	}
	if c.closed.Load() {
		return net.ErrClosed
	}
	return err
}

func (c *conn) wait(events int16, deadline func() time.Time) error {
	for {
		if c.closed.Load() {
			return net.ErrClosed
		}
		currentDeadline := deadline()
		timeout, expired := ioTimeoutMS(currentDeadline)
		if expired {
			return os.ErrDeadlineExceeded
		}
		pfds := []pollFD{
			{fd: int32(c.fd), events: events},
			{fd: int32(c.wakeRead), events: pollIn},
		}
		n, errno := ppoll(pfds, timeout)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			if c.closed.Load() {
				return net.ErrClosed
			}
			return errno
		}
		if n == 0 {
			if !currentDeadline.IsZero() {
				return os.ErrDeadlineExceeded
			}
			continue
		}
		if pfds[1].revents&(pollIn|pollHup|pollErr|pollNval) != 0 {
			c.drainWake()
			if c.closed.Load() {
				return net.ErrClosed
			}
			continue
		}
		if c.closed.Load() {
			return net.ErrClosed
		}
		if pfds[0].revents&pollNval != 0 {
			return net.ErrClosed
		}
		if pfds[0].revents&(events|pollErr|pollHup) != 0 {
			return nil
		}
	}
}

func ioTimeoutMS(deadline time.Time) (int, bool) {
	const maxPollMS = 1000
	if deadline.IsZero() {
		return maxPollMS, false
	}
	remaining := timeUntil(deadline)
	if remaining <= 0 {
		return 0, true
	}
	ms := int(remaining.Milliseconds())
	if ms <= 0 {
		return 1, false
	}
	if ms > maxPollMS {
		return maxPollMS, false
	}
	return ms, false
}

func isAgain(err error) bool {
	return err == syscall.EAGAIN || err == syscall.EWOULDBLOCK
}

func (c *conn) drainWake() {
	var buf [64]byte
	for {
		n, err := syscall.Read(c.wakeRead, buf[:])
		if err == syscall.EINTR {
			continue
		}
		if err != nil || n == 0 || n < len(buf) {
			return
		}
	}
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

type addr struct {
	cid  uint32
	port uint32
}

func (a addr) Network() string {
	return "vsock"
}

func (a addr) String() string {
	return fmt.Sprintf("%d:%d", a.cid, a.port)
}
