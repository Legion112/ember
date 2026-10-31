package node

import (
	"strings"
	"testing"
)

// TestComputeCapOf_RealProviderSpellings runs the names providers actually use
// through the whole path -- NormalizeGPU and then the table -- rather than
// asserting about keys chosen here.
//
// This is the test that stops the table drifting from the spelling it is keyed
// on. A key that NormalizeGPU never produces is dead code that looks correct,
// and that is precisely how the local provider's own normaliser went unnoticed.
func TestComputeCapOf_RealProviderSpellings(t *testing.T) {
	for in, want := range map[string]ComputeCap{
		"NVIDIA GeForce RTX 2080 Ti":                   75,
		"Quadro RTX 8000":                              75,
		"NVIDIA A100 80GB PCIe":                        80,
		"NVIDIA A100-SXM4-80GB":                        80,
		"A100 PCIe 40GB":                               80,
		"NVIDIA A40":                                   86,
		"NVIDIA RTX A4500":                             86,
		"NVIDIA GeForce RTX 3090":                      86,
		"NVIDIA L40S":                                  89,
		"NVIDIA GeForce RTX 4090":                      89,
		"NVIDIA H100 80GB HBM3":                        90,
		"NVIDIA H100 PCIe 80GB":                        90,
		"NVIDIA H100 NVL":                              90,
		"NVIDIA H200":                                  90,
		"NVIDIA H200 NVL":                              90,
		"NVIDIA B200":                                  100,
		"NVIDIA GeForce RTX 5090":                      120,
		"NVIDIA RTX PRO 6000 Blackwell Server Edition": 120,
	} {
		got, how := ComputeCapOf(NormalizeGPU(in))
		if how != CapKnown {
			t.Errorf("ComputeCapOf(%q) is %v; it normalises to %q, which is not in the table",
				in, how, NormalizeGPU(in))
			continue
		}
		if got != want {
			t.Errorf("ComputeCapOf(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestComputeCapOf_AnUnknownCardIsNotAGuess.
//
// NormalizeGPU promises never to reject a card it has not heard of, so this
// cannot be an error -- but it must not be a capability either. Zero plus
// CapUnknown is what makes a caller that ignores the second return fail
// closed, because there is no sm_0.
func TestComputeCapOf_AnUnknownCardIsNotAGuess(t *testing.T) {
	got, how := ComputeCapOf(NormalizeGPU("Acme Hypercard 9000"))
	if how != CapUnknown {
		t.Errorf("an unheard-of card is %v, want unknown", how)
	}
	if got != 0 {
		t.Errorf("an unheard-of card has capability %s, want zero", got)
	}
}

// TestComputeCapOf_ACardWithNoCUDACapability.
//
// RunPod really lists AMD Instinct MI300X. "Not a CUDA card" and "never heard
// of it" send an operator to two different places, and only one of them
// exists, so they are different answers.
func TestComputeCapOf_ACardWithNoCUDACapability(t *testing.T) {
	got, how := ComputeCapOf(NormalizeGPU("AMD Instinct MI300X"))
	if how != CapNotCUDA {
		t.Errorf("MI300X is %v, want not a CUDA card", how)
	}
	if got != 0 {
		t.Errorf("MI300X has capability %s, want zero", got)
	}
}

// TestComputeCapOf_TheAmbiguousRTXNamesAreAbsent.
//
// NormalizeGPU folds the Turing Quadro and the Ada refresh onto one name,
// because "quadro", "ada" and "generation" are all noise words: "Quadro RTX
// 6000" is sm_75 and "NVIDIA RTX 6000 Ada Generation" is sm_89, and both
// normalise to "rtx6000". From the normalised name there is no way to tell
// them apart, so the table must not claim to.
//
// Asserted rather than left implicit, so a later commit cannot helpfully add
// an entry and silently start renting the wrong silicon.
func TestComputeCapOf_TheAmbiguousRTXNamesAreAbsent(t *testing.T) {
	// The collision itself, first: if NormalizeGPU ever learned to tell these
	// apart, this test is the wrong shape and should be deleted rather than
	// updated.
	turing, ada := NormalizeGPU("Quadro RTX 6000"), NormalizeGPU("NVIDIA RTX 6000 Ada Generation")
	if turing != ada {
		t.Fatalf("the Turing and Ada RTX 6000 now normalise apart (%q and %q); "+
			"the table can tell them apart and this test is obsolete", turing, ada)
	}

	for _, m := range []GPUModel{"rtx4000", "rtx5000", "rtx6000"} {
		if _, how := ComputeCapOf(m); how != CapUnknown {
			t.Errorf("%q has a capability in the table, but the Turing and Ada cards of that "+
				"name are different architectures and normalise the same; a guess here rents "+
				"the wrong silicon", m)
		}
	}
}

// TestComputeCapTable_IsConsistent.
//
// Three properties, each of which has a failure that looks like working code:
// an entry NormalizeGPU never produces is unreachable; a card in both tables
// answers whichever map is read first; and a zero capability is indistinguishable
// from an unknown card.
func TestComputeCapTable_IsConsistent(t *testing.T) {
	for m, c := range computeCaps {
		if got := NormalizeGPU(string(m)); got != m {
			t.Errorf("key %q does not survive NormalizeGPU (it becomes %q), so nothing can "+
				"ever look it up", m, got)
		}
		if c <= 0 {
			t.Errorf("key %q has capability %d; zero is how an unknown card answers", m, c)
		}
		if notCUDA[m] {
			t.Errorf("key %q is in both computeCaps and notCUDA", m)
		}
	}
	for m := range notCUDA {
		if got := NormalizeGPU(string(m)); got != m {
			t.Errorf("notCUDA key %q does not survive NormalizeGPU (it becomes %q)", m, got)
		}
	}
}

// TestKnownCaps_EveryArchitectureIsClaimedByACard, and the other way round.
//
// This is the property the image lab used to own: it asserted the binary
// covered every rentable card. A single-architecture image cannot assert that,
// so it moves here, where it is free and runs on every pull request.
//
// The reverse direction matters as much: an architecture in the list that no
// card needs is a matrix job, twenty minutes of a runner and a line in every
// operator's config, for an image nothing will ever run.
func TestKnownCaps_EveryArchitectureIsClaimedByACard(t *testing.T) {
	caps := KnownCaps()
	if len(caps) == 0 {
		t.Fatal("no architectures at all")
	}
	for _, c := range caps {
		if cards := CardsFor(c); len(cards) == 0 {
			t.Errorf("%s is in KnownCaps but no card needs it", c)
		}
	}
	for _, m := range KnownCards() {
		c, how := ComputeCapOf(m)
		if how != CapKnown {
			t.Errorf("%q is in KnownCards but ComputeCapOf says %v", m, how)
			continue
		}
		var found bool
		for _, got := range caps {
			if got == c {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%q needs %s, which is not in KnownCaps", m, c)
		}
	}
	// Ascending, because the matrix and every error message that lists them
	// read in that order.
	for i := 1; i < len(caps); i++ {
		if caps[i-1] >= caps[i] {
			t.Errorf("KnownCaps is not ascending: %v", caps)
			break
		}
	}
}

// TestOfferComputeCap_AMeasurementBeatsTheTable.
//
// Not because a measurement is more likely to be right in general, but
// because it is a reading of the card that will run the container where the
// table is a reading of a name a provider chose. A disagreement means the
// table is wrong about that card, and acting on the table would select an
// image for silicon that is not there.
func TestOfferComputeCap_AMeasurementBeatsTheTable(t *testing.T) {
	o := Offer{GPU: "a100-80g", GPUCount: 1, MeasuredCap: 86}
	got, how := o.ComputeCap()
	if how != CapKnown {
		t.Fatalf("a measured card is %v, want known", how)
	}
	if got != 86 {
		t.Errorf("ComputeCap = %s, want sm_86 -- the table says sm_80 for this model and the "+
			"measurement has to win", got)
	}
}

// TestOfferComputeCap_AMeasuredCardEmberHasNeverHeardOf.
//
// The 2080 Ti in the machine this repository is developed on, and the whole
// reason MeasuredCap exists: a card whose own driver has just named it in
// full must not be unselectable because a table in this package is short.
func TestOfferComputeCap_AMeasuredCardEmberHasNeverHeardOf(t *testing.T) {
	o := Offer{GPU: "acmehypercard9000", GPUCount: 1, MeasuredCap: 75}
	got, how := o.ComputeCap()
	if how != CapKnown {
		t.Errorf("a measured but unlisted card is %v, want known", how)
	}
	if got != 75 {
		t.Errorf("ComputeCap = %s, want sm_75", got)
	}
}

// TestOfferComputeCap_FallsBackToTheTable, which is every cloud provider:
// no API ember has seen publishes the field.
func TestOfferComputeCap_FallsBackToTheTable(t *testing.T) {
	for _, tc := range []struct {
		o   Offer
		cap ComputeCap
		how CapKnowledge
	}{
		{Offer{GPU: "h100-80g", GPUCount: 1}, 90, CapKnown},
		{Offer{GPU: "mi300x", GPUCount: 1}, 0, CapNotCUDA},
		{Offer{GPU: "acmehypercard9000", GPUCount: 1}, 0, CapUnknown},
		{Offer{GPU: GPUNone, GPUCount: 1}, 0, CapUnknown},
	} {
		got, how := tc.o.ComputeCap()
		if got != tc.cap || how != tc.how {
			t.Errorf("Offer{GPU:%q}.ComputeCap() = %s/%v, want %s/%v",
				tc.o.GPU, got, how, tc.cap, tc.how)
		}
	}
}

// TestOfferHasGPU_TheLocalNoCardOfferIsNotACard.
//
// The local provider reports one GPU of model "none" with no vCPUs when the
// machine has no card, so IsCPU() is false and the offer looks like a card to
// anything that only asks that question. Reading it as an unknown card would
// tell an operator on a laptop to go and teach ember a new GPU.
func TestOfferHasGPU_TheLocalNoCardOfferIsNotACard(t *testing.T) {
	for _, tc := range []struct {
		o    Offer
		want bool
	}{
		{Offer{GPU: GPUNone, GPUCount: 1}, false},
		{Offer{GPU: "", GPUCount: 1}, false},
		{Offer{GPU: "rtx4090", GPUCount: 0}, false},
		{Offer{VCPU: 8}, false},
		{Offer{GPU: "rtx4090", GPUCount: 1}, true},
	} {
		if got := tc.o.HasGPU(); got != tc.want {
			t.Errorf("Offer{GPU:%q, GPUCount:%d, VCPU:%d}.HasGPU() = %v, want %v",
				tc.o.GPU, tc.o.GPUCount, tc.o.VCPU, got, tc.want)
		}
	}
}

func TestParseComputeCap(t *testing.T) {
	for in, want := range map[string]ComputeCap{
		"7.5": 75, "8.0": 80, "8.6": 86, "8.9": 89, "9.0": 90, "10.0": 100, "12.0": 120,
	} {
		got, err := ParseComputeCap(in)
		if err != nil {
			t.Errorf("ParseComputeCap(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseComputeCap(%q) = %s, want %s", in, got, want)
		}
	}
	// "[N/A]" is what some drivers really print for this column, and it has
	// to be an error rather than a zero that reads as a capability.
	for _, bad := range []string{"", "7", "seven.five", "x.y", "[N/A]", "0.0"} {
		if got, err := ParseComputeCap(bad); err == nil {
			t.Errorf("ParseComputeCap(%q) accepted it as %s", bad, got)
		}
	}
}

func TestComputeCap_RendersOnlyAsAnImageKey(t *testing.T) {
	for c, want := range map[ComputeCap]string{75: "sm_75", 80: "sm_80", 100: "sm_100", 120: "sm_120"} {
		if got := c.Key(); got != want {
			t.Errorf("ComputeCap(%d).Key() = %q, want %q", c, got, want)
		}
		if got := c.String(); got != want {
			t.Errorf("ComputeCap(%d).String() = %q, want %q -- there is one rendering, "+
				"and it is not the nvidia-smi spelling", c, got, want)
		}
	}
}

// TestCapKnowledge_String, because the three answers appear in refusals and
// "unknown" and "not a CUDA card" are the words that tell an operator which
// of two different things to go and do.
func TestCapKnowledge_String(t *testing.T) {
	for k, want := range map[CapKnowledge]string{
		CapUnknown:      "unknown",
		CapKnown:        "known",
		CapNotCUDA:      "not a CUDA card",
		CapKnowledge(9): "unknown",
	} {
		if got := k.String(); got != want {
			t.Errorf("CapKnowledge(%d).String() = %q, want %q", k, got, want)
		}
	}
}

func TestParseImageKey_RoundTripsEveryArchitecture(t *testing.T) {
	for _, c := range KnownCaps() {
		got, err := ParseImageKey(c.Key())
		if err != nil {
			t.Errorf("ParseImageKey(%q): %v", c.Key(), err)
			continue
		}
		if got != c {
			t.Errorf("ParseImageKey(%q) = %s, want %s", c.Key(), got, c)
		}
	}
}

// TestParseImageKey_RefusesEverySpellingButOne.
//
// Each of these is something an operator might reasonably type. Accepting any
// of them would mean the key in the config is not the key in the error
// message, and the whole value of the key is that it can be named back.
func TestParseImageKey_RefusesEverySpellingButOne(t *testing.T) {
	for _, bad := range []string{
		"", "sm", "sm_", "sm75", "SM_75", "Sm_75", "75", "7.5", "sm_7.5",
		"sm_0", "sm_-1", "sm_75g", "sm_ 75", " sm_75", "sm__75", "arch_75",
		// All digits, so it reaches the conversion, and then does not fit in
		// an int. A key nobody would type, but the branch is there and an
		// uncovered branch is one nobody has read.
		"sm_999999999999999999999999",
	} {
		if got, err := ParseImageKey(bad); err == nil {
			t.Errorf("ParseImageKey(%q) accepted it as %s", bad, got)
		}
	}
}

func TestValidateImageForArch(t *testing.T) {
	digest := "@sha256:" + strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		ref     string
		cap     ComputeCap
		refused bool
	}{
		// The common case: no architecture in the tag at all. A tag is for a
		// human to read and ember does not get to dictate it.
		"a tag that says nothing":      {"ghcr.io/x/ember-sd:v3" + digest, 89, false},
		"no tag at all":                {"ghcr.io/x/ember-sd" + digest, 89, false},
		"a bare digest reference":      {"ghcr.io/x/ember-sd" + digest, 120, false},
		"agreeing, with a digest":      {"ghcr.io/x/ember-sd:sd-cuda-sm89" + digest, 89, false},
		"agreeing, tag only":           {"ghcr.io/x/ember-sd:sm89", 89, false},
		"agreeing, underscored":        {"ghcr.io/x/ember-sd:sd_cuda_sm120", 120, false},
		"a registry port is not a tag": {"localhost:5000/ember-sd" + digest, 89, false},

		// The mis-paste this exists to catch.
		"disagreeing":             {"ghcr.io/x/ember-sd:sd-cuda-sm89" + digest, 90, true},
		"disagreeing, tag only":   {"ghcr.io/x/ember-sd:sm75", 120, true},
		"two architectures named": {"ghcr.io/x/ember-sd:sm89-sm90" + digest, 89, true},

		// Not an architecture claim: the digits have to be the whole
		// component after sm, or "sm8gb" would read as sm_8.
		"sm with no digits":   {"ghcr.io/x/ember-sd:smoke-test" + digest, 89, false},
		"digits not after sm": {"ghcr.io/x/ember-sd:v89" + digest, 90, false},
	} {
		err := ValidateImageForArch(tc.ref, tc.cap)
		switch {
		case tc.refused && err == nil:
			t.Errorf("%s: %q under %s was accepted", name, tc.ref, tc.cap)
		case !tc.refused && err != nil:
			t.Errorf("%s: %q under %s was refused: %v", name, tc.ref, tc.cap, err)
		}
	}
}

// TestValidateImage_IsTheSameRuleExported, because the alternative to
// exporting it was a second copy in internal/config.
func TestValidateImage_IsTheSameRuleExported(t *testing.T) {
	for _, bad := range []string{"", "ember-sd", "ghcr.io/x/ember-sd:latest"} {
		if err := ValidateImage(bad); err == nil {
			t.Errorf("ValidateImage(%q) accepted an unpinned reference", bad)
		}
	}
	for _, good := range []string{"ember:lab", "ghcr.io/x/ember-sd@sha256:" + strings.Repeat("a", 64)} {
		if err := ValidateImage(good); err != nil {
			t.Errorf("ValidateImage(%q): %v", good, err)
		}
	}
}
