//go:build integration && imagelab

// Assertions about the published workload images.
//
// Behind their own tag because they are about artifacts the main CI does not
// build: `make docker-build` produces ember:lab, and the CUDA image takes
// forty minutes to compile. Requiring it on every pull request would make
// every pull request slow; failing without it -- which is what these did at
// first -- makes every pull request red, which is worse, because it teaches
// people to ignore red.
//
// They are not skipped. A lab that skips itself reports success for something
// that never ran, and this repository greps for `--- SKIP` because of it.
// These simply are not compiled unless the tag says the images are here, and
// the workflow that builds an image runs them against it *before pushing* --
// which is a better place than CI anyway: a bad image never ships, rather
// than being noticed afterwards.
//
//   make test-image-labs

package integration

import (
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/assets"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/test/integration/harness"
)

// TestWorkloadImage_ListensOnNothingButSSH is the same assertion about the
// image that actually goes onto a public IP.
//
// ember-sd cannot be built FROM the node image -- that one is debian-slim and
// CUDA needs an NVIDIA base -- so it repeats the sshd configuration by hand,
// on a different base, with a different package manager. Until this existed,
// the duplication was verified by nothing: the platform-port concession was
// justified by a property of the image that never leaves the laptop, while a
// different image was the one RunPod published a port to.
//
// It does not pull. A lab that reached the network would be testing GitHub,
// and one that skipped itself would report success for something that never
// ran -- so a missing image is a hard failure naming the command that builds
// it.
func TestWorkloadImage_ListensOnNothingButSSH(t *testing.T) {
	image, _ := sdCUDAUnderTest(t)
	assertListensOnlyOnSSH(t, image)
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
			t.Errorf("sd-cli does not mention %q; the pinned SD_COMMIT may predate Wan support:\n%s",
				want, help)
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

	image, _ := sdCUDAUnderTest(t)
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

// TestWorkloadImage_UnderstandsTheFetchCommand asserts that the image's
// embernode knows the subcommand ember invokes on it.
//
// This one is written from a failure. The published ember-sd was built from a
// commit that predated `embernode fetch`, and that binary does not reject an
// unknown subcommand -- main() dispatches on os.Args[1] and anything else is
// the node entrypoint, which is the right shape for a process started as PID
// 1 with no arguments at all. So `embernode fetch --dir /models --url ...`
// was read as "be a node", and the node entrypoint failed on the one thing
// missing from an SSH session's environment: EMBER_HOST_KEY, which PID 1
// unsets on purpose so no later exec can read it back out of /proc.
//
// The symptom in production was therefore a host-key error during an asset
// fetch, on a pod whose host key was fine. Nothing was wrong with ember or
// with the image except that they were different ages.
//
// This lab cannot assert that symptom, and the reason is worth keeping. In
// production the fetch arrives as an SSH remote command, and an SSH session's
// environment never held EMBER_HOST_KEY, so the entrypoint fails on the
// missing key. Here it arrives by `docker exec`, which inherits the
// container's *configured* environment rather than PID 1's live one -- so the
// key is there, installHostKey succeeds, and the old binary gets as far as
// failing to bind a second sshd to port 22. Same bug, two symptoms, neither
// of them mentioning a subcommand.
//
// So what is asserted is the one thing that holds either way: a binary that
// dispatches on "fetch" prints that flag set's usage, and a binary that does
// not cannot print it whatever its environment contains.
//
// The flag names come from assets.Command rather than from a literal here, so
// this compares what ember will actually send against what the image will
// actually accept. assets.Command and runFetch living in one package stops
// those two drifting within a build; it cannot stop a build from being older
// than the caller, which is what a published image always might be.
func TestWorkloadImage_UnderstandsTheFetchCommand(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	image, _ := sdCUDAUnderTest(t)
	if !l.HaveImage(ctx, image) {
		t.Fatalf("%s is not built, and this asserts about the embernode in it:\n  make workload-sd-cuda", image)
	}

	mount, err := node.MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	// A plausible asset, never fetched: -h returns before any of these
	// values is used, so the test neither touches the network nor needs the
	// mount to exist.
	cmd, err := assets.Command(mount, node.Asset{
		URL:    "https://example.invalid/weights.gguf",
		SHA256: strings.Repeat("a", 64),
		As:     "weights.gguf",
	}, assets.VerifyAlways)
	if err != nil {
		t.Fatal(err)
	}

	n := mintAndStartImage(t, l, "fetchcmd", image)

	// `|| true` because -h is a non-zero exit by design, and ExecOut treats a
	// non-zero exit as a failed lab.
	out := l.ExecOut(ctx, n.ID, "sh", "-c", cmd.Path+" fetch -h 2>&1 || true")

	// flag.NewFlagSet("embernode fetch", ...) is what prints this, so it
	// appears if and only if main() dispatched on the subcommand.
	if !strings.Contains(out, "Usage of embernode fetch") {
		t.Fatalf("%s did not dispatch on `fetch` and ran the node entrypoint instead, so its "+
			"embernode predates `embernode fetch`; rebuild and republish the image:\n%s", image, out)
	}

	for _, arg := range cmd.Args {
		name, ok := strings.CutPrefix(arg, "--")
		if !ok {
			continue
		}
		if !strings.Contains(out, "-"+name) {
			t.Errorf("ember sends --%s but the embernode in %s does not define it:\n%s", name, image, out)
		}
	}
}

// sdCUDAUnderTest names the per-architecture image this run is about, and the
// architecture it was asked for.
//
// Both from the environment, because both have to come from outside the build.
// EMBER_CUDA_ARCH is what the caller asked for -- a matrix job, or a developer
// typing CUDA_ARCH=75 -- and it is the only leg of the comparison below that
// the build itself cannot influence. A build arg that silently stopped taking
// effect would otherwise look exactly like a correct build.
//
// Missing is a hard failure naming the command, not a default and not a skip.
// A default would make this lab assert about whatever happened to be under the
// tag, and a skip reports success for something that never ran.
func sdCUDAUnderTest(t *testing.T) (image string, arch int) {
	t.Helper()

	image = strings.TrimSpace(os.Getenv("EMBER_SD_CUDA_IMAGE"))
	raw := strings.TrimSpace(os.Getenv("EMBER_CUDA_ARCH"))
	if image == "" || raw == "" {
		t.Fatalf("EMBER_SD_CUDA_IMAGE and EMBER_CUDA_ARCH must both be set: one image per\n"+
			"architecture, so a lab about one has to be told which.\n"+
			"  make test-image-labs-publish CUDA_ARCH=75\n"+
			"got image=%q arch=%q", image, raw)
	}
	arch, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("EMBER_CUDA_ARCH=%q is not a number", raw)
	}
	return image, arch
}

// TestWorkloadImage_CarriesOneArchitectureAndNoPTX.
//
// This replaced a lab that asserted the binary covered every rentable card,
// which a single-architecture image cannot do by construction. That property
// did not disappear -- it moved to internal/node, where every card having an
// architecture and every architecture having a card is a free unit test, and
// to the build matrix, which reads the list from `ember archs` and fails
// naming any architecture that published nothing.
//
// What is left here is what only an image can answer, and it is three
// statements compared against each other:
//
//	EMBER_CUDA_ARCH          what the caller asked for    -- outside the build
//	LABEL ember.cuda-arch    what the build thought        -- baked by it
//	the absence of PTX       what nvcc emitted             -- read from the bytes
//
// The SASS architecture itself is checked inside the build, by cuobjdump,
// where the toolkit exists and the failure costs nothing: see
// workload-sd-cuda.Dockerfile. It cannot be checked here, because the markers
// that used to be greppable were the text of the embedded PTX -- a PTX module
// carries ".target sm_75" in plain characters -- and dropping the PTX took
// them with it. Their absence is now the evidence rather than the obstacle.
func TestWorkloadImage_CarriesOneArchitectureAndNoPTX(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()
	image, want := sdCUDAUnderTest(t)

	if !l.HaveImage(ctx, image) {
		t.Fatalf("%s is not built, and this asserts about the binary in it:\n"+
			"  make workload-sd-cuda CUDA_ARCH=%d", image, want)
	}

	// The label travels with the image into a registry, so a published
	// artifact can say what it is without being run -- which is what makes it
	// worth comparing against the request.
	if got := l.ImageLabel(ctx, image, "ember.cuda-arch"); got != strconv.Itoa(want) {
		t.Errorf("%s is labelled ember.cuda-arch=%q but this run asked for sm_%d.\n"+
			"The label is what a published image says about itself, so a wrong one is a digest "+
			"an operator files under the wrong images: key -- and ember would then run it on a "+
			"card it has no kernel for, which does not fail, it runs on the CPU at the GPU's price.",
			image, got, want)
	}

	// An architecture no card needs is a matrix job, twenty minutes of a
	// runner and a line in every operator's config, for an image nothing will
	// ever run.
	if cards := node.CardsFor(node.ComputeCap(want)); len(cards) == 0 {
		t.Errorf("sm_%d is not the architecture of any card ember can select; it publishes %v",
			want, node.KnownCaps())
	}

	n := mintAndStartImage(t, l, "cudaarch", image)

	// grep rather than `strings`, which the runtime base does not carry. -a
	// treats the binary as text; PTX, when there is any, is text.
	out := l.ExecOut(ctx, n.ID, "sh", "-c",
		"grep -aoE 'compute_[0-9]+|[.]target sm_[0-9]+' /opt/sd/sd-cli | sort -u || true")
	if got := strings.TrimSpace(out); got != "" {
		t.Errorf("/opt/sd/sd-cli carries PTX, and this image ships none:\n%s\n"+
			"PTX JITs forward only, so it can never help an older card; on a newer one it means "+
			"the driver compiles every kernel at first launch, on a cold cache, on every pod, "+
			"unmeasured. Nothing is compiled on a rented machine. "+
			"CMAKE_CUDA_ARCHITECTURES must be \"NN-real\" -- a bare NN emits PTX too.", got)
	}
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
