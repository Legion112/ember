package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// ErrClosed is the far end having hung up in good order.
//
// A SENTINEL BECAUSE BOTH ENDS ACT ON IT AND THE ACTIONS DIFFER. For the
// worker it is the operator's laptop going away, and the right answer is to
// stop: "a node ember cannot reach is a node ember cannot destroy, so there is
// no value in a workload outliving the connection" -- a job finished after the
// channel closed is a card paid for a picture nobody will fetch. For ember it
// is the worker having exited, which is the expected end of a rental and not a
// failure. Telling it from a transport error is what keeps a normal shutdown
// out of the error path at both ends.
var ErrClosed = errors.New("the control channel is closed")

// DialFunc opens a connection to the node's own loopback.
//
// The same shape sdserver.New takes, and in production it is the same value:
// sshdial.Client.DialContext, a `direct-tcpip` channel over the SSH connection
// ember already holds. Taking the func rather than an *ssh.Client is what lets
// both ends of this package be driven over net.Pipe with no SSH server, no
// Docker and no node.
type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

// Conn is one control channel, from either end.
//
// SENDS ARE SERIALISED AND RECEIVES ARE NOT, and the asymmetry is the shape of
// both callers rather than a hedge. A websocket permits one writer at a time
// and the library returns an error on a second -- which here would be a cancel
// racing an event and the rental failing with a transport message instead of
// stopping. ember's cancel genuinely does come from a different goroutine than
// the one reading events, because the operator presses the button while a card
// is sampling, so the mutex is load-bearing on exactly that path.
//
// Reads are left alone because there is one reader at each end by
// construction: the worker's reader goroutine, and ember's own event loop. A
// second reader would be two halves of one batch's events arriving in two
// places, which no mutex can fix.
type Conn struct {
	ws *websocket.Conn

	send sync.Mutex
}

// Send writes one frame.
func (c *Conn) Send(ctx context.Context, f Frame) error {
	body, err := json.Marshal(f)
	if err != nil {
		// Unreachable for every Frame this program builds: every field is a
		// scalar, a []byte or a json.RawMessage that came off the wire. Not
		// swallowed, because the one way to get here is a RawMessage holding
		// bytes that are not JSON, and silently sending a frame the far end
		// cannot decode is worse than saying so.
		return fmt.Errorf("encode a %s frame: %w", f.Kind, err)
	}
	// Bounded, so a far end that has stopped reading cannot hold this
	// goroutine for as long as the rental lasts. internal/webapi's progress
	// socket bounds its writes for the same reason and with the same figure.
	//
	// The bound is per write and is taken after the lock, so a sender queued
	// behind a stalled one waits that stall out and then gets its own ten
	// seconds. There is at most one other writer at each end, so the worst
	// case is twice the figure and not an unbounded pile-up.
	c.send.Lock()
	defer c.send.Unlock()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := c.ws.Write(wctx, websocket.MessageText, body); err != nil {
		return fmt.Errorf("send a %s frame: %w", f.Kind, closed(err))
	}
	return nil
}

// Recv reads one frame.
//
// A frame that is not text is refused rather than decoded. This build sends
// nothing but text, so a binary message is either a different protocol or a far
// end choosing the parse -- and a decoder that accepts whatever arrives is how
// the untrusted end of a channel gets to pick which code runs.
func (c *Conn) Recv(ctx context.Context) (Frame, error) {
	typ, body, err := c.ws.Read(ctx)
	if err != nil {
		return Frame{}, closed(err)
	}
	if typ != websocket.MessageText {
		return Frame{}, fmt.Errorf("the far end sent a %v message; this channel carries text frames only", typ)
	}
	var f Frame
	if err := json.Unmarshal(body, &f); err != nil {
		return Frame{}, fmt.Errorf("decode a control frame: %w", err)
	}
	if f.Kind == "" {
		// An empty kind is what `{}` decodes to, and it would otherwise fall
		// through every switch on the far end and be ignored -- a frame that
		// did nothing and said nothing, which is the shape this repository
		// keeps finding.
		return Frame{}, errors.New("a control frame arrived with no kind")
	}
	return f, nil
}

// Close ends the channel in good order.
func (c *Conn) Close() error {
	return c.ws.Close(websocket.StatusNormalClosure, "")
}

// Abort drops the channel without waiting, for a defer.
//
// Close writes a close frame and waits for the acknowledgement, which is wrong
// on a path that is already failing: the far end may be the thing that failed.
func (c *Conn) Abort() { _ = c.ws.CloseNow() }

// closed maps a tidy hang-up onto ErrClosed and leaves everything else alone.
//
// ONLY AN ORDERLY CLOSE, and a connection that simply died stays an error.
// The temptation is to fold both into ErrClosed, because the worker's reaction
// is the same either way -- stop -- but ember's is not: a rental whose channel
// dropped mid-batch has failed and the operator needs the reason, while one
// whose worker said goodbye has finished. Flattening the two would file every
// dropped SSH channel as a successful rental.
func closed(err error) error {
	switch websocket.CloseStatus(err) {
	case websocket.StatusNormalClosure, websocket.StatusGoingAway:
		return ErrClosed
	}
	return err
}

// Open is ember's end: it dials the worker and completes the handshake.
//
// THE HANDSHAKE IS IN HERE AND NOT IN THE CALLER, so a version mismatch is
// unrepresentable above this line. A caller that could hold an unchecked Conn
// is a caller that can push a batch to a node that will not understand it,
// and the moment that becomes expensive is the moment the card is allocated.
func Open(ctx context.Context, dial DialFunc) (*Conn, error) {
	hctx, cancel := context.WithTimeout(ctx, HandshakeTimeout)
	defer cancel()

	ws, resp, err := websocket.Dial(hctx, fmt.Sprintf("ws://127.0.0.1:%d%s", Port, Path),
		&websocket.DialOptions{
			HTTPClient: &http.Client{
				// No client timeout: the channel is open for the whole rental
				// and the deadline for each step belongs to the context that
				// knows which step it is. The handshake's own bound is hctx.
				Transport: &http.Transport{DialContext: dial},
			},
		})
	if err != nil {
		// The status is the useful half when there is one: a 409 is the
		// worker refusing a second channel, which means another ember thinks
		// it owns this node.
		if resp != nil {
			// Closed here and nowhere else: a failed handshake is the one
			// path on which this response has a body, and leaving it open
			// holds the SSH channel it arrived on for as long as the rental
			// lasts.
			_ = resp.Body.Close()
			return nil, fmt.Errorf("open the control channel to the worker (HTTP %s): %w", resp.Status, err)
		}
		return nil, fmt.Errorf("open the control channel to the worker on port %d of the node: %w; the node's own log says whether its worker started", Port, err)
	}

	c := &Conn{ws: ws}
	// What a NODE may make ember allocate. See EventLimit: a rented machine is
	// untrusted input, and this is the number that decides how much of the
	// operator's memory it can ask for.
	c.ws.SetReadLimit(EventLimit)

	f, err := c.Recv(hctx)
	if err != nil {
		c.Abort()
		return nil, fmt.Errorf("read the worker's hello: %w", err)
	}
	if err := checkHello(f); err != nil {
		c.Abort()
		return nil, err
	}
	return c, nil
}

// Serve is the worker's end: it answers one channel on ln and hands it to fn.
//
// ONE CHANNEL FOR THE WORKER'S WHOLE LIFE, and the second one is refused with
// a 409 rather than queued. A node is one rental, and two embers pushing into
// one in-memory queue is two rentals believing they own one machine -- which
// is precisely the invariant `max_nodes` bounds on the other side of the wire.
// Refused loudly, because the node's log is the only place that could ever say
// it happened.
//
// fn returning is what ends Serve, which is how "the worker exits when its
// queue empties" reaches the process: the session decides, this returns, and
// embernode's supervisor reports the child's exit.
func Serve(ctx context.Context, ln net.Listener, log *slog.Logger, fn func(context.Context, *Conn) error) error {
	var taken atomic.Bool
	done := make(chan error, 1)

	// THE SESSION'S CONTEXT IS THIS ONE AND NOT THE REQUEST'S, which is a
	// correction rather than a preference. A websocket handler has hijacked
	// its connection, and `net/http` neither cancels a hijacked request's
	// context nor closes the connection on `Server.Close` -- it says so: Close
	// "does not attempt to close (and does not even know about) any hijacked
	// connections". So a session given `r.Context()` is a session nothing can
	// interrupt: on SIGTERM this function returned while the worker carried on
	// polling a card, and what ended it was the process exiting underneath it.
	//
	// The deadline is the thing that stops the bill when the operator's laptop
	// dies, so "the card is released when the process happens to exit" is the
	// wrong mechanism to leave it resting on. Derived from ctx, the session's
	// own reads and writes fail with context.Canceled and the worker logs that
	// it was stopped mid-job.
	//
	// A dropped connection still ends it, from the other direction: the
	// websocket read returns an error at once, which is the case ErrClosed is
	// for.
	sessionCtx, endSession := context.WithCancel(ctx)
	defer endSession()
	// Closed when the session has actually returned. The cleanup in
	// controltest claimed to wait for this and was waiting for Serve instead,
	// which let a worker goroutine outlive the test that started it -- caught
	// as a test's own TempDir being removed while the worker was creating a
	// directory inside it.
	finished := make(chan struct{})
	var once sync.Once
	sessionOver := func() { once.Do(func() { close(finished) }) }
	defer sessionOver()

	mux := http.NewServeMux()
	mux.HandleFunc(Path, func(w http.ResponseWriter, r *http.Request) {
		if !taken.CompareAndSwap(false, true) {
			log.Error("refused a second control channel: this node already has one, and two would be two embers pushing into one queue",
				"remote", r.RemoteAddr)
			http.Error(w, "this node already has a control channel", http.StatusConflict)
			return
		}
		// OriginPatterns is left at its default, which refuses a cross-origin
		// handshake outright -- the same decision internal/webapi makes, and
		// for even less cost here: ember's client sends no Origin at all, so
		// the default can only ever turn away a browser, and a browser cannot
		// reach this interface in the first place.
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			// Accept has already answered the client.
			log.Error("the control channel handshake failed", "err", err)
			done <- fmt.Errorf("control channel handshake: %w", err)
			return
		}
		c := &Conn{ws: ws}
		// What EMBER may make the worker allocate. Larger than EventLimit
		// because a job can carry an init image; see JobLimit.
		c.ws.SetReadLimit(JobLimit)
		defer c.Abort()
		defer sessionOver()

		// The version goes first and before anything is read, so ember can
		// refuse an image that predates it without having pushed any work.
		if err := c.Send(sessionCtx, Frame{Kind: KindHello, Version: Version}); err != nil {
			done <- err
			return
		}
		log.Info("control channel open", "remote", r.RemoteAddr, "version", Version)
		done <- fn(sessionCtx, c)
	})

	srv := &http.Server{
		Handler: mux,
		// Bounded, because this listener is reachable from the node's own
		// loopback and a connection that sends no headers would otherwise
		// hold a goroutine for the rental's whole life.
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()

	select {
	case err := <-done:
		// Close and not Shutdown: Shutdown waits for handlers, and the handler
		// this is answering is the one that just sent on done.
		_ = srv.Close()
		return answer(ctx.Err(), err)
	case err := <-serveErr:
		return fmt.Errorf("the worker's control listener stopped: %w", err)
	case <-ctx.Done():
		_ = srv.Close()
		// AND THE SESSION IS WAITED FOR, bounded, which is the half that was
		// missing. srv.Close does not touch a hijacked connection, so without
		// this the worker goroutine outlives the function that started it --
		// on a node that is only a window before the process exits, and in a
		// test it is a goroutine still writing files after the test has
		// finished. endSession is what unblocks it; this is what proves it
		// went.
		//
		// Bounded because the alternative is a node that cannot be stopped:
		// this path is a deadline or a `docker stop`, and a session that will
		// not return must not be able to hold either. Reported rather than
		// waited out for ever -- the node's log is the only record of a worker
		// that had to be abandoned.
		endSession()
		select {
		case <-finished:
		case <-time.After(sessionStopTimeout):
			log.Error("the session did not stop when the node was told to; abandoning it",
				"after", sessionStopTimeout)
		}
		return ctx.Err()
	}
}

// answer decides what Serve reports when a session has ended.
//
// THE NODE BEING STOPPED WINS OVER WHATEVER THE SESSION SAID ABOUT IT, which
// is the inverse of orExited's rule one package over and is right for the
// inverse reason. Cancelling the session's context is HOW a stopped node ends
// a read, and the library reports that as either the context's error or
// `use of closed network connection` depending on where in a read it landed --
// so the session's answer is a consequence of the stop and not a fact about
// the worker. Without this, a node that reached its deadline logged its worker
// as having crashed, at ERROR, on every single deadline: "this node can no
// longer be given work" about a node that was told to stop.
//
// A FUNCTION SO THAT IT HAS AN EXIT CODE, which is the whole reason it is not
// two lines inline. Serve's select has both arms ready the moment a node is
// stopped, so which one runs is up to the scheduler -- and the end-to-end test
// above cannot reach the arm this decides, because its session deliberately
// takes 300ms to return and `ctx.Done()` therefore always wins. Reinstating
// the defect inline was GREEN over four runs, measured, which is how this came
// to be extracted rather than argued: the same move findListens made in
// internal/loopback for the same reason.
func answer(stopped, session error) error {
	if stopped != nil {
		return stopped
	}
	return session
}

// sessionStopTimeout bounds how long a stopped node waits for its session.
//
// Short: what it is waiting for is a poll loop noticing a cancelled context
// between two polls, and the poll cadence caps at two seconds. Anything longer
// than a few of those is a session that is not going to notice, and holding a
// container's shutdown on it would turn a deadline into a ten-second grace
// period and then a kill.
const sessionStopTimeout = 10 * time.Second
