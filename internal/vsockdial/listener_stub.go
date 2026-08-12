//go:build !linux

package vsockdial

import (
	"errors"
	"net"
)

type ListenerFactory interface {
	Listen(port uint32) (net.Listener, error)
}

type listenerFactory struct{}

func NewListenerFactory() ListenerFactory {
	return listenerFactory{}
}

func (listenerFactory) Listen(uint32) (net.Listener, error) {
	return nil, errors.New("vsock listener is only available on linux")
}
