//go:build !linux

package vsockdial

import (
	"context"
	"errors"
	"net"
)

type dialer struct{}

func New() Dialer {
	return dialer{}
}

func (dialer) Dial(context.Context, uint32, uint32) (net.Conn, error) {
	return nil, errors.New("vsock dialer is only available on linux")
}
