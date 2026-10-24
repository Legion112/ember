// Package session connects to a ready node and holds the tunnel open.
//
// It exists as its own package because the two decisions in it — when to
// retry a dial and when to refuse one — are the kind that need tests, and
// they were previously buried in a CLI command where the only way to exercise
// them was to rent a machine.
package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
)

// outf writes to the operator's stream. The error is discarded on purpose: a
// failed write to stdout means the pipe is gone -- `ember up | head` is the
// ordinary way that happens -- and surfacing it would end a healthy session
// with a non-zero status.
func outf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// Options tune the connection and the hold.
type Options struct {
	Dialer sshdial.Dialer
	Log    *slog.Logger

	// SSHReady bounds how long sshd may take to start accepting.
	SSHReady time.Duration

	// RetryEvery is the gap between dial attempts.
	RetryEvery time.Duration

	// Hold ends the session after this long. Zero waits for the context.
	Hold time.Duration
}

const (
	defaultSSHReady   = 90 * time.Second
	defaultRetryEvery = time.Second
)

func (o Options) sshReady() time.Duration {
	if o.SSHReady > 0 {
		return o.SSHReady
	}
	return defaultSSHReady
}

func (o Options) retryEvery() time.Duration {
	if o.RetryEvery > 0 {
		return o.RetryEvery
	}
	return defaultRetryEvery
}

func (o Options) log() *slog.Logger {
	if o.Log != nil {
		return o.Log
	}
	return slog.New(slog.DiscardHandler)
}

// Dial waits for sshd rather than for the port.
//
// A provider reports a node ready once its port is published, and Docker
// publishes the moment the container starts — which is before sshd has read
// its config and bound a socket. The first dial therefore gets "connection
// reset by peer", and treating that as fatal makes `ember up` fail on a node
// that is perfectly fine half a second later. That is not hypothetical: it is
// what the first working end-to-end run did.
//
// A host key mismatch is **not** retried. That one is never transient, and
// retrying it would turn a refusal into a hang — and a hang is how an
// operator ends up disabling the check.
func Dial(ctx context.Context, target sshdial.Target, trust hostkey.Trust, signers []ssh.Signer, o Options) (*sshdial.Client, error) {
	deadline := time.Now().Add(o.sshReady())
	var last error

	for attempt := 1; ; attempt++ {
		client, err := o.Dialer.Dial(ctx, target, trust, signers)
		if err == nil {
			return client, nil
		}
		last = err

		var mismatch *hostkey.MismatchError
		if errors.As(err, &mismatch) {
			return nil, err
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("sshd on node %s never accepted a connection within %v: %w",
				target.NodeID, o.sshReady(), last)
		}
		o.log().Debug("sshd not accepting yet", "node", target.NodeID, "attempt", attempt, "err", err)

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(o.retryEvery()):
		}
	}
}

// Forwards builds the forward set for a spec's ports.
//
// Both loopback families are bound, because a browser that resolves localhost
// to ::1 first sees connection-refused on a working tunnel and reports it as
// a bug in ember.
func Forwards(ports []node.PortMap) []tunnel.Forward {
	out := make([]tunnel.Forward, 0, len(ports))
	for _, p := range ports {
		out = append(out, tunnel.Forward{
			Name:   p.Name,
			Local:  []loopback.Bind{loopback.Port(p.Local), loopback.Port6(p.Local)},
			Remote: tunnel.RemoteLoopback(p.Remote),
		})
	}
	return out
}

// Serve holds the tunnel open until the context ends or Hold elapses.
//
// A context that ends is the ordinary way out — the operator pressed Ctrl-C,
// or the node reached its deadline — so it is not an error. Returning one
// would teach the operator to ignore ember's exit code, and the exit code is
// how `ember ls` reports findings.
func Serve(ctx context.Context, out io.Writer, d tunnel.Dialer, fwds []tunnel.Forward, o Options) error {
	if o.Hold > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Hold)
		defer cancel()
	}

	if len(fwds) == 0 {
		outf(out, "no ports to forward; holding the node until you stop it\n")
		<-ctx.Done()
		return nil
	}

	for _, f := range fwds {
		outf(out, "  %-12s http://%s\n", f.Name, f.Local[0])
	}
	outf(out, "\nCtrl-C to stop. The node is destroyed either way.\n")

	f := &tunnel.Forwarder{Dialer: d, Log: o.Log}
	err := f.Run(ctx, fwds)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}
	return err
}
