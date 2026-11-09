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

		// The version goes first and before anything is read, so ember can
		// refuse an image that predates it without having pushed any work.
		if err := c.Send(r.Context(), Frame{Kind: KindHello, Version: Version}); err != nil {
			done <- err
			return
		}
		log.Info("control channel open", "remote", r.RemoteAddr, "version", Version)
		done <- fn(r.Context(), c)
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
		return err
	case err := <-serveErr:
		return fmt.Errorf("the worker's control listener stopped: %w", err)
	case <-ctx.Done():
		_ = srv.Close()
		return ctx.Err()
	}
}
