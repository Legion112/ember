// Package sshdial opens the one SSH connection ember uses per node.
//
// It is the only place in the program where an ssh.HostKeyCallback is
// assigned, and it can only obtain one from hostkey.Callback — whose argument
// is an interface this package cannot implement, because its single method is
// unexported. The insecure path therefore has nowhere to be written, rather
// than being forbidden by review.
package sshdial

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/node"
)

// Target is the node to reach.
type Target struct {
	// NodeID is ember's own identifier, used for the host-key pin and in
	// error messages. It is not the hostname: pins are keyed by node so that
	// a recycled provider IP cannot match a stale one.
	NodeID   string
	Endpoint node.Endpoint
}

// Dialer opens SSH connections.
//
// Net is a seam: the tests run a real ssh.NewServerConn over net.Pipe, so the
// handshake and the host-key check are exercised in milliseconds with no
// sshd, no Docker and no build tag — which matters because this is where the
// security property lives.
type Dialer struct {
	Net       func(ctx context.Context, network, addr string) (net.Conn, error)
	Timeout   time.Duration
	Keepalive time.Duration
}

const (
	defaultTimeout   = 30 * time.Second
	defaultKeepalive = 30 * time.Second
)

// Client is an SSH connection to a node. It satisfies tunnel.Dialer.
type Client struct {
	ssh  *ssh.Client
	stop chan struct{}
}

// DialContext opens a connection to addr from the node's side of the tunnel.
func (c *Client) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	return c.ssh.DialContext(ctx, network, addr)
}

// Close tears down the connection and its keepalive.
func (c *Client) Close() error {
	select {
	case <-c.stop:
	default:
		close(c.stop)
	}
	return c.ssh.Close()
}

// Dial authenticates to the target and returns a Client.
//
// trust is the only way the host key is judged; there is no parameter, flag or
// config key that disables it.
func (d Dialer) Dial(ctx context.Context, t Target, trust hostkey.Trust, signers []ssh.Signer) (*Client, error) {
	if trust == nil {
		return nil, errors.New("refusing to dial without a host key policy")
	}
	if len(signers) == 0 {
		return nil, fmt.Errorf("no signers for node %s: nothing to authenticate with", t.NodeID)
	}
	if err := t.Endpoint.Validate(); err != nil {
		return nil, fmt.Errorf("node %s: %w", t.NodeID, err)
	}

	timeout := d.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	addr := fmt.Sprintf("%s:%d", t.Endpoint.Host, t.Endpoint.Port)
	conn, err := d.netDial(ctx, addr, timeout)
	if err != nil {
		return nil, fmt.Errorf("connect to node %s at %s: %w", t.NodeID, addr, err)
	}

	cfg := &ssh.ClientConfig{
		User:            t.Endpoint.User,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signers...)},
		HostKeyCallback: hostkey.Callback(trust, t.NodeID),
		Timeout:         timeout,
	}

	sc, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake with node %s at %s: %w", t.NodeID, addr, err)
	}

	c := &Client{ssh: ssh.NewClient(sc, chans, reqs), stop: make(chan struct{})}

	interval := d.Keepalive
	if interval <= 0 {
		interval = defaultKeepalive
	}
	go c.keepalive(interval)
	return c, nil
}

func (d Dialer) netDial(ctx context.Context, addr string, timeout time.Duration) (net.Conn, error) {
	if d.Net != nil {
		return d.Net(ctx, "tcp", addr)
	}
	var nd net.Dialer
	nd.Timeout = timeout
	return nd.DialContext(ctx, "tcp", addr)
}

// keepalive stops a provider's idle timeout from black-holing the session.
//
// Without it the connection stays open as far as this end can tell, and the
// next browser request hangs until TCP gives up minutes later. The operator
// reads that as "ember is slow", which is the worst possible diagnosis to be
// handed: it points at the wrong component and there is nothing in any log.
func (c *Client) keepalive(every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-c.stop:
			return
		case <-ticker.C:
			if _, _, err := c.ssh.SendRequest("keepalive@openssh.com", true, nil); err != nil {
				return
			}
		}
	}
}
