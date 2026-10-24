//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestReap_DestroysAnExpiredNodeNobodyIsHolding.
//
// The node is started by the lab with its deadline already in the past and
// its own self-destruct disarmed, so reap is the only thing that can end it.
// Letting a TTL expire naturally would race the node's self-destruct, and a
// lab that passes because the node ended itself proves nothing about reap --
// which is the authority, because poweroff stops the GPU but not necessarily
// the bill.
func TestReap_DestroysAnExpiredNodeNobodyIsHolding(t *testing.T) {
	l := harness.New(t)

	id, _ := l.StartNode(t.Context(), harness.NodeSpec{
		Name:          "expired",
		AuthorizedKey: identity(t, l).AuthorizedKey,
		HostKeyPEM:    mintPEM(t),
		EmberLabels:   true,
		Deadline:      time.Now().Add(-time.Hour),
	})

	ls := l.Run(t.Context(), "ls")
	if ls.ExitCode != 2 {
		t.Errorf("ls exited %d over an expired node, want 2\n%s", ls.ExitCode, ls.Combined())
	}
	if !strings.Contains(ls.Combined(), "expired") {
		t.Errorf("ls does not call it expired:\n%s", ls.Combined())
	}

	reap := l.Run(t.Context(), "reap")
	if reap.ExitCode != 0 {
		t.Fatalf("reap exited %d\n%s", reap.ExitCode, reap.Combined())
	}
	l.WaitNoNodes(t.Context(), time.Minute, reap.Combined)
	_ = id

	if after := l.Run(t.Context(), "ls"); after.ExitCode != 0 {
		t.Errorf("ls still reports findings after reap:\n%s", after.Combined())
	}
}

// TestReap_LeavesANodeInsideItsDeadline is the restraint, and it is
// deliberate. A node wearing ember's label with no local record may belong to
// another machine of the operator's, and destroying somebody's running work
// because this laptop had not heard of it is not a trade to make from a cron
// entry. The deadline is the authority on when a node may be destroyed.
func TestReap_LeavesANodeInsideItsDeadline(t *testing.T) {
	l := harness.New(t)

	l.StartNode(t.Context(), harness.NodeSpec{
		Name:          "live",
		AuthorizedKey: identity(t, l).AuthorizedKey,
		HostKeyPEM:    mintPEM(t),
		EmberLabels:   true,
		Deadline:      time.Now().Add(time.Hour),
	})

	// Still a finding: untracked and billing is worth saying out loud.
	if ls := l.Run(t.Context(), "ls"); ls.ExitCode != 2 {
		t.Errorf("ls exited %d over an untracked live node, want 2\n%s", ls.ExitCode, ls.Combined())
	}

	reap := l.Run(t.Context(), "reap")
	if reap.ExitCode != 0 {
		t.Fatalf("reap exited %d\n%s", reap.ExitCode, reap.Combined())
	}
	if n := len(l.Nodes(t.Context())); n != 1 {
		t.Errorf("reap destroyed a node inside its deadline; %d left", n)
	}
	if !strings.Contains(reap.Combined(), "nothing to reap") {
		t.Errorf("reap should say it found nothing to do:\n%s", reap.Combined())
	}
}

// TestSelfDestruct_TheNodeEndsItselfWithoutEmber is the defence that covers a
// dead laptop, and the one that was silently broken once: embernode used to
// hand off to sshd with syscall.Exec, which replaced the process image and
// took the TTL goroutine with it. The self-destruct did nothing at all, and
// nothing failed -- which is exactly why it is a lab now.
func TestSelfDestruct_TheNodeEndsItselfWithoutEmber(t *testing.T) {
	l := harness.New(t)

	id, addr := l.StartNode(t.Context(), harness.NodeSpec{
		Name:          "selfdestruct",
		AuthorizedKey: identity(t, l).AuthorizedKey,
		HostKeyPEM:    mintPEM(t),
		TTLSeconds:    10,
	})
	// It must be genuinely up first, or a node that never started would pass
	// this lab by being dead for the wrong reason.
	l.WaitSSH(addr, 90*time.Second)

	l.WaitExited(t.Context(), id, 90*time.Second)
}
