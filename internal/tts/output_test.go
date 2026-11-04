package tts

import (
	"strings"
	"testing"
)

// TestValidateOutput_BoundsTheStemBecauseTheFilesystemWill.
//
// THE STEM IS THE SECOND REQUEST-SUPPLIED STRING THAT REACHES A FILENAME, and
// it was the one with no length bound while MaxVoiceID is 64 and MaxText is
// 8000 and both say why. A 100,000-character stem passed ValidateOutput,
// Job.Validate and Resolve, and then failed at `piper --output_file` ON THE
// NODE -- after the create call, the wait for sshd and the image pull had all
// been paid for, which is the one thing ValidateOutput's own doc says it
// exists to prevent.
//
// The bound is NameMax minus the longest suffix ember composes, so what is
// asserted is the arithmetic rather than a number somebody liked. A stem at
// exactly the bound is accepted, because "too long" and "the bound" are
// different answers -- internal/s3's `do` rule and MaxText's.
func TestValidateOutput_BoundsTheStemBecauseTheFilesystemWill(t *testing.T) {
	if err := ValidateOutput(strings.Repeat("a", MaxOutput)); err != nil {
		t.Errorf("a stem of exactly the bound was refused: %v", err)
	}
	err := ValidateOutput(strings.Repeat("a", MaxOutput+1))
	if err == nil {
		t.Fatal("a stem past the bound was accepted; it would fail on the node instead")
	}
	// The figures and the reason, because "too long" with neither is a
	// refusal an operator cannot act on -- and NameMax is the one that
	// explains why ember has an opinion at all.
	for _, want := range []string{"241", "240", "255"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q", err, want)
		}
	}

	// AND THE SAME BOUND THROUGH THE LAYERS THAT WILL ACTUALLY SEE IT,
	// because a check nothing calls is this repository's recurring defect: a
	// config's jobs reach Job.Validate and a request's reach Ask.Validate and
	// Ask.Resolve.
	long := strings.Repeat("a", MaxOutput+1)
	if err := withPaths(Job{Text: "x", Output: long}).Validate(); err == nil {
		t.Error("Job.Validate accepted an over-long stem")
	}
	if err := (Ask{Text: "x", VoiceID: "lessac", Output: long}).Validate(); err == nil {
		t.Error("Ask.Validate accepted an over-long stem")
	}
	if _, err := (Ask{Text: "x", VoiceID: "lessac", Output: long}).Resolve(cat()); err == nil {
		t.Error("Ask.Resolve accepted an over-long stem")
	}
}

// TestMaxOutput_LeavesRoomForTheSuffixEmberComposes is the arithmetic itself,
// asserted rather than trusted: SpokenStem's output plus the extension has to
// fit in one filename, and the way this goes wrong later is somebody
// lengthening the suffix without touching the bound.
func TestMaxOutput_LeavesRoomForTheSuffixEmberComposes(t *testing.T) {
	if MaxOutput+suffixReserve != NameMax {
		t.Errorf("MaxOutput %d plus the reserve %d is not NameMax %d; the bound is supposed to "+
			"be arithmetic rather than a figure somebody liked",
			MaxOutput, suffixReserve, NameMax)
	}

	// THE OTHER HALF -- that the reserve is big enough for what SpokenStem
	// really writes -- CANNOT BE ASSERTED HERE, and that is a package
	// boundary rather than an omission: SpokenStem is internal/app's, which
	// imports this package, so this one cannot call it. It is asserted where
	// it lives, in TestSpokenStem_FitsInsideTheFilenameBoundItWasGiven, and
	// that is the test which caught this bound reserving EIGHT -- derived
	// from a two-digit index padded to "-100.wav", where "-1000.wav" is nine.
}
