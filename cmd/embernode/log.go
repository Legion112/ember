package main

import (
	"io"
	"log/slog"
	"os"

	"github.com/Legion112/ember/internal/logx"
)

// WHY THIS FILE EXISTS: the node's logs used to die with the node.
//
// embernode installs the host key, installs the authorized key, starts sshd
// and arms the self-destruct timer -- the four things whose failure makes a
// rented machine useless -- and it wrote plain text to stderr, which nothing
// in ember ever read. `grep -rn 'docker logs\|ContainerLogs\|journalctl'`
// over this module returned nothing. So the component where debugging is
// hardest and most expensive was the one whose evidence ember then destroyed,
// on a context that survives Ctrl-C, by design.
//
// The host-key incident, the trust-on-first-use fallback, the missing
// /workspace and the wrong-architecture abort were every one of them
// diagnosed by a person watching a terminal while it happened. Nothing would
// have survived to be read afterwards.
//
// WHAT THIS COSTS, SAID PLAINLY. cmd/embernode now imports internal/logx.
// That is one more intra-module dependency for the binary that runs on the
// rented machine, and it is not free: a change to logx can break the node
// binary, which is the component that cannot be redeployed to a pod that is
// already running. Three things bound it. logx imports only the standard
// library, so nothing can arrive behind the node binary's back. This package
// was already not standard-library-only -- fetch.go imports internal/assets
// and internal/node -- so the property being given up was already given up.
// And the alternative was a second place deciding ember's log format, which
// is the defect internal/logx exists to remove: a node whose format differed
// from the CLI's is a node whose logs cannot be read with the same filter as
// everything else.
//
// WHAT IT DOES NOT DO: there is no egress from the node and nothing here
// pushes anywhere. The log comes home because internal/app copies the file
// over the SSH connection it already has, which is the mechanism that
// inherits "outputs are fetched on a context that survives Ctrl-C" for free.

// logger builds the node's logger: stderr, and a file that outlives the
// process.
//
// BOTH, NOT ONE. stderr is what `docker logs` and a provider's own log view
// show, and it is what a person watching a lab reads; the file is the copy
// ember brings home. Replacing stderr with the file would have made the live
// view worse in order to make the post-mortem possible.
//
// A FILE THAT CANNOT BE OPENED IS NOT FATAL, and this is the one rule here
// that is not negotiable: embernode is PID 1 in the container, so a node that
// refuses to start because it could not open a log file is a node that never
// accepts a connection and never arms its own deadline -- ember would report
// a handshake failure and the real reason would be a read-only /tmp. Logging
// is the one subsystem that must never be load-bearing. The failure is
// reported on the stderr copy, so an operator reading the container's log can
// still see why nothing came home.
//
// THE LEVEL IS NOT READ FROM THE ENVIRONMENT HERE, although logx names two
// variables for it. The node's environment is written by ember through a
// provider's API, so a level read here could not be set without a new field
// on node.CreateRequest -- and everything embernode logs is a lifecycle
// moment it should always report. There is nothing at debug for a level to
// reveal.
func logger() *slog.Logger {
	opts := logx.Options{Level: slog.LevelInfo, Format: logx.FormatText}

	f, err := os.OpenFile(logx.NodeLogPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		l := logx.New(os.Stderr, opts)
		l.Warn("could not open the log file; this node's log will not survive it",
			"path", logx.NodeLogPath, "err", err)
		return l
	}
	// Never closed, on purpose. This process is the container's init and
	// lives as long as the node does; a Close on a path out of run() would
	// silence the deadline timer, which is the one line written after
	// everything else has finished.
	return logx.New(io.MultiWriter(os.Stderr, f), opts)
}
