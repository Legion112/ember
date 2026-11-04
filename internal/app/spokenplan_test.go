package app

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
)

// spokenConfig is a speech run on the local provider, which is free.
func spokenConfig() config.Config {
	return config.Config{
		Provider: "local",
		Image:    "ghcr.io/example/tts@sha256:" + strings.Repeat("a", 64),
		TTL:      time.Hour,
		TTS: config.TTSConfig{Jobs: []config.TTSJob{
			{Text: "Hello from ember.", Output: "hello"},
		}},
	}
}

func TestNewPlan_ASpeechBatchTravelsBesideTheSpecAndNotOnIt(t *testing.T) {
	plan, err := NewPlan(t.Context(), Request{Config: spokenConfig(), Deps: planDeps(t)}, nil)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if len(plan.Spoken.Jobs) != 1 {
		t.Fatalf("plan carries %d utterance(s), want 1", len(plan.Spoken.Jobs))
	}
	// The defaults the image bakes in, resolved once by the config rather than
	// by the driver, so a dry run prints what will actually be sent.
	if plan.Spoken.Voice != config.DefaultVoice {
		t.Errorf("voice = %q, want %q", plan.Spoken.Voice, config.DefaultVoice)
	}
	// AND NOTHING ON THE SPEC, which is the seal rather than a detail: a spec
	// field is a thing a request could one day be consulted about, and
	// internal/node imports nothing else in this module.
	if plan.Spec.Workload() != "" {
		t.Errorf("the spec names a workload (%q) for a speech run", plan.Spec.Workload())
	}
	if len(plan.Spec.Serve) != 0 || len(plan.Spec.Cmds) != 0 {
		t.Errorf("the spec carries serve=%v cmds=%v", plan.Spec.Serve, plan.Spec.Cmds)
	}
	// And the dispatch finds it.
	b := plan.batchOf()
	if b == nil || !strings.HasPrefix(b.Describe(), "tts") {
		t.Error("batchOf did not choose the speech batch")
	}
}

// TestNewPlan_RefusesTwoWorkloadKinds.
//
// node.NewSpec already refuses any two of `cmd`, `cmds` and `sd.serve`, and
// CANNOT see this one, because Spec has no speech field. Without the refusal a
// layer up, Plan.serve reaches `len(p.Spec.Cmds) > 0` first and runs the
// commands -- leaving an operator who wrote both with a rental that said
// nothing and no line anywhere saying why.
func TestNewPlan_RefusesTwoWorkloadKinds(t *testing.T) {
	cases := []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{"with cmd", func(c *config.Config) { c.Cmd = []string{"true"} }, "cmd or cmds"},
		{"with cmds", func(c *config.Config) { c.Cmds = [][]string{{"true"}} }, "cmd or cmds"},
		{"with sd.serve", func(c *config.Config) {
			c.SD = config.SDConfig{
				Serve: []string{"sd-server", "--model", "/m.gguf"},
				Jobs:  []config.SDJob{{Prompt: "a cat"}},
			}
		}, "sd.serve"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			cfg := spokenConfig()
			c.edit(&cfg)
			_, err := NewPlan(t.Context(), Request{Config: cfg, Deps: planDeps(t)}, nil)
			if err == nil {
				t.Fatal("two workload kinds were accepted")
			}
			if !strings.Contains(err.Error(), "tts.jobs") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("error = %q, want it to name tts.jobs and %s", err, c.want)
			}
			// A usage error, so `ember up` exits 3 rather than 1: a config
			// with two answers is a thing the operator typed.
			if !errors.Is(err, ErrUsage) {
				t.Errorf("error is not a usage error: %v", err)
			}
		})
	}
}

// WHAT IS NOT ASSERTED HERE, AND WHY, SO IT IS NOT READ AS AN OMISSION. A
// CPU-only run reads no speech batch -- it shares the `req.CPUOnly` arm with
// `sd.jobs`, which parses nothing -- and there is no test of it, because the
// local provider has no CPU-only offer: `planDeps` drives a recording Docker
// engine and `Filter.CPUOnly` is exclusive in both directions, so a CPU-only
// NewPlan on this provider refuses at PickOffer with "local has nothing
// matching the filter" before it reaches any of this. There is no such test
// anywhere in the tree for the same reason -- `grep -rn "CPUOnly: true"` over
// the test files reaches only internal/provider/runpod -- so this is a bound
// of the local provider rather than a gap in this change, and the arm adds no
// statement of its own.
