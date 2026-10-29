// Package workload runs the command a node was rented for.
//
// It runs over the SSH connection ember already has, rather than as the
// container's entrypoint. That ordering is the design: a node ember cannot
// reach is a node ember cannot destroy either, so there is no value in a
// workload that outlives the connection — and running it here means the
// operator watches the output live instead of reading a log afterwards, and
// the exit status is known at the moment it happens rather than polled for
// through a file.
//
// The consequence worth naming: if the connection drops, the workload is
// orphaned on a node ember is about to destroy anyway. The deadline and the
// node's own self-destruct both still apply.
package workload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
)

// Result is what one workload run came to.
type Result struct {
	Cmd      string
	Duration time.Duration

	// ExitCode is the remote command's status. Zero on success; -1 when the
	// command never produced one, which is a transport failure rather than a
	// workload that failed.
	ExitCode int
}

// Run executes spec.Cmd on the node and streams its output.
//
// Both streams go to out as they arrive. A workload that spends four minutes
// loading a model and then fails deserves to have shown what it was doing;
// collecting the output and printing it at the end is the behaviour that makes
// an operator reach for the console instead.
func Run(ctx context.Context, r sshx.Runner, cmd []string, out io.Writer) (Result, error) {
	if len(cmd) == 0 {
		return Result{}, errors.New("no workload command")
	}

	c := sshx.Cmd(cmd[0], cmd[1:]...)
	c.Stderr = out
	if err := c.Validate(); err != nil {
		return Result{}, err
	}

	res := Result{Cmd: c.String()}
	start := time.Now()

	stdout, wait, err := r.Run(ctx, c)
	if err != nil {
		res.Duration = time.Since(start)
		res.ExitCode = -1
		return res, fmt.Errorf("start workload: %w", err)
	}
	defer func() { _ = stdout.Close() }()

	// Copied rather than buffered, for the same reason stderr is streamed.
	// The error is kept and reported after the exit status, because a read
	// that ended early on a command that then failed is explained by the
	// failure.
	_, copyErr := io.Copy(out, stdout)

	waitErr := wait()
	res.Duration = time.Since(start)

	var exit *sshx.ExitError
	switch {
	case waitErr == nil:
		if copyErr != nil {
			res.ExitCode = -1
			return res, fmt.Errorf("the workload succeeded but its output could not be read: %w", copyErr)
		}
		return res, nil
	case errors.As(waitErr, &exit):
		res.ExitCode = exit.Code
		return res, fmt.Errorf("the workload failed: %w", waitErr)
	default:
		res.ExitCode = -1
		return res, fmt.Errorf("the workload could not be run to completion: %w", waitErr)
	}
}

// Describe is the one line printed before a workload starts.
//
// The command is shown exactly as it will be sent, quoting included, so an
// operator debugging a prompt that did not arrive as intended can see what
// the remote shell was actually handed.
func Describe(cmd []string) string {
	if len(cmd) == 0 {
		return ""
	}
	return sshx.Cmd(cmd[0], cmd[1:]...).String()
}

// Validate checks a command before a node is rented.
//
// Called from NewSpec's caller rather than after the machine is running,
// because a command ember cannot send is a config error, and finding a config
// error after the create call is finding it after the billing started.
func Validate(cmd []string) error {
	if len(cmd) == 0 {
		return nil
	}
	if strings.TrimSpace(cmd[0]) == "" {
		return errors.New("the workload command has no program to run")
	}
	return sshx.Cmd(cmd[0], cmd[1:]...).Validate()
}

// Outputs reports whether a spec asks for anything to be brought home.
//
// A workload that generates files and no outputs is almost always a mistake:
// the node is destroyed when the command finishes, taking whatever it made
// with it.
func Outputs(s node.Spec) bool { return len(s.Outputs) > 0 }
