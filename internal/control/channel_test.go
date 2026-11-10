package control_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/control/controltest"
)

// TestChannel_CarriesAFrameBothWays, which is the whole package working: a
// real handshake, a real websocket, a real codec, over net.Pipe instead of
// over an SSH direct-tcpip channel.
func TestChannel_CarriesAFrameBothWays(t *testing.T) {
	got := make(chan control.Frame, 1)
	ember := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		f, err := c.Recv(ctx)
		if err != nil {
			return err
		}
		got <- f
		return c.Send(ctx, control.Frame{Kind: control.KindEnded, Index: f.Index, Total: f.Total, Status: "completed"})
	})

	if err := ember.Send(t.Context(), control.Frame{Kind: control.KindJob, Index: 1, Total: 1, Stem: "boat"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	down := <-got
	if down.Kind != control.KindJob || down.Stem != "boat" {
		t.Errorf("the worker got %+v", down)
	}
	up, err := ember.Recv(t.Context())
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if up.Kind != control.KindEnded || up.Index != 1 || up.Status != "completed" {
		t.Errorf("ember got %+v", up)
	}
}

// TestOpen_RefusesANodeThatIsNotThisBuildsWorker.
//
// THE HANDSHAKE THROUGH THE REAL PATH, which checkHello's own test cannot be:
// what has to be true is that a caller cannot come out of Open holding a
// channel to a node that will not understand it. The refusal lands before any
// work has been pushed, which is the only moment it is cheap -- after it, the
// card is allocated and the bill is running.
//
// The node image and the ember driving it ship separately, so every row here
// is a thing that happens: an image from an older tree, one from a newer tree,
// and something else listening on the port the worker was supposed to have.
func TestOpen_RefusesANodeThatIsNotThisBuildsWorker(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hello control.Frame
		want  string
	}{
		{"an older image", control.Frame{Kind: control.KindHello, Version: control.Version - 1}, "rebuild and republish"},
		{"a newer image", control.Frame{Kind: control.KindHello, Version: control.Version + 1}, "rebuild and republish"},
		{"a hello with no version", control.Frame{Kind: control.KindHello}, "rebuild and republish"},
		{"something else on the port", control.Frame{Kind: control.KindProgress, Index: 1}, "not an ember control channel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dial := controltest.RawNode(t, func(ctx context.Context, ws *websocket.Conn) error {
				body, err := json.Marshal(tc.hello)
				if err != nil {
					return err
				}
				return ws.Write(ctx, websocket.MessageText, body)
			})
			_, err := control.Open(t.Context(), dial)
			if err == nil {
				t.Fatalf("%+v was accepted as a hello", tc.hello)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the refusal does not say what to do about it: %v", err)
			}
		})
	}

	// The negative control. A rig that refused everything would satisfy every
	// row above, which is this repository's own TestNoListenOutsideLoopback
	// story -- so the hello this build really sends has to come back accepted
	// through the same rig.
	t.Run("not this build's own worker", func(t *testing.T) {
		dial := controltest.RawNode(t, func(ctx context.Context, ws *websocket.Conn) error {
			body, err := json.Marshal(control.Frame{Kind: control.KindHello, Version: control.Version})
			if err != nil {
				return err
			}
			if err := ws.Write(ctx, websocket.MessageText, body); err != nil {
				return err
			}
			<-ctx.Done()
			return nil
		})
		c, err := control.Open(t.Context(), dial)
		if err != nil {
			t.Fatalf("this build's own hello was refused: %v", err)
		}
		c.Abort()
	})
}

// TestServe_RefusesASecondChannel.
//
// A node is one rental. Two embers pushing into one in-memory queue is two
// rentals believing they own one machine, which is precisely the invariant
// `max_nodes` bounds on the other side of the wire -- so the second channel is
// refused with a status rather than queued, and the refusal carries it so
// ember can tell this apart from a worker that is not there.
func TestServe_RefusesASecondChannel(t *testing.T) {
	held := make(chan struct{})
	dial := controltest.Node(t, func(ctx context.Context, c *control.Conn) error {
		<-held
		return nil
	})
	defer close(held)

	first, err := control.Open(t.Context(), dial)
	if err != nil {
		t.Fatalf("the first channel: %v", err)
	}
	defer first.Abort()

	_, err = control.Open(t.Context(), dial)
	if err == nil {
		t.Fatal("a second channel was accepted; one node is one rental")
	}
	if !strings.Contains(err.Error(), "409") {
		t.Errorf("the refusal does not carry the status that says another ember has this node: %v", err)
	}
}

// TestRecv_RefusesAFrameItDidNotExpect.
//
// Each of these is the untrusted end choosing the parse. A binary message is
// a different protocol, or a far end picking which code runs; `{}` decodes
// into a frame with no kind, which would otherwise fall through every switch
// at the other end and be ignored -- a frame that did nothing and said
// nothing, which is the shape this repository keeps finding.
func TestRecv_RefusesAFrameItDidNotExpect(t *testing.T) {
	for _, tc := range []struct {
		name string
		typ  websocket.MessageType
		body string
		want string
	}{
		{"a binary message", websocket.MessageBinary, `{"kind":"progress"}`, "text frames only"},
		{"a frame with no kind", websocket.MessageText, `{}`, "no kind"},
		{"a frame that is not JSON", websocket.MessageText, `not json at all`, "decode a control frame"},
		{"a frame that is a JSON array", websocket.MessageText, `[1,2,3]`, "decode a control frame"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dial := controltest.RawNode(t, func(ctx context.Context, ws *websocket.Conn) error {
				hello, err := json.Marshal(control.Frame{Kind: control.KindHello, Version: control.Version})
				if err != nil {
					return err
				}
				if err := ws.Write(ctx, websocket.MessageText, hello); err != nil {
					return err
				}
				return ws.Write(ctx, tc.typ, []byte(tc.body))
			})
			c, err := control.Open(t.Context(), dial)
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer c.Abort()

			_, err = c.Recv(t.Context())
			if err == nil {
				t.Fatalf("%s was accepted", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestServe_WaitsForTheSessionWhenTheNodeIsStopped.
//
// A WEBSOCKET HANDLER HAS HIJACKED ITS CONNECTION, and `net/http` neither
// cancels a hijacked request's context nor closes the connection on
// `Server.Close` -- its own documentation says Close "does not attempt to
// close (and does not even know about) any hijacked connections". So a
// session given `r.Context()` was a session nothing could interrupt: Serve
// returned on a SIGTERM while the worker carried on polling a card, and what
// ended it was the process exiting underneath it.
//
// That is the wrong mechanism for the thing that stops the bill when an
// operator's laptop dies. Both halves are asserted here: the session's context
// is cancelled, and Serve does not return until the session has.
//
// FOUND AS A TEST'S OWN TempDir BEING REMOVED while a worker was still
// creating a directory inside it -- 1 of 24 stressed instances -- which is
// this rig's cleanup claiming to wait for the session and waiting for Serve
// instead.
func TestServe_WaitsForTheSessionWhenTheNodeIsStopped(t *testing.T) {
	ctx, stopNode := context.WithCancel(context.Background())

	ln := controltest.NewPipe()
	noticed := make(chan error, 1)
	left := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- control.Serve(ctx, ln, controltest.Log(t), func(sctx context.Context, c *control.Conn) error {
			// What a worker does: block on a read until something ends it.
			_, err := c.Recv(sctx)
			noticed <- err
			// Long enough that a Serve which did not wait would certainly
			// have returned first.
			time.Sleep(300 * time.Millisecond)
			close(left)
			return err
		})
	}()

	c, err := control.Open(t.Context(), ln.DialContext)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer c.Abort()

	stopNode()

	// THAT THE READ ENDED, AND NOT WHICH WAY. Cancelling the context is how a
	// stopped node ends a read, and the library reports that as either the
	// context's error or `use of closed network connection` depending on where
	// in a read it landed -- so asserting the identity is asserting the
	// library's internals, and it was flaky at 2 of 12 stressed instances
	// before this comment existed. What has to be true is that the read ends
	// at all: with the request's context it never returned, which is the arm
	// below.
	select {
	case err := <-noticed:
		if err == nil {
			t.Error("the session's read returned no error on a node that was stopped")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WEDGED: the session never noticed the node being stopped, so a deadline would " +
			"stop sshd and leave a generation running")
	}

	select {
	case err := <-served:
		select {
		case <-left:
		default:
			t.Error("Serve returned while the session was still running, so a worker goroutine " +
				"outlives the thing that started it")
		}
		// AND THE ANSWER IS THAT THE NODE WAS STOPPED, which is the fact the
		// node's own log reads. The session's error is a consequence of the
		// stop; reporting it instead made a node that reached its deadline
		// log its worker as having crashed, at ERROR, on every deadline --
		// "this node can no longer be given work" about a node that was told
		// to stop.
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Serve answered %v, want context.Canceled: what the node's log says about a "+
				"deadline must be that the node was stopped and not that its worker failed", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("WEDGED: Serve did not return after the session did")
	}
}

// TestRecv_ATidyCloseIsErrClosed, over the real socket.
//
// ember's event loop and the worker's reader both act on this, and they act
// differently -- so the distinction has to survive the actual close handshake
// and not only the error-mapping function.
func TestRecv_ATidyCloseIsErrClosed(t *testing.T) {
	ember := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		return c.Close()
	})
	if _, err := ember.Recv(t.Context()); !errors.Is(err, control.ErrClosed) {
		t.Errorf("a worker that said goodbye reads as %v, want ErrClosed", err)
	}
}

// TestEventLimit_StopsANodeFromMakingEmberAllocate.
//
// A rented machine is untrusted input -- the rule internal/transfer enforces
// twice over a tar stream -- and this is the number that decides how much of
// the operator's memory it can ask for. The library's own default is 32 KiB,
// which this raises, so what has to be proved is that the limit in force is
// the one named here and that it is still in force at all.
func TestEventLimit_StopsANodeFromMakingEmberAllocate(t *testing.T) {
	huge := make([]byte, control.EventLimit+1<<20)
	ember := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		// Its own error here is "message too big" as well, and is not what
		// this test is about.
		_ = c.Send(ctx, control.Frame{Kind: control.KindProgress, Index: 1, Total: 1, PNG: huge})
		return nil
	})
	if _, err := ember.Recv(t.Context()); err == nil {
		t.Fatalf("ember read a frame of %d bytes; the limit is %d", len(huge), control.EventLimit)
	}
}

// TestEventLimit_IsNotSoTightThatARealPreviewTripsIt.
//
// The other half, without which the assertion above is satisfied by a limit of
// one byte. A megabyte of PNG is a large preview and it has to arrive intact,
// because the whole point of a preview is that it is on screen while the card
// is still working.
func TestEventLimit_IsNotSoTightThatARealPreviewTripsIt(t *testing.T) {
	png := make([]byte, 1<<20)
	for i := range png {
		png[i] = byte(i)
	}
	ember := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		return c.Send(ctx, control.Frame{Kind: control.KindProgress, Index: 1, Total: 1, PNG: png})
	})
	f, err := ember.Recv(t.Context())
	if err != nil {
		t.Fatalf("a one-megabyte preview was refused: %v", err)
	}
	if len(f.PNG) != len(png) || f.PNG[7] != png[7] {
		t.Errorf("preview = %d bytes, want %d", len(f.PNG), len(png))
	}
}

// TestJobLimit_IsInForceAtTheWorkersEnd.
//
// The two halves of a read limit are different claims and this is the one the
// constants cannot make: that the worker's end really raises the library's
// 32 KiB default, which would otherwise cut every job carrying an init image
// at the moment the node is already rented and the weights are resident.
//
// Eight mebibytes is blob.MaxInputBytes, so this is the largest job an
// operator can actually cause.
func TestJobLimit_IsInForceAtTheWorkersEnd(t *testing.T) {
	big := make([]byte, blob.MaxInputBytes)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	body, err := json.Marshal(map[string]any{"prompt": "a paper boat", "init_image": string(big)})
	if err != nil {
		t.Fatal(err)
	}

	got := make(chan int, 1)
	ember := controltest.Pair(t, func(ctx context.Context, c *control.Conn) error {
		f, err := c.Recv(ctx)
		if err != nil {
			return err
		}
		got <- len(f.Job)
		return nil
	})
	if err := ember.Send(t.Context(), control.Frame{
		Kind: control.KindJob, Index: 1, Total: 1, Stem: "boat", Job: body,
	}); err != nil {
		t.Fatalf("send a job with the largest staged input: %v", err)
	}
	select {
	case n := <-got:
		if n != len(body) {
			t.Errorf("the worker read %d bytes of a %d-byte job", n, len(body))
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the worker never read a job carrying the largest staged input, so the " +
			"read limit at its end is the library's default rather than JobLimit")
	}
}

// TestJobLimit_HoldsTheLargestInitImageTheBlobStoreAccepts.
//
// The two numbers are in different packages and nothing but this keeps them in
// step. blob.MaxInputBytes is what an operator may stage; a job naming one
// travels with it base64 inside the generation server's own request body, four
// bytes on the wire for every three of image. A JobLimit below that is a
// rental that fails at its first job with an image, after the node is up and
// the weights are resident.
func TestJobLimit_HoldsTheLargestInitImageTheBlobStoreAccepts(t *testing.T) {
	need := int64(blob.MaxInputBytes) * 4 / 3
	if control.JobLimit <= need {
		t.Errorf("JobLimit = %d but the largest staged input is %d bytes, which is %d base64-encoded",
			control.JobLimit, blob.MaxInputBytes, need)
	}
	// And the asymmetry is deliberate: what a node may send ember is much
	// smaller than what ember may send a node, because nothing large comes
	// up. Raising both at once would be the seal above quietly loosened.
	if control.EventLimit >= control.JobLimit {
		t.Errorf("EventLimit = %d is not smaller than JobLimit = %d; the asymmetry is the seal on a node's reply",
			control.EventLimit, control.JobLimit)
	}
}

// TestOpen_SaysWhatToLookAtWhenNothingIsListening.
//
// The worker is started by embernode at boot, so "nothing answers on this
// port" means the worker did not start -- a port collision in the workload
// image, or a read-only /tmp. ember can see neither, the node's own log has
// both, so the message has to point there.
func TestOpen_SaysWhatToLookAtWhenNothingIsListening(t *testing.T) {
	dead := func(ctx context.Context, network, addr string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}
	_, err := control.Open(t.Context(), dead)
	if err == nil {
		t.Fatal("a dial that cannot connect produced a channel")
	}
	if !strings.Contains(err.Error(), "the node's own log") {
		t.Errorf("the message does not point at the one place that knows why: %v", err)
	}
}

// TestOpen_IsBounded, because a rental that hangs on a connection nobody will
// finish is a card allocated and billing with no output at all.
func TestOpen_IsBounded(t *testing.T) {
	if control.HandshakeTimeout <= 0 || control.HandshakeTimeout > time.Minute {
		t.Errorf("HandshakeTimeout = %v, which is either unbounded or longer than a node takes to boot",
			control.HandshakeTimeout)
	}
	// A node that accepts the connection and never says hello, which is the
	// case a dial timeout alone does not cover: the TCP connect succeeded, so
	// nothing below the handshake can notice.
	stuck := make(chan struct{})
	defer close(stuck)
	dial := controltest.RawNode(t, func(ctx context.Context, ws *websocket.Conn) error {
		<-stuck
		return nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := control.Open(ctx, dial); err == nil {
		t.Fatal("a worker that never finished the handshake produced a channel")
	}
}
