package exposure

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"
)

// DialFunc is the seam a probe dials through, satisfied by
// (*net.Dialer).DialContext in production.
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Probe dials a node from outside to see what answers.
//
// What a probe can and cannot establish, stated plainly because it is easy to
// over-read: a port that ACCEPTS a connection is conclusive -- it is open, the
// assertion was wrong, and that is a finding. A port that refuses or times out
// is only EVIDENCE of being closed. A provider firewall that DROPs packets is
// indistinguishable from a closed port from here, and so is a transient
// network failure on the way out of the operator's own machine.
//
// So a probe is never the primary control. AssertSSHOnly reading the
// provider's own mappings is, because it fails closed on the case the probe
// cannot see; the probe catches the different case where the provider's API
// says one thing and its network does another.
type Probe struct {
	// Dial defaults to a net.Dialer with Timeout.
	Dial DialFunc

	// Timeout bounds each individual port. It is per-port and not for the
	// whole set: a run of DROPped ports would otherwise eat one budget
	// between them and the later ports would report "closed" because the
	// clock ran out, which is a false pass on the only answer that matters.
	Timeout time.Duration
}

func (p Probe) timeout() time.Duration {
	if p.Timeout > 0 {
		return p.Timeout
	}
	return 5 * time.Second
}

func (p Probe) dial() DialFunc {
	if p.Dial != nil {
		return p.Dial
	}
	d := &net.Dialer{Timeout: p.timeout()}
	return d.DialContext
}

// Open reports which of ports accepted a TCP connection on host.
//
// It returns the open ports rather than an error for them, because "port 8188
// answered" is a fact the caller decides what to do about; a non-nil error
// here means the probe itself could not run.
func (p Probe) Open(ctx context.Context, host string, ports []uint16) ([]uint16, error) {
	if host == "" {
		return nil, fmt.Errorf("probe needs a host to dial; an empty host dials this machine, which would report the operator's own listeners as the node's")
	}

	var open []uint16
	for _, port := range ports {
		pctx, cancel := context.WithTimeout(ctx, p.timeout())
		conn, err := p.dial()(pctx, "tcp", net.JoinHostPort(host, strconv.Itoa(int(port))))
		cancel()

		if err != nil {
			// Refused, dropped, or the context ended. All three are
			// "no evidence that it is open", which is the most this can say.
			if ctx.Err() != nil {
				return open, fmt.Errorf("probe of %s was cut short after %d port(s); the ports not yet dialled are unverified, not closed: %w", host, len(open), ctx.Err())
			}
			continue
		}
		_ = conn.Close()
		open = append(open, port)
	}
	return open, nil
}

// AssertClosed requires that none of ports answer on host.
//
// The caller's own cancellation is honoured and reported as an error rather
// than as a pass, since an interrupted probe has verified nothing.
func (p Probe) AssertClosed(ctx context.Context, host string, ports []uint16) error {
	open, err := p.Open(ctx, host, ports)
	if err != nil {
		return err
	}
	if len(open) > 0 {
		return fmt.Errorf("%d port(s) on %s accept connections from outside and should not: %v", len(open), host, open)
	}
	return nil
}
