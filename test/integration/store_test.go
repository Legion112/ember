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

// TestStore_CreateMountRunDestroyRemove is the whole lifecycle through the
// binary, with nothing seeded out-of-band.
//
// The property at the centre is the one that makes a store worth the risk it
// carries: the workload writes to the mount, the node is destroyed, and what
// it wrote is still there for the next run.
func TestStore_CreateMountRunDestroyRemove(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	l.WriteConfig(map[string]string{
		"store": "\n  name: models\n  size_gib: 40\n  region: EU-RO-1",
		"cmd":   `[/bin/sh, -c, "echo weights > /models/model.bin; cat /models/model.bin"]`,
	})

	if out := l.RunSub(ctx, "store", "create"); out.ExitCode != 0 {
		t.Fatalf("store create exited %d: %s", out.ExitCode, out.Combined())
	}

	// First run writes to the store.
	first := l.Run(ctx, "up")
	if first.ExitCode != 0 {
		t.Fatalf("first up exited %d: %s", first.ExitCode, first.Combined())
	}
	if !strings.Contains(first.Stdout, "weights") {
		t.Errorf("the workload did not read back what it wrote:\n%s", first.Combined())
	}

	// The node is gone. The store is not.
	if vs := l.Volumes(ctx); len(vs) != 1 {
		t.Fatalf("after the run the store is %v", names(vs))
	}

	// Second run, a different node, finds the first one's file already
	// there. This is the whole point: weights survive the machine.
	l.WriteConfig(map[string]string{
		"store": "\n  name: models\n  size_gib: 40\n  region: EU-RO-1",
		"cmd":   `[/bin/sh, -c, "cat /models/model.bin"]`,
	})
	second := l.Run(ctx, "up")
	if second.ExitCode != 0 {
		t.Fatalf("second up exited %d: %s", second.ExitCode, second.Combined())
	}
	if !strings.Contains(second.Stdout, "weights") {
		t.Errorf("a second node did not see what the first wrote:\n%s", second.Combined())
	}

	// And ember removes it when told to, which is the only thing that ever
	// will.
	if out := l.RunSub(ctx, "store", "rm", "models"); out.ExitCode != 0 {
		t.Fatalf("store rm exited %d: %s", out.ExitCode, out.Combined())
	}
	if vs := l.Volumes(ctx); len(vs) != 0 {
		t.Errorf("the store survived its own removal: %v", names(vs))
	}
}

// TestStore_UpRefusesAStoreThatDoesNotExist.
//
// Creating it here would mean a config that mentions a name brings a standing
// monthly charge into existence. The refusal has to come before anything is
// rented, too: a run that discovers this after creating a node is already
// paying for the discovery.
func TestStore_UpRefusesAStoreThatDoesNotExist(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	l.WriteConfig(map[string]string{
		"store": "\n  name: absent\n  size_gib: 40\n  region: EU-RO-1",
		"cmd":   "[/bin/true]",
	})

	out := l.Run(ctx, "up")
	if out.ExitCode == 0 {
		t.Fatalf("up ran against a store that does not exist:\n%s", out.Combined())
	}
	if !strings.Contains(out.Combined(), "ember store create") {
		t.Errorf("the refusal does not name the command that makes one:\n%s", out.Combined())
	}
	if n := len(l.Nodes(ctx)); n != 0 {
		t.Errorf("%d node(s) were created before the store was resolved", n)
	}
	if vs := l.Volumes(ctx); len(vs) != 0 {
		t.Errorf("up created a store rather than refusing: %v", names(vs))
	}
}
