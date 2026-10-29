// Package sshexec runs a command over a real SSH connection.
//
// Thin on purpose, and excluded from the coverage gate for the same reason
// dockerclient is: it has no decisions in it. Everything worth testing --
// which command to build, what to do with the bytes, what a non-zero exit
// means -- lives above it in sshx and transfer, against a recording fake.
package sshexec

import (
	"context"
	"fmt"
	"io"
	"sync"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/sshx"
)

// Runner runs commands on one SSH connection.
type Runner struct {
	// Client is an authenticated connection. sshdial owns making one, so the
	// host-key check has already happened by the time anything reaches here.
	Client *ssh.Client
}

// New returns a Runner over an existing connection.
func New(c *ssh.Client) *Runner { return &Runner{Client: c} }

// Run starts cmd and returns its stdout.
//
// The session is closed by the wait function, not by the reader, because the
// caller has to finish reading before the exit status means anything. Closing
// on the reader would cut the stream off at whatever had arrived.
func (r *Runner) Run(ctx context.Context, cmd sshx.Command) (io.ReadCloser, func() error, error) {
	if err := cmd.Validate(); err != nil {
		return nil, nil, err
	}

	sess, err := r.Client.NewSession()
	if err != nil {
		return nil, nil, fmt.Errorf("open an ssh session: %w", err)
	}

	stdout, err := sess.StdoutPipe()
	if err != nil {
		_ = sess.Close()
		return nil, nil, fmt.Errorf("attach to stdout: %w", err)
	}

	// Buffered by default: stderr is small for the commands ember runs on
	// its own behalf, and it is the only thing that explains a non-zero
	// exit. An error saying "tar exited 2" without "No such file or
	// directory" sends the operator looking in the wrong place.
	//
	// A workload sets Stderr and gets it streamed as well, because it runs
	// for minutes and its progress lives there.
	var stderr limitedBuffer
	if cmd.Stderr != nil {
		sess.Stderr = io.MultiWriter(&stderr, cmd.Stderr)
	} else {
		sess.Stderr = &stderr
	}

	line := cmd.String()
	if err := sess.Start(line); err != nil {
		_ = sess.Close()
		return nil, nil, fmt.Errorf("start %q: %w", line, err)
	}

	// Cancellation closes the session, which unblocks a read. A bare select
	// would never be scheduled out of a blocking Read on the stream.
	stop := context.AfterFunc(ctx, func() { _ = sess.Close() })

	var once sync.Once
	wait := func() error {
		var err error
		once.Do(func() {
			err = sess.Wait()
			stop()
			_ = sess.Close()
		})
		if err == nil {
			return nil
		}
		var exit *ssh.ExitError
		if asExitError(err, &exit) {
			return &sshx.ExitError{Cmd: line, Code: exit.ExitStatus(), Stderr: stderr.String()}
		}
		return fmt.Errorf("%q: %w", line, err)
	}
	return io.NopCloser(stdout), wait, nil
}
