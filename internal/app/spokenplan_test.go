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
	if plan.Spoken.Jobs[0].Voice != config.DefaultVoice {
		t.Errorf("voice = %q, want %q", plan.Spoken.Jobs[0].Voice, config.DefaultVoice)
	}
	// AND NOTHING ON THE SPEC, which is the seal rather than a detail: a spec
	// field is a thing a request could one day be consulted about, and
	// internal/node imports nothing else in this module.
	if plan.Spec.Workload() != "" {
		t.Errorf("the spec names a workload (%q) for a speech run", plan.Spec.Workload())
	}
	if len(plan.Spec.Serve) != 0 {
		t.Errorf("the spec carries serve=%v for a speech run", plan.Spec.Serve)
	}
	// And the dispatch finds it.
	b := plan.batchOf()
	if b == nil || !strings.HasPrefix(b.Describe(), "tts") {
		t.Error("batchOf did not choose the speech batch")
	}
}

// TestNewPlan_RefusesTwoWorkloadKinds.
//
// node.NewSpec CANNOT see this one, because Spec has no speech field. Without
// the refusal a layer up, `Plan.batchOf` would pick one of the two and the
// other would be silently dropped -- leaving an operator who wrote both with
// a rental that generated the wrong thing and no line saying why.
//
// THE ACCEPTANCE IS ASSERTED BESIDE IT, because a NewPlan that refused every
// speech configuration would satisfy the refusal on its own -- and a speech
// config with no `sd:` block is the shape every tts run has.
func TestNewPlan_RefusesTwoWorkloadKinds(t *testing.T) {
	cfg := spokenConfig()
	cfg.SD = config.SDConfig{
		Serve: []string{"sd-server", "--model", "/m.gguf"},
		Jobs:  []config.SDJob{{Prompt: "a cat"}},
	}
	_, err := NewPlan(t.Context(), Request{Config: cfg, Deps: planDeps(t)}, nil)
	if err == nil {
		t.Fatal("two workload kinds were accepted")
	}
	if !strings.Contains(err.Error(), "tts.jobs") || !strings.Contains(err.Error(), "sd.serve") {
		t.Errorf("error = %q, want it to name tts.jobs and sd.serve", err)
	}
	// A usage error, so `ember up` exits 3 rather than 1: a config with two
	// answers is a thing the operator typed.
	if !errors.Is(err, ErrUsage) {
		t.Errorf("error is not a usage error: %v", err)
	}

	if _, err := NewPlan(t.Context(), Request{Config: spokenConfig(), Deps: planDeps(t)}, nil); err != nil {
		t.Fatalf("a speech configuration naming one kind was refused: %v", err)
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
