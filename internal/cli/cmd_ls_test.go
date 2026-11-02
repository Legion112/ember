package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/provider/local"
)

func TestLs_EmptyIsCleanAndExitsZero(t *testing.T) {
	var out bytes.Buffer
	if err := runLs(nil, &out, labDeps(t)); err != nil {
		t.Fatalf("nothing running should exit 0: %v", err)
	}
	if !strings.Contains(out.String(), "no nodes") {
		t.Errorf("got %q", out.String())
	}
}

// TestLs_AnUntrackedNodeExitsTwo is the contract with cron: an orphan and a
// broken ember must not look the same to a timer that only sees a status.
func TestLs_AnUntrackedNodeExitsTwo(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	// A node wearing ember's label that no local record mentions: what a lost
	// state directory looks like, and it is still billing.
	id, err := e.ContainerCreate(t.Context(), dockerengine.ContainerSpec{
		Name:  "ember-node-stray",
		Image: local.DefaultImage,
		Labels: map[string]string{
			dockerengine.LabelRunID: "0123456789abcdef",
			local.LabelKind:         "node",
			local.LabelLabel:        "ember-0123456789abcdef-99999999999",
			local.LabelUser:         "root",
		},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ContainerStart(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runLs(nil, &out, d)
	if err == nil {
		t.Fatal("an untracked running node exited 0")
	}
	if ExitCode(err) != ExitFindings {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitFindings)
	}
	if !strings.Contains(out.String(), "expired") && !strings.Contains(out.String(), "untracked") {
		t.Errorf("the finding was not printed: %q", out.String())
	}
}

func TestLs_ReportsAProviderThatCouldNotBeAsked(t *testing.T) {
	d := labDeps(t)
	d.Engine.(*dockerengine.RecordingEngine).FailOn = "ContainerList"

	var out bytes.Buffer
	err := runLs(nil, &out, d)
	if err == nil {
		t.Fatal("ls reported a clean bill of health without being able to check")
	}
	if ExitCode(err) != ExitFindings {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitFindings)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("got %q", out.String())
	}
}

func TestLs_ReportsABadFlag(t *testing.T) {
	var out bytes.Buffer
	if err := runLs([]string{"--nope"}, &out, labDeps(t)); ExitCode(err) != ExitUsage {
		t.Fatalf("got %v", err)
	}
}

func TestLs_HelpExplainsTheExitCode(t *testing.T) {
	var out bytes.Buffer
	_ = runLs([]string{"-h"}, &out, labDeps(t))
	if !strings.Contains(out.String(), "Exit 2") {
		t.Errorf("help should explain the exit code: %q", out.String())
	}
}

func TestReap_NothingToDo(t *testing.T) {
	var out bytes.Buffer
	if err := runReap(nil, &out, labDeps(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to reap") {
		t.Errorf("got %q", out.String())
	}
}

// TestReap_DestroysAnExpiredNode and clears nothing else.
func TestReap_DestroysAnExpiredNode(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	// Deadline in 1970: comfortably expired.
	id, err := e.ContainerCreate(t.Context(), dockerengine.ContainerSpec{
		Name:  "ember-node-old",
		Image: local.DefaultImage,
		Labels: map[string]string{
			dockerengine.LabelRunID: "0123456789abcdef",
			local.LabelKind:         "node",
			local.LabelLabel:        "ember-0123456789abcdef-1",
			local.LabelUser:         "root",
		},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ContainerStart(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runReap(nil, &out, d); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !strings.Contains(out.String(), "destroyed 1") {
		t.Errorf("got %q", out.String())
	}
	if len(e.Containers()) != 0 {
		t.Error("the expired node survived reap")
	}
}

// TestReap_DryRunDestroysNothing.
func TestReap_DryRunDestroysNothing(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	id, err := e.ContainerCreate(t.Context(), dockerengine.ContainerSpec{
		Name:  "ember-node-old",
		Image: local.DefaultImage,
		Labels: map[string]string{
			dockerengine.LabelRunID: "0123456789abcdef",
			local.LabelKind:         "node",
			local.LabelLabel:        "ember-0123456789abcdef-1",
			local.LabelUser:         "root",
		},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ContainerStart(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runReap([]string{"--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if len(e.Containers()) != 1 {
		t.Fatal("a dry run destroyed something")
	}
	if !strings.Contains(out.String(), "would destroy") {
		t.Errorf("got %q", out.String())
	}
}

func TestReap_ReportsABadFlag(t *testing.T) {
	var out bytes.Buffer
	if err := runReap([]string{"--nope"}, &out, labDeps(t)); ExitCode(err) != ExitUsage {
		t.Fatalf("got %v", err)
	}
}

func TestReap_HelpExplainsWhyUntrackedNodesSurvive(t *testing.T) {
	var out bytes.Buffer
	_ = runReap([]string{"-h"}, &out, labDeps(t))
	if !strings.Contains(out.String(), "another machine") {
		t.Errorf("help should explain the deliberate restraint: %q", out.String())
	}
}

// TestLs_ReportsAMalformedConfig. An absent ember.yaml is fine -- the defaults
// are a valid configuration -- but one that exists and does not parse is not,
// because the operator believes it is in force.
func TestLs_ReportsAMalformedConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ember.yaml")
	if err := os.WriteFile(p, []byte("provider: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runLs([]string{"--config", p}, &out, labDeps(t)); err == nil {
		t.Fatal("a config that does not parse was accepted")
	}
}

// TestLs_ReportsAnUnusableStateDirectory: the journal is how an orphan is
// found, so "I could not open it" has to surface rather than scan as empty.
func TestLs_ReportsAnUnusableStateDirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := dockerengine.NewRecordingEngine()
	e.AddImage(local.DefaultImage)

	var out bytes.Buffer
	if err := runLs(nil, &out, deps{Engine: e, HomeDir: home}); err == nil {
		t.Fatal("a state directory that is a file was accepted")
	}
}

func TestReap_ReportsAnUnusableStateDirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := dockerengine.NewRecordingEngine()
	e.AddImage(local.DefaultImage)

	var out bytes.Buffer
	if err := runReap(nil, &out, deps{Engine: e, HomeDir: home}); err == nil {
		t.Fatal("a state directory that is a file was accepted")
	}
}

// seedLeftoverNetwork creates a bridge network wearing ember's labels with no
// node container beside it: what a destroy that could not find the run id
// leaves on the daemon.
//
// Seeded straight at the engine rather than by running `ember up` and removing
// the container, because what is under test is the reader. deadline is the
// unix second that goes into the label.
func seedLeftoverNetwork(t *testing.T, e *dockerengine.RecordingEngine, runID, deadline string) {
	t.Helper()
	if _, err := e.NetworkCreate(t.Context(), "ember-"+runID, map[string]string{
		dockerengine.LabelRunID: runID,
		local.LabelKind:         "network",
		local.LabelLabel:        "ember-" + runID + "-" + deadline,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestLs_ReportsALeftoverNetworkAndExitsTwo.
//
// Nothing bills for a bridge network, which is why this leak was invisible for
// the life of the lab harness. What it costs is every local run failing at
// once, weeks later, with "all predefined address pools have been fully
// subnetted" -- so it has to be visible on the run that leaves it rather than
// discovered by an exhausted address pool.
func TestLs_ReportsALeftoverNetworkAndExitsTwo(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)
	seedLeftoverNetwork(t, e, "0123456789abcdef", "1000000000")

	var out bytes.Buffer
	err := runLs(nil, &out, d)
	if err == nil {
		t.Fatal("ls exited 0 over a network nothing will ever remove")
	}
	if ExitCode(err) != ExitFindings {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitFindings)
	}
	if !strings.Contains(out.String(), "leftover") {
		t.Errorf("got %q", out.String())
	}
	if !strings.Contains(out.String(), "ember-0123456789abcdef") {
		t.Errorf("the line does not name the network: %q", out.String())
	}
}

// TestLs_SaysNothingAboutALiveRunsNetwork is the create window: local.Create
// makes the network before the container, so for a moment a healthy run looks
// exactly like wreckage. A finding that is sometimes false teaches an operator
// to ignore exit 2.
func TestLs_SaysNothingAboutALiveRunsNetwork(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)
	seedLeftoverNetwork(t, e, "0123456789abcdef", "99999999999")

	var out bytes.Buffer
	if err := runLs(nil, &out, d); err != nil {
		t.Fatalf("ls exited non-zero over a network inside its deadline: %v (%q)", err, out.String())
	}
	if strings.Contains(out.String(), "leftover") {
		t.Errorf("a run inside its deadline was reported as wreckage: %q", out.String())
	}
}

func TestReap_RemovesALeftoverNetwork(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)
	seedLeftoverNetwork(t, e, "0123456789abcdef", "1000000000")

	var out bytes.Buffer
	if err := runReap(nil, &out, d); err != nil {
		t.Fatalf("reap: %v (%q)", err, out.String())
	}
	if n := e.Networks(); len(n) != 0 {
		t.Errorf("reap left %+v", n)
	}
	if !strings.Contains(out.String(), "removed 1 of 1 leftover") {
		t.Errorf("got %q", out.String())
	}
}

// TestReap_DryRunTouchesNothing: the whole point of --dry-run is that it is
// safe, and a sweep added to reap must not be the thing that breaks that.
func TestReap_DryRunLeavesALeftoverAlone(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)
	seedLeftoverNetwork(t, e, "0123456789abcdef", "1000000000")

	var out bytes.Buffer
	if err := runReap([]string{"--dry-run"}, &out, d); err != nil {
		t.Fatalf("reap --dry-run: %v", err)
	}
	if len(e.Networks()) != 1 {
		t.Error("--dry-run removed the network")
	}
	if !strings.Contains(out.String(), "would destroy 0 node(s) and remove 1 leftover(s)") {
		t.Errorf("got %q", out.String())
	}
}

// TestReap_NothingToReapCountsLeftoversToo. "nothing to reap" over a daemon
// holding a stale network would be the same false clean bill of health the
// `--- SKIP` gate exists to prevent, reached from the other side.
func TestReap_SaysNothingToReapOnlyWhenThereIsNothing(t *testing.T) {
	var out bytes.Buffer
	if err := runReap(nil, &out, labDeps(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to reap") {
		t.Errorf("got %q", out.String())
	}
}

// TestReap_DoesNotCountAProviderItCouldNotAskAsALeftover.
//
// A failed leftover listing arrives in the same slice as the leftovers, as a
// KindUnreachable finding -- "I could not ask the daemon what ember left on
// it", which must not render as "nothing was left". Counting those as things
// to remove made reap announce a sweep of a provider it could not reach and
// then report "removed 0 of 1": a number about nothing, on the command whose
// failures are the ones that matter.
func TestReap_DoesNotCountAProviderItCouldNotAskAsALeftover(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)
	// The daemon answers the node listing and refuses the network one, which
	// is why these are two findings rather than one: a provider can fail one
	// and answer the other.
	e.FailOn = "NetworkList"

	var out bytes.Buffer
	err := runReap(nil, &out, d)
	if strings.Contains(out.String(), "leftover(s)") {
		t.Errorf("reap counted an unreachable listing as something to remove:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "nothing to reap") {
		t.Errorf("got %q (%v)", out.String(), err)
	}
}
