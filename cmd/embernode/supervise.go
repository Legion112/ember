package main

import (
	"errors"
	"log/slog"
	"os"
	"os/exec"
)

// proc is one process embernode supervises.
//
// AN INTERFACE BECAUSE THE RULE REQUIRES ONE. AGENTS.md: "external binaries at
// runtime (docker, ssh on the node) are fine, invoked from Go through an
// interface that has an in-process fake". sshd and the worker are exactly
// that, and until this file there was one child and the supervision was four
// lines of select inlined in run() -- which is why the one thing it decides,
// what a child's exit means, had no exit code anywhere.
//
// It is three methods and not a wrapper over *exec.Cmd's whole surface: what
// supervision needs is to forward a signal, to learn when the thing ended, and
// to name it in a log line.
type proc interface {
	// Signal forwards a signal. It is a no-op on a process that has already
	// exited -- os.Process refuses to signal one it has waited for, which is
	// what makes signalling the whole group safe after half of it is gone.
	Signal(sig os.Signal) error

	// Wait blocks until it exits and reports its status.
	Wait() error

	// Pid is for the log line.
	Pid() int
}

// cmdProc is the real one.
type cmdProc struct{ cmd *exec.Cmd }

func (p cmdProc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }
func (p cmdProc) Wait() error                { return p.cmd.Wait() }
func (p cmdProc) Pid() int                   { return p.cmd.Process.Pid }

// exited is one child's end.
type exited struct {
	name string
	err  error
}

// group is the children embernode holds, and the one place that knows what
// each one's exit means.
//
// WHY THIS EXISTS AT ALL: the run loop used to be a two-case select over ONE
// child, and a second child cannot be added to a select without a
// multiplexer. The property that had to survive the change is the meaning of
// the sshd arm -- "sshd exited on its own; nothing can connect to this node"
// -- because that is the case which makes a node observable at all, and the
// obvious refactor (return on whichever child exits first) silently turns a
// worker that finished its queue into a node that destroys its own sshd while
// ember is still fetching results over it.
type group struct {
	log   *slog.Logger
	kids  []named
	exits chan exited
}

type named struct {
	name string
	p    proc
}

// newGroup is empty and buffered for every child that can be added.
//
// BUFFERED SO A CHILD'S EXIT IS NEVER LOST. With an unbuffered channel, a
// child that exits while the loop is handling the other one's exit would block
// its own reporting goroutine forever -- and the exit that goes missing is the
// one the node's log exists to carry.
func newGroup(log *slog.Logger) *group {
	return &group{log: log, exits: make(chan exited, 4)}
}

// add starts watching p.
func (g *group) add(name string, p proc) {
	g.kids = append(g.kids, named{name: name, p: p})
	go func() { g.exits <- exited{name: name, err: p.Wait()} }()
}

// signalAll forwards sig to every child.
//
// EVERY CHILD, which is the whole point of the change: armDeadline used to
// signal sshd alone, so a node whose operator's laptop died would have had its
// sshd stopped and its worker left generating on a card nobody was paying
// attention to. The deadline is the thing that ends the bill, and a deadline
// that stops half the node is advisory.
func (g *group) signalAll(sig os.Signal) {
	for _, k := range g.kids {
		if err := k.p.Signal(sig); err != nil {
			// At debug, and never fatal: the ordinary reason is a child that
			// has already exited, which os.Process reports rather than
			// signalling a pid the kernel may have reused.
			g.log.Debug("could not signal a child", "child", k.name, "signal", sig.String(), "err", err)
		}
	}
}

// names is every child, for a log line.
func (g *group) names() []string {
	out := make([]string, 0, len(g.kids))
	for _, k := range g.kids {
		out = append(out, k.name)
	}
	return out
}

// childSSHD and childWorker are the two names, as constants because the
// supervisor compares against them and a typo would turn "sshd died" into
// "something died" -- the arm that keeps a node observable, silently widened.
const (
	childSSHD   = "sshd"
	childWorker = "worker"
)

// supervise is the run loop: forward a signal, or report whichever child
// ended.
//
// It returns when sshd ends or a signal arrives, and NOT when the worker ends.
// Those two are different events on a rented machine:
//
//   - sshd ending means nothing can connect, so the node is useless and
//     saying so is the only thing left to do. This is the arm that had no
//     record at all before it was written: "no hostkeys available -- exiting"
//     is what a private key one byte short produces, it happens in the second
//     before ember's first dial, and what ember then reports is a connection
//     refused on a node that looks perfectly healthy.
//   - the worker ending means its queue is empty and ember has told it no
//     more work is coming. ember is at that moment still fetching results and
//     the node's own log over the SSH connection, and it is about to destroy
//     the node. Ending sshd here would cut the fetch, so what stops the bill
//     is ember's destroy or the deadline -- which is where it already was.
//
// THE WORKER'S EXIT THEREFORE DOES NOT STOP THE BILL, and the plan this
// implements says it would: "a pod whose container exits stops billing".
// That is true of a container whose only process exits and it does not apply
// to a supervised worker, because embernode is PID 1 and stays. The worker's
// exit is the SIGNAL that the work is done, carried on the control channel as
// well; ember's destroy is what ends the bill, and the TTL is what ends it
// when ember is gone.
func supervise(log *slog.Logger, g *group, sigs <-chan os.Signal) (int, error) {
	for {
		select {
		case sig := <-sigs:
			log.Info("signal received, forwarding it to every child and stopping",
				"signal", sig.String(), "children", g.names())
			g.signalAll(sig)
			// Wait for them so `docker stop` is quick rather than taking the
			// full ten-second grace period before a kill. Their exit statuses
			// are not interesting: the node is being stopped on purpose, and
			// reporting a child's reaction to a signal we sent would turn an
			// orderly shutdown into a non-zero container exit.
			for range g.kids {
				<-g.exits
			}
			return 0, nil

		case ex := <-g.exits:
			if ex.name != childSSHD {
				logChild(log, ex)
				continue
			}
			var exitErr *exec.ExitError
			if errors.As(ex.err, &exitErr) {
				log.Error("sshd exited on its own; nothing can connect to this node",
					"exit", exitErr.ExitCode())
				return exitErr.ExitCode(), nil
			}
			if ex.err != nil {
				return 0, errors.New("sshd: " + ex.err.Error())
			}
			return 0, nil
		}
	}
}

// logChild records a child that is not sshd ending, at the level its status
// deserves.
//
// NOT SILENT, AND THE TWO CASES READ DIFFERENTLY. A clean exit is the expected
// end of a worker and says the node is finished with; a crash says the node is
// still reachable and can no longer be given work, which is a state ember
// discovers on its next push and an operator can otherwise only infer from a
// rental that did nothing.
func logChild(log *slog.Logger, ex exited) {
	var exitErr *exec.ExitError
	switch {
	case ex.err == nil:
		log.Info("the worker's queue is empty and it has exited; ember is expected to destroy this node now",
			"child", ex.name)
	case errors.As(ex.err, &exitErr):
		log.Error("a child exited on its own; this node can no longer be given work",
			"child", ex.name, "exit", exitErr.ExitCode())
	default:
		log.Error("a child could not be waited for; this node can no longer be given work",
			"child", ex.name, "err", ex.err)
	}
}
