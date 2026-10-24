package session

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
)

func signer(t *testing.T) ssh.Signer {
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

func pinnedTo(t *testing.T, s ssh.Signer) hostkey.Trust {
	t.Helper()
	tr, err := hostkey.Pinned(hostkey.Of(s.PublicKey()))
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func target() sshdial.Target {
	return sshdial.Target{
		NodeID:   "n1",
		Endpoint: node.Endpoint{Host: "node.invalid", Port: 22, User: "root"},
	}
}

// sshServer runs a real SSH server on loopback. refuseFirst connections are
// closed before the handshake, which is how "sshd has not bound yet" is
// reproduced without timing.
func sshServer(t *testing.T, host ssh.Signer, refuseFirst int32) func(context.Context, string, string) (net.Conn, error) {
	t.Helper()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
			return &ssh.Permissions{}, nil
		},
	}
	cfg.AddHostKey(host)

	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:gosec // loopback only, closed with the test
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	remaining := refuseFirst
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if atomic.LoadInt32(&remaining) > 0 {
				atomic.AddInt32(&remaining, -1)
				_ = c.Close() // reset by peer, exactly what a starting sshd gives
				continue
			}
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					_ = c.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				go func() {
					for ch := range chans {
						_ = ch.Reject(ssh.Prohibited, "not needed here")
					}
				}()
				_ = sc.Wait()
			}()
		}
	}()

	return func(ctx context.Context, network, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, network, ln.Addr().String())
	}
}

// TestDial_WaitsForSshdRatherThanForThePort is the behaviour this package was
// extracted to make testable. A provider reports ready when the port is
// published, which is before sshd has bound a socket.
func TestDial_WaitsForSshdRatherThanForThePort(t *testing.T) {
	host := signer(t)
	o := Options{
		Dialer:     sshdial.Dialer{Net: sshServer(t, host, 3)},
		SSHReady:   10 * time.Second,
		RetryEvery: 10 * time.Millisecond,
	}

	client, err := Dial(t.Context(), target(), pinnedTo(t, host), []ssh.Signer{signer(t)}, o)
	if err != nil {
		t.Fatalf("Dial gave up on a node that was merely starting: %v", err)
	}
	_ = client.Close()
}

// TestDial_DoesNotRetryAHostKeyMismatch. That failure is never transient, and
// retrying it turns a refusal into a hang — which is how an operator ends up
// disabling the check.
func TestDial_DoesNotRetryAHostKeyMismatch(t *testing.T) {
	presented, expected := signer(t), signer(t)
	o := Options{
		Dialer:     sshdial.Dialer{Net: sshServer(t, presented, 0)},
		SSHReady:   10 * time.Second,
		RetryEvery: 10 * time.Millisecond,
	}

	start := time.Now()
	_, err := Dial(t.Context(), target(), pinnedTo(t, expected), []ssh.Signer{signer(t)}, o)
	if err == nil {
		t.Fatal("a mismatched host key was accepted")
	}
	var mismatch *hostkey.MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("want a MismatchError, got %T: %v", err, err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("it retried for %v; a mismatch must fail at once", elapsed)
	}
}

func TestDial_GivesUpWithTheReasonItLastSaw(t *testing.T) {
	host := signer(t)
	o := Options{
		Dialer:     sshdial.Dialer{Net: sshServer(t, host, 1000)}, // never accepts
		SSHReady:   150 * time.Millisecond,
		RetryEvery: 10 * time.Millisecond,
	}

	_, err := Dial(t.Context(), target(), pinnedTo(t, host), []ssh.Signer{signer(t)}, o)
	if err == nil {
		t.Fatal("Dial waited forever")
	}
	for _, want := range []string{"n1", "never accepted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error should contain %q: %v", want, err)
		}
	}
}

func TestDial_StopsWhenTheContextDoes(t *testing.T) {
	host := signer(t)
	o := Options{
		Dialer:     sshdial.Dialer{Net: sshServer(t, host, 1000)},
		SSHReady:   time.Minute,
		RetryEvery: 10 * time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Millisecond)
	defer cancel()

	_, err := Dial(ctx, target(), pinnedTo(t, host), []ssh.Signer{signer(t)}, o)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want the context's reason", err)
	}
}

func TestForwards_BindsBothLoopbackFamilies(t *testing.T) {
	got := Forwards([]node.PortMap{{Name: "comfyui", Remote: 8188, Local: 18188}})
	if len(got) != 1 {
		t.Fatalf("got %d forwards", len(got))
	}
	f := got[0]
	if len(f.Local) != 2 {
		t.Fatalf("got %d local binds, want v4 and v6", len(f.Local))
	}
	if f.Local[0].String() != "127.0.0.1:18188" || f.Local[1].String() != "[::1]:18188" {
		t.Errorf("binds = %v, %v", f.Local[0], f.Local[1])
	}
	// The remote end is always the node's own loopback, never configurable.
	if f.Remote.String() != "127.0.0.1:8188" {
		t.Errorf("remote = %s", f.Remote)
	}
	if len(Forwards(nil)) != 0 {
		t.Error("no ports should produce no forwards")
	}
}

// TestServe_EndsCleanlyWhenHeld: the ordinary way out is not an error, because
// returning one would teach the operator to ignore ember's exit code.
func TestServe_EndsCleanlyWhenHeld(t *testing.T) {
	d := tunnel.NewRecordingDialer()
	defer d.Close()

	var out bytes.Buffer
	fwds := Forwards([]node.PortMap{{Name: "demo", Remote: 8188, Local: freePort(t)}})

	start := time.Now()
	if err := Serve(t.Context(), &out, d, fwds, Options{Hold: 80 * time.Millisecond}); err != nil {
		t.Fatalf("a held session should end cleanly: %v", err)
	}
	if time.Since(start) < 50*time.Millisecond {
		t.Error("Serve returned before its hold elapsed")
	}
	if !strings.Contains(out.String(), "demo") || !strings.Contains(out.String(), "127.0.0.1") {
		t.Errorf("the operator needs to be told where to point a browser: %q", out.String())
	}
}

func TestServe_EndsCleanlyWhenCancelled(t *testing.T) {
	d := tunnel.NewRecordingDialer()
	defer d.Close()

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, io.Discard, d, Forwards([]node.PortMap{{Name: "x", Remote: 1, Local: freePort(t)}}), Options{})
	}()
	time.Sleep(30 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a cancelled session returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not return after cancellation")
	}
}

// TestServe_WithNoPortsStillHoldsTheNode. `ember up` with no -port is a
// legitimate way to keep a machine alive and reach it with `ember ssh`.
func TestServe_WithNoPortsStillHoldsTheNode(t *testing.T) {
	d := tunnel.NewRecordingDialer()
	defer d.Close()

	var out bytes.Buffer
	if err := Serve(t.Context(), &out, d, nil, Options{Hold: 50 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "no ports to forward") {
		t.Errorf("the operator should be told why nothing is listening: %q", out.String())
	}
}

func TestServe_SurfacesARealForwardFailure(t *testing.T) {
	d := tunnel.NewRecordingDialer()
	defer d.Close()

	busy, err := net.Listen("tcp", "127.0.0.1:0") //nolint:gosec // loopback only
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	taken := busy.Addr().(*net.TCPAddr).AddrPort().Port()

	err = Serve(t.Context(), io.Discard, d, Forwards([]node.PortMap{{Name: "clash", Remote: 1, Local: taken}}), Options{Hold: time.Second})
	if err == nil {
		t.Fatal("a port that could not be bound was reported as a clean session")
	}
}

func TestOptions_Defaults(t *testing.T) {
	var o Options
	if o.sshReady() != defaultSSHReady || o.retryEvery() != defaultRetryEvery {
		t.Error("zero values must fall back, not mean \"never\"")
	}
	if o.log() == nil {
		t.Error("log() must never be nil")
	}

	var sb strings.Builder
	o.Log = slog.New(slog.NewTextHandler(&sb, nil))
	o.log().Info("hi")
	if !strings.Contains(sb.String(), "hi") {
		t.Error("the injected logger was ignored")
	}
}

func freePort(t *testing.T) uint16 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0") //nolint:gosec // loopback only
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).AddrPort().Port()
	_ = ln.Close()
	_ = strconv.Itoa(int(p))
	return p
}
