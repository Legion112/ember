//go:build integration

package integration

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestLifecycle_ForwardsRealBytesAndDestroys is the whole product in one test.
//
// ember rents a container, authenticates against a host key it minted and
// injected itself, forwards a port to loopback, and destroys the node. Every
// layer runs its production code: the real Docker client in dockerclient,
// which the coverage gate excludes as a branchless adapter and which therefore
// runs nowhere else; a real sshd; a real SSH handshake against a pinned host
// key; a real direct-tcpip forward.
//
// The forward is pointed at the node's own sshd so the lab can read a banner
// back. That is bytes which provably crossed the tunnel from inside the
// container -- a successful accept on the local listener would not show that,
// and a tunnel which completes a connect and then drops payload passes the
// weaker test and fails in production.
func TestLifecycle_ForwardsRealBytesAndDestroys(t *testing.T) {
	l := harness.New(t)
	port := harness.FreePort(t)

	cmd, wait := l.Start(t.Context(), "up",
		"--hold", "3m",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)

	l.WaitNode(t.Context(), 90*time.Second)

	// The forward is opened after the node is ready, so the banner read is
	// retried rather than attempted once against a listener that may not be
	// bound yet.
	var banner string
	deadline := time.Now().Add(90 * time.Second)
	for {
		b, err := l.ReadBanner(port, 5*time.Second)
		if err == nil && b != "" {
			banner = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing came back through the forward within 90s: %v\n%s", err, wait())
		}
		time.Sleep(500 * time.Millisecond)
	}

	if !strings.Contains(banner, "SSH-2.0") {
		t.Errorf("the bytes through the tunnel are not an sshd banner: %q", banner)
	}

	if err := cmd.Process.Signal(interrupt()); err != nil {
		t.Fatalf("signal ember: %v", err)
	}
	out := wait()

	// Ctrl-C is the single most likely way to leak a rented GPU: the
	// operator's context is cancelled, and a destroy made on that same
	// context fails instantly. provision runs destroy on
	// context.WithoutCancel for exactly this reason, and this is where that
	// is proven against a real daemon rather than a fake.
	l.WaitNoNodes(t.Context(), 2*time.Minute, func() string { return out })

	if !strings.Contains(out, "node ready") {
		t.Errorf("ember never reported a ready node:\n%s", out)
	}
}

// TestLifecycle_CleanRunLeavesAnEmptyJournal. A record that outlives its node
// makes every later `ember ls` report an orphan that is not there, and an
// operator who learns to ignore that output is an operator who will ignore a
// real one.
func TestLifecycle_CleanRunLeavesAnEmptyJournal(t *testing.T) {
	l := harness.New(t)
	port := harness.FreePort(t)

	res := l.Run(t.Context(), "up",
		"--hold", "2s",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}

	if n := len(l.Nodes(t.Context())); n != 0 {
		t.Errorf("%d node(s) survived a clean run", n)
	}

	// With nothing running and nothing recorded, ls is clean and exits 0.
	ls := l.Run(t.Context(), "ls")
	if ls.ExitCode != 0 {
		t.Errorf("ls after a clean run exited %d:\n%s", ls.ExitCode, ls.Combined())
	}
	if !strings.Contains(ls.Combined(), "no nodes") {
		t.Errorf("ls does not report a clean slate:\n%s", ls.Combined())
	}
}
