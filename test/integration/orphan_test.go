//go:build integration

package integration

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestOrphan_SigkillLeavesExactlyOneFindableNode.
//
// SIGKILL is the case no amount of deferred cleanup can cover: ember gets no
// chance to destroy anything. This is where the journal earns its existence --
// the intent record is written before the create call, so a process killed
// anywhere in that window still leaves enough to find the node again.
//
// The assertion worth making is "exactly one finding". Writing this lab turned
// up a real bug: a run killed in the window between Create returning and the
// node id being recorded produced *two* findings -- an orphaned record that
// could not name its node, and an untracked node that no record mentioned --
// and reap could act on neither. Both carried the same run id in ember's own
// label the whole time. reconcile now joins them on it, which is what
// "labels are the source of truth, the store is a cache" has to mean in
// practice.
//
// ls must also exit 2 rather than 0: an ls that exited 0 here would mean an
// operator's timer reporting health over a GPU that is still billing.
func TestOrphan_SigkillLeavesExactlyOneFindableNode(t *testing.T) {
	l := harness.New(t)
	port := harness.FreePort(t)

	cmd, wait := l.Start(t.Context(), "up",
		"--hold", "10m",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)
	l.WaitNode(t.Context(), 90*time.Second)

	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill ember: %v", err)
	}
	_ = wait()

	// Nothing ran a destroy, so the node is still there and still billing.
	if n := len(l.Nodes(t.Context())); n != 1 {
		t.Fatalf("expected the killed run to leave exactly 1 node, got %d", n)
	}

	ls := l.Run(t.Context(), "ls")
	if ls.ExitCode != 2 {
		t.Errorf("ls exited %d after a SIGKILL, want 2; a timer would read this as healthy\n%s",
			ls.ExitCode, ls.Combined())
	}

	// One node, so one finding. Two means the record and its node were not
	// joined, and an operator reading the output cannot tell whether they are
	// looking at one problem or two.
	if n := countFindings(ls.Combined()); n != 1 {
		t.Errorf("one killed run produced %d findings, want 1:\n%s", n, ls.Combined())
	}

	// reap leaves it: the node is inside its deadline, and the deadline is
	// the authority on when a node may be destroyed. What ends this one is
	// its own self-destruct, which TestSelfDestruct covers.
	reap := l.Run(t.Context(), "reap")
	if reap.ExitCode != 0 {
		t.Fatalf("reap exited %d\n%s", reap.ExitCode, reap.Combined())
	}
	if n := len(l.Nodes(t.Context())); n != 1 {
		t.Errorf("reap destroyed a node inside its deadline; %d left", n)
	}
}

// countFindings counts the listing lines, ignoring the trailing summary.
func countFindings(out string) int {
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" || strings.HasPrefix(line, "ember:") {
			continue
		}
		n++
	}
	return n
}

// TestOrphan_DryRunReapDestroysNothing. The flag exists so an operator can
// read what a cron entry is about to do before trusting it with real money,
// and a dry run that was wrong about even one node would be worse than no
// flag at all.
func TestOrphan_DryRunReapDestroysNothing(t *testing.T) {
	l := harness.New(t)
	port := harness.FreePort(t)

	cmd, wait := l.Start(t.Context(), "up",
		"--hold", "10m",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)
	l.WaitNode(t.Context(), 90*time.Second)
	if err := cmd.Process.Kill(); err != nil {
		t.Fatalf("kill ember: %v", err)
	}
	_ = wait()

	before := len(l.Nodes(t.Context()))
	if before == 0 {
		t.Fatal("no node to dry-run against")
	}
	dry := l.Run(t.Context(), "reap", "--dry-run")
	if dry.ExitCode != 0 {
		t.Fatalf("reap --dry-run exited %d\n%s", dry.ExitCode, dry.Combined())
	}
	if n := len(l.Nodes(t.Context())); n != before {
		t.Errorf("a dry run changed the node count from %d to %d", before, n)
	}
}

// TestUp_DryRunCreatesNothing, not even a journal entry. The journal is
// written before the create call, so a dry run that wrote one would leave a
// record of a node that was never asked for -- and every later `ls` would
// report an orphan that does not exist.
func TestUp_DryRunCreatesNothing(t *testing.T) {
	l := harness.New(t)

	res := l.Run(t.Context(), "up", "--dry-run")
	if res.ExitCode != 0 {
		t.Fatalf("ember up --dry-run exited %d\n%s", res.ExitCode, res.Combined())
	}
	if !strings.Contains(res.Combined(), "nothing was created") {
		t.Errorf("a dry run should say so plainly:\n%s", res.Combined())
	}
	if n := len(l.Nodes(t.Context())); n != 0 {
		t.Errorf("a dry run created %d node(s)", n)
	}
	if recs := l.Records(); strings.Contains(recs, "\"phase\"") {
		t.Errorf("a dry run wrote a journal record:\n%s", recs)
	}
}

// TestUp_DestroysWhenTheForwardCannotBind is the failure path that matters
// most after a successful rent: the node exists and is costing money, and the
// thing that failed is on this side of the connection. Taking the local port
// first is the honest way to reproduce it -- it is also a real thing that
// happens, when a previous run's browser tab is still open.
func TestUp_DestroysWhenTheForwardCannotBind(t *testing.T) {
	l := harness.New(t)
	port := harness.FreePort(t)

	// Hold the port for the whole lab, so ember cannot have it.
	// A test file, so outside the single-listener rule.
	blocker, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatalf("take the port first: %v", err)
	}
	defer func() { _ = blocker.Close() }()

	res := l.Run(t.Context(), "up",
		"--hold", "10m",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)
	if res.ExitCode == 0 {
		t.Fatalf("ember up succeeded with its local port already taken\n%s", res.Combined())
	}

	// The node must be gone regardless. A rented GPU does not care which side
	// of the connection broke.
	l.WaitNoNodes(t.Context(), 2*time.Minute, res.Combined)
}
