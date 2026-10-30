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
	"strings"
	"testing"

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
