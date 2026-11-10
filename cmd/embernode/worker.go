package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/worker"
)

// runWorker is the node's second supervised child: it answers ember's control
// channel and does the work.
//
// IT IS A SUBCOMMAND OF THIS BINARY RATHER THAN A GOROUTINE INSIDE IT, and
// that is what "a second supervised child" buys. embernode is PID 1 in the
// container and the three things it does -- install the host key, keep sshd
// alive, end the node at its deadline -- are the node's own safety. A worker
// in the same process would mean a panic in a job loop taking the deadline
// timer with it, which is the syscall.Exec mistake one level up: a safety
// property quietly doing nothing, with nothing about the running container
// looking wrong.
//
// IT TAKES NO ARGUMENTS, and that is a seal rather than a simplification.
// Every value it needs -- the port it answers on, the directory it writes
// into -- is a constant both ends know, so there is nothing for ember to
// inject and nothing for an operator's config to reach. The one thing that
// does cross, a job, is a typed frame and never an argv.
func runWorker(log *slog.Logger, args []string) error {
	// Parsed although there is nothing to parse, so `embernode worker --help`
	// answers and a typo is refused rather than ignored. A child started with
	// an argument this build does not know is a worker that silently did
	// something else.
	fs := flag.NewFlagSet("embernode worker", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("embernode worker takes no arguments, got %q", fs.Args())
	}

	// Interruptible, because the deadline and `docker stop` both arrive as a
	// signal and a worker that ignored them would be a generation carrying on
	// after the node was told to stop -- a card billing past its own deadline,
	// which is the exact failure armDeadline exists to prevent.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(control.ResultsDir, 0o700); err != nil {
		return fmt.Errorf("create %s: %w", control.ResultsDir, err)
	}

	// loopback.Listen AND NOT net.Listen, which is the whole reason this can
	// exist at all. #192 names the hazard exactly: "a gRPC server on the node
	// would be a listener, and TestNoListenOutsideLoopback fails the build for
	// net.Listen outside one package". Routing through the package that owns
	// the rule makes the worker's port unrepresentably non-loopback rather
	// than exempt from the check -- and internal/loopback imports only the
	// standard library, so the node binary takes on nothing it did not have.
	ln, err := loopback.Listen(ctx, loopback.Port(control.Port))
	if err != nil {
		return err
	}
	defer func() { _ = ln.Close() }()
	log.Info("the worker is listening for ember's control channel", "port", control.Port)

	// A plain dialler to this node's own loopback. Nothing is published and
	// nothing leaves the node: sd-server is a process in the same container,
	// and ember reaches THIS worker the same way it used to reach sd-server --
	// through the SSH connection it already holds.
	var d net.Dialer
	o := worker.Options{
		Gen:     sdserver.New(d.DialContext),
		Results: control.ResultsDir,
		Log:     log,
	}

	err = control.Serve(ctx, ln, log, func(ctx context.Context, c *control.Conn) error {
		return worker.Session(ctx, c, o)
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, context.Canceled):
		// The node was told to stop. Not a failure: it is the deadline or
		// `docker stop` doing what it is for, and an exit status here would
		// make the supervisor log a crash over an orderly shutdown.
		log.Info("the worker was stopped before its queue was empty")
		return nil
	default:
		return err
	}
}
