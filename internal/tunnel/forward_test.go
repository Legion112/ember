package tunnel

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/loopback"
)

func fwd(name string, local uint16, remote uint16) Forward {
	return Forward{
		Name:   name,
		Local:  []loopback.Bind{loopback.Port(local)},
		Remote: RemoteLoopback(remote),
	}
}

// runFwd starts a Forwarder and returns a stop function plus the error channel.
func runFwd(t *testing.T, d Dialer, fwds ...Forward) (context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	f := &Forwarder{Dialer: d}
	go func() { errc <- f.Run(ctx, fwds) }()
	return cancel, errc
}

// stopFwd cancels a running Forwarder and asserts it unwound cleanly. A
// cancelled Run returning an error would teach the operator to ignore ember's
// exit code, so the tests check it rather than discarding it.
func stopFwd(t *testing.T, cancel context.CancelFunc, errc <-chan error) {
	t.Helper()
	cancel()
	if err := <-errc; err != nil {
		t.Errorf("a cancelled Run should return nil, got %v", err)
	}
}

// freePort asks the kernel for a port and gives it straight back, so the test
// binds something the machine is not already using.
func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := loopback.Listen(t.Context(), loopback.Port(0))
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).AddrPort().Port()
	_ = ln.Close()
	return p
}

func dialLocal(t *testing.T, port uint16) net.Conn {
	t.Helper()
	return dialAddr(t, loopback.Port(port).String())
}

// assertBindable asserts a port can be listened on again, retrying until a
// deadline.
//
// THE RETRY IS ABOUT THE MACHINE, NOT ABOUT THE FORWARDER. Run closes every
// listener before it returns -- twice over, through context.AfterFunc on
// cancellation and through closeAll on the way out -- so the port really is
// free by then. What is not guaranteed is that it STAYS free: freePort asks
// the kernel for an ephemeral port and gives it straight back, and between
// Run releasing the port and this assertion taking it, anything else on the
// machine can bind it. CI runs every package's test binary at once, each
// handing out ephemeral ports, and that is what happened:
//
//	port 45531 was still bound after Run returned: listen on 127.0.0.1:45531:
//	listen tcp 127.0.0.1:45531: bind: address already in use
//
// once, on a pull request that had not touched this package, and never in 300
// local runs. A single attempt turns that into a red build on an unrelated
// change, which is how a working check gets ignored rather than obeyed.
//
// The property is unweakened: a forwarder that genuinely held its listener
// still fails here, a second or two later. What the retry removes is the
// false failure, and the cost is that a leak is reported late rather than at
// once. EADDRINUSE cannot say WHO holds the port, so the two cannot be told
// apart from here and the message says so rather than accusing the forwarder.
//
// Same shape and same reason as dialAddr below, from the other side: that one
// retries because a listener may not be open yet, this one because a port may
// not be free yet.
func assertBindable(t *testing.T, port uint16, when string) {
	t.Helper()
	deadline := time.Now().Add(portReleaseWait)
	var err error
	for {
		var ln net.Listener
		if ln, err = loopback.Listen(t.Context(), loopback.Port(port)); err == nil {
			_ = ln.Close()
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("port %d could not be bound %s, after %v of retrying: %v\n"+
				"Either the forwarder did not release it, or something else on this "+
				"machine holds it; the error cannot tell those apart.",
				port, when, portReleaseWait, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// portReleaseWait bounds that retry.
//
// Long enough to outlast another test binary's ephemeral listener, short
// enough that a forwarder which really leaked is reported in about the time
// one of these tests already takes. Not a guarantee: a process that holds the
// port for longer still fails this, which is the honest limit of asserting on
// a port number the kernel is free to hand to anybody.
const portReleaseWait = 3 * time.Second

// dialAddr retries, because Run opens its listeners on another goroutine and
// a test that dialled once would be asserting on the scheduler.
func dialAddr(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			return c
		}
		if time.Now().After(deadline) {
			t.Fatalf("never connected to the forwarder at %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestRemoteLoopback_IsAlwaysTheNodesOwnLoopback. direct-tcpip reaches
// anywhere the node can, including the provider's metadata service, so the
// remote host is not configurable anywhere in ember.
func TestRemoteLoopback_IsAlwaysTheNodesOwnLoopback(t *testing.T) {
	ap := RemoteLoopback(8188)
	if !ap.Addr().IsLoopback() {
		t.Fatalf("RemoteLoopback produced %s", ap)
	}
	if got, want := ap.String(), "127.0.0.1:8188"; got != want {
		t.Errorf("RemoteLoopback(8188) = %q, want %q", got, want)
	}
}

func TestForwarder_DialsOnlyTheRemoteItWasGiven(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	port := freePort(t)
	cancel, errc := runFwd(t, d, fwd("comfyui", port, 8188))
	defer stopFwd(t, cancel, errc)

	c := dialLocal(t, port)
	defer func() { _ = c.Close() }()

	deadline := time.Now().Add(2 * time.Second)
	for len(d.Dialled()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	calls := d.Dialled()
	if len(calls) != 1 || calls[0] != "tcp 127.0.0.1:8188" {
		t.Fatalf("dialled %v, want exactly [tcp 127.0.0.1:8188]", calls)
	}
}

func TestForwarder_CopiesBothDirections(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	port := freePort(t)
	cancel, errc := runFwd(t, d, fwd("echo", port, 9000))
	defer stopFwd(t, cancel, errc)

	c := dialLocal(t, port)
	defer func() { _ = c.Close() }()

	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	want := "hello through the tunnel"
	if _, err := io.WriteString(c, want); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(c, buf); err != nil {
		t.Fatalf("nothing came back: %v", err)
	}
	if string(buf) != want {
		t.Errorf("got %q, want %q", buf, want)
	}
}

// TestForwarder_RemoteDialFailureDoesNotKillTheListener is the behaviour that
// matters while a node is still starting: ComfyUI takes a minute to bind its
// port, and the browser retries. A forwarder that died on the first refusal
// would exit while the node it is forwarding is still coming up.
func TestForwarder_RemoteDialFailureDoesNotKillTheListener(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()
	d.FailOn = "127.0.0.1:9100"

	port := freePort(t)
	cancel, errc := runFwd(t, d, fwd("slow", port, 9100))
	defer stopFwd(t, cancel, errc)

	// First attempt: the far side refuses.
	first := dialLocal(t, port)
	_, _ = io.WriteString(first, "x")
	_ = first.Close()

	// The workload comes up.
	d.mu.Lock()
	d.FailOn = ""
	d.mu.Unlock()

	second := dialLocal(t, port)
	defer func() { _ = second.Close() }()
	if err := second.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(second, "ping"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(second, buf); err != nil {
		t.Fatalf("the forwarder did not recover after a refused remote: %v", err)
	}
}

// TestForwarder_CancelReleasesEveryPort asserts the real property rather than
// a call count: after Run returns, the same ports must be bindable again.
//
// WHY IT HAS NEVER FAILED, which is worth knowing before trusting it. On this
// path the leak is unrepresentable: closing the listener is what unblocks a
// goroutine parked in Accept, so wg.Wait cannot return until every listener
// is closed, and Run returning therefore IMPLIES the ports are free.
// Measured, not assumed -- deleting closeAll from the exit path entirely
// leaves this test green, because context.AfterFunc has already done it.
//
// So what it guards is the SHAPE rather than the current code: the day
// somebody unblocks Accept another way -- a deadline, a select on ctx.Done in
// serve -- closeAll becomes the only thing releasing the ports and this is
// what notices. The leak that IS reachable today is the early-return path,
// which TestForwarder_RefusesToStartWhenAPortIsTaken covers through the same
// assertion, and reinstating it there is what proves the retry below did not
// turn either of them into a no-op.
func TestForwarder_CancelReleasesEveryPort(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	a, b := freePort(t), freePort(t)
	cancel, errc := runFwd(t, d, fwd("one", a, 1), fwd("two", b, 2))

	_ = dialLocal(t, a).Close()
	cancel()
	if err := <-errc; err != nil {
		t.Fatalf("a cancelled Run should return nil, got %v", err)
	}

	for _, p := range []uint16{a, b} {
		assertBindable(t, p, "after Run returned")
	}
}

// TestForwarder_RefusesToStartWhenAPortIsTaken: a partially forwarded node is
// a state the operator cannot reason about.
func TestForwarder_RefusesToStartWhenAPortIsTaken(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	taken, err := loopback.Listen(t.Context(), loopback.Port(0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	busy := taken.Addr().(*net.TCPAddr).AddrPort().Port()
	free := freePort(t)

	f := &Forwarder{Dialer: d}
	err = f.Run(t.Context(), []Forward{fwd("ok", free, 1), fwd("clash", busy, 2)})
	if err == nil {
		t.Fatal("Run succeeded with a port already in use")
	}
	if !strings.Contains(err.Error(), "clash") {
		t.Errorf("the error should name the forward, got %v", err)
	}

	// And nothing may be left bound, including the one that did open.
	assertBindable(t, free, "after a Run that refused to start")
}

func TestForwarder_RefusesMalformedForwards(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()
	f := &Forwarder{Dialer: d}

	for name, bad := range map[string]Forward{
		"no remote": {Name: "x", Local: []loopback.Bind{loopback.Port(freePort(t))}},
		"no local":  {Name: "y", Remote: RemoteLoopback(1)},
	} {
		if err := f.Run(t.Context(), []Forward{bad}); err == nil {
			t.Errorf("%s: Run accepted it", name)
		}
	}
}

func TestForwarder_RefusesWithoutADialer(t *testing.T) {
	f := &Forwarder{}
	if err := f.Run(t.Context(), nil); err == nil {
		t.Fatal("Run with no dialer succeeded")
	}
}

func TestForwarder_BindsBothLoopbackFamilies(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	port := freePort(t)
	f := Forward{
		Name:   "dual",
		Local:  []loopback.Bind{loopback.Port(port), loopback.Port6(port)},
		Remote: RemoteLoopback(1),
	}
	cancel, errc := runFwd(t, d, f)
	defer stopFwd(t, cancel, errc)

	// A browser resolving localhost to ::1 first must find a listener there.
	for _, addr := range []string{loopback.Port(port).String(), loopback.Port6(port).String()} {
		c := dialAddr(t, addr)
		_ = c.Close()
	}
}

func TestWaitReady(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	if err := WaitReady(t.Context(), d, RemoteLoopback(1), 0); err != nil {
		t.Fatalf("WaitReady against a ready remote: %v", err)
	}

	// A remote that never comes up must surface the context's reason rather
	// than spinning forever.
	d.FailOn = "127.0.0.1:2"
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()
	err := WaitReady(ctx, d, RemoteLoopback(2), 10*time.Millisecond)
	if err == nil {
		t.Fatal("WaitReady returned nil for a remote that never listened")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("want a context error, got %v", err)
	}
}

func TestRecordingDialer_HonoursExplicitErrors(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()
	d.Errors["127.0.0.1:7"] = ErrRefused

	_, err := d.DialContext(t.Context(), "tcp", "127.0.0.1:7")
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("got %v, want ErrRefused", err)
	}
	if got := d.Dialled(); len(got) != 1 {
		t.Errorf("a failed dial should still be recorded, got %v", got)
	}
}

func TestRecordingDialer_WithoutEchoStillConnects(t *testing.T) {
	d := NewRecordingDialer()
	d.Echo = false
	defer d.Close()

	c, err := d.DialContext(t.Context(), "tcp", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

// TestForwarder_ConcurrentConnections keeps the WaitGroup honest: Run must not
// return while connections are still live.
func TestForwarder_ConcurrentConnections(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	port := freePort(t)
	cancel, errc := runFwd(t, d, fwd("many", port, 5000))
	defer stopFwd(t, cancel, errc)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := dialLocal(t, port)
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.WriteString(c, "ping"); err != nil {
				return
			}
			buf := make([]byte, 4)
			_, _ = io.ReadFull(c, buf)
		}()
	}
	wg.Wait()
}

// TestForwarder_UsesTheLoggerItWasGiven covers the nil-check in log(). The
// default is a discard handler, so without this nothing would notice the
// branch being inverted and every forward going silent.
func TestForwarder_UsesTheLoggerItWasGiven(t *testing.T) {
	var sb strings.Builder
	d := NewRecordingDialer()
	defer d.Close()

	port := freePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	f := &Forwarder{Dialer: d, Log: slog.New(slog.NewTextHandler(&sb, nil))}
	go func() { errc <- f.Run(ctx, []Forward{fwd("named", port, 1)}) }()

	_ = dialLocal(t, port).Close()
	stopFwd(t, cancel, errc)

	out := sb.String()
	if !strings.Contains(out, "named") || !strings.Contains(out, "forwarding") {
		t.Errorf("the forward was not logged: %q", out)
	}
}

// TestForwarder_CancelClosesAnIdleConnection covers the ctx branch in pipe.
// An idle connection has no copy finishing to unblock it, so cancellation is
// the only thing that can — and if it did not, Run would hang on its
// WaitGroup forever and `ember up` would never exit after Ctrl-C.
func TestForwarder_CancelClosesAnIdleConnection(t *testing.T) {
	d := NewRecordingDialer()
	d.Echo = false // nothing flows, so neither copy completes
	defer d.Close()

	port := freePort(t)
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	f := &Forwarder{Dialer: d}
	go func() { errc <- f.Run(ctx, []Forward{fwd("idle", port, 1)}) }()

	c := dialLocal(t, port)
	defer func() { _ = c.Close() }()
	for len(d.Dialled()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("cancelled Run returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel with an idle connection open")
	}
}
