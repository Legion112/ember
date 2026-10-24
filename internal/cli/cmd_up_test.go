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

func labDeps(t *testing.T) deps {
	t.Helper()
	e := dockerengine.NewRecordingEngine()
	e.AddImage(local.DefaultImage)
	return deps{Engine: e, HomeDir: t.TempDir()}
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
