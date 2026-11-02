//go:build integration

package integration

import (
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestLeftover_DestroyRemovesTheNetworkWhenSomethingElseTookTheContainer is
// the bug, against a real daemon.
//
// `local.Destroy` used to find the run id by inspecting the container, so a
// container removed by anything other than ember -- `docker rm`, a prune, a
// daemon restart, a crashed lab's cleanup -- made that inspect return
// not-found. Destroy treats not-found as success, so the run id stayed empty
// and Destroy returned nil *before* the network lookup. The bridge network
// survived wearing ember's label and the destroy reported success.
//
// Nothing bills for a bridge network, which is why this was invisible. What it
// costs is every local run failing at once, weeks later, with "all predefined
// address pools have been fully subnetted" -- an error naming Docker and not
// ember. That has already happened here once, to the lab harness.
//
// The container is removed with the Docker client and not through ember,
// because "something that is not ember took it away" is the entire premise.
func TestLeftover_DestroyRemovesTheNetworkWhenSomethingElseTookTheContainer(t *testing.T) {
	l := harness.New(t)

	cmd, wait := l.Start(t.Context(), "up", "--hold", "3m")
	l.WaitNode(t.Context(), 90*time.Second)

	nodes := l.Nodes(t.Context())
	if len(nodes) != 1 {
		t.Fatalf("want one node, got %d\n%s", len(nodes), wait())
	}
	if n := len(l.Networks(t.Context())); n != 1 {
		t.Fatalf("ember up made %d networks, want 1", n)
	}

	// Out of band, forcefully, while ember is still holding the node.
	l.Remove(t.Context(), nodes[0].ID)

	if err := cmd.Process.Signal(interrupt()); err != nil {
		t.Fatalf("signal ember: %v", err)
	}
	out := wait()

	// provision.destroy already had the run id -- it is destroying a run it
	// created -- and now it passes it, so the network goes even though the
	// container that used to carry the label is gone.
	if n := l.Networks(t.Context()); len(n) != 0 {
		var names []string
		for _, net := range n {
			names = append(names, net.Name)
		}
		t.Errorf("ember destroyed the node and left %d network(s): %s\n%s",
			len(n), strings.Join(names, ", "), out)
	}
}

// TestLeftover_ReapRemovesANetworkNothingElseWould is the second mechanism,
// and the only one that can recover what is already on the daemon.
//
// Destroy carrying the run id closes the case going forward. It cannot help a
// network that leaked before this change, and it cannot help `ember down
// provider/id` on a node with no local record whose container somebody already
// removed -- there is nothing left anywhere to say which run it belonged to.
// The label is, as everywhere else here, the source of truth: the sweep finds
// the network by it, which is exactly the information that survives.
func TestLeftover_ReapRemovesANetworkNothingElseWould(t *testing.T) {
	l := harness.New(t)

	// Already past its deadline, so reap may act on it. The deadline is the
	// authority on destruction for a leftover as it is for a node, and here it
	// is also what tells wreckage from a live run: Create makes the network
	// before the container, so for a moment every healthy run looks exactly
	// like this.
	id := l.CreateNetwork(t.Context(), "00000000deadbeef", time.Now().Add(-time.Hour))

	ls := l.Run(t.Context(), "ls")
	if ls.ExitCode != 2 {
		t.Errorf("ls exited %d over a network nothing will ever remove, want 2\n%s", ls.ExitCode, ls.Combined())
	}
	if !strings.Contains(ls.Combined(), "leftover") {
		t.Errorf("ls does not call it a leftover:\n%s", ls.Combined())
	}
	if !strings.Contains(ls.Combined(), "ember-00000000deadbeef") {
		t.Errorf("ls does not name the network, so nobody can find it in `docker network ls`:\n%s", ls.Combined())
	}

	reap := l.Run(t.Context(), "reap")
	if reap.ExitCode != 0 {
		t.Fatalf("reap exited %d\n%s", reap.ExitCode, reap.Combined())
	}
	if !strings.Contains(reap.Combined(), "removed 1 of 1 leftover") {
		t.Errorf("reap does not say what it removed:\n%s", reap.Combined())
	}

	for _, n := range l.Networks(t.Context()) {
		if n.ID == id {
			t.Errorf("reap left the network %s behind:\n%s", n.Name, reap.Combined())
		}
	}
	if after := l.Run(t.Context(), "ls"); after.ExitCode != 0 {
		t.Errorf("ls still reports findings after reap:\n%s", after.Combined())
	}
}

// TestLeftover_ReapLeavesALiveRunsNetworkAlone is the restraint, and the one
// that matters most: local.Create makes the network before the container, so
// for a moment a perfectly healthy run has a labelled network with nothing
// attached. A sweep that acted on that would delete a working run's network.
func TestLeftover_ReapLeavesALiveRunsNetworkAlone(t *testing.T) {
	l := harness.New(t)

	id := l.CreateNetwork(t.Context(), "00000000c0ffee00", time.Now().Add(time.Hour))

	if ls := l.Run(t.Context(), "ls"); ls.ExitCode != 0 {
		t.Errorf("ls exited %d over a network inside its deadline, want 0\n%s", ls.ExitCode, ls.Combined())
	}

	reap := l.Run(t.Context(), "reap")
	if reap.ExitCode != 0 {
		t.Fatalf("reap exited %d\n%s", reap.ExitCode, reap.Combined())
	}
	if !strings.Contains(reap.Combined(), "nothing to reap") {
		t.Errorf("reap should have found nothing to do:\n%s", reap.Combined())
	}

	var found bool
	for _, n := range l.Networks(t.Context()) {
		found = found || n.ID == id
	}
	if !found {
		t.Error("reap removed a network inside its deadline; a live run would have lost its bridge")
	}
}
