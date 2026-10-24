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
		ln, err := loopback.Listen(t.Context(), loopback.Port(p))
		if err != nil {
			t.Fatalf("port %d was still bound after Run returned: %v", p, err)
		}
		_ = ln.Close()
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
	ln, lerr := loopback.Listen(t.Context(), loopback.Port(free))
	if lerr != nil {
		t.Fatalf("the first listener survived a failed Run: %v", lerr)
	}
	_ = ln.Close()
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
