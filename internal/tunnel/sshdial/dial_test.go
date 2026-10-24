package sshdial

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/node"
)

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// server runs a real SSH server on a loopback TCP listener and returns a dial
// function reaching it, so the handshake and the host-key check are exercised
// against the actual protocol in milliseconds — no sshd, no Docker, no build
// tag. This is where the security property lives, so it is tested for real.
//
// net.Pipe would be tidier and does not work: it is unbuffered, and the SSH
// handshake has both ends write their version banner before either reads one.
// On real TCP the kernel's socket buffer absorbs that; on a pipe both writes
// block forever.
func server(t *testing.T, hostKey ssh.Signer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	t.Helper()
	dial, _ := serverAt(t, hostKey)
	return dial
}

// serverAt is server, and also reports the address it is listening on so a
// test can reach it through the default net.Dialer.
func serverAt(t *testing.T, hostKey ssh.Signer) (func(ctx context.Context, network, addr string) (net.Conn, error), string) {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(hostKey)

	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:gosec // loopback only, torn down with the test
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					_ = c.Close()
					return
				}
				go func() {
					for r := range reqs {
						if r.WantReply {
							_ = r.Reply(false, nil)
						}
					}
				}()
				go func() {
					for ch := range chans {
						serveChannel(ch)
					}
				}()
				_ = sc.Wait()
			}()
		}
	}()

	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}, ln.Addr().String()
}

// serveChannel implements enough of direct-tcpip for the forwarding test:
// the client asks for a host and port, and the server connects to it and
// joins the two. That is exactly what a real sshd does for ssh -L.
func serveChannel(ch ssh.NewChannel) {
	if ch.ChannelType() != "direct-tcpip" {
		_ = ch.Reject(ssh.UnknownChannelType, ch.ChannelType())
		return
	}
	var payload struct {
		Host  string
		Port  uint32
		Orig  string
		OPort uint32
	}
	if err := ssh.Unmarshal(ch.ExtraData(), &payload); err != nil {
		_ = ch.Reject(ssh.ConnectionFailed, "bad payload")
		return
	}
	target, err := net.DialTimeout("tcp", net.JoinHostPort(payload.Host, itoa(int(payload.Port))), time.Second)
	if err != nil {
		_ = ch.Reject(ssh.ConnectionFailed, err.Error())
		return
	}
	c, reqs, err := ch.Accept()
	if err != nil {
		_ = target.Close()
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { _, _ = io.Copy(target, c); _ = target.Close() }()
	go func() { _, _ = io.Copy(c, target); _ = c.Close() }()
}

func itoa(i int) string { return strconv.Itoa(i) }

func target() Target {
	return Target{NodeID: "n1", Endpoint: node.Endpoint{Host: "node.invalid", Port: 22, User: "root"}}
}

func pinnedTo(t *testing.T, s ssh.Signer) hostkey.Trust {
	t.Helper()
	tr, err := hostkey.Pinned(hostkey.Of(s.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestDial_AcceptsThePinnedHostKey(t *testing.T) {
	host := newSigner(t)
	d := Dialer{Net: server(t, host), Keepalive: time.Hour}

	c, err := d.Dial(t.Context(), target(), pinnedTo(t, host), []ssh.Signer{newSigner(t)})
	if err != nil {
		t.Fatalf("the pinned host key was refused: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	// Close must be safe twice: `ember up` closes on its own teardown path
	// and again from a deferred cleanup.
	_ = c.Close()
}

// TestDial_RefusesAnUnexpectedHostKey is the test this package exists for. A
// node presenting a key ember did not mint is a node ember is not talking to.
func TestDial_RefusesAnUnexpectedHostKey(t *testing.T) {
	presented, expected := newSigner(t), newSigner(t)
	d := Dialer{Net: server(t, presented)}

	_, err := d.Dial(t.Context(), target(), pinnedTo(t, expected), []ssh.Signer{newSigner(t)})
	if err == nil {
		t.Fatal("a server presenting the wrong host key was accepted")
	}
	var mm *hostkey.MismatchError
	if !errors.As(err, &mm) {
		t.Fatalf("want a MismatchError through the handshake, got %T: %v", err, err)
	}
}

// TestDial_TOFUPinsThenRefusesRotation walks the fallback path end to end
// against a real handshake: first connection pins, a rotated key is refused.
func TestDial_TOFUPinsThenRefusesRotation(t *testing.T) {
	store, err := hostkey.OpenStore(t.TempDir() + "/pins.json")
	if err != nil {
		t.Fatal(err)
	}
	first, rotated := newSigner(t), newSigner(t)
	signers := []ssh.Signer{newSigner(t)}

	d := Dialer{Net: server(t, first)}
	c, err := d.Dial(t.Context(), target(), store.TrustOnFirstUse("n1"), signers)
	if err != nil {
		t.Fatalf("the first connection should pin and succeed: %v", err)
	}
	_ = c.Close()

	d = Dialer{Net: server(t, rotated)}
	if _, err := d.Dial(t.Context(), target(), store.TrustOnFirstUse("n1"), signers); err == nil {
		t.Fatal("a rotated host key was accepted after the pin")
	}
}

func TestDial_RefusesWithoutATrustPolicy(t *testing.T) {
	d := Dialer{Net: server(t, newSigner(t))}
	_, err := d.Dial(t.Context(), target(), nil, []ssh.Signer{newSigner(t)})
	if err == nil {
		t.Fatal("Dial succeeded with no host key policy")
	}
	if !strings.Contains(err.Error(), "host key policy") {
		t.Errorf("got %v", err)
	}
}

func TestDial_RefusesWithoutSigners(t *testing.T) {
	host := newSigner(t)
	d := Dialer{Net: server(t, host)}
	if _, err := d.Dial(t.Context(), target(), pinnedTo(t, host), nil); err == nil {
		t.Fatal("Dial succeeded with nothing to authenticate with")
	}
}

func TestDial_RefusesAnIncompleteEndpoint(t *testing.T) {
	host := newSigner(t)
	d := Dialer{Net: server(t, host)}
	bad := Target{NodeID: "n1", Endpoint: node.Endpoint{Host: "h", User: "root"}} // port 0
	if _, err := d.Dial(t.Context(), bad, pinnedTo(t, host), []ssh.Signer{newSigner(t)}); err == nil {
		t.Fatal("Dial accepted an endpoint with no port")
	}
}

func TestDial_ReportsAConnectFailure(t *testing.T) {
	host := newSigner(t)
	d := Dialer{Net: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("network unreachable")
	}}
	_, err := d.Dial(t.Context(), target(), pinnedTo(t, host), []ssh.Signer{newSigner(t)})
	if err == nil {
		t.Fatal("Dial reported success with no connection")
	}
	// The node and the address belong in the message: with several nodes up,
	// "network unreachable" alone does not say which one.
	for _, want := range []string{"n1", "node.invalid:22"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q: %v", want, err)
		}
	}
}

func TestDial_ReportsAHandshakeFailure(t *testing.T) {
	host := newSigner(t)
	// A peer that is not an SSH server at all: a stale port mapping pointing
	// somewhere else is the realistic cause.
	d := Dialer{Net: func(context.Context, string, string) (net.Conn, error) {
		client, srv := net.Pipe()
		go func() { _ = srv.Close() }()
		return client, nil
	}}
	if _, err := d.Dial(t.Context(), target(), pinnedTo(t, host), []ssh.Signer{newSigner(t)}); err == nil {
		t.Fatal("a non-SSH peer completed a handshake")
	}
}

// TestDial_KeepaliveRunsAndStops: without keepalives a provider's idle timeout
// black-holes the session and the next request hangs for minutes, which the
// operator reads as ember being slow.
func TestDial_KeepaliveRunsAndStops(t *testing.T) {
	host := newSigner(t)
	d := Dialer{Net: server(t, host), Keepalive: 5 * time.Millisecond}

	c, err := d.Dial(t.Context(), target(), pinnedTo(t, host), []ssh.Signer{newSigner(t)})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond) // several ticks against the real server
	if err := c.Close(); err != nil {
		t.Errorf("Close after keepalives: %v", err)
	}
	time.Sleep(20 * time.Millisecond) // the goroutine must have stopped, not panicked
}

func TestDial_DefaultsAreApplied(t *testing.T) {
	host := newSigner(t)
	// Zero Timeout and Keepalive must fall back rather than meaning "never".
	d := Dialer{Net: server(t, host)}
	c, err := d.Dial(t.Context(), target(), pinnedTo(t, host), []ssh.Signer{newSigner(t)})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

// TestClient_SatisfiesTunnelDialer keeps the two packages honest about the
// seam they share without importing tunnel here.
func TestClient_SatisfiesTunnelDialer(t *testing.T) {
	var _ interface {
		DialContext(ctx context.Context, network, addr string) (net.Conn, error)
	} = (*Client)(nil)
}

// TestDial_ForwardsThroughARealSSHChannel is the end-to-end claim: bytes
// written on this side come out of a listener reached only from the server's
// side, over a real direct-tcpip channel on a real SSH connection.
func TestDial_ForwardsThroughARealSSHChannel(t *testing.T) {
	// The "workload": an echo listener the client can only reach through SSH.
	echo, err := net.Listen("tcp", "127.0.0.1:0") //nolint:gosec // loopback only
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()

	host := newSigner(t)
	d := Dialer{Net: server(t, host), Keepalive: time.Hour}
	client, err := d.Dial(t.Context(), target(), pinnedTo(t, host), []ssh.Signer{newSigner(t)})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	conn, err := client.DialContext(t.Context(), "tcp", echo.Addr().String())
	if err != nil {
		t.Fatalf("direct-tcpip through the tunnel failed: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	want := "through a real ssh channel"
	if _, err := io.WriteString(conn, want); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(want))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != want {
		t.Errorf("got %q, want %q", buf, want)
	}
}

// TestDial_UsesTheDefaultNetDialer covers the branch taken in production,
// where Net is nil and ember dials TCP itself.
func TestDial_UsesTheDefaultNetDialer(t *testing.T) {
	host := newSigner(t)
	_, addr := serverAt(t, host)

	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.ParseUint(p, 10, 16)
	if err != nil {
		t.Fatal(err)
	}

	d := Dialer{Keepalive: time.Hour} // Net is nil: the production path
	tgt := Target{NodeID: "n1", Endpoint: node.Endpoint{Host: h, Port: uint16(port), User: "root"}}
	c, err := d.Dial(t.Context(), tgt, pinnedTo(t, host), []ssh.Signer{newSigner(t)})
	if err != nil {
		t.Fatalf("the default dialer could not reach a real listener: %v", err)
	}
	_ = c.Close()
}
