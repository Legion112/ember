package node

import (
	"fmt"
	"strings"
)

// OfferToken is a provider's own name for a thing ember can rent, carried
// back to that provider unmodified at create time.
//
// It is opaque on purpose. RunPod's is a GPU type id, Vast's is the id of one
// specific ask on one specific host, and the local provider's is a constant.
// Nothing above the provider may parse it, because the moment something does,
// the three stop being interchangeable.
type OfferToken string

// GPUModel is a normalised GPU name such as "rtx4090" or "a100-80g".
//
// Providers spell the same card several ways — "NVIDIA GeForce RTX 4090",
// "RTX 4090", "rtx_4090" — and a filter that compares raw strings matches one
// provider and silently returns nothing for another. Normalising at the
// mapping edge means the filter compares one spelling.
type GPUModel string

// noiseWords are the tokens providers add around a model number that do not
// change which card you get: the vendor, the form factor, the marketing line
// and the architecture code name.
//
// Matching is by whole token and never by substring. Substring matching is how
// the first version of this function turned "A100 SXM4 80GB" into "a1004-80g":
// it rewrote "sxm" inside "sxm4" and left the 4 behind, welded to the model
// number. Nothing failed — the filter simply matched no offers at all from
// every provider that spells the card that way, which looks exactly like a
// provider having no capacity.
var noiseWords = map[string]bool{
	"nvidia": true, "geforce": true, "tesla": true, "amd": true,
	"pcie": true, "sxm": true, "sxm2": true, "sxm3": true, "sxm4": true, "sxm5": true,
	"nvl": true, "oam": true,
	"blackwell": true, "hopper": true, "ampere": true, "ada": true,
	"lovelace": true, "turing": true, "volta": true,
	"workstation": true, "server": true, "edition": true, "gpu": true,
}

// NormalizeGPU folds a provider's GPU name into a GPUModel.
//
// Unrecognised names are lowercased and joined rather than rejected: a card
// ember has never heard of is still rentable, and refusing it would make ember
// unable to see new hardware until someone updated a table.
func NormalizeGPU(s string) GPUModel {
	t := strings.ToLower(strings.TrimSpace(s))
	t = strings.NewReplacer("_", " ", "-", " ", "/", " ", ",", " ").Replace(t)

	fields := strings.Fields(t)
	keep := make([]string, 0, len(fields))
	for _, f := range fields {
		if noiseWords[f] {
			continue
		}
		keep = append(keep, f)
	}
	joined := strings.Join(keep, "")

	// Memory suffixes are the part operators actually filter on: an A100 40G
	// and an A100 80G are not substitutes, and a pool that mixes them is a
	// pool where half the offers cannot hold the model.
	joined = strings.ReplaceAll(joined, "80gb", "-80g")
	joined = strings.ReplaceAll(joined, "40gb", "-40g")
	joined = strings.ReplaceAll(joined, "96gb", "-96g")
	joined = strings.ReplaceAll(joined, "141gb", "-141g")
	joined = strings.ReplaceAll(joined, "188gb", "-188g")
	return GPUModel(joined)
}

// Offer is one rentable configuration at one provider.
type Offer struct {
	Provider ProviderID
	Token    OfferToken
	GPU      GPUModel
	GPUCount int
	VRAM     MiB // per GPU, not total

	// Price is for the whole offer, every GPU included. Providers disagree
	// about this — some quote per GPU — so it is normalised at the mapping
	// edge. A per-GPU price compared against a whole-offer budget is how an
	// 8-GPU node gets rented on a one-GPU budget.
	Price Rate

	Region        string
	Interruptible bool
	Label         string // one human line for `ember offers`
}

// Validate rejects an Offer that could not be acted on.
func (o Offer) Validate() error {
	if o.Provider == "" {
		return fmt.Errorf("offer has no provider")
	}
	if o.Token == "" {
		return fmt.Errorf("offer from %s has no token", o.Provider)
	}
	if o.GPUCount < 0 {
		return fmt.Errorf("offer %s/%s has negative gpu count", o.Provider, o.Token)
	}
	if o.Price < 0 {
		return fmt.Errorf("offer %s/%s has negative price", o.Provider, o.Token)
	}
	if o.Price > MaxRate {
		return fmt.Errorf("offer %s/%s priced at %v, above the %v sanity ceiling", o.Provider, o.Token, o.Price, MaxRate)
	}
	return nil
}

func (o Offer) String() string {
	return fmt.Sprintf("%s/%s %dx%s %v", o.Provider, o.Token, o.GPUCount, o.GPU, o.Price)
}
