// Package session connects to a ready node.
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
	"log/slog"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
)

// Options tune the connection.
type Options struct {
	Dialer sshdial.Dialer
	Log    *slog.Logger

	// SSHReady bounds how long sshd may take to start accepting.
	SSHReady time.Duration

	// RetryEvery is the gap between dial attempts.
	RetryEvery time.Duration
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
