package webapi

import (
	"testing"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/tts"
)

// TestConfig_AConfigThatCanRunNothingOffersNoKinds is the guard that keeps
// servableKinds from quietly forgetting a kind.
//
// The filter is a `switch` over `genstore.Kinds()`, which that function's own
// comment warns is where the kind added last goes missing -- and its
// fallthrough is deliberately in the safe direction, so a kind with no case
// stays OFFERED. That is the right default (it is what every kind did before
// the filter existed, so a new one cannot be made worse than the status quo)
// and it is exactly the kind of safe default that ships for a year.
//
// One assertion closes it, and it cannot be satisfied by accident: a
// configuration with nothing in it must narrow to nothing. A kind with no case
// here survives an empty config, so this goes red the moment one is added --
// before anybody can offer an operator a choice that is always refused.
func TestConfig_AConfigThatCanRunNothingOffersNoKinds(t *testing.T) {
	got, unoffered := servableKinds(config.Config{}, nil)
	if len(got) != 0 {
		t.Errorf("an empty configuration offers %v.\nEvery kind in genstore.Kinds() needs a "+
			"case in servableKinds saying what makes it runnable; one of these has none, "+
			"so it is offered to an operator who cannot run it", got)
	}
	// AND EVERY ONE OF THEM IS ACCOUNTED FOR ON THE OTHER SIDE. The two lists
	// are a partition of genstore.Kinds() and not two filters that happen to
	// agree -- a kind that fell out of both would be one the page can neither
	// offer nor mention, which is the state that made an operator ask whether
	// speech had been deleted.
	if len(unoffered) != len(genstore.Kinds()) {
		t.Errorf("an empty configuration offers nothing but accounts for %v of %v; a kind in "+
			"neither list is one the page can neither run nor name",
			unoffered, genstore.Kinds())
	}
}

// TestConfig_OffersEachKindOnlyWhenItIsRunnable walks the three shapes an
// operator's file actually takes.
//
// EACH KIND IS CHECKED IN BOTH DIRECTIONS, which is the part worth being
// explicit about: a filter that always said yes and a filter that always said
// no both pass a test that only ever looks at one config. The both-configured
// row is the one that proves the narrowing is per-kind rather than a single
// switch over the whole list.
func TestConfig_OffersEachKindOnlyWhenItIsRunnable(t *testing.T) {
	serve := config.Config{}
	serve.SD.Serve = []string{"sd-server", "--model", "/opt/models/sd15.gguf"}

	voices := tts.Catalogue{{ID: "lessac", Name: "Lessac", Voice: "/opt/voices/lessac.onnx"}}

	for _, c := range []struct {
		what   string
		cfg    config.Config
		voices tts.Catalogue
		want   []string
	}{
		{"a serve argv and no voices", serve, nil, []string{genstore.KindSD}},
		{"voices and no serve argv", config.Config{}, voices, []string{genstore.KindTTS}},
		{"both", serve, voices, []string{genstore.KindSD, genstore.KindTTS}},
		{"neither", config.Config{}, nil, nil},
	} {
		got, _ := servableKinds(c.cfg, c.voices)
		if len(got) != len(c.want) {
			t.Errorf("%s offers %v, want %v", c.what, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s offers %v, want %v", c.what, got, c.want)
				break
			}
		}
	}
}

// TestConfig_NeverSendsANilKindList.
//
// An empty list and never null, which is the rule every slice on this wire
// follows and the one this field acquired a way to break: it was a constant
// and is now built, so the nothing-to-offer case is a `var out []string` away
// from arriving as `null`. serde's `default` covers a field that is MISSING
// rather than one that is present and null, and this repository has already
// lost a whole websocket envelope to that distinction.
func TestConfig_NeverSendsANilKindList(t *testing.T) {
	offered, unoffered := servableKinds(config.Config{}, nil)
	if offered == nil || unoffered == nil {
		t.Error("a configuration that can run nothing sends a nil kind list, which marshals " +
			"as `null` and fails the client's whole response rather than one field of it")
	}
}
