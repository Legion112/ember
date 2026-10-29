package node

import (
	"strings"
	"testing"
)

// TestNormalizeGPU_EverySpellingOfOneCardAgrees is the property that matters.
// A filter compares GPUModel with ==, so two spellings of the same card that
// normalise differently make the filter return nothing for one provider —
// which is indistinguishable from that provider having no capacity.
func TestNormalizeGPU_EverySpellingOfOneCardAgrees(t *testing.T) {
	for want, spellings := range map[GPUModel][]string{
		"rtx4090":    {"NVIDIA GeForce RTX 4090", "RTX 4090", "rtx_4090", "GeForce RTX 4090"},
		"a100-80g":   {"A100 80GB PCIe", "A100 SXM4 80GB", "A100-SXM4-80GB", "NVIDIA A100 80GB", "a100 80gb"},
		"a100-40g":   {"NVIDIA A100-SXM4-40GB", "A100 40GB PCIe"},
		"h100":       {"H100 SXM5", "H100 PCIe", "NVIDIA H100 NVL", "h100"},
		"rtxpro6000": {"RTX PRO 6000 Blackwell", "RTX PRO 6000", "NVIDIA RTX PRO 6000 Blackwell Workstation Edition"},
		"b200":       {"B200 SXM", "NVIDIA B200", "b200"},
	} {
		for _, s := range spellings {
			if got := NormalizeGPU(s); got != want {
				t.Errorf("NormalizeGPU(%q) = %q, want %q", s, got, want)
			}
		}
	}
}

// TestNormalizeGPU_KeepsDistinctionsThatMatter: an A100 40G is not a
// substitute for an 80G, and Max-Q is a different power envelope the operator
// may have asked for on purpose.
func TestNormalizeGPU_KeepsDistinctionsThatMatter(t *testing.T) {
	if NormalizeGPU("A100 40GB") == NormalizeGPU("A100 80GB") {
		t.Error("40G and 80G must not normalise together: half the pool could not hold the model")
	}
	if NormalizeGPU("RTX PRO 6000 Max-Q") == NormalizeGPU("RTX PRO 6000") {
		t.Error("Max-Q must stay distinct")
	}
}

func TestNormalizeGPU_PassesThroughUnknownCards(t *testing.T) {
	// A card ember has never heard of is still rentable.
	if got := NormalizeGPU("Acme Hypercard 9000"); got != "acmehypercard9000" {
		t.Errorf("NormalizeGPU of an unknown card = %q", got)
	}
	if got := NormalizeGPU(""); got != "" {
		t.Errorf("NormalizeGPU(\"\") = %q, want empty", got)
	}
}

func validOffer() Offer {
	return Offer{Provider: "runpod", Token: "tok", GPU: "rtx4090", GPUCount: 1, VRAM: 24576, Price: 340000}
}

func TestOffer_ValidateRefuses(t *testing.T) {
	for name, mut := range map[string]func(*Offer){
		"no provider":    func(o *Offer) { o.Provider = "" },
		"no token":       func(o *Offer) { o.Token = "" },
		"negative count": func(o *Offer) { o.GPUCount = -1 },
		"negative price": func(o *Offer) { o.Price = -1 },
		"absurd price":   func(o *Offer) { o.Price = MaxRate + 1 },
	} {
		o := validOffer()
		mut(&o)
		if err := o.Validate(); err == nil {
			t.Errorf("%s: Validate() = nil, want an error", name)
		}
	}
	if err := validOffer().Validate(); err != nil {
		t.Errorf("a valid offer was refused: %v", err)
	}
}

func TestOffer_String(t *testing.T) {
	if got, want := validOffer().String(), "runpod/tok 1xrtx4090 $0.34/hr"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

// TestNormalizeGPU_ProviderSpellingsReduceToTheModel covers the real strings
// providers use. Each of the last three was a silent filter miss: the card was
// rentable, the filter matched nothing, and the output was identical to a
// provider with no capacity.
func TestNormalizeGPU_ProviderSpellingsReduceToTheModel(t *testing.T) {
	for in, want := range map[string]GPUModel{
		"NVIDIA GeForce RTX 4090": "rtx4090",
		"rtx4090":                 "rtx4090",
		"RTX 4090":                "rtx4090",
		"NVIDIA A40":              "a40",
		"NVIDIA L40S":             "l40s",
		"NVIDIA H200":             "h200",

		// Memory type is not memory capacity. RunPod spells the card this
		// way, and leaving HBM3 in welded it onto the capacity suffix as
		// "h100-80ghbm3", which nothing would ever match.
		"NVIDIA H100 80GB HBM3": "h100-80g",
		"NVIDIA H100 PCIe 80GB": "h100-80g",

		// AMD's marketing line, the counterpart of GeForce.
		"AMD Instinct MI300X": "mi300x",

		// The bug noiseWords was written for, still fixed.
		"A100 SXM4 80GB": "a100-80g",
		"A100 PCIe 40GB": "a100-40g",
	} {
		if got := NormalizeGPU(in); got != want {
			t.Errorf("NormalizeGPU(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestGPUModelAccepts_ACapacitySuffixIsMoreSpecific is the other half of that
// fix. NormalizeGPU deliberately keeps the capacity, because an A100 40G and
// an A100 80G are not substitutes -- so an operator typing the plain model
// name has to match both, and string equality made it match neither.
func TestGPUModelAccepts_ACapacitySuffixIsMoreSpecific(t *testing.T) {
	for _, tc := range []struct {
		offer, want GPUModel
		accept      bool
	}{
		// An empty filter accepts anything.
		{"h100-80g", "", true},
		{"h100-80g", "h100", true},
		{"a100-40g", "a100", true},
		{"a100-80g", "a100", true},
		{"a100-80g", "a100-80g", true},
		{"rtx4090", "rtx4090", true},

		// A more specific filter than the offer is not satisfied: asking for
		// an 80G and getting a 40G is how half a pool cannot hold the model.
		{"a100-40g", "a100-80g", false},
		{"a100", "a100-80g", false},

		// Different cards.
		{"rtx4090", "rtx4080", false},
		{"h100-80g", "h200", false},

		// The boundary that keeps this a parse rather than a substring
		// search: the next character after the filter must be the capacity
		// separator, so a1000 is not an a100.
		{"a1000", "a100", false},
		{"h1000-80g", "h100", false},
	} {
		if got := tc.offer.Accepts(tc.want); got != tc.accept {
			t.Errorf("%q.Accepts(%q) = %v, want %v", tc.offer, tc.want, got, tc.accept)
		}
	}
}

// TestNormalizeGPU_RealRunPodCatalogueNames are names copied from a live
// RunPod catalogue response, not from documentation.
//
// Each of the last three was a silent filter miss found by running
// `ember offers` against the real API and reading the output: the card was
// listed and rentable, and `--gpu <what anyone would type>` matched nothing.
func TestNormalizeGPU_RealRunPodCatalogueNames(t *testing.T) {
	for in, want := range map[string]GPUModel{
		"NVIDIA GeForce RTX 4090":                      "rtx4090",
		"NVIDIA GeForce RTX 5090":                      "rtx5090",
		"NVIDIA A100 80GB PCIe":                        "a100-80g",
		"NVIDIA A100-SXM4-80GB":                        "a100-80g",
		"NVIDIA H100 80GB HBM3":                        "h100-80g",
		"NVIDIA H200 NVL":                              "h200",
		"NVIDIA L40S":                                  "l40s",
		"NVIDIA RTX A4500":                             "rtxa4500",
		"NVIDIA RTX PRO 6000 Blackwell Server Edition": "rtxpro6000",

		// Cooling is not silicon.
		"NVIDIA B300 SXM6 AC": "b300",
		// "ada" was a noise word and "generation" was not.
		"NVIDIA RTX 6000 Ada Generation": "rtx6000",
	} {
		if got := NormalizeGPU(in); got != want {
			t.Errorf("NormalizeGPU(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestNormalizeGPU_TwoSpellingsOfOneCardAgree, which is what lets `--gpu
// a100-80g` rent either a PCIe or an SXM4 board.
func TestNormalizeGPU_TwoSpellingsOfOneCardAgree(t *testing.T) {
	for _, pair := range [][2]string{
		{"NVIDIA A100 80GB PCIe", "NVIDIA A100-SXM4-80GB"},
		{"NVIDIA RTX PRO 6000 Blackwell Server Edition", "NVIDIA RTX PRO 6000 Blackwell Workstation Edition"},
	} {
		a, b := NormalizeGPU(pair[0]), NormalizeGPU(pair[1])
		if a != b {
			t.Errorf("%q -> %q and %q -> %q; the same card should normalise alike", pair[0], a, pair[1], b)
		}
	}
}

// TestNormalizeGPU_AMIGSliceStaysDistinct. "RTX PRO 6000 ... MIG 1g.24gb" is a
// partition of a card, not the card: it has a fraction of the memory, and
// folding it into the full model would have ember rent a slice for a job
// sized to the whole thing.
func TestNormalizeGPU_AMIGSliceStaysDistinct(t *testing.T) {
	full := NormalizeGPU("NVIDIA RTX PRO 6000 Blackwell Server Edition")
	slice := NormalizeGPU("NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb")
	if full == slice {
		t.Errorf("a MIG slice normalised to the full card (%q)", full)
	}
	if !strings.Contains(string(slice), "mig") {
		t.Errorf("the slice does not say it is one: %q", slice)
	}
}
