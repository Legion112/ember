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
	"errors"
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

	// Stderr, when set, receives the command's standard error as it arrives.
	//
	// Nil keeps the default: stderr is buffered and attached to an ExitError,
	// which is right for a short command like tar where the only thing that
	// matters is why it failed. A workload is the other case -- it runs for
	// minutes and its progress is on stderr, and an operator watching a model
	// load deserves to see it rather than a silent terminal followed by a
	// summary.
	Stderr io.Writer
}

// Cmd builds a Command.
func Cmd(path string, args ...string) Command { return Command{Path: path, Args: args} }

// bare reports whether s can go on a command line untouched.
//
// An allow-list of characters no POSIX shell treats specially. Anything else
// is quoted rather than rejected -- see Quote. The list stays conservative
// because its only job is to keep ordinary commands readable in a log; being
// wrong here costs a pair of quotes, not a security property.
func bare(s string) bool {
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

// Quote renders s as a single POSIX shell word.
//
// Single quotes, because inside them every character except the single quote
// itself is literal -- no variable expansion, no command substitution, no
// backslash escapes, no word splitting. A single quote is ended, escaped and
// reopened as '\”. That is the whole algorithm, and its smallness is the
// argument for it: an allow-list of "dangerous" characters has to be right
// about every shell's metacharacters, while this has to be right about one.
//
// ember used to refuse anything unquotable instead, which was correct while
// every remote command was tar over paths from a config file. A workload
// command cannot work that way: a prompt is arbitrary text and a model's
// flags are arbitrary values. So the quoting exists now, and it is exercised
// on hostile input in both a unit test and a lab that round-trips the
// arguments through a real sshd and compares what arrived.
func Quote(s string) string {
	if bare(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Validate refuses what cannot be sent at all.
//
// Only a NUL byte and an empty path, because everything else survives
// quoting. A newline inside single quotes is a literal newline, which is
// what the caller asked for.
func (c Command) Validate() error {
	if c.Path == "" {
		return errors.New("remote command has no program to run")
	}
	if strings.ContainsRune(c.Path, 0) {
		return fmt.Errorf("remote command %q contains a NUL byte, which cannot appear in an argument at all", c.Path)
	}
	for _, a := range c.Args {
		if strings.ContainsRune(a, 0) {
			return fmt.Errorf("remote argument %q contains a NUL byte, which cannot appear in an argument at all", a)
		}
	}
	return nil
}

// String renders the command for the SSH exec request, quoted.
func (c Command) String() string {
	parts := make([]string, 0, len(c.Args)+1)
	parts = append(parts, Quote(c.Path))
	for _, a := range c.Args {
		parts = append(parts, Quote(a))
	}
	return strings.Join(parts, " ")
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
