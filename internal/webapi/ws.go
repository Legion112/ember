package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
)

// wsPoll is how often the socket looks for new events.
//
// IT READS THE DATABASE RATHER THAN SUBSCRIBING, and that is a choice worth
// defending. A fan-out hub inside this process would push with no latency and
// would be a second path for progress to travel -- so a reconnect would come
// back through the rows while a live client came through the hub, and the two
// could disagree about what had happened. Reading the rows means a page
// reload and a dropped connection are literally the same code, and a client
// can never be told something a reconnect would not tell it.
//
// The cost is bounded and small: an indexed `seq > ?` against a local SQLite
// file, four times a second, per open socket. The real latency floor is the
// queue's own flush, which is 250ms.
const wsPoll = 250 * time.Millisecond

// wsPing keeps a connection a browser has forgotten about from lingering.
const wsPing = 30 * time.Second

// wsEnvelope is what goes down the socket.
//
// Events AND the request, every time something changed, because a client
// needs both and polling two endpoints to assemble one view is how they get
// out of step: a progress line for a request the client still thinks is
// queued.
type wsEnvelope struct {
	Next    int64          `json:"next"`
	Events  []report.Event `json:"events"`
	Request *requestView   `json:"request,omitempty"`

	// Running is what the queue is generating right now, which may be a
	// different request than the one being watched -- that is how a UI shows
	// "yours is next" rather than nothing.
	Running string `json:"running,omitempty"`
}

// serveWS streams one request's progress.
//
// ?id=<request> and ?after=<seq>. The cursor is the client's, so a reconnect
// resumes exactly where it stopped -- see getEvents, which answers the same
// question for a client that would rather poll.
func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	// The handshake gets the same Host check every other route gets.
	// coder/websocket refuses a cross-origin handshake by default, which
	// covers Origin; this covers the rebinding case, where the page is
	// same-origin with a name that resolves here.
	//
	// hostOK AND NOT checkHost, which is the one line that keeps this route in
	// step with the rest: a request on the unix socket arrived on no port, so
	// the comparison has nothing to compare against -- see hostOK, which is
	// where that decision lives rather than being spelled twice.
	if err := hostOK(r, s.port); err != nil {
		s.fail(w, http.StatusForbidden, err)
		return
	}
	// AND THE CREDENTIAL, because a guard that covers the JSON routes and not
	// the socket guards nothing: this is how a watcher reads every event of
	// every generation, including the prompts.
	//
	// The cookie is what carries it, with nothing in the URL. That is the
	// reason the carrier is a cookie at all -- coder/websocket cannot set a
	// request header from a browser, so a header scheme would have had to put
	// the token in this query string, where it is the one secret in the
	// program living in a URL. A browser sends cookies on a same-origin
	// handshake, so the page needed no plumbing and the socket needs no
	// exception.
	if err := s.authenticate(r); err != nil {
		s.refuseCredential(w, r)
		return
	}

	id := genstore.ReqID(r.URL.Query().Get("id"))
	if id == "" {
		s.fail(w, http.StatusBadRequest, errors.New("the socket needs ?id=<request>"))
		return
	}
	after, err := cursor(r.URL.Query().Get("after"))
	if err != nil {
		s.fail(w, http.StatusBadRequest, err)
		return
	}

	// OriginPatterns is left at its default, which refuses a cross-origin
	// handshake outright. Naming the loopback forms explicitly would widen
	// nothing and is one more list to keep in step with isLoopbackName.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		// Accept has already answered the client.
		s.log().Debug("websocket handshake failed", "err", err)
		return
	}
	defer func() { _ = conn.CloseNow() }()

	// Nothing is read from the client -- the cursor arrives in the query and
	// the only control is to disconnect -- so a reader goroutine exists
	// solely to notice a close and to answer a ping. Without it, a browser
	// that walked away holds this loop until the next write fails.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go func() {
		defer cancel()
		for {
			if _, _, rerr := conn.Read(ctx); rerr != nil {
				return
			}
		}
	}()

	if err := s.stream(ctx, conn, id, after); err != nil && ctx.Err() == nil {
		s.log().Debug("websocket stream ended", "request", id, "err", err)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "")
}

// wsCursor is what one stream remembers between polls.
//
// THE REQUEST VIEW AND NOT ONLY THE SEQUENCE NUMBER, which is the half that
// was missing. `after` answers "is there a new event"; `req` answers "has the
// request changed", and those are different questions for the one state that
// matters -- a request refused before anything was rented has a reason in its
// row and no event anywhere, because `queue.fail` writes the row and there is
// no sink for a rental that was never opened.
//
// The MARSHALLED view and not a few chosen fields, so this cannot come to
// disagree with what the client is actually sent: every field that reaches a
// browser is in the comparison by construction, and one added later needs no
// second edit here. It is the bytes `send` is about to write either way.
type wsCursor struct {
	after int64
	req   []byte
}

// stream sends everything after the cursor, then everything that arrives.
func (s *Server) stream(ctx context.Context, conn *websocket.Conn, id genstore.ReqID, after int64) error {
	// The first send happens before any wait, so a client that reconnects
	// mid-generation sees the backlog at once rather than a quarter of a
	// second of nothing.
	cur, err := s.send(ctx, conn, id, wsCursor{after: after}, true)
	if err != nil {
		return err
	}

	ticker := time.NewTicker(wsPoll)
	defer ticker.Stop()
	ping := time.NewTicker(wsPing)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ping.C:
			if err := conn.Ping(ctx); err != nil {
				return err
			}
		case <-ticker.C:
			next, err := s.send(ctx, conn, id, cur, false)
			if err != nil {
				return err
			}
			cur = next
		}
	}
}

// send writes one envelope if there is anything to say, and reports the new
// cursor.
//
// force sends even with nothing new, which is what the first write does: a
// client needs the request's current state before it has seen any event, and
// a request that finished before the socket opened produces no further ones.
// It is also the only thing that sends an envelope for an id no request
// exists under, which a UI racing its own POST depends on.
func (s *Server) send(ctx context.Context, conn *websocket.Conn, id genstore.ReqID, cur wsCursor, force bool) (wsCursor, error) {
	es, err := s.opts.Store.Events(ctx, id, cur.after)
	if err != nil {
		return cur, err
	}

	// AN EMPTY LIST AND NEVER null.
	//
	// This is the bug AGENTS.md already records for WithExposed, reached from
	// the other direction: Go marshals a nil slice as `null`, and a client
	// whose field is a list gets a type error and drops the WHOLE envelope --
	// the request state with it. Found by watching a generation in a browser:
	// the first envelope of every connection carries no events, so every
	// watcher lost its opening state and the UI showed a job stuck at
	// "starting" while the image was already on disk.
	if es == nil {
		es = []report.Event{}
	}
	env := wsEnvelope{Next: cur.after, Events: es}
	if n := len(es); n > 0 {
		env.Next = es[n-1].Seq
	}
	if running, ok := s.running(); ok {
		env.Running = running
	}

	// The request travels with its events whenever either changed, so a
	// client cannot hold a progress line for a request it still believes is
	// queued.
	next := wsCursor{after: env.Next}
	if req, gerr := s.opts.Store.Get(ctx, id); gerr == nil {
		ims, _ := s.opts.Store.Images(ctx, id)
		v := viewRequest(req, ims)
		env.Request = &v
		if next.req, err = json.Marshal(v); err != nil {
			return cur, err
		}
	} else if !errors.Is(gerr, genstore.ErrNotFound) {
		return cur, gerr
	}

	// NEITHER HALF CHANGED, SO NOTHING IS WRITTEN. A socket that re-sent the
	// same state four times a second would make a UI re-render on a timer and
	// would hide the difference between "no progress" and "ember stopped" --
	// which is TestWS_SaysNothingWhileNothingHappens, and it is why the fix
	// for the missing push is a comparison rather than an unconditional send.
	//
	// The cursor is returned whether or not anything went out, because the
	// request view has to be remembered even on the poll that found it
	// unchanged; returning `cur` here would compare every poll against the
	// state of two polls ago.
	if len(es) == 0 && bytes.Equal(next.req, cur.req) && !force {
		return next, nil
	}

	body, err := json.Marshal(env)
	if err != nil {
		return cur, err
	}
	// Bounded, so a client that has stopped reading cannot hold this
	// goroutine for as long as the generation lasts.
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := conn.Write(wctx, websocket.MessageText, body); err != nil {
		return next, err
	}
	return next, nil
}
