package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/provider/local"
)

func labDeps(t *testing.T) deps {
	t.Helper()
	e := dockerengine.NewRecordingEngine()
	e.AddImage(local.DefaultImage)
	// Credentials are injected empty rather than left to be resolved. Without
	// this, every test here reads RUNPOD_API_KEY from the developer's own
	// environment: a machine that has one would register the real provider,
	// and the tests would make live API calls and behave differently from
	// one machine to the next.
	return deps{
		Engine: e, HomeDir: t.TempDir(), Creds: &config.Credentials{},
		// And the graphics card, for the same reason as the credentials
		// above: without this, a machine with a GPU registers a local offer
		// carrying it, and assertions here mean something different from one
		// developer's desk to the next.
		DetectGPU: func(context.Context) local.GPU { return local.GPU{} },
	}
}

func TestUp_RefusesWithoutAnImage(t *testing.T) {
	var out bytes.Buffer
	err := runUp([]string{"--provider", "local"}, &out, labDeps(t))
	if err == nil {
		t.Fatal("up ran with no image")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("a missing image is a usage error, got exit %d", ExitCode(err))
	}
	if !strings.Contains(err.Error(), "-image") {
		t.Errorf("the error should name the flag, got %v", err)
	}
}

// TestUp_RefusesAnUnpinnedImage before anything is created, because the run
// would not be reproducible and the measurement would be worthless.
func TestUp_RefusesAnUnpinnedImage(t *testing.T) {
	var out bytes.Buffer
	err := runUp([]string{"--image", "comfyui:latest"}, &out, labDeps(t))
	if err == nil {
		t.Fatal(":latest was accepted")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit = %d, want usage", ExitCode(err))
	}
}

func TestUp_RefusesAnUnknownProvider(t *testing.T) {
	var out bytes.Buffer
	err := runUp([]string{"--provider", "runpod", "--image", "x:v1"}, &out, labDeps(t))
	if err == nil {
		t.Fatal("an unconfigured provider was accepted")
	}
	// The message lists what is configured, so a typo is visible.
	if !strings.Contains(err.Error(), "local") {
		t.Errorf("got %v", err)
	}
}

func TestUp_RefusesAMalformedPort(t *testing.T) {
	var out bytes.Buffer
	if err := runUp([]string{"--image", "x:v1", "--port", "nonsense"}, &out, labDeps(t)); err == nil {
		t.Fatal("a malformed port spec was accepted")
	}
}

// TestUp_DryRunCreatesNothing is the cheapest safety net in the program.
func TestUp_DryRunCreatesNothing(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	var out bytes.Buffer
	if err := runUp([]string{"--image", "x:v1", "--port", "p:8188", "--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if len(e.Containers()) != 0 || len(e.Networks()) != 0 {
		t.Fatal("a dry run created something")
	}
	for _, want := range []string{"would rent", "nothing was created"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("a dry run must say what it would do; missing %q in %q", want, out.String())
		}
	}
}

// TestUp_DryRunStillEnforcesTheBudget: the ceiling is checked before anything
// is created, which is the only place it can be useful.
func TestUp_DryRunStillEnforcesTheBudget(t *testing.T) {
	d := labDeps(t)
	cfg := filepath.Join(t.TempDir(), "ember.yaml")
	// The local provider is free, so a zero-cost offer must pass any ceiling;
	// an impossible GPU filter is what makes this fail for the right reason.
	if err := os.WriteFile(cfg, []byte("gpu: h100\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err := runUp([]string{"--image", "x:v1", "--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		t.Fatal("a dry run claimed it could rent an H100 from the local daemon")
	}
}

func TestUp_ReadsTheConfigFile(t *testing.T) {
	d := labDeps(t)
	cfg := filepath.Join(t.TempDir(), "ember.yaml")
	body := "image: ghcr.io/example/wl:v2\nports:\n  - demo:8188\nttl: 15m\n"
	if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ghcr.io/example/wl:v2") {
		t.Errorf("the config's image was not used: %q", out.String())
	}
	if !strings.Contains(out.String(), "15m") {
		t.Errorf("the config's ttl was not used: %q", out.String())
	}
}

// TestUp_FlagsBeatTheConfigFile, so a bad value in a committed file can be
// overridden without editing it.
func TestUp_FlagsBeatTheConfigFile(t *testing.T) {
	d := labDeps(t)
	cfg := filepath.Join(t.TempDir(), "ember.yaml")
	if err := os.WriteFile(cfg, []byte("image: ghcr.io/example/old:v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runUp([]string{"--config", cfg, "--image", "ghcr.io/example/new:v9", "--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "new:v9") {
		t.Errorf("the flag did not win: %q", out.String())
	}
}

func TestUp_ReportsAMalformedConfig(t *testing.T) {
	d := labDeps(t)
	cfg := filepath.Join(t.TempDir(), "ember.yaml")
	if err := os.WriteFile(cfg, []byte("ttl: [not a duration]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runUp([]string{"--config", cfg, "--image", "x:v1"}, &out, d); err == nil {
		t.Fatal("a malformed config was ignored")
	}
}

func TestUp_ReportsABadFlag(t *testing.T) {
	var out bytes.Buffer
	err := runUp([]string{"--nonsense"}, &out, labDeps(t))
	if ExitCode(err) != ExitUsage {
		t.Fatalf("a bad flag should be a usage error, got %v", err)
	}
}

func TestUp_HelpExplainsTheLoopbackGuarantee(t *testing.T) {
	var out bytes.Buffer
	_ = runUp([]string{"-h"}, &out, labDeps(t))
	got := out.String()
	for _, want := range []string{"127.0.0.1", "SSH", "destroys"} {
		if !strings.Contains(got, want) {
			t.Errorf("help should mention %q:\n%s", want, got)
		}
	}
}

func TestUp_RefusesABadBudget(t *testing.T) {
	d := labDeps(t)
	var out bytes.Buffer
	err := runUp([]string{"--image", "x:v1", "--max-hourly-usd", "cheap"}, &out, d)
	if err == nil {
		t.Fatal("a malformed ceiling was accepted")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit = %d, want usage", ExitCode(err))
	}
}

func TestMultiFlag(t *testing.T) {
	var m multiFlag
	if err := m.Set("a:1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Set("b:2"); err != nil {
		t.Fatal(err)
	}
	if len(m) != 2 || !strings.Contains(m.String(), "a:1") {
		t.Errorf("multiFlag = %v / %q", []string(m), m.String())
	}
}

// labDepsWithCard is labDeps plus a graphics card the daemon can pass through.
//
// Both, because local.Offers needs both before it advertises a card and they
// fail independently -- the machine this was developed on had the card and not
// the runtime. Declared by the test rather than detected, for the reason
// labDeps pins DetectGPU at all: a test whose result depends on the hardware
// under it is not a test.
func labDepsWithCard(t *testing.T, g local.GPU) deps {
	t.Helper()
	d := labDeps(t)
	d.DetectGPU = func(context.Context) local.GPU { return g }
	d.Engine.(*dockerengine.RecordingEngine).GPURuntime = true
	return d
}

// a2080Ti is the card in the machine this repository is developed on.
var a2080Ti = local.GPU{Model: "rtx2080ti", VRAM: 11264, ComputeCap: "7.5"}

func imagesCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ember.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUp_PicksTheImageForTheCardItWouldRent.
//
// Asserted both ways: that the right reference is printed and that the wrong
// one is not. Only checking for the right one would pass against a resolver
// that printed the whole map.
func TestUp_PicksTheImageForTheCardItWouldRent(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	cfg := imagesCfg(t, "images:\n  sm_75: ember:lab-sm75\n  sm_90: ember:lab-sm90\n")

	var out bytes.Buffer
	if err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ember:lab-sm75") {
		t.Errorf("the sm_75 image was not chosen for an sm_75 card: %q", out.String())
	}
	if strings.Contains(out.String(), "ember:lab-sm90") {
		t.Errorf("an image for another architecture was named: %q", out.String())
	}
	// And it says which key answered, because a digest on its own tells an
	// operator nothing about why it was chosen.
	if !strings.Contains(out.String(), "images.sm_75") {
		t.Errorf("the dry run does not say which key it resolved from: %q", out.String())
	}
}

// TestUp_RefusesWhenNoImageCoversTheCard, before anything is rented.
func TestUp_RefusesWhenNoImageCoversTheCard(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	e := d.Engine.(*dockerengine.RecordingEngine)
	cfg := imagesCfg(t, "images:\n  sm_90: ember:lab-sm90\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg}, &out, d)
	if err == nil {
		t.Fatal("a card with no image was accepted")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("a config that covers no card is a usage error, got exit %d", ExitCode(err))
	}
	// The remediation needs all three: which architecture, which card, and
	// that the map is where the answer goes. "no image" alone sends the
	// operator to look at their image: line, which is not where it is.
	for _, want := range []string{"sm_75", "rtx2080ti", "images:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
	if len(e.Containers()) != 0 || len(e.Networks()) != 0 {
		t.Error("it created something before refusing")
	}
}

// TestUp_NeverFallsBackToTheScalarImage is the most important test here.
//
// The fallback it forbids is not a crash, which is why it has to be forbidden
// explicitly: an image built for another architecture finds no usable kernel,
// drops to the CPU backend, and bills a GPU to run there. It looks like a
// slow success, and the only cheap moment to catch it is before the rental.
func TestUp_NeverFallsBackToTheScalarImage(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	cfg := imagesCfg(t, "image: ember:lab\nimages:\n  sm_90: ember:lab-sm90\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		t.Fatalf("it fell back to image: for a card the map does not cover:\n%s", out.String())
	}
	if strings.Contains(out.String(), "would rent") {
		t.Errorf("it got as far as naming an offer: %q", out.String())
	}
}

// TestUp_RefusesACardItCannotIdentify.
//
// NormalizeGPU never rejects an unknown card, so the offer exists and the
// refusal has to come from the image choice. A guess here would rent.
func TestUp_RefusesACardItCannotIdentify(t *testing.T) {
	d := labDepsWithCard(t, local.GPU{Model: "acmehypercard9000", VRAM: 24576})
	cfg := imagesCfg(t, "images:\n  sm_90: ember:lab-sm90\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		t.Fatal("a card ember cannot identify was accepted")
	}
	if !strings.Contains(err.Error(), "acmehypercard9000") {
		t.Errorf("the refusal does not name the card: %v", err)
	}
}

// TestUp_RefusesACardWithNoCUDACapability. RunPod really lists an MI300X, and
// the remediation is image: rather than a table entry.
func TestUp_RefusesACardWithNoCUDACapability(t *testing.T) {
	d := labDepsWithCard(t, local.GPU{Model: "mi300x", VRAM: 196608})
	cfg := imagesCfg(t, "images:\n  sm_90: ember:lab-sm90\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		t.Fatal("a non-CUDA card was accepted")
	}
	if !strings.Contains(err.Error(), "not a CUDA card") {
		t.Errorf("the refusal should say the card is not CUDA, got %v", err)
	}
}

// TestUp_RefusesAnImagesMapOnAMachineWithNoCard.
//
// The most common configuration in CI, and the one where "unknown card" would
// be the wrong thing to say: the local provider's no-card offer claims one GPU
// of model "none", so anything that only asks IsCPU() thinks it has a card.
func TestUp_RefusesAnImagesMapOnAMachineWithNoCard(t *testing.T) {
	d := labDeps(t) // no card, no runtime
	cfg := imagesCfg(t, "images:\n  sm_90: ember:lab-sm90\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		t.Fatal("an images: map was resolved on a machine with no GPU")
	}
	if !strings.Contains(err.Error(), "no GPU") {
		t.Errorf("the refusal should say there is no card, not that one is unknown: %v", err)
	}
	// And with image: set as well, the same run is fine: there is nothing to
	// choose by, so the scalar is the answer.
	cfg2 := imagesCfg(t, "image: ember:lab\nimages:\n  sm_90: ember:lab-sm90\n")
	out.Reset()
	if err := runUp([]string{"--config", cfg2, "--dry-run"}, &out, d); err != nil {
		t.Fatalf("a no-card run with image: set still failed: %v", err)
	}
	if !strings.Contains(out.String(), "ember:lab") {
		t.Errorf("the scalar image was not used: %q", out.String())
	}
}

// TestUp_TheImageFlagClearsTheMap, so pinning one image for one run works on
// a config that has a map. Otherwise -image would silently do nothing.
func TestUp_TheImageFlagClearsTheMap(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	cfg := imagesCfg(t, "images:\n  sm_90: ember:lab-sm90\n")

	var out bytes.Buffer
	if err := runUp([]string{"--config", cfg, "--image", "ember:lab-pinned", "--dry-run"}, &out, d); err != nil {
		t.Fatalf("-image did not override the map: %v", err)
	}
	if !strings.Contains(out.String(), "ember:lab-pinned") {
		t.Errorf("the flag's image was not used: %q", out.String())
	}
}

// TestUp_RefusesABadImagesEntryEvenForAnotherCard.
//
// Validated as a set. A config with a bad digest under a card this run will
// not rent would otherwise sit committed until the day that card is cheapest.
func TestUp_RefusesABadImagesEntryEvenForAnotherCard(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	// sm_75 is what this machine would rent and it is fine. sm_90 has no tag
	// and no digest, so it is not a pin.
	cfg := imagesCfg(t, "images:\n  sm_75: ember:lab-sm75\n  sm_90: ghcr.io/example/ember-sd\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg, "--dry-run"}, &out, d)
	if err == nil {
		t.Fatal("a bad entry under an architecture this run does not need was accepted")
	}
	if !strings.Contains(err.Error(), "images[sm_90]") {
		t.Errorf("the refusal should name the entry, got %v", err)
	}
}
