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

	// NO JOBS, which is the whole of what this lab needs from the run: a
	// configuration naming none rents, proves the card, brings `outputs:`
	// home and destroys the node. The store is not mounted here on purpose --
	// what is under test is that a run does not take a store with it.
	l.WriteConfig(nil)
	out := l.Run(ctx, "up")
	if out.ExitCode != 0 {
		t.Fatalf("up exited %d: %s", out.ExitCode, out.Combined())
	}

	// The node is gone -- the lab's cleanup covers that, by snapshotting the
	// daemon before it reaps and reporting the snapshot -- and the store is
	// not.
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
// carries: something writes to the mount, the node is destroyed, and what it
// wrote is still there for the next run.
//
// EMBER IS WHAT WRITES IT, and that is the only shape there is. The writer is
// `embernode fetch` on the first node, downloading the configured asset onto
// the mount; the second run is a different node and finds it already there.
// `assetServer.hits()` is what tells the two apart -- one fetch for two runs
// is the file surviving the machine, and two would mean the second node found
// an empty mount and quietly re-downloaded, which is the defect this lab is
// against and the one a `present and verified` line alone cannot see.
func TestStore_CreateMountRunDestroyRemove(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	// Reachable from inside a node container, which loopback is not: the
	// address is the Docker bridge's own gateway, read from the daemon.
	assets := newAssetServer(t, l.BridgeGateway(ctx), storeAsset)

	// `verify` is left at its default of `always`, so the second run re-reads
	// and re-hashes what the first one put there. That is the stronger check
	// of the two: `present` would answer from the file's existence alone, and
	// what this lab is about is the BYTES surviving the machine.
	cfg := map[string]string{
		"store":  storeYAML,
		"assets": servedAssetYAML(assets.URL, harness.DigestOf(storeAsset)),
	}
	l.WriteConfig(cfg)
	l.CreateStore(ctx, "models")

	// First run: the mount is empty, so embernode fetches onto it.
	first := l.Run(ctx, "up")
	if first.ExitCode != 0 {
		t.Fatalf("first up exited %d: %s", first.ExitCode, first.Combined())
	}
	if assets.hits() != 1 {
		t.Fatalf("the first run made %d request(s) for the asset, want exactly 1", assets.hits())
	}

	// The node is gone. The store is not.
	if vs := l.Volumes(ctx); len(vs) != 1 {
		t.Fatalf("after the run the store is %v", names(vs))
	}

	// Second run, a different node, finds the first one's file already
	// there. This is the whole point: weights survive the machine.
	l.WriteConfig(cfg)
	second := l.Run(ctx, "up")
	if second.ExitCode != 0 {
		t.Fatalf("second up exited %d: %s", second.ExitCode, second.Combined())
	}
	if !strings.Contains(second.Stdout, "present and verified") {
		t.Errorf("the second node did not find the first one's file:\n%s", second.Combined())
	}
	if assets.hits() != 1 {
		t.Errorf("the second run fetched the asset again (%d request(s) in total), so the "+
			"mount did not carry it across the two nodes", assets.hits())
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
	})

	out := l.Run(ctx, "up")
	if out.ExitCode != 0 {
		t.Fatalf("up exited %d: %s", out.ExitCode, out.Combined())
	}
	if !strings.Contains(out.Stdout, "present and verified") {
		t.Errorf("the asset was not verified in place:\n%s", out.Combined())
	}
	// AND THE RUN GOT PAST IT, which is the half the sentence above cannot
	// give: `reportMemory` is the step immediately after EnsureAssets, so its
	// line is the first thing a run prints once the weights are accepted.
	// Without it, "present and verified" would also be what a run that
	// verified and then died printed -- and the tampered lab below asserts
	// this line's ABSENCE for exactly that reason.
	if !strings.Contains(out.Stdout, "node memory:") {
		t.Errorf("the run did not get past the asset verify:\n%s", out.Combined())
	}
	// The URL points nowhere. Reaching it would have failed the run, so a
	// clean exit is the proof that nothing was downloaded.
	if strings.Contains(out.Stdout, "fetching") {
		t.Errorf("the asset was already there and was fetched anyway:\n%s", out.Combined())
	}
}

// TestAssets_TamperedWeightsRefuseBeforeAnythingElseRuns.
//
// The control that stands in for a read-only mount. A store is writable by
// every node that mounts it, so one run can leave something behind that the
// next would load as its model; RunPod offers no way to prevent the write, so
// it is detected -- and detected before any GPU time is spent on it.
func TestAssets_TamperedWeightsRefuseBeforeAnythingElseRuns(t *testing.T) {
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
	})

	out := l.Run(ctx, "up")
	if out.ExitCode == 0 {
		t.Fatalf("a tampered asset was accepted:\n%s", out.Combined())
	}
	if !strings.Contains(out.Combined(), "does not match its digest") {
		t.Errorf("the refusal does not say why:\n%s", out.Combined())
	}
	// THE WHOLE POINT OF CHECKING FIRST, and `node memory:` is the marker
	// because it is the step immediately after EnsureAssets in `Plan.ready`:
	// a run that reached it is a run that accepted the weights. The lab above
	// asserts the same line's PRESENCE over a verify that passed, so the two
	// discriminate rather than both resting on an exit code.
	if strings.Contains(out.Combined(), "node memory:") {
		t.Errorf("the run carried on past weights that failed verification:\n%s", out.Combined())
	}
	// And the node still went away.
	if n := len(l.Nodes(ctx)); n != 0 {
		t.Errorf("%d node(s) survived a refused run", n)
	}
}

const storeYAML = "\n  name: models\n  size_gib: 40\n  region: EU-RO-1"

// servedAssetYAML points at a URL something does answer, for the one lab that
// needs ember to be the thing that writes to the mount.
func servedAssetYAML(url, sum string) string {
	return "\n  - url: " + url + "\n    sha256: " + sum + "\n    as: " + storeAssetName
}

// assetYAML points at a URL nothing answers. Every asset lab here has the
// file on the store already, so a fetch would be a bug -- and one that shows
// up as a failed run rather than as a silently slower one.
func assetYAML(sum string) string {
	return "\n  - url: http://127.0.0.1:9/never\n    sha256: " + sum + "\n    as: sd15.gguf"
}

// TestWorkloadImages_PinTheSameUpstreamRevision.
//
// The CPU and CUDA images are built from the same stable-diffusion.cpp
// revision on purpose, so a result from one is comparable with the other --
// and so a capability checked on one is true of the other. The image labs
// rely on that: the CUDA binary cannot run on a machine with no GPU, so its
// video support is verified through the CPU build.
//
// No image needed, which is why this one runs everywhere.
func TestWorkloadImages_PinTheSameUpstreamRevision(t *testing.T) {
	cpu := sdPin(t, "workload-sd.Dockerfile", `SD_RELEASE=master-\d+-([0-9a-f]+)`)
	gpu := sdPin(t, "workload-sd-cuda.Dockerfile", `SD_COMMIT=([0-9a-f]+)`)
	if cpu != gpu {
		t.Fatalf("the CPU image is built from stable-diffusion.cpp %s and the CUDA image from %s. "+
			"They are pinned together so a result from one is comparable with the other", cpu, gpu)
	}
}

// TestWorkloadImage_ShipsTheServerItAlreadyCompiles is a Dockerfile-shape
// assertion, so it is free and runs on every pull request.
//
// The resident-model path needs sd-server on the node, and the build has
// always produced it -- examples/CMakeLists.txt adds the server subdirectory
// unconditionally at the pinned revision -- while the image copied only
// sd-cli out. The thing that can regress is the copy, not the compile, and a
// regression there has no symptom until a node is rented and answers
// "sd-server: not found" over SSH.
//
// Checked as text rather than against a built image because the CUDA binary
// needs libcuda.so.1 to do anything at all, so there is no runner without a
// GPU on which starting it proves something -- see the deferred lab in
// AGENTS.md.
func TestWorkloadImage_ShipsTheServerItAlreadyCompiles(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "workload-sd-cuda.Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)

	for _, want := range []string{
		// copied out of the build tree
		"-name 'sd-server'",
		// and reachable by name over a non-interactive SSH session
		"/usr/local/bin/sd-server",
		// and the copy is checked, because find -exec cp succeeds when it
		// matches nothing
		"test -x /out/sd-server",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("workload-sd-cuda.Dockerfile is missing %q, so the published image "+
				"would carry no sd-server and every served run would fail after renting a node", want)
		}
	}

	// Braced, or a cmake failure falls into the guard and blames the copy for
	// a compile error at the end of a forty-minute build.
	if !strings.Contains(body, "{ test -x /out/sd-server ||") {
		t.Error("the sd-server guard is not braced; an unbraced `A && test || { exit }` " +
			"reports the wrong failure when the build itself fails")
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
