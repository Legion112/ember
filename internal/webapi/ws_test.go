package webapi

import (
	"context"
	"encoding/json"
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
	// No queue is wired in this harness, so nothing is generating. The
	// field's absence is the honest answer rather than a guess.
	if env.Running != "" {
		t.Errorf("running = %q with no queue wired", env.Running)
	}
	if env.Request == nil {
		t.Fatal("no request on the envelope")
	}
}

// contextWithTimeout is t.Context with a deadline, so a hung read fails the
// test instead of the suite's whole timeout.
func contextWithTimeout(t *testing.T, d time.Duration) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(t.Context(), d)
}
