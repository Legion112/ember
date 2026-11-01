package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/dockerengine"
)

// The dry run is where an operator checks a served config before paying for
// it, so it has to show what ember added as clearly as what they wrote.
func TestUp_DryRunShowsTheServedShape(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	cfg := imagesCfg(t, `
image: ember:lab
sd:
  serve: [sd-server, --diffusion-model, /models/x.gguf]
  jobs:
    - prompt: a lovely orange cat
      width: 512
      height: 512
      steps: 20
      seed: 7
      output: cat
    - prompt: a dog
`)
	var out bytes.Buffer
	if err := runUp([]string{"--provider", "local", "--config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{
		"would serve: sd-server --diffusion-model /models/x.gguf",
		// ember's, not the config's, and visible for that reason.
		"--listen-ip 127.0.0.1",
		`job 1/2: "a lovely orange cat" 512x512 20 steps seed 7`,
		`job 2/2: "a dog"`,
		"previews proj every 5 step(s)",
		"nothing was created",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// EMBER OWNS THE LISTEN ADDRESS, and the refusal happens before anything is
// rented. A serve command choosing 0.0.0.0 would publish an unauthenticated
// generator, with arbitrary file paths in its request body, on a rented
// machine's public interface.
func TestUp_RefusesAServeCommandThatChoosesItsOwnAddress(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	e := d.Engine.(*dockerengine.RecordingEngine)
	cfg := imagesCfg(t, `
image: ember:lab
sd:
  serve: [sd-server, --listen-ip, 0.0.0.0]
  jobs:
    - prompt: a cat
`)
	var out bytes.Buffer
	err := runUp([]string{"--provider", "local", "--config", cfg}, &out, d)
	if err == nil {
		t.Fatal("up accepted a serve command that chose its own listen address")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit = %d, want %d for a config error", ExitCode(err), ExitUsage)
	}
	if !strings.Contains(err.Error(), "public interface") {
		t.Errorf("err = %v, want it to say why", err)
	}
	if len(e.Containers()) != 0 {
		t.Error("something was created before the refusal")
	}
}

// Half a served run is refused before anything is rented, in both directions.
func TestUp_RefusesHalfAServedRunBeforeRenting(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"jobs with no server", "image: ember:lab\nsd:\n  jobs:\n    - prompt: a cat\n",
			"no server to send them to"},
		{"a server with no jobs", "image: ember:lab\nsd:\n  serve: [sd-server]\n",
			"generate nothing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := labDepsWithCard(t, a2080Ti)
			e := d.Engine.(*dockerengine.RecordingEngine)
			var out bytes.Buffer
			err := runUp([]string{"--provider", "local", "--config", imagesCfg(t, tc.body)}, &out, d)
			if err == nil {
				t.Fatal("want a refusal")
			}
			if ExitCode(err) != ExitUsage {
				t.Errorf("exit = %d, want %d", ExitCode(err), ExitUsage)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %q", err, tc.want)
			}
			if len(e.Containers()) != 0 {
				t.Error("something was created before the refusal")
			}
		})
	}
}

// `ember store sync` shares a config with the runs that generate, and a CPU
// box has no card to hold weights resident for. NewSpec refuses the
// combination, so buildSpec has to drop the server the way it drops the probe
// -- otherwise a config with an sd: block cannot sync its own weights.
func TestStoreSync_DropsTheServerOnACPUOnlyRun(t *testing.T) {
	d := labDepsWithCard(t, a2080Ti)
	cfg := imagesCfg(t, `
image: ember:lab
store:
  name: models
  mount: /models
assets:
  - url: https://example.invalid/w.gguf
    sha256: `+strings.Repeat("a", 64)+`
sd:
  serve: [sd-server, --diffusion-model, /models/w.gguf]
  jobs:
    - prompt: a cat
`)
	var out bytes.Buffer
	// It will fail for want of a store on this daemon, but it must not fail
	// because a server was configured.
	err := runStore([]string{"sync", "--config", cfg, "--dry-run"}, &out, d)
	if err != nil && strings.Contains(err.Error(), "no GPU") {
		t.Fatalf("store sync refused because of the sd: block: %v", err)
	}
	if strings.Contains(out.String(), "would serve") {
		t.Errorf("store sync would have started a server:\n%s", out.String())
	}
}
