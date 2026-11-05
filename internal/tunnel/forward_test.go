package tunnel

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRemoteLoopback_IsAlwaysTheNodesOwnLoopback. direct-tcpip reaches
// anywhere the node can, including the provider's metadata service, so the
// remote host is not configurable anywhere in ember.
func TestRemoteLoopback_IsAlwaysTheNodesOwnLoopback(t *testing.T) {
	ap := RemoteLoopback(1234)
	if !ap.Addr().IsLoopback() {
		t.Fatalf("RemoteLoopback produced %s", ap)
	}
	if got, want := ap.String(), "127.0.0.1:1234"; got != want {
		t.Errorf("RemoteLoopback(1234) = %q, want %q", got, want)
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

// TestWaitReady_DialsNothingButTheRemoteItWasGiven is the dialling half of
// RemoteLoopback's seal: the address on the wire has to be the one the caller
// named, because the one thing this package must never do is dial somewhere
// the node can reach and ember did not choose.
func TestWaitReady_DialsNothingButTheRemoteItWasGiven(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	if err := WaitReady(t.Context(), d, RemoteLoopback(1234), 0); err != nil {
		t.Fatal(err)
	}
	got := d.Dialled()
	if len(got) != 1 || got[0] != "tcp 127.0.0.1:1234" {
		t.Errorf("dialled %v, want one tcp 127.0.0.1:1234", got)
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

// TestRecordingDialer_EchoesSoACopyCanBeAsserted keeps the fake able to prove
// that bytes crossed it, which is the one thing a dial count cannot say.
func TestRecordingDialer_Echoes(t *testing.T) {
	d := NewRecordingDialer()
	defer d.Close()

	c, err := d.DialContext(t.Context(), "tcp", "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()

	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := c.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Errorf("read %q, want ping", buf)
	}
}
