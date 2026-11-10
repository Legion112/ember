package session

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/node"
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
