// Package sshx runs one command on a node and streams its output back.
//
// It is a seam, not an abstraction for its own sake. The thing above it --
// fetching a run's outputs as a tar stream -- is where the interesting logic
// is, and that logic should be testable against an in-memory tar with no SSH
// server, no Docker and no build tag. The real implementation lives in
// sshexec and is thin enough to be excluded from the coverage gate.
package sshx

import (
	"context"
	"fmt"
	"io"
	"strings"
)

// Runner runs a single command on a node.
//
// Stdout is streamed rather than buffered, because an output tar is the whole
// point and a run that produced four gigabytes of frames must not need four
// gigabytes of memory on the operator's laptop to collect them.
//
// Wait is separate from the reader so the caller can finish reading the stream
// before learning the exit status. A Runner that returned the status first
// would have to buffer, and one that returned only the reader would let a
// failing remote command look like a truncated-but-successful transfer -- which
// is the specific way an output fetch loses data quietly.
type Runner interface {
	Run(ctx context.Context, cmd Command) (stdout io.ReadCloser, wait func() error, err error)
}

// Command is a remote command and its arguments.
//
// Argv and not a string. An SSH "exec" request is handed to the remote shell,
// so a command assembled by concatenation is a shell injection with the
// operator's own config as the input. Keeping the parts separate means the one
// place that has to quote them is String, and the one place that has to refuse
// anything unquotable is Validate.
type Command struct {
	Path string
	Args []string
}

// Cmd builds a Command.
func Cmd(path string, args ...string) Command { return Command{Path: path, Args: args} }

// safe reports whether s needs no quoting at all.
//
// An allow-list, because the set of characters a shell treats specially is
// long, varies between shells, and is exactly the kind of list that is wrong
// in the one entry nobody thought of. Everything outside this set is refused
// rather than escaped: ember's remote commands are `tar` over paths from the
// operator's own config, and a path that cannot be expressed without quoting
// tricks is one worth rejecting out loud.
func safe(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '/', r == '.', r == '_', r == '-', r == '+', r == '=', r == ':', r == ',':
		default:
			return false
		}
	}
	return true
}

// Validate refuses anything that could not be sent to a remote shell
// unambiguously.
func (c Command) Validate() error {
	if !safe(c.Path) {
		return fmt.Errorf("remote command %q contains characters ember will not send to a shell", c.Path)
	}
	for _, a := range c.Args {
		if !safe(a) {
			return fmt.Errorf("remote argument %q contains characters ember will not send to a shell; a path it cannot express plainly is one worth refusing", a)
		}
	}
	return nil
}

// String renders the command for the SSH exec request.
//
// Validate must have passed, so no quoting is needed and none is done: a
// quoting routine that is never exercised on a hostile input is a quoting
// routine nobody knows is wrong.
func (c Command) String() string {
	if len(c.Args) == 0 {
		return c.Path
	}
	return c.Path + " " + strings.Join(c.Args, " ")
}

// ExitError is a remote command that ran and failed.
//
// Distinct from a transport failure, because the remedies differ: a non-zero
// tar means the path was not there, and a dropped connection means try again.
type ExitError struct {
	Cmd    string
	Code   int
	Stderr string
}

func (e *ExitError) Error() string {
	s := fmt.Sprintf("remote command %q exited %d", e.Cmd, e.Code)
	if e.Stderr != "" {
		s += ": " + strings.TrimSpace(e.Stderr)
	}
	return s
}
