package app

import (
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
)

// TestOverrides_ReplaceRatherThanNarrow is the money assertion, and it is in
// this package because both callers are.
//
// `ember up --max-hourly-usd 999` RAISES the ceiling, because the operator
// typing it is the operator who set it. queue.Narrow takes the SMALLER of the
// two, because a queued request arrives from somewhere the operator is not.
// Two opposite rules in one program is exactly what gets copied to the wrong
// place, so there is one implementation of each -- and the privileged socket
// route calls this one rather than growing a third.
func TestOverrides_ReplaceRatherThanNarrow(t *testing.T) {
	cfg := config.Config{
		Provider:     "local",
		Image:        "a:v1",
		GPU:          "rtx2080ti",
		MaxHourlyUSD: "0.10",
		TTL:          time.Hour,
	}
	Overrides{
		Provider:     "runpod",
		GPU:          "h100-80g",
		MaxHourlyUSD: "999",
		TTL:          2 * time.Hour,
	}.Apply(&cfg)

	if cfg.MaxHourlyUSD != "999" {
		t.Errorf("max_hourly_usd = %q, want the flag's 999: narrowing it here would make the "+
			"flag mean the opposite of what it means in a shell", cfg.MaxHourlyUSD)
	}
	if cfg.TTL != 2*time.Hour {
		t.Errorf("ttl = %v, want the flag's 2h", cfg.TTL)
	}
	if cfg.Provider != "runpod" || cfg.GPU != "h100-80g" {
		t.Errorf("selection = %s/%s", cfg.Provider, cfg.GPU)
	}
}

// TestOverrides_AnEmptyValueLeavesTheFileAlone.
//
// "The flag was not passed" is what an empty string means, and there is no way
// to blank a configured value from the command line today -- inventing one
// here would be a flag nobody asked for.
func TestOverrides_AnEmptyValueLeavesTheFileAlone(t *testing.T) {
	cfg := config.Config{
		Provider: "local", Image: "a:v1", GPU: "rtx2080ti", MaxHourlyUSD: "0.10",
		TTL: time.Hour,
	}
	before := cfg
	Overrides{}.Apply(&cfg)

	if cfg.Provider != before.Provider || cfg.Image != before.Image || cfg.GPU != before.GPU ||
		cfg.MaxHourlyUSD != before.MaxHourlyUSD || cfg.TTL != before.TTL {
		t.Errorf("a zero Overrides changed the config: %+v -> %+v", before, cfg)
	}
}

// TestOverrides_AnImageTakesTheImagesMapWithIt.
//
// The one non-obvious line. An operator pinning one image for one run is the
// first thing anybody reaches for when a run misbehaves, and leaving the map
// in place would mean -image silently did nothing on every config that has
// one. One field holds the answer, so there is no "which wins" rule at the
// decision site -- and the failure it prevents is a wrong-architecture image,
// which aborts after the node exists, the image is pulled and the weights are
// fetched, all billed.
func TestOverrides_AnImageTakesTheImagesMapWithIt(t *testing.T) {
	cfg := config.Config{Image: "a:v1", Images: map[string]string{"sm_89": "b:v1"}}
	Overrides{Image: "c:v1"}.Apply(&cfg)
	if cfg.Image != "c:v1" {
		t.Errorf("image = %q", cfg.Image)
	}
	if cfg.Images != nil {
		t.Errorf("images: survived an -image override as %v, so the flag would have done nothing "+
			"on a config that has a map", cfg.Images)
	}
}

// TestRunAsk_RefusesWhatOnlyAMistakeCouldMean.
//
// THE ABSOLUTE PATH IS THE ONE THAT MATTERS, and it is the one a reader would
// not think of: `ember serve` runs from its own working directory, not the
// client's, so a relative `ember.yaml` would name a different file -- and that
// file sets the ceilings, the image and the store. The client calls
// filepath.Abs before it sends; this is what catches a hand-written curl.
func TestRunAsk_RefusesWhatOnlyAMistakeCouldMean(t *testing.T) {
	cases := []struct {
		name string
		ask  RunAsk
		want string
	}{
		{"no config", RunAsk{}, "config file"},
		{"a relative path", RunAsk{Config: "ember.yaml"}, "relative"},
		{"a relative path with a dot", RunAsk{Config: "./ember.yaml"}, "relative"},
		{"a negative ttl", RunAsk{Config: "/e.yaml", Overrides: Overrides{TTL: -time.Hour}}, "ttl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := c.ask.Validate()
			if err == nil {
				t.Fatalf("Validate accepted %+v", c.ask)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal should name %q, got %v", c.want, err)
			}
		})
	}

	// And the acceptance beside the refusals, because a Validate that refused
	// everything would satisfy every case above -- the zero-Pin lesson.
	ok := RunAsk{Config: "/home/op/ember.yaml", Overrides: Overrides{TTL: time.Hour}}
	if err := ok.Validate(); err != nil {
		t.Errorf("a well-formed ask was refused: %v", err)
	}
}
