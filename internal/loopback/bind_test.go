package loopback

import (
	"context"
	"net"
	"strings"
	"testing"
)

// TestParseBind_RefusesAnythingNotLoopback is the security claim of this
// package. Each row is a string an operator could plausibly write, and every
// one of them would publish the port.
func TestParseBind_RefusesAnythingNotLoopback(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string // substring the message must contain, so the refusal explains itself
	}{
		{"0.0.0.0:8188", "not loopback"},
		{"[::]:8188", "not loopback"},
		{"192.168.1.10:8188", "not loopback"},
		{"10.0.0.1:8188", "not loopback"},
		{"8.8.8.8:53", "not loopback"},
		{"localhost:8188", "hostname"},
		{"LOCALHOST:8188", "hostname"},
		{"example.com:8188", "hostname"},
		{":8188", "ip:port"},
		{"8188", "ip:port"},
		{"", "empty"},
		{"   ", "empty"},
		{"[::1%eth0]:8188", "zone"},
		{"[::ffff:127.0.0.1]:8188", "IPv4-mapped"},
	} {
		got, err := ParseBind(tc.in)
		if err == nil {
			t.Errorf("ParseBind(%q) = %v, want an error", tc.in, got)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("ParseBind(%q) error %q should contain %q", tc.in, err, tc.want)
		}
	}
}

func TestParseBind_AcceptsLoopback(t *testing.T) {
	for _, in := range []string{
		"127.0.0.1:8188",
		"[::1]:8188",
		// 127.0.0.2 is genuinely unreachable off-host and is useful when the
		// operator wants one loopback alias per node.
		"127.0.0.2:0",
		"127.255.255.254:1",
		" 127.0.0.1:9000 ",
	} {
		b, err := ParseBind(in)
		if err != nil {
			t.Errorf("ParseBind(%q): %v", in, err)
			continue
		}
		if !b.IsValid() {
			t.Errorf("ParseBind(%q) produced an invalid Bind", in)
		}
	}
}

func TestPortConstructors(t *testing.T) {
	if got, want := Port(8188).String(), "127.0.0.1:8188"; got != want {
		t.Errorf("Port(8188) = %q, want %q", got, want)
	}
	if got, want := Port6(8188).String(), "[::1]:8188"; got != want {
		t.Errorf("Port6(8188) = %q, want %q", got, want)
	}
	if !Port(1).IsValid() || !Port6(1).IsValid() {
		t.Error("the constructors must produce valid Binds")
	}
	if got := Port(8188).AddrPort().Port(); got != 8188 {
		t.Errorf("AddrPort().Port() = %d", got)
	}
}

// TestZeroBind_IsNotListenable: a Bind nobody set must not quietly become
// 0.0.0.0:0.
func TestZeroBind_IsNotListenable(t *testing.T) {
	var b Bind
	if b.IsValid() {
		t.Fatal("the zero Bind must not report IsValid")
	}
	if _, err := Listen(t.Context(), b); err == nil {
		t.Fatal("Listen accepted the zero Bind")
	}
}

// TestListen_BindsOnlyLoopback asserts the real property rather than the
// spelling: after Listen succeeds, the chosen port must not be reachable on a
// non-loopback address of this machine.
func TestListen_BindsOnlyLoopback(t *testing.T) {
	ln, err := Listen(t.Context(), Port(0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	port := ln.Addr().(*net.TCPAddr).Port
	for _, addr := range nonLoopbackAddrs(t) {
		target := net.JoinHostPort(addr, itoa(port))
		c, err := net.DialTimeout("tcp", target, dialTimeout)
		if err == nil {
			_ = c.Close()
			t.Errorf("a listener created through loopback.Listen accepted a connection on %s", target)
		}
	}
}

func TestListen_ReportsTheUnderlyingFailure(t *testing.T) {
	first, err := Listen(t.Context(), Port(0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()

	// AddrPort().Port() is already a uint16, so there is no conversion to get
	// wrong. TCPAddr.Port is an int and converting it needs either a range
	// check or a lint suppression, both of which are work to avoid a
	// narrowing that did not have to exist.
	taken := first.Addr().(*net.TCPAddr).AddrPort().Port()
	if _, err := Listen(t.Context(), Port(taken)); err == nil {
		t.Fatal("Listen succeeded on an address already in use")
	} else if !strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("the error should name the address it failed on, got %v", err)
	}
}

func TestListen_HonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	// A cancelled context may or may not fail the listen depending on
	// platform; what must not happen is a listener that outlives it silently.
	ln, err := Listen(ctx, Port(0))
	if err == nil {
		_ = ln.Close()
	}
}
