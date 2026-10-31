package node

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ComputeCap is an NVIDIA CUDA compute capability, as the single number that
// names an architecture everywhere it is written down: nvcc's -arch, the sm_NN
// markers in a fatbin, CMAKE_CUDA_ARCHITECTURES and ember's `images:` keys all
// spell 7.5 as 75.
//
// Not to be confused with Spec.MinCUDA, which is a CUDA *runtime* version the
// host must offer and is parsed by ParseCUDAVersion. Both are "a CUDA version"
// in conversation and they constrain different things -- the card's silicon
// against the driver's software -- so this type renders itself only as "sm_75"
// and never as "7.5". nvidia-smi's "7.5" spelling is an input format with no
// matching output.
type ComputeCap int

// CapKnowledge is how ember knows a card's capability, and it has three
// answers rather than two.
//
// NormalizeGPU promises never to reject a card it has not heard of, so a
// lookup miss cannot be an error. It must not be a guess either: an image
// built for the wrong architecture does not fail on a card, it finds no usable
// kernel and falls back to the CPU backend at the GPU's price -- the same
// failure internal/gpucheck exists to catch, arrived at before the node is
// even created, which is where it is free to catch.
//
// And a card with no CUDA capability at all is a different answer from one
// ember has never heard of. RunPod really lists AMD Instinct MI300X; the
// remediation for that is "set image:", and for an unheard-of card it is
// "teach ember the card".
//
// CapUnknown is the zero value, which is the safe direction for the same
// reason provider.Caps.PublicSSH defaults to the stricter check: a card added
// to one table and forgotten in the other reads as unknown and refuses,
// rather than reading as known and selecting an image for silicon that is not
// there.
type CapKnowledge uint8

const (
	CapUnknown CapKnowledge = iota
	CapKnown
	CapNotCUDA
)

func (k CapKnowledge) String() string {
	switch k {
	case CapKnown:
		return "known"
	case CapNotCUDA:
		return "not a CUDA card"
	default:
		return "unknown"
	}
}

// computeCaps is every card ember can select, and the architecture its silicon
// is.
//
// Keyed on the GPUModel that NormalizeGPU produces, which is the one spelling
// the filter compares -- see internal/provider/local/gpu.go for what a second
// speller cost. The keys are not guesses: offer_test.go pins them against
// strings copied from a live RunPod catalogue response, and
// TestComputeCapTable_IsConsistent asserts every key here survives
// NormalizeGPU unchanged, which is what catches an entry that is dead code.
//
// This table used to live in test/integration/imagelab_test.go, where its
// comment said the capability "is not derivable from anything in this
// repository". That stopped being true the moment ember had to choose an image
// by it, and two copies of a table with a GPU bill attached is the defect
// "one name for a path" is about.
//
// A card below the architectures the image is built for -- a V100 at sm_70 --
// belongs here anyway. An entry with no matching `images:` key refuses while
// naming sm_70, which is a useful answer; leaving it out would say "ember does
// not know a V100", which is false.
var computeCaps = map[GPUModel]ComputeCap{
	// Turing. The card in the machine this repository is developed on, which
	// is why it is here: the local provider offers it, so it is a card ember
	// can select even though nobody rents one.
	"rtx2080ti": 75,
	"rtx8000":   75,

	// Ampere, GA100.
	"a100-40g": 80,
	"a100-80g": 80,

	// Ampere, GA10x. One architecture, many names.
	"a10":      86,
	"a16":      86,
	"a40":      86,
	"rtx3090":  86,
	"rtxa4000": 86,
	"rtxa4500": 86,
	"rtxa5000": 86,
	"rtxa6000": 86,

	// Ada Lovelace.
	"l4":      89,
	"l40":     89,
	"l40s":    89,
	"rtx4080": 89,
	"rtx4090": 89,

	// Hopper. "NVIDIA H100 NVL" normalises to a bare "h100" because NVL is a
	// form factor and 94GB is not one of the capacity suffixes NormalizeGPU
	// rewrites, so both spellings need an entry.
	"h100":     90,
	"h100-80g": 90,
	"h200":     90,

	// Blackwell, GB100.
	"b200": 100,

	// Blackwell, GB202.
	"rtx5090":    120,
	"rtxpro6000": 120,
}

// notCUDA are cards ember can see that have no CUDA compute capability at all.
//
// Listed rather than left to miss, because "this card is not a CUDA card" and
// "ember has never heard of this card" send an operator to two different
// places, and only one of those places exists.
var notCUDA = map[GPUModel]bool{
	"mi300x": true,
}

// Cards deliberately absent, and why. Adding one of these without resolving
// the ambiguity would select an image for the wrong silicon, which is the
// whole failure this table exists to prevent.
//
//   - rtx4000, rtx5000, rtx6000: NormalizeGPU folds both the Turing Quadro and
//     the Ada refresh onto the same name, because "quadro", "ada" and
//     "generation" are all noise words. "Quadro RTX 6000" is sm_75 and
//     "NVIDIA RTX 6000 Ada Generation" is sm_89, and from the normalised name
//     alone there is no way to tell them apart. A miss refuses; a guess rents.
//
//   - b300: the repository's old table claimed 100, grouping it with the B200
//     as "NVIDIA B200 / B300 SXM6". Blackwell Ultra is widely described as a
//     distinct target and the published sources do not settle which, so the
//     claim is unverified and stays out. The one place ground truth exists is
//     a rented B300 printing its own compute_cap, which is how any entry here
//     should be confirmed.

// ComputeCapOf reports the architecture of a card, and how confidently.
//
// The second return is not a bool because there are three answers; see
// CapKnowledge. The capability is zero for both non-known answers, and there
// is no sm_0, so a caller that ignores the second return fails closed.
func ComputeCapOf(m GPUModel) (ComputeCap, CapKnowledge) {
	if notCUDA[m] {
		return 0, CapNotCUDA
	}
	if c, ok := computeCaps[m]; ok {
		return c, CapKnown
	}
	return 0, CapUnknown
}

// ParseComputeCap reads a capability as nvidia-smi prints it: "7.5" is sm_75.
//
// The input format, and the only one -- ComputeCap renders itself only as
// sm_NN, so this is the single place the dotted spelling is understood. It
// shares major.minor parsing with ParseCUDAVersion because the shape is the
// same; what differs is that this is silicon and that is a driver.
func ParseComputeCap(s string) (ComputeCap, error) {
	maj, min, err := ParseCUDAVersion(s)
	if err != nil {
		return 0, fmt.Errorf("compute capability %q: %w", s, err)
	}
	if maj <= 0 {
		return 0, fmt.Errorf("compute capability %q: there is no sm_0", s)
	}
	return ComputeCap(maj*10 + min), nil
}

// KnownCaps lists every architecture some card in the table needs, ascending.
//
// This is the list the published images have to cover, and the matrix that
// builds them reads it rather than repeating it -- a list in a workflow that
// drifted from the list it mirrored is the defect PUBLISH_LABS was written
// about.
func KnownCaps() []ComputeCap {
	seen := map[ComputeCap]bool{}
	out := make([]ComputeCap, 0, len(computeCaps))
	for _, c := range computeCaps {
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// CardsFor lists the cards that need a given architecture, sorted.
//
// For error messages, and for the image lab's check that an architecture it
// was asked to build is one some card actually needs -- an image nothing will
// ever run is a runner's twenty minutes and a line in a config for nothing.
func CardsFor(c ComputeCap) []GPUModel {
	var out []GPUModel
	for m, got := range computeCaps {
		if got == c {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// KnownCards lists every card in the table, sorted. For error messages.
func KnownCards() []GPUModel {
	out := make([]GPUModel, 0, len(computeCaps))
	for m := range computeCaps {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Key renders the capability the way an `images:` key spells it.
func (c ComputeCap) Key() string { return "sm_" + strconv.Itoa(int(c)) }

// String is Key. One rendering, not two: the "7.5" spelling belongs to
// nvidia-smi's output and to ParseCUDAVersion's input, and a type that could
// print itself either way is a type that will print the wrong one in an error.
func (c ComputeCap) String() string { return c.Key() }

// ParseImageKey reads an `images:` key.
//
// Strict, and deliberately so. "sm75", "SM_75" and "75" are all things an
// operator might type, and accepting them would mean the key in the config is
// not the key in the error message -- so each is refused by name, with the one
// spelling that works. Parse, don't grep: the key is an identifier, and the
// refusal has to be able to say what it should have been.
func ParseImageKey(s string) (ComputeCap, error) {
	rest, ok := strings.CutPrefix(s, "sm_")
	if !ok {
		return 0, fmt.Errorf("image key %q is not an architecture: write it as sm_NN, "+
			"for example sm_90 for an H100", s)
	}
	if rest == "" {
		return 0, fmt.Errorf("image key %q names no architecture: write it as sm_NN, "+
			"for example sm_90 for an H100", s)
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("image key %q is not an architecture: sm_ must be followed by "+
				"digits only, as nvcc spells it -- compute capability 7.5 is sm_75, not sm_7.5", s)
		}
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0, fmt.Errorf("image key %q: %w", s, err)
	}
	if n <= 0 {
		return 0, fmt.Errorf("image key %q is not an architecture: there is no sm_0", s)
	}
	return ComputeCap(n), nil
}

// ValidateImageForArch refuses a reference whose tag names a different
// architecture than the key it is filed under.
//
// A parse of the tag's structure rather than a substring search: the tag is
// split on the separators a tag may contain, and a component spelled smNN is
// read as a claim about the architecture. None found is the common case and is
// accepted silently -- a tag is for a human to read and ember does not get to
// dictate it. Exactly one that disagrees is refused. More than one is refused
// as ambiguous, because a tag naming two architectures tells a reader nothing,
// and picking one would send them somewhere the bug is not.
//
// This is a legibility check and not a guarantee. What a binary can actually
// run is in the binary, which test/integration/imagelab_test.go reads out of
// the fatbin directly; a tag is a label somebody typed. It exists because the
// digest beside it is sixty-four characters of hex, and six lines of those
// under six keys is a place where a mis-paste is invisible.
func ValidateImageForArch(ref string, c ComputeCap) error {
	tag := imageTag(ref)
	if tag == "" {
		return nil
	}

	claims := map[ComputeCap]bool{}
	for _, part := range strings.FieldsFunc(tag, func(r rune) bool {
		return r == '-' || r == '.' || r == '_'
	}) {
		digits, ok := strings.CutPrefix(part, "sm")
		if !ok || digits == "" {
			continue
		}
		n, err := strconv.Atoi(digits)
		if err != nil || n <= 0 {
			continue
		}
		claims[ComputeCap(n)] = true
	}

	switch len(claims) {
	case 0:
		return nil
	case 1:
		for got := range claims {
			if got != c {
				return fmt.Errorf("image %q is filed under %s but its tag says %s; "+
					"the digest is what runs, so one of the two is a mis-paste and ember "+
					"cannot tell which", ref, c, got)
			}
		}
		return nil
	default:
		var names []string
		for got := range claims {
			names = append(names, got.String())
		}
		sort.Strings(names)
		return fmt.Errorf("image %q has a tag naming more than one architecture (%s); "+
			"a tag that claims two says nothing about which digest this is",
			ref, strings.Join(names, ", "))
	}
}

// imageTag returns the tag part of an OCI reference, or "" when there is none.
//
// Only the part after the last slash can carry the tag; a registry host may
// contain a colon for its port, as in "localhost:5000/comfyui". A digest is
// cut off first, so "repo:sm89@sha256:..." yields "sm89" -- which is the whole
// point, since that is the form this check exists to read.
func imageTag(ref string) string {
	s := strings.TrimSpace(ref)
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	last := s[strings.LastIndex(s, "/")+1:]
	_, tag, ok := strings.Cut(last, ":")
	if !ok {
		return ""
	}
	return tag
}
