package node

import "testing"

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
