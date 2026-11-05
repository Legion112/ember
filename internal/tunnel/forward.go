// Package tunnel reaches a service on a node's own loopback interface over the
// SSH connection ember already holds.
//
// Everything here is written against a Dialer rather than against
// *ssh.Client, so it is tested end to end over net.Pipe with no SSH server, no
// Docker and no network — and so a future WireGuard transport plugs in without
// a caller changing a line.
package tunnel

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"time"
)

// Dialer opens a connection to an address on the far side of the tunnel.
//
// *ssh.Client satisfies this through DialContext.
type Dialer interface {
	DialContext(ctx context.Context, network, addr string) (net.Conn, error)
}

// RemoteLoopback names a port on the node's own loopback interface.
//
// It is the only way to name the far end, and it takes a port rather than an
// address on purpose. SSH's direct-tcpip will connect anywhere the node can
// reach — another tenant, the provider's metadata service, the open internet —
// so a free-form remote host turns every ember node into an open relay billed
// to the operator. The generation server always listens on the node's
// loopback, so there is nothing else ember needs to name.
func RemoteLoopback(port uint16) netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), port)
}

// WaitReady polls the far side through the tunnel until it accepts.
//
// A generation server does not open its port until its weights are loaded, so
// this is the first moment anything on the node can be asked a question --
// and a dial that is refused on a working tunnel is the single most likely
// false bug report this program can produce.
func WaitReady(ctx context.Context, d Dialer, remote netip.AddrPort, every time.Duration) error {
	if every <= 0 {
		every = 500 * time.Millisecond
	}
	for {
		conn, err := d.DialContext(ctx, "tcp", remote.String())
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s through the tunnel: %w", remote, ctx.Err())
		case <-time.After(every):
		}
	}
}
