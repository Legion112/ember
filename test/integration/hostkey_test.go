//go:build integration

package integration

import (
	"errors"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/test/integration/harness"
)

// TestHostKey_AMintedKeyAuthenticatesTheFirstConnection.
//
// This is the claim that makes trust-on-first-use unnecessary here. ember
// generates the host key on this machine, ships the private half to the node
// through the provider's API body, and the node's sshd adopts it. The very
// first dial is therefore verified against a fingerprint ember already knew,
// and nobody on the path to a brand-new IP has a window -- which is precisely
// the window TOFU cannot close.
func TestHostKey_AMintedKeyAuthenticatesTheFirstConnection(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "firstdial")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}

	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatalf("the first dial against an injected host key failed: %v", err)
	}
	_ = client.Close()
}

// TestHostKey_ARotatedKeyIsRefused is the test that proves the security
// control, and it is the reason this lab package starts its own node: the
// control only triggers on a second dial to the same node id, and ember's own
// nodes are destroyed by design before there can be a second dial.
//
// Rotating the host key inside the container is what an attacker substituting
// a machine looks like from the client's side. The dial must fail, there must
// be no prompt, and there is no --force anywhere in ember to get past it: a
// prompt is a bypass with extra steps, and for an ephemeral node the correct
// response is always to destroy it.
func TestHostKey_ARotatedKeyIsRefused(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "rotate")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}

	// First dial: succeeds against the key ember injected.
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatalf("the first dial failed, so the rotation proves nothing: %v", err)
	}
	_ = client.Close()

	l.RotateHostKey(t.Context(), n.ID)
	l.WaitSSH(n.Addr, 60*time.Second)

	// Second dial: the machine now presents a key ember never minted.
	_, err = dial(t, l, n, trust)
	if err == nil {
		t.Fatal("ember connected to a node presenting a host key it never minted")
	}

	var mismatch *hostkey.MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("got %T (%v), want *hostkey.MismatchError", err, err)
	}
}

// TestHostKey_AMismatchIsNotRetried. session.Dial retries, because sshd binds
// its listener before it will negotiate and the first dial legitimately fails.
// A mismatch is the one error it must not retry: retrying an authentication
// failure against a substituted machine is a slow-motion way of eventually
// accepting it, and it turns one clear error into a minute of silence.
func TestHostKey_AMismatchIsNotRetried(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "noretry")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	l.RotateHostKey(t.Context(), n.ID)
	l.WaitSSH(n.Addr, 60*time.Second)

	start := time.Now()
	_, err = dial(t, l, n, trust)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("the dial succeeded against a rotated key")
	}
	var mismatch *hostkey.MismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("got %T (%v), want *hostkey.MismatchError", err, err)
	}
	// The retry budget is tens of seconds. Returning inside a few means the
	// mismatch short-circuited it rather than being retried to exhaustion.
	if elapsed > 15*time.Second {
		t.Errorf("a mismatch took %v to report, so it was retried rather than refused outright", elapsed)
	}
}
