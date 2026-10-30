//go:build integration

package integration

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/volume"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestStore_LsReportsAStandingChargeWithNoNodeRunning.
//
// The failure this lab exists to catch is the quietest one ember has. A store
// bills whether or not anything is attached to it, has no deadline, is not
// reaped, and is not ended by Ctrl-C -- so the run where everything is
// correctly destroyed and `ember ls` prints a clean bill of health is exactly
// the run in which a forgotten store is invisible.
//
// The volume is created through Docker rather than through ember, because
// ember cannot create one yet and because a lab that seeds state out-of-band
// is testing the reader rather than the writer.
func TestStore_LsReportsAStandingChargeWithNoNodeRunning(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	l.CreateVolume(ctx, "ember-store-models")

	out := l.Run(ctx, "ls")
	if out.ExitCode != 0 {
		t.Fatalf("ls exited %d: %s", out.ExitCode, out.Combined())
	}

	// There are no nodes, and that line still appears: the store is reported
	// beside it rather than instead of it.
	if !strings.Contains(out.Stdout, "no nodes.") {
		t.Errorf("ls did not report the absence of nodes:\n%s", out.Combined())
	}
	if !strings.Contains(out.Stdout, "store") {
		t.Errorf("ls did not report the store at all:\n%s", out.Combined())
	}
	// Named as ember names it, without the label prefix, so the name in the
	// output is the one `ember store rm` will take.
	if !strings.Contains(out.Stdout, "models") {
		t.Errorf("ls did not name the store:\n%s", out.Combined())
	}

	// A store is reported, never flagged. Exit 2 here would teach the
	// operator to ignore exit 2, which is worse than not flagging at all.
	if out.ExitCode != 0 {
		t.Errorf("a store made ls exit %d", out.ExitCode)
	}
}

// TestStore_ANodeLeavesTheStoreBehindWhenItIsDestroyed.
//
// The property that makes a store worth having: the node is torn down at the
// end of every run and the weights are not. Driven through `ember up` so the
// teardown is the real one, including the deferred destroy.
func TestStore_ANodeLeavesTheStoreBehindWhenItIsDestroyed(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	l.CreateVolume(ctx, "ember-store-models")

	l.WriteConfig(map[string]string{"cmd": "[/bin/true]"})
	out := l.Run(ctx, "up")
	if out.ExitCode != 0 {
		t.Fatalf("up exited %d: %s", out.ExitCode, out.Combined())
	}

	// The node is gone -- AssertNoStray in the lab's cleanup covers that --
	// and the store is not.
	vols := l.Volumes(ctx)
	if len(vols) != 1 {
		t.Fatalf("after a completed run the store is %+v; it must outlive the node", names(vols))
	}
	if vols[0].Name != "ember-store-models" {
		t.Errorf("surviving volume is %q", vols[0].Name)
	}

	// And it is still the one ember reports.
	if ls := l.Run(ctx, "ls"); !strings.Contains(ls.Stdout, "models") {
		t.Errorf("ls stopped reporting the store after a run:\n%s", ls.Combined())
	}

}

func names(vs []*volume.Volume) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}
