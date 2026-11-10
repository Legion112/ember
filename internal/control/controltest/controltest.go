// Package controltest drives a real control channel with no node.
//
// WHY A PACKAGE AND NOT A HELPER IN EACH TEST FILE. Two packages need the same
// rig -- internal/worker, which is the node end, and internal/app, which is
// ember's -- and the half that is fiddly is the listener: the channel is a
// websocket over HTTP, so the node end needs something to accept connections
// on, and a second copy of that is a second thing to get subtly wrong. This is
// internal/provider/providertest's arrangement one layer down.
//
// EVERYTHING HERE IS THE PRODUCTION PATH. The handshake, the version check,
// the JSON codec, the read limits and the one-channel-at-a-time refusal are
// control's own, over net.Pipe instead of over an SSH direct-tcpip channel.
// Nothing in this package can build a *control.Conn that skipped the
// handshake, which is deliberate: that check is the seal on a node image older
// than the ember driving it, and a test rig that walked around it would be an
// escape hatch for exactly the thing it is supposed to prove.
package controltest

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Legion112/ember/internal/control"
)

// Node starts the worker end of a channel and returns the dial function ember
// reaches it through.
//
// fn is what the node does with the channel -- worker.Session in anger, a
// script of frames in a test about ember. It is called once, for the first
// channel opened: control.Serve answers one per worker, because a node is one
// rental.
//
// The returned DialFunc is what sshdial.Client.DialContext is in production,
// and it ignores the address for the same reason tunnel.RemoteLoopback takes
// a port and not an address: there is exactly one place on the far side that
// ember is allowed to reach.
func Node(t *testing.T, fn func(context.Context, *control.Conn) error) control.DialFunc {
	t.Helper()

	ln := NewPipe()
	ctx, cancel := context.WithCancel(context.Background())

	served := make(chan error, 1)
	go func() { served <- control.Serve(ctx, ln, Log(t), fn) }()

	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		// Waited for, so a session goroutine cannot outlive the test that
		// started it and write to a logger whose *testing.T has gone. That is
		// a panic in an unrelated test, which is the hardest kind of flake to
		// attribute. Its error is not read: by here the test has already
		// asserted whatever it was about, and the ordinary value is the
		// context cancellation two lines above.
		<-served //nolint:errcheck // deliberate: see above
	})
	return ln.DialContext
}

// RawNode is a node that is not this build's worker.
//
// IT EXISTS FOR THE REFUSALS, which are the assertions that cannot be made
// through Node: control.Serve sends the right hello and the right message
// types, so nothing driven through it can produce a version mismatch, a
// binary frame or bytes that are not JSON -- and those are exactly what the
// handshake and the codec are for. A node image built from another tree, or
// something else listening on the worker's port, is what this stands in for.
//
// It is here rather than as a hook on control.Conn on purpose: a method that
// let a caller send whatever it liked would be an escape hatch in production
// code for the seal it is supposed to prove.
func RawNode(t *testing.T, fn func(context.Context, *websocket.Conn) error) control.DialFunc {
	t.Helper()

	ln := NewPipe()
	ctx, cancel := context.WithCancel(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc(control.Path, func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.CloseNow() }()
		_ = fn(r.Context(), ws)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return ctx },
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ln)
	}()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		_ = ln.Close()
		<-done
	})
	return ln.DialContext
}

// Pair returns ember's end of a channel whose other end is fn.
//
// The handshake has already happened, so a caller holding this can push work.
func Pair(t *testing.T, fn func(context.Context, *control.Conn) error) *control.Conn {
	t.Helper()

	c, err := control.Open(t.Context(), Node(t, fn))
	if err != nil {
		t.Fatalf("open the control channel: %v", err)
	}
	t.Cleanup(c.Abort)
	return c
}

// Log is a logger that discards.
//
// DISCARDS RATHER THAN WRITING TO t.Log, and that is not laziness: the worker
// logs from a goroutine that is still running when the test ends -- it is
// blocked reading a channel the test is about to drop -- and a *testing.T
// written to after its test returns is a panic in whatever runs next.
//
// slog.DiscardHandler and not a text handler over io.Discard, which is what
// internal/logx's TestNoHandlerOutsideLogx permits and refuses respectively:
// one place decides ember's level and format, and a handler built here would
// be a second -- while a discard decides neither.
func Log(t *testing.T) *slog.Logger {
	t.Helper()
	return slog.New(slog.DiscardHandler)
}

// NewPipe is a listener and the dialler that feeds it.
//
// Exported for the one test that has to call control.Serve itself, with a
// context of its own, because what it is about is what Serve does when the
// node is stopped -- which the rig above cannot show, since the rig is the
// thing doing the stopping.
func NewPipe() *Pipe {
	return &Pipe{conns: make(chan net.Conn), closed: make(chan struct{})}
}

// Pipe is a net.Listener over net.Pipe, plus the dialler that feeds
// it.
//
// Each dial makes a fresh pipe and hands one end over. That is what lets
// tunnel.WaitReady's dial-and-close land on this listener the way it lands on
// a node -- the connection is accepted, carries no HTTP request, and goes away
// -- and the channel ember actually uses be the next one.
type Pipe struct {
	conns  chan net.Conn
	closed chan struct{}

	closeOnce sync.Once
}

func (l *Pipe) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	ours, theirs := net.Pipe()
	select {
	case l.conns <- ours:
		return theirs, nil
	case <-l.closed:
		return nil, net.ErrClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (l *Pipe) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.closed:
		// net.ErrClosed is what http.Server reads as "stop serving" rather
		// than as an error to retry, which is how a listener that has been
		// closed is supposed to answer.
		return nil, net.ErrClosed
	}
}

// Close is idempotent, and it takes a sync.Once to be so.
//
// IT WAS A SELECT ON THE CHANNEL WITH A `default` THAT CLOSED IT, which is
// check-then-act and not idempotent at all: two concurrent closes both reach
// the default arm and the second panics with "close of closed channel". Both
// closes happen in practice -- control.Serve closes the server, which closes
// its listener, while the test's own cleanup closes it too -- and the panic
// takes down whichever test is running rather than the one that caused it.
//
// It went red on CI inside TestJobLimit_IsInForceAtTheWorkersEnd, which makes
// no claim about any of this and was simply the longest-lived user of the rig.
// Reproduced locally in 5 of 16 stressed instances before this line changed;
// the test this package already had closed twice SEQUENTIALLY, which is the
// arrangement that cannot fail.
func (l *Pipe) Close() error {
	l.closeOnce.Do(func() { close(l.closed) })
	return nil
}

func (l *Pipe) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "pipe" }
