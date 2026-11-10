package app

import (
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/tts"
)

// TestSpokenStem_FitsInsideTheFilenameBoundItWasGiven is the other half of
// tts.MaxOutput, and it lives here because this is where the composing is
// done.
//
// THE SPLIT IS A PACKAGE BOUNDARY AND NOT A CHOICE. `tts.suffixReserve` is the
// claim -- "ember appends at most this much around a stem" -- and `SpokenStem`
// is what appends it; `internal/tts` cannot call it, because this package
// imports that one. So the arithmetic (`MaxOutput + suffixReserve == NameMax`)
// is asserted there and the composition is asserted here, and between them the
// property is that a stem ember accepted cannot produce a filename the node
// refuses.
//
// IT IS THE TEST THAT CAUGHT THE BOUND BEING WRONG. The first version reserved
// EIGHT bytes, derived from a two-digit index padded into "-100.wav" -- and
// "-1000.wav" is nine, because `%02d` is a MINIMUM width and nothing bounds
// how many utterances a batch holds. A stem at that bound plus a batch of a
// thousand was one byte over NAME_MAX, which fails at `piper --output_file` on
// a machine that is already being paid for: the exact failure the bound exists
// to move off the node.
func TestSpokenStem_FitsInsideTheFilenameBoundItWasGiven(t *testing.T) {
	// A stem at exactly the bound, which is the only one worth testing: a
	// shorter one cannot overflow if this does not.
	stem := strings.Repeat("a", tts.MaxOutput)
	if err := tts.ValidateOutput(stem); err != nil {
		t.Fatalf("a stem at the bound was refused, so this test is about the wrong value: %v", err)
	}

	// Indices across every digit width the format can produce. 999_999_999 is
	// far beyond any batch a rental could run and is here because the reserve
	// claims to cover it -- an assertion about the widest case the constant
	// promises, rather than about the widest case anybody has seen.
	for _, i := range []int{0, 8, 98, 998, 9998, 999_999_998} {
		name := SpokenStem(tts.Job{Output: stem}, i) + tts.Ext
		if len(name) > tts.NameMax {
			t.Errorf("a stem at the bound with index %d composes a %d-byte filename, above "+
				"NAME_MAX of %d: it would be refused on the node, after a card was paid for",
				i+1, len(name), tts.NameMax)
		}
	}
}

// TestSpokenStem_AnUnnamedJobAlsoFits, because the fallback stem is ember's
// own and a bound that only covered operator-supplied names would miss it.
func TestSpokenStem_AnUnnamedJobAlsoFits(t *testing.T) {
	name := SpokenStem(tts.Job{}, 999_999_998) + tts.Ext
	if len(name) > tts.NameMax {
		t.Errorf("an unnamed job composes a %d-byte filename, above %d", len(name), tts.NameMax)
	}
}
