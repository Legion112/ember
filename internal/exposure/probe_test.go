package exposure

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// listenLoopback opens a real listener so the probe has something that
// genuinely accepts. A fake dialer proves the bookkeeping; this proves the
// probe reaches an actual socket.
func listenLoopback(t *testing.T) uint16 {
	t.Helper()
	// This is a test file, so it is outside the one-listener rule that
	// TestNoListenOutsideLoopback enforces over non-test sources.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	return uint16(l.Addr().(*net.TCPAddr).Port)
}

func TestProbe_FindsAPortThatAnswers(t *testing.T) {
	port := listenLoopback(t)

	open, err := Probe{}.Open(t.Context(), "127.0.0.1", []uint16{port})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0] != port {
		t.Fatalf("got %v, want [%d]", open, port)
	}

	if err := (Probe{}).AssertClosed(t.Context(), "127.0.0.1", []uint16{port}); err == nil {
		t.Fatal("AssertClosed passed on a port that accepts connections")
	}
}

func TestProbe_AClosedPortPasses(t *testing.T) {
	// Port 1 on loopback: privileged, and nothing in a test environment
	// binds it. A refused connection is the best case a probe can observe.
	if err := (Probe{Timeout: time.Second}).AssertClosed(t.Context(), "127.0.0.1", []uint16{1}); err != nil {
		t.Fatalf("a closed port was reported open: %v", err)
	}
}

func TestProbe_RefusesAnEmptyHost(t *testing.T) {
	// An empty host dials this machine, which would report the operator's own
	// listeners as the node's.
	if _, err := (Probe{}).Open(t.Context(), "", []uint16{22}); err == nil {
		t.Fatal("an empty host was accepted")
	}
	if err := (Probe{}).AssertClosed(t.Context(), "", []uint16{22}); err == nil {
		t.Fatal("an empty host was accepted")
	}
}

// TestProbe_ADroppedPortIsNotProofOfBeingClosed states the honest limit of
// this tool in code. The dialer here behaves like a firewall that DROPs: it
// never answers and the context expires. The probe reports "not open", which
// is all it can say, and the comment on Probe says so too -- this test exists
// so that limit cannot be quietly upgraded to a guarantee later.
func TestProbe_ADroppedPortIsNotProofOfBeingClosed(t *testing.T) {
	dropped := func(ctx context.Context, _, _ string) (net.Conn, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	open, err := Probe{Dial: dropped, Timeout: 10 * time.Millisecond}.Open(t.Context(), "198.51.100.1", []uint16{22, 8188})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("got %v", open)
	}
}

// TestProbe_EachPortGetsItsOwnTimeout. A shared budget would let a run of
// dropped ports consume it, and the later ports would report "closed" because
// the clock ran out -- a false pass on the only answer that matters.
func TestProbe_EachPortGetsItsOwnTimeout(t *testing.T) {
	port := listenLoopback(t)

	calls := 0
	slowThenReal := func(ctx context.Context, network, addr string) (net.Conn, error) {
		calls++
		if calls <= 3 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		var d net.Dialer
		return d.DialContext(ctx, network, addr)
	}

	open, err := Probe{Dial: slowThenReal, Timeout: 10 * time.Millisecond}.
		Open(t.Context(), "127.0.0.1", []uint16{1, 2, 3, port})
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 || open[0] != port {
		t.Fatalf("got %v, want [%d]; three dropped ports exhausted a shared budget", open, port)
	}
}

// TestProbe_AnInterruptedProbeIsAnError, not a pass. A probe that was cut
// short has verified nothing, and reporting that as "all closed" is the
// failure this whole package is built to avoid.
func TestProbe_AnInterruptedProbeIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Probe{Timeout: time.Second}.Open(ctx, "198.51.100.1", []uint16{22})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), "unverified, not closed") {
		t.Errorf("the error does not say what was left unverified: %v", err)
	}

	if err := (Probe{Timeout: time.Second}).AssertClosed(ctx, "198.51.100.1", []uint16{22}); !errors.Is(err, context.Canceled) {
		t.Fatalf("AssertClosed on a cancelled context returned %v", err)
	}
}

func TestProbe_DefaultTimeoutIsUsedWhenUnset(t *testing.T) {
	if got := (Probe{}).timeout(); got != 5*time.Second {
		t.Errorf("got %v", got)
	}
	if got := (Probe{Timeout: time.Minute}).timeout(); got != time.Minute {
		t.Errorf("got %v", got)
	}
}
