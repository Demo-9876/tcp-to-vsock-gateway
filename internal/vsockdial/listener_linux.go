//go:build linux

package vsockdial

import (
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"
)

const vmaddrCIDAny uint32 = 0xFFFF_FFFF

type ListenerFactory interface {
	Listen(port uint32) (net.Listener, error)
}

type listenerFactory struct{}

func NewListenerFactory() ListenerFactory {
	return listenerFactory{}
}

func (listenerFactory) Listen(port uint32) (net.Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("socket(AF_VSOCK): %w", err)
	}
	vmAddr := sockaddrVM{
		Family: afVsock,
		Port:   port,
		CID:    vmaddrCIDAny,
	}
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&vmAddr)), unsafe.Sizeof(vmAddr)); errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("bind(AF_VSOCK:%d): %w", port, errno)
	}
	if err := syscall.Listen(fd, 1); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("listen(AF_VSOCK:%d): %w", port, err)
	}
	wake := []int{-1, -1}
	if err := syscall.Pipe2(wake, syscall.O_CLOEXEC|syscall.O_NONBLOCK); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("pipe2 wake fd: %w", err)
	}
	return &listener{
		fd:        fd,
		wakeRead:  wake[0],
		wakeWrite: wake[1],
		addr:      addr{cid: vmaddrCIDAny, port: port},
	}, nil
}

type listener struct {
	fd        int
	wakeRead  int
	wakeWrite int
	addr      addr
	closeOnce sync.Once
	closed    atomic.Bool
}

func (l *listener) Accept() (net.Conn, error) {
	for {
		if l.closed.Load() {
			return nil, net.ErrClosed
		}
		nfd, _, err := syscall.Accept4(l.fd, syscall.SOCK_CLOEXEC|syscall.SOCK_NONBLOCK)
		if err == nil {
			c, err := newConn(nfd, 0, l.addr.port)
			if err != nil {
				_ = syscall.Close(nfd)
				return nil, err
			}
			c.local = l.addr
			return c, nil
		}
		if err == syscall.EINTR {
			continue
		}
		if !isAgain(err) {
			if l.closed.Load() {
				return nil, net.ErrClosed
			}
			return nil, err
		}
		if err := l.wait(); err != nil {
			return nil, err
		}
	}
}

func (l *listener) Close() error {
	var closeErr error
	l.closeOnce.Do(func() {
		l.closed.Store(true)
		_, _ = syscall.Write(l.wakeWrite, []byte{1})
		closeErr = firstErr(
			syscall.Close(l.fd),
			syscall.Close(l.wakeRead),
			syscall.Close(l.wakeWrite),
		)
	})
	return closeErr
}

func (l *listener) Addr() net.Addr {
	return l.addr
}

func (l *listener) wait() error {
	for {
		pfds := []pollFD{
			{fd: int32(l.fd), events: pollIn},
			{fd: int32(l.wakeRead), events: pollIn},
		}
		_, errno := ppoll(pfds, 1000)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			if l.closed.Load() {
				return net.ErrClosed
			}
			return errno
		}
		if pfds[1].revents&(pollIn|pollHup|pollErr|pollNval) != 0 {
			return net.ErrClosed
		}
		if pfds[0].revents&(pollIn|pollErr|pollHup|pollNval) != 0 {
			if l.closed.Load() {
				return net.ErrClosed
			}
			return nil
		}
		if l.closed.Load() {
			return net.ErrClosed
		}
	}
}
