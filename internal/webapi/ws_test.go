package webapi

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Legion112/ember/internal/genstore"
)

// wsServer runs the handler on a real listener.
//
// A real one and not httptest.NewRecorder, because a websocket handshake
// hijacks the connection and a recorder has none to hijack. Through `live`
// rather than its own httptest.NewServer, because the port dance -- httptest
// picks the port and the guard compares the Host header against this
// server's own -- was two copies of one thing the moment headers_test.go
// needed a socket too.
func wsServer(t *testing.T, h *harness) (*httptest.Server, string) {
	t.Helper()
	srv := live(t, h)
	return srv, "ws" + strings.TrimPrefix(srv.URL, "http")
}

func dialWS(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, 5*time.Second)
	defer cancel()
	// The credential, as a non-browser client carries it. coder/websocket CAN
	// set a request header from Go; it is a browser that cannot, which is why
	// the page's carrier is a cookie instead.
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + string(testToken.Bytes())}},
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		t.Fatalf("dial %s: %v", url, err)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c
}

func readEnvelope(t *testing.T, c *websocket.Conn) wsEnvelope {
	t.Helper()
	ctx, cancel := contextWithTimeout(t, 5*time.Second)
	defer cancel()
	_, body, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var env wsEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("decode %q: %v", body, err)
	}
	return env
}

// TestWS_SendsTheBacklogBeforeWaiting.
//
// A client that reconnects mid-generation -- or opens the page on a request
// that finished a minute ago -- has to see what happened, not a quarter of a
// second of nothing followed by whatever comes next. That is the whole reason
// events are rows: a page reload and a dropped connection are the same code
// path.
func TestWS_SendsTheBacklogBeforeWaiting(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
	if err := h.store.AppendEvents(t.Context(), genstore.ReqID(id), progress(4)); err != nil {
		t.Fatal(err)
	}

	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id="+id)

	env := readEnvelope(t, c)
	if len(env.Events) != 4 {
		t.Fatalf("got %d event(s) on connect, want the whole backlog of 4", len(env.Events))
	}
	if env.Next != 4 {
		t.Errorf("next = %d, want 4", env.Next)
	}
	// The request travels with its events, so a client cannot hold a
	// progress line for a request it still believes is queued.
	if env.Request == nil {
		t.Fatal("the envelope carries no request")
	}
	if env.Request.ID != id {
		t.Errorf("request = %s, want %s", env.Request.ID, id)
	}
}

// TestWS_ResumesFromTheClientsCursor, so a reconnect is not a replay of an
// hour of progress.
func TestWS_ResumesFromTheClientsCursor(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]
	if err := h.store.AppendEvents(t.Context(), genstore.ReqID(id), progress(6)); err != nil {
		t.Fatal(err)
	}

	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id="+id+"&after=4")

	env := readEnvelope(t, c)
	if len(env.Events) != 2 {
		t.Fatalf("got %d event(s) after 4, want 2", len(env.Events))
	}
	if env.Events[0].Seq != 5 {
		t.Errorf("first event has Seq %d, want 5", env.Events[0].Seq)
	}
}

// TestWS_RecordsTheCursorItWasAskedToResumeFrom.
//
// THE LAB'S INSTRUMENT, SEALED HERE. Whether a browser resumes from its cursor
// or from zero is not visible on the page at all -- `Progress::apply` is
// idempotent per sequence number, so a client that replayed a whole hour of
// progress renders exactly what one that resumed correctly renders. The only
// place that knows is this handler, and TestServe_UIResumesAProgressStreamItLost
// reads this line out of `ember serve --log-level debug` to prove the reconnect
// carried a cursor. A lab resting on a log line wants that line asserted
// somewhere an exit code can see it, or the lab's evidence is a grep for a
// string nothing promises to print.
func TestWS_RecordsTheCursorItWasAskedToResumeFrom(t *testing.T) {
	var logged bytes.Buffer
	h := newHarness(t, func(o *Options) {
		o.Log = slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id="+id+"&after=4")
	// Read one envelope so the handler has certainly run past the log line
	// before the buffer is examined.
	readEnvelope(t, c)

	if got := logged.String(); !strings.Contains(got, "after=4") {
		t.Errorf("the cursor the client resumed from was not recorded:\n%s", got)
	}
	// The negative control: the number has to be the one the client sent, or
	// the line would read `after=4` for every connection and the lab's
	// assertion would hold against a client that resumed from nothing.
	if got := logged.String(); strings.Contains(got, "after=0") {
		t.Errorf("the line reports a cursor nobody asked for:\n%s", got)
	}
}

// TestWS_StreamsWhatArrivesAfterTheConnection.
func TestWS_StreamsWhatArrivesAfterTheConnection(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id="+id)

	// The first envelope is the state with no events, which is what a client
	// opening on a request that has not started yet gets.
	first := readEnvelope(t, c)
	if len(first.Events) != 0 {
		t.Fatalf("got %d event(s) before anything happened", len(first.Events))
	}
	if first.Request == nil || first.Request.Status != string(genstore.StatusQueued) {
		t.Fatalf("the first envelope does not describe a queued request: %+v", first.Request)
	}

	if err := h.store.AppendEvents(t.Context(), genstore.ReqID(id), progress(2)); err != nil {
		t.Fatal(err)
	}

	env := readEnvelope(t, c)
	if len(env.Events) != 2 {
		t.Errorf("got %d event(s), want the 2 that arrived", len(env.Events))
	}
}

// TestWS_SaysNothingWhileNothingHappens.
//
// A socket that re-sent the same state four times a second would make a UI
// re-render on a timer and would hide the difference between "no progress"
// and "ember stopped".
func TestWS_SaysNothingWhileNothingHappens(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id="+id)
	readEnvelope(t, c) // the initial state

	ctx, cancel := contextWithTimeout(t, 3*wsPoll)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Error("the socket sent something although nothing had changed")
	}
}

// TestWS_PushesARequestThatChangedWithNoEventBehindIt.
//
// THE ENVELOPE'S OWN COMMENT CLAIMED THIS AND THE CODE DID NOT DO IT: "events
// AND the request, every time something changed". `send` returned early on
// `len(es) == 0 && !force`, so what it really sent was "the request, every
// time an EVENT changed" -- and the one state with no event behind it is the
// one an operator most needs. A request refused before a provider is reached
// (a money ceiling, a sampler the cached caps do not list) is failed by
// `queue.fail`, which writes the row and emits nothing at all: there is no
// sink for a rental that was never opened. So a watcher sat on the state the
// socket sent when it connected, with the reason already in the database and
// on the wire type, for as long as the tab was open.
//
// The queue table never had the gap, because it polls `/api/requests` -- which
// is why this is a socket test and not a UI one, and why #103's "without
// touching Go" was half right.
//
// NOT A RE-SEND ON A TIMER. TestWS_SaysNothingWhileNothingHappens is the
// other half and still holds: what is compared is the request view itself, so
// a poll that finds nothing new still writes nothing.
func TestWS_PushesARequestThatChangedWithNoEventBehindIt(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id="+id)

	first := readEnvelope(t, c)
	if first.Request == nil || first.Request.Status != string(genstore.StatusQueued) {
		t.Fatalf("the first envelope does not describe a queued request: %+v", first.Request)
	}
	if first.Request.Error != "" {
		t.Fatalf("a queued request already carries an error: %q", first.Request.Error)
	}

	const why = "max_month_usd would be exceeded: $47.80 spent of $50 and this rental projects $3.58"
	if err := h.store.Update(t.Context(), genstore.ReqID(id), func(r *genstore.Request) {
		r.Status = genstore.StatusFailed
		r.Err = why
		r.Ended = time.Now().UTC()
	}); err != nil {
		t.Fatal(err)
	}

	env := readEnvelope(t, c)
	if len(env.Events) != 0 {
		t.Errorf("got %d event(s); nothing emitted any", len(env.Events))
	}
	if env.Request == nil {
		t.Fatal("the envelope that reported the change carries no request")
	}
	if got := env.Request.Status; got != string(genstore.StatusFailed) {
		t.Errorf("status = %q, want failed", got)
	}
	if got := env.Request.Error; got != why {
		t.Errorf("error = %q, want the reason the row carries: %q", got, why)
	}
}

func TestWS_RefusesWhatItCannotStream(t *testing.T) {
	h := newHarness(t)
	_, wsURL := wsServer(t, h)

	for _, c := range []struct{ name, query string }{
		{"no request", "/ws"},
		{"a cursor that is not a sequence number", "/ws?id=abc&after=soon"},
		{"a negative cursor", "/ws?id=abc&after=-1"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := contextWithTimeout(t, 5*time.Second)
			defer cancel()
			conn, resp, err := websocket.Dial(ctx, wsURL+c.query, nil)
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			if err == nil {
				_ = conn.CloseNow()
				t.Error("the handshake succeeded")
			}
		})
	}
}

// TestWS_RefusesAHostThatIsNotThisServer -- the DNS rebinding case, which
// coder/websocket's own Origin check cannot see: the attacker's page is
// same-origin with their own name.
func TestWS_RefusesAHostThatIsNotThisServer(t *testing.T) {
	h := newHarness(t)
	_, wsURL := wsServer(t, h)

	ctx, cancel := contextWithTimeout(t, 5*time.Second)
	defer cancel()
	conn, resp, err := websocket.Dial(ctx, wsURL+"/ws?id=abc", &websocket.DialOptions{
		Host: "evil.example:7777",
	})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err == nil {
		_ = conn.CloseNow()
		t.Error("the handshake succeeded with a foreign Host")
	}
}

// TestWS_AMissingRequestIsNotAnError.
//
// A UI can open a socket on an id it has only just been given, and losing the
// race with its own POST must not close the connection -- the next poll finds
// it.
func TestWS_AMissingRequestIsNotAnError(t *testing.T) {
	h := newHarness(t)
	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id=not-yet")

	env := readEnvelope(t, c)
	if env.Request != nil {
		t.Errorf("a request that does not exist was described: %+v", env.Request)
	}
	if len(env.Events) != 0 {
		t.Errorf("got %d event(s) for a request that does not exist", len(env.Events))
	}
}

// TestWS_ReportsWhichRequestIsGenerating, which is how a UI shows "yours is
// next" rather than nothing.
func TestWS_ReportsWhichRequestIsGenerating(t *testing.T) {
	h := newHarness(t)
	mine := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id="+mine)
	env := readEnvelope(t, c)
	// No queue is wired in this harness, so nothing is generating -- and the
	// honest answer is an EMPTY LIST rather than a missing field. Since
	// max_nodes the field is a list, and Go marshals a nil slice as `null`,
	// which a client whose field is a list drops the whole envelope over: the
	// defect that cost this program its queue table once. So the assertion is
	// that it is present and empty, which is a stronger claim than "not set"
	// and is what Server.running guarantees.
	if env.Running == nil {
		t.Error("running was null rather than [] with no queue wired, which is the shape a " +
			"client whose field is a list cannot decode")
	}
	if len(env.Running) != 0 {
		t.Errorf("running = %q with no queue wired", env.Running)
	}
	if env.Request == nil {
		t.Fatal("no request on the envelope")
	}
}

// TestWS_AWriteThatCannotProceedIsBoundedRatherThanHoldingTheWatcher.
//
// `send`'s write is wrapped in a ten-second timeout so that a client which has
// stopped reading cannot hold this goroutine for as long as the generation
// lasts. It is the lesser half of the family `runServed`'s deferred reaper
// belongs to -- one watcher goroutine against a peer, rather than a process
// that will not exit -- and it was equally untested: replacing the
// `context.WithTimeout` with `context.WithCancel` left
// `go test -race ./internal/webapi/` green.
//
// THE PARENT MUST HAVE NO DEADLINE OF ITS OWN, which is the whole reason this
// takes ten seconds. `WithCancel(ctx)` inherits the parent's deadline, so a
// test that passed a bounded context would pass against the defect as well --
// and the real parent is the websocket handler's request context, which has
// none. In production the write is the only thing standing between a dead peer
// and a goroutine per watched request.
//
// A REAL CONN AND NO NEW SEAM. coder/websocket allows one message writer at a
// time and acquires it through a context-aware channel, so an unclosed
// `Writer` makes the next `Write` block in exactly the way a filled socket
// buffer does. Narrowing `send`'s `*websocket.Conn` to an interface so a fake
// could block would be a production type shaped by a test's convenience, which
// is the line `sshx.Runner` is not widened across either. Nothing goes on the
// wire for this, so the client does not even have to be reading.
//
// Its own deadline and its own message, because A WEDGE PRINTS NO `--- FAIL`:
// the SKIP grep and `go test`'s status both see a hang as nothing at all until
// the package timeout dumps goroutines.
func TestWS_AWriteThatCannotProceedIsBoundedRatherThanHoldingTheWatcher(t *testing.T) {
	h := newHarness(t)
	id := decode[map[string]string](t, h.do(t, "POST", "/api/requests", oneJob))["id"]

	_, wsURL := wsServer(t, h)
	c := dialWS(t, wsURL+"/ws?id="+id)

	// Held open and never closed, so the conn's single message writer is
	// taken for the rest of the test.
	held, cancelHeld := context.WithCancel(t.Context())
	defer cancelHeld()
	if _, err := c.Writer(held, websocket.MessageText); err != nil {
		t.Fatalf("could not take the conn's message writer: %v", err)
	}

	done := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		// force, so there is certainly an envelope to write, and a parent
		// with no deadline, so only the bound can end it.
		_, _ = h.send(context.WithoutCancel(t.Context()), c, genstore.ReqID(id), wsCursor{}, true)
		done <- time.Since(start)
	}()

	select {
	case took := <-done:
		if took > 30*time.Second {
			t.Fatalf("send returned after %v, want the ten-second bound to have ended it", took)
		}
	case <-time.After(40 * time.Second):
		t.Fatal("WEDGED: send did not return within 40s against a write that cannot proceed, " +
			"so the watcher goroutine has no bound -- a hang rather than a failure, which is " +
			"why this assertion carries its own deadline")
	}
}

// contextWithTimeout is t.Context with a deadline, so a hung read fails the
// test instead of the suite's whole timeout.
func contextWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), d)
}
