//go:build unix

package webapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/loopback"
)

// A REAL LISTENER AND NOT A RECORDER, AND THIS TIME IT IS NOT ABOUT HEADERS.
//
// headers_test.go owns a listener because httptest.ResponseRecorder reads the
// live header map and so cannot see WHEN a header was set. The reason here is
// one step further out: what every assertion in this file turns on is WHICH
// LISTENER a request arrived on, and a recorder has no listener at all. A
// handler test that put the socket marker on a context by hand would be
// asserting that ember agrees with itself -- the closed loop
// capabilities.model and the unrecorded preview frame both sat in -- because
// the thing worth checking is that ConnContext is wired to the socket's
// server and to no other.

// socketServer starts h on a real unix socket and returns a client that talks
// to it.
//
// The http.Client dials the socket for every request, so what it exercises is
// the whole of ServeSocket: the listener, the server, the ConnContext mark and
// the route table a real New built.
func socketServer(t *testing.T, h *harness) (*http.Client, string) {
	t.Helper()

	state := filepath.Join(t.TempDir(), "state")
	if err := os.MkdirAll(state, 0o700); err != nil {
		t.Fatal(err)
	}
	sock, err := loopback.StateSocket(state)
	if err != nil {
		t.Fatal(err)
	}
	h.opts.Socket = sock

	ln, err := h.ListenSocket(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.ServeSocket(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("ServeSocket = %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("ServeSocket did not return after its context ended")
		}
	})

	path := sock.Path()
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", path)
		},
	}}, path
}

// overSocket sends one request and reads what came back.
//
// The Host is whatever the caller spells, because that is the thing under
// test: `curl --unix-socket` sends `Host: localhost`, with no port, and the
// TCP guard compares a port.
func overSocket(t *testing.T, c *http.Client, method, host, origin, path, body string) answer {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	// The host in the URL is ignored by the dialler above; it is what lands in
	// the Host header.
	r, err := http.NewRequestWithContext(t.Context(), method, "http://"+host+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatalf("%s %s over the socket: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	got, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return answer{Status: resp.StatusCode, Header: resp.Header, Body: got}
}

// enqueues counts what reached the store, so a /api/config read earlier in a
// test is not mistaken for a queued rental.
func enqueues(h *harness) int {
	n := 0
	for _, c := range h.store.Calls {
		if strings.HasPrefix(c, "Enqueue") {
			n++
		}
	}
	return n
}

// TestSocket_NeedsNoTokenBecauseTheKernelAlreadyAnswered.
//
// THE WHOLE POINT OF THE CHANNEL. connect(2) on a 0600 socket in a 0700
// directory is refused for every account but the operator's own before ember
// sees a byte, so there is nothing a token could add -- and a token on this
// channel would be a secret a CLI client has to find, which is the thing the
// socket exists to avoid.
//
// A READ AND A WRITE, because a channel that can list but not enqueue is not
// the channel #146 needs, and because the write is where the money is: if
// this route were reachable without the kernel's answer, the account next
// door could queue a rental.
func TestSocket_NeedsNoTokenBecauseTheKernelAlreadyAnswered(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Queue = &RecordingDrain{} })
	c, _ := socketServer(t, h)

	// No Authorization header anywhere below, and no cookie.
	if a := overSocket(t, c, "GET", "localhost", "", "/api/config", ""); a.Status != http.StatusOK {
		t.Errorf("GET /api/config over the socket = %d, want 200: %s", a.Status, a.Body)
	}
	a := overSocket(t, c, "POST", "localhost", "", "/api/requests", oneJob)
	if a.Status != http.StatusAccepted {
		t.Fatalf("POST /api/requests over the socket = %d, want 202: %s", a.Status, a.Body)
	}
	// And it reached the store rather than being answered out of the handler.
	if n := enqueues(h); n != 1 {
		t.Errorf("%d enqueue(s) reached the store, want 1: %v", n, h.store.Calls)
	}
}

// TestSocket_TheMarkIsNotSomethingARequestCanSay.
//
// THE ONE WAY THIS CHANGE COULD HAVE BEEN WORSE THAN NO CHANGE. If "I arrived
// on the socket" were a header, the loopback port would accept the header
// too and the mandatory credential would have been replaced by a string
// anyone on the machine can type. It is a value on the request's context,
// written by one ConnContext on one http.Server, so there is no spelling of
// it on the wire -- and the proof is that every spelling somebody might
// invent is still refused on TCP.
//
// The list is header spellings and a query parameter rather than the one name
// the code uses, because there is no such name: the key is an unexported
// zero-size type. What is being asserted is that the set of ways to claim the
// channel is empty.
func TestSocket_TheMarkIsNotSomethingARequestCanSay(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Queue = &RecordingDrain{} })
	srv := live(t, h)

	for _, claim := range []struct{ key, value string }{
		{"X-Ember-Socket", "1"},
		{"X-Ember-Channel", "socket"},
		{"X-Forwarded-Proto", "unix"},
		{"X-Forwarded-For", "unix"},
		{"Ember-Peer", "local"},
		{"X-Real-Ip", "@"},
	} {
		t.Run(claim.key, func(t *testing.T) {
			r, err := http.NewRequestWithContext(t.Context(), "POST", srv.URL+"/api/requests",
				strings.NewReader(oneJob))
			if err != nil {
				t.Fatal(err)
			}
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Origin", srv.URL)
			// No credential, and the claim instead of one.
			r.Header.Set(claim.key, claim.value)
			resp, err := srv.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("%s: %d, want 403 -- a header claimed the socket channel", claim.key, resp.StatusCode)
			}
		})
	}
	// A query parameter too, since the one thing already honoured in a URL is
	// a token. The credentialled control is beside it: without it a refusal
	// would be consistent with a path that is refused for every caller.
	t.Run("a query parameter", func(t *testing.T) {
		if a := h.wire(t, srv, "GET", "/api/requests?socket=1&channel=socket", "", nil); a.Status != http.StatusOK {
			t.Fatalf("the credentialled control failed: %d: %s", a.Status, a.Body)
		}
		r, err := http.NewRequestWithContext(t.Context(), "GET",
			srv.URL+"/api/requests?socket=1&channel=socket", nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Origin", srv.URL)
		resp, err := srv.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("status = %d, want 403 -- a query parameter claimed the socket channel", resp.StatusCode)
		}
	})

	// Nothing was queued by any of it, which is the side effect the refusal
	// exists to prevent rather than the status code.
	if n := enqueues(h); n != 0 {
		t.Errorf("%d request(s) were queued by a request claiming the socket: %v", n, h.store.Calls)
	}
}

// TestSocket_KeepsTheOriginCheckAndDropsTheHostCheck.
//
// THE DECISION hostOK RECORDS, asserted rather than left to fall out. The Host
// check is a statement about a TCP port and a socket request has none --
// `curl --unix-socket` sends `Host: localhost`, which does not even parse as
// host:port -- so requiring it would not be a stricter check, it would be a
// channel that refuses everything. The Origin check is about who COMPOSED the
// request rather than where it arrived, so it still runs: through a proxy an
// operator puts in front of the socket it is the only thing left that refuses
// a page on another site.
func TestSocket_KeepsTheOriginCheckAndDropsTheHostCheck(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Queue = &RecordingDrain{} })
	c, _ := socketServer(t, h)

	for _, host := range []string{
		// What curl --unix-socket sends: a name and no port at all.
		"localhost",
		// And a name that would be a rebinding attempt over TCP. It names no
		// port of this server's because the socket is not on one.
		"evil.example",
	} {
		if a := overSocket(t, c, "GET", host, "", "/api/config", ""); a.Status != http.StatusOK {
			t.Errorf("Host %q over the socket = %d, want 200: %s", host, a.Status, a.Body)
		}
	}

	// And a page's Origin is still refused, with nothing queued by it.
	a := overSocket(t, c, "POST", "localhost", "https://evil.example", "/api/requests", oneJob)
	if a.Status != http.StatusForbidden {
		t.Errorf("a foreign Origin over the socket = %d, want 403: %s", a.Status, a.Body)
	}
	if n := enqueues(h); n != 0 {
		t.Errorf("%d request(s) were queued by a foreign Origin: %v", n, h.store.Calls)
	}
}

// TestSocket_TheWebsocketTakesTheSameDecision.
//
// A guard that covers the JSON routes and not /ws guards nothing: the socket
// is how a watcher reads every event of every generation. The handshake is
// the one route that checks the Host without the Origin -- coder/websocket
// answers that half -- so it is the second caller of hostOK and the one a
// change to the exemption would most easily miss.
func TestSocket_TheWebsocketTakesTheSameDecision(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Queue = &RecordingDrain{} })
	c, _ := socketServer(t, h)

	// No credential, a Host with no port, and no websocket headers: what is
	// asserted is that it got PAST the guard and the credential, which is a
	// 400 about the upgrade rather than a 403 about who is asking.
	a := overSocket(t, c, "GET", "localhost", "", "/ws?id=abc", "")
	if a.Status == http.StatusForbidden {
		t.Fatalf("the handshake over the socket was refused as unauthenticated: %s", a.Body)
	}
	if a.Status == http.StatusOK {
		t.Fatalf("a request with no upgrade headers was accepted as a handshake: %s", a.Body)
	}
}

// TestListenSocket_RefusesAServerBuiltWithoutOne.
//
// The zero loopback.Socket is a server with no channel, which is the stricter
// configuration and therefore the default -- so every handler test in this
// package keeps needing the token. Inventing a path here would be this
// package choosing where ember's state lives.
func TestListenSocket_RefusesAServerBuiltWithoutOne(t *testing.T) {
	h := newHarness(t)
	if p := h.SocketPath(); p != "" {
		t.Errorf("a server with no socket reports a path: %q", p)
	}
	if _, err := h.ListenSocket(t.Context()); err == nil {
		t.Error("ListenSocket invented a path for a server built without one")
	}
}

// TestServeSocket_RefusesAListenerNobodyOpened.
//
// SocketListener has to be exported for ServeSocket to name it, so a zero one
// is writable by anybody who imports loopback -- and serving it would panic in
// Accept inside an http.Server's goroutine, which takes the process and its
// rental with it. Refused instead, which is loopback.Listen's answer for a
// Bind nobody set.
func TestServeSocket_RefusesAListenerNobodyOpened(t *testing.T) {
	h := newHarness(t)
	for _, ln := range []*loopback.SocketListener{nil, {}} {
		if err := h.ServeSocket(t.Context(), ln); err == nil {
			t.Errorf("ServeSocket accepted %#v", ln)
		}
	}
}

// TestSocket_TheTokenIsStillTheOnlyAnswerOnThePort.
//
// THE HALF THAT MUST NOT HAVE MOVED. The whole risk in adding a second
// channel is that the two drift into "either is fine", and the direction that
// costs money is a port that stopped asking. So the same server that serves
// an unauthenticated socket is asked over TCP, in the same test, with no
// credential -- because a refusal asserted in another file could pass against
// a server that has no socket at all.
func TestSocket_TheTokenIsStillTheOnlyAnswerOnThePort(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Queue = &RecordingDrain{} })
	c, _ := socketServer(t, h)
	srv := live(t, h)

	// The socket answers with nothing.
	if a := overSocket(t, c, "GET", "localhost", "", "/api/config", ""); a.Status != http.StatusOK {
		t.Fatalf("the socket stopped answering: %d", a.Status)
	}

	// The port does not.
	r, err := http.NewRequestWithContext(t.Context(), "GET", srv.URL+"/api/config", nil)
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Origin", srv.URL)
	resp, err := srv.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("the loopback port answered an uncredentialled request with %d, want 403", resp.StatusCode)
	}

	// And with the credential it does, so the refusal above is about the
	// credential rather than about a broken server.
	if a := h.wire(t, srv, "GET", "/api/config", "", nil); a.Status != http.StatusOK {
		t.Errorf("the credentialled control failed: %d: %s", a.Status, a.Body)
	}
}

// TestSocket_PathIsUnderTheStateDirectoryAndNamedByLoopback.
//
// A caller hands over a directory and loopback appends the rest, so what
// `ember serve` announces is a path this package did not choose. Asserted
// because the announcement is the only way a client finds it.
func TestSocket_PathIsUnderTheStateDirectoryAndNamedByLoopback(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.Queue = &RecordingDrain{} })
	_, path := socketServer(t, h)

	if got := h.SocketPath(); got != path {
		t.Errorf("SocketPath() = %q, want %q", got, path)
	}
	if !strings.HasSuffix(path, filepath.Join("state", "socket", "serve.sock")) {
		t.Errorf("the socket is not where the state directory is: %q", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("the socket ember serve announces is mode %04o, want 0600", got)
	}
}
