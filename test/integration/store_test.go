//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"regexp"
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

	l.CreateStore(ctx, "models")

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

// TestAssets_VerifiedInPlaceWithoutTouchingTheNetwork.
//
// The asset is seeded onto the store first, so the configured URL is never
// fetched -- and it is deliberately unreachable, which is what proves that.
// What this exercises is the argv `assets.Command` writes being parsed by
// `embernode fetch` on a real node, over a real SSH connection, through real
// shell quoting: a flag renamed on one side and not the other is a node that
// fetches nothing and a workload that finds an empty directory.
func TestAssets_VerifiedInPlaceWithoutTouchingTheNetwork(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	l.WriteConfig(map[string]string{"store": storeYAML})
	l.CreateStore(ctx, "models")
	sum := l.SeedVolume(ctx, "ember-store-models", "sd15.gguf", "these are the weights")

	l.WriteConfig(map[string]string{
		"store":  storeYAML,
		"assets": assetYAML(sum),
		"cmd":    `[/bin/sh, -c, "cat /models/sd15.gguf"]`,
	})

	out := l.Run(ctx, "up")
	if out.ExitCode != 0 {
		t.Fatalf("up exited %d: %s", out.ExitCode, out.Combined())
	}
	if !strings.Contains(out.Stdout, "present and verified") {
		t.Errorf("the asset was not verified in place:\n%s", out.Combined())
	}
	if !strings.Contains(out.Stdout, "these are the weights") {
		t.Errorf("the workload did not read the asset:\n%s", out.Combined())
	}
	// The URL points nowhere. Reaching it would have failed the run, so a
	// clean exit is the proof that nothing was downloaded.
	if strings.Contains(out.Stdout, "fetching") {
		t.Errorf("the asset was already there and was fetched anyway:\n%s", out.Combined())
	}
}

// TestAssets_TamperedWeightsRefuseBeforeTheWorkloadRuns.
//
// The control that stands in for a read-only mount. A store is writable by
// every node that mounts it, so one run can leave something behind that the
// next would load as its model; RunPod offers no way to prevent the write, so
// it is detected -- and detected before any GPU time is spent on it.
func TestAssets_TamperedWeightsRefuseBeforeTheWorkloadRuns(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	l.WriteConfig(map[string]string{"store": storeYAML})
	l.CreateStore(ctx, "models")
	// Seeded with something other than what the config will claim.
	sum := l.SeedVolume(ctx, "ember-store-models", "sd15.gguf", "not the weights at all")
	_ = sum

	l.WriteConfig(map[string]string{
		"store":  storeYAML,
		"assets": assetYAML(harness.DigestOf("these are the weights")),
		"cmd":    `[/bin/sh, -c, "echo THE WORKLOAD RAN"]`,
	})

	out := l.Run(ctx, "up")
	if out.ExitCode == 0 {
		t.Fatalf("a tampered asset was accepted:\n%s", out.Combined())
	}
	if !strings.Contains(out.Combined(), "does not match its digest") {
		t.Errorf("the refusal does not say why:\n%s", out.Combined())
	}
	// The whole point of checking first.
	if strings.Contains(out.Combined(), "THE WORKLOAD RAN") {
		t.Errorf("the workload ran against weights that failed verification:\n%s", out.Combined())
	}
	// And the node still went away.
	if n := len(l.Nodes(ctx)); n != 0 {
		t.Errorf("%d node(s) survived a refused run", n)
	}
}

const storeYAML = "\n  name: models\n  size_gib: 40\n  region: EU-RO-1"

// assetYAML points at a URL nothing answers. Every asset lab here has the
// file on the store already, so a fetch would be a bug -- and one that shows
// up as a failed run rather than as a silently slower one.
func assetYAML(sum string) string {
	return "\n  - url: http://127.0.0.1:9/never\n    sha256: " + sum + "\n    as: sd15.gguf"
}

// TestWorkloadImage_SupportsVideoGeneration.
//
// Not a generation. A real Wan clip is minutes of GPU time and the model is
// gigabytes, so running one is a calibration that costs money rather than a
// lab that runs on every build -- and a lab that pulled a checkpoint would be
// testing Hugging Face.
//
// What is free and worth asserting is that the binary can do it at all. The
// plan for this phase said to check whether the pinned stable-diffusion.cpp
// commit predates Wan support and bump it if so; this is that check, kept as
// a test so the answer cannot quietly stop being true when the pin moves.
//
// Asserted against the CPU image, because the CUDA build cannot run on a
// machine with no GPU -- sd-cli there needs libcuda.so.1 to print its own
// help. The two are pinned to the same upstream commit on purpose, which is
// what the first assertion below checks: without it this would be proving
// something about a different program than the one that goes to RunPod.
func TestWorkloadImage_SupportsVideoGeneration(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	cpuCommit := sdPin(t, "workload-sd.Dockerfile", `SD_RELEASE=master-\d+-([0-9a-f]+)`)
	gpuCommit := sdPin(t, "workload-sd-cuda.Dockerfile", `SD_COMMIT=([0-9a-f]+)`)
	if cpuCommit != gpuCommit {
		t.Fatalf("the CPU image is built from stable-diffusion.cpp %s and the CUDA image from %s. "+
			"They are pinned together so a result from one is comparable with the other, and so a "+
			"capability checked on one is true of the other", cpuCommit, gpuCommit)
	}

	const image = "ember:sd"
	if !l.HaveImage(ctx, image) {
		t.Fatalf("%s is not built, and this asserts about the sd-cli in it:\n"+
			"  make workload-sd MODEL=/path/to/a.gguf", image)
	}

	n := mintAndStartImage(t, l, "vidgen", image)
	help := l.ExecOut(ctx, n.ID, "sd-cli", "--help")

	for _, want := range []string{
		// The mode a Wan model needs. Without it the pin predates video
		// entirely and ember.video.example.yaml is fiction.
		"vid_gen",
		// How long a clip is, and the flag a chained pipeline drives.
		"--video-frames",
		// Wan's own latent format, which is how a Wan checkpoint is decoded
		// rather than read as a Stable Diffusion one.
		"wan",
	} {
		if !strings.Contains(help, want) {
			t.Errorf("sd-cli at %s does not mention %q; the pin may predate Wan support:\n%s",
				cpuCommit, want, help)
		}
	}
}

// TestWorkloadImage_CarriesNoWeights.
//
// The point of the store, asserted rather than trusted: a Dockerfile that
// grew a COPY back would otherwise show up only as a pull that got slower.
func TestWorkloadImage_CarriesNoWeights(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	const image = "ember:sd-cuda"
	if !l.HaveImage(ctx, image) {
		t.Fatalf("%s is not built, and this asserts about that image:\n  make workload-sd-cuda", image)
	}

	n := mintAndStartImage(t, l, "noweights", image)
	out := l.ExecOut(ctx, n.ID, "sh", "-c", "ls /opt/models 2>&1 || true")
	if !strings.Contains(out, "No such file") {
		t.Errorf("the workload image carries weights at /opt/models:\n%s", out)
	}

	// And it still carries the program, so the check above is not passing
	// because the image is empty.
	if got := l.ExecOut(ctx, n.ID, "sh", "-c", "test -e /opt/sd/sd-cli && echo present"); !strings.Contains(got, "present") {
		t.Errorf("sd-cli is missing from the image: %q", got)
	}
}

// sdPin reads the stable-diffusion.cpp revision a Dockerfile pins.
func sdPin(t *testing.T, file, pattern string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", file))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(pattern).FindSubmatch(raw)
	if m == nil {
		t.Fatalf("%s does not pin stable-diffusion.cpp in the expected form (%s)", file, pattern)
	}
	return string(m[1])
}
