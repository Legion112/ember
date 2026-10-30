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
	"path/filepath"
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
	assertListensOnlyOnSSH(t, "ember:sd-cuda")
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

	const image = "ember:sd-cuda"
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

// cudaCard is a GPU ember can select, and what compute capability it has.
//
// The capability is what decides whether the binary can run on it at all, and
// it is not derivable from anything in this repository -- so it is written
// down, with the catalogue entry it came from. Every one of these has appeared
// in `ember offers` against RunPod.
type cudaCard struct {
	name string
	// cc is the compute capability times ten: sm_80 is 80.
	cc int
}

// cardsEmberCanRent is the table this lab checks the image against.
//
// sm_75 cards (2080 Ti and friends) are absent on purpose. They have 11 GiB,
// which cannot hold the model sets this image exists to run, and the CPU
// variant covers working locally. Their absence is a decision recorded in
// workload-sd-cuda.Dockerfile, not an oversight.
var cardsEmberCanRent = []cudaCard{
	{"NVIDIA A100 80GB PCIe", 80},
	{"NVIDIA A100-SXM4-80GB", 80},
	{"NVIDIA RTX A5000 / A10 / RTX 3090", 86},
	{"NVIDIA RTX 4090 / L40S", 89},
	{"NVIDIA H100 80GB HBM3 / H100 NVL", 90},
	{"NVIDIA B200 / B300 SXM6", 100},
	{"NVIDIA RTX PRO 6000 Blackwell", 120},
}

// TestWorkloadImage_CoversTheCardsEmberCanRent reads the architectures out of
// the built binary and checks them against the cards that can be rented.
//
// This exists because the list was wrong and nothing noticed. CUDA_ARCHS was
// "86;89" -- 3090/A10/A5000 and 4090/L40S -- and every card with enough VRAM
// for a large model was outside it. An `ember.yaml` selecting on VRAM rather
// than naming a card therefore picked the cheapest 80 GiB offer, got an A100
// at sm_80, and did not fail: sd-cli fell back to the CPU backend and ran a
// 32-billion-parameter model on 128 threads at the GPU's price.
//
// The markers are read out of the binary rather than taken from the
// Dockerfile, because the Dockerfile is the input and this is about the
// output. They are also compared against the Dockerfile, so the two cannot
// drift apart silently in either direction.
func TestWorkloadImage_CoversTheCardsEmberCanRent(t *testing.T) {
	l := harness.New(t)
	ctx := t.Context()

	const image = "ember:sd-cuda"
	if !l.HaveImage(ctx, image) {
		t.Fatalf("%s is not built, and this asserts about the binary in it:\n  make workload-sd-cuda", image)
	}

	n := mintAndStartImage(t, l, "cudaarch", image)

	// grep rather than `strings`, which the runtime base does not carry. -a
	// treats the binary as text; the markers are plain ASCII inside the
	// fatbin.
	out := l.ExecOut(ctx, n.ID, "sh", "-c",
		"grep -aoE 'sm_[0-9]+|compute_[0-9]+' /opt/sd/sd-cli | sort -u || true")

	native := map[int]bool{}  // sm_NN  -- machine code
	virtual := map[int]bool{} // compute_NN -- PTX, JITs forward only
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "sm_"):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "sm_")); err == nil {
				native[n] = true
			}
		case strings.HasPrefix(line, "compute_"):
			if n, err := strconv.Atoi(strings.TrimPrefix(line, "compute_")); err == nil {
				virtual[n] = true
			}
		}
	}
	if len(native) == 0 {
		t.Fatalf("no sm_* markers in /opt/sd/sd-cli; the binary may not be a CUDA build at all:\n%s", out)
	}

	// What the Dockerfile asked for is what the binary must contain. A build
	// arg that silently stopped taking effect would otherwise look exactly
	// like a correct build.
	for _, want := range dockerfileCUDAArchs(t) {
		if !native[want] {
			t.Errorf("the Dockerfile asks for sm_%d but the binary does not carry it; "+
				"CUDA_ARCHS is not reaching the compiler. Found: %v", want, sortedKeys(native))
		}
	}

	// lowestPTX is what a newer card can JIT from. PTX is forward-compatible
	// only: compute_90 runs on Blackwell and can do nothing for an A100.
	lowestPTX := -1
	for v := range virtual {
		if lowestPTX == -1 || v < lowestPTX {
			lowestPTX = v
		}
	}

	for _, c := range cardsEmberCanRent {
		if native[c.cc] {
			continue
		}
		if lowestPTX != -1 && lowestPTX <= c.cc {
			// Usable, but it pays a JIT compile on every cold start, and
			// nothing here has measured what that costs.
			t.Logf("%s (sm_%d) has no native code and will JIT from compute_%d", c.name, c.cc, lowestPTX)
			continue
		}
		t.Errorf("%s is sm_%d and this image cannot run on it: no native sm_%d and no PTX at or below it "+
			"(native %v, ptx %v).\nA run on this card will not fail -- it will fall back to the CPU backend "+
			"and bill at the GPU's rate. Add %d to CUDA_ARCHS in workload-sd-cuda.Dockerfile.",
			c.name, c.cc, c.cc, sortedKeys(native), sortedKeys(virtual), c.cc)
	}
}

// dockerfileCUDAArchs reads the CUDA_ARCHS default out of the Dockerfile.
//
// Parsed rather than duplicated here: a copy would be a second place to
// forget.
func dockerfileCUDAArchs(t *testing.T) []int {
	t.Helper()

	b, err := os.ReadFile(filepath.Join("..", "..", "workload-sd-cuda.Dockerfile"))
	if err != nil {
		t.Fatalf("read the Dockerfile: %v", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), "ARG CUDA_ARCHS=")
		if !ok {
			continue
		}
		var out []int
		for _, part := range strings.Split(strings.Trim(rest, `"`), ";") {
			n, err := strconv.Atoi(strings.TrimSpace(part))
			if err != nil {
				t.Fatalf("CUDA_ARCHS entry %q is not a number", part)
			}
			out = append(out, n)
		}
		if len(out) == 0 {
			t.Fatal("CUDA_ARCHS is empty")
		}
		return out
	}
	t.Fatal("no ARG CUDA_ARCHS line in workload-sd-cuda.Dockerfile")
	return nil
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}
