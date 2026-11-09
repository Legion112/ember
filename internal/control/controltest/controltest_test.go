package controltest

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/coder/websocket"

	"github.com/Legion112/ember/internal/control"
)

// TestNode_AnsweresTheDialAndTheHandshake.
//
// A RIG IS A THING THAT CAN BE BROKEN, which is why it has a test of its own:
// every assertion in internal/worker and internal/app rests on this answering
// the way a node does, and a rig that silently stopped completing the
// handshake would turn those suites green against nothing. providertest has
// its own tests for the same reason.
func TestNode_AnswersTheDialAndTheHandshake(t *testing.T) {
	ember := Pair(t, func(ctx context.Context, c *control.Conn) error {
		f, err := c.Recv(ctx)
		if err != nil {
			return err
		}
		return c.Send(ctx, control.Frame{Kind: control.KindDone, Stem: f.Stem})
	})
	if err := ember.Send(t.Context(), control.Frame{Kind: control.KindJob, Index: 1, Total: 1, Stem: "x"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	f, err := ember.Recv(t.Context())
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if f.Kind != control.KindDone || f.Stem != "x" {
		t.Errorf("got %+v", f)
	}
}

// TestNode_SurvivesADialThatIsClosedWithoutARequest.
//
// This is the case the rig exists in this shape for: tunnel.WaitReady dials
// the worker's port and closes the connection without sending an HTTP request,
// which is how ember tells "the worker has not bound yet" from "the worker is
// not there". A listener that could only serve one connection would be
// consumed by that probe, and every test above it would be testing a channel
// ember never actually opened.
func TestNode_SurvivesADialThatIsClosedWithoutARequest(t *testing.T) {
	dial := Node(t, func(ctx context.Context, c *control.Conn) error {
		return c.Send(ctx, control.Frame{Kind: control.KindDone})
	})

	probe, err := dial(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("the probe dial: %v", err)
	}
	_ = probe.Close()

	c, err := control.Open(t.Context(), dial)
	if err != nil {
		t.Fatalf("open after a probe: %v", err)
	}
	defer c.Abort()
	if f, err := c.Recv(t.Context()); err != nil || f.Kind != control.KindDone {
		t.Errorf("recv = %+v, %v", f, err)
	}
}

// TestRawNode_CanSaySomethingControlServeNeverWould, which is the whole
// reason it exists: the refusals in internal/control cannot be reached through
// a correct worker.
func TestRawNode_CanSaySomethingControlServeNeverWould(t *testing.T) {
	dial := RawNode(t, func(ctx context.Context, ws *websocket.Conn) error {
		return ws.Write(ctx, websocket.MessageText, []byte(`{"kind":"hello","version":999}`))
	})
	if _, err := control.Open(t.Context(), dial); err == nil {
		t.Fatal("a version this build does not speak was accepted")
	}
}

// TestDial_AfterTheListenerIsClosedIsRefusedRatherThanBlocking.
//
// A dial that blocked for ever on a dead rig is a test that hangs with no
// `--- FAIL` to read, which this repository has a rule about: every waiting
// test carries its own deadline, and a rig is the one place a hang is easiest
// to introduce.
func TestDial_AfterTheListenerIsClosedIsRefusedRatherThanBlocking(t *testing.T) {
	l := &pipeListener{conns: make(chan net.Conn), closed: make(chan struct{})}
	_ = l.Close()

	if _, err := l.DialContext(t.Context(), "tcp", "x"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("dial after close = %v, want net.ErrClosed", err)
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("accept after close = %v, want net.ErrClosed", err)
	}
	// Closing twice is what happens in practice: control.Serve closes the
	// server, which closes the listener, and the test's own cleanup closes it
	// again. A second close that panicked would be a panic in every test
	// using this rig.
	if err := l.Close(); err != nil {
		t.Errorf("the second close: %v", err)
	}
	if got := l.Addr().String(); got != "pipe" {
		t.Errorf("Addr = %q", got)
	}
	if got := l.Addr().Network(); got != "pipe" {
		t.Errorf("Network = %q", got)
	}
}

// TestLog_Discards, because a worker goroutine outlives the test that started
// it -- it is blocked reading a channel the test is about to drop -- and a
// *testing.T written to after its test returns is a panic in whatever runs
// next.
func TestLog_Discards(t *testing.T) {
	if Log(t) == nil {
		t.Fatal("no logger")
	}
	Log(t).Info("this must reach nothing that outlives the test")
}
