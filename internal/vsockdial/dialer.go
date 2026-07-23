package vsockdial

import (
	"context"
	"net"
)

type Dialer interface {
	Dial(ctx context.Context, cid, port uint32) (net.Conn, error)
}
