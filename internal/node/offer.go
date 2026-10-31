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
	// AMD's marketing line, the counterpart of GeForce. Without it
	// "AMD Instinct MI300X" normalises to "instinctmi300x" and no operator
	// types that.
	"instinct": true, "radeon": true, "quadro": true,
	"pcie": true, "sxm": true, "sxm2": true, "sxm3": true, "sxm4": true, "sxm5": true,
	"sxm6": true, "sxm7": true,
	"nvl": true, "oam": true,
	// Cooling, not silicon. RunPod lists "NVIDIA B300 SXM6 AC"; air- and
	// liquid-cooled are the same chip, and without these an operator typing
	// `--gpu b300` matches nothing at all.
	"ac": true, "lc": true,
	// "NVIDIA RTX 6000 Ada Generation" -- "ada" was already here and
	// "generation" was not, which left "rtx6000generation".
	"generation": true,
	// Memory *type*, which is not memory *capacity* and does not change
	// which card you get. RunPod spells the card "NVIDIA H100 80GB HBM3",
	// and leaving this in welded it onto the capacity suffix as
	// "h100-80ghbm3" -- a spelling no filter would ever match.
	"hbm": true, "hbm2": true, "hbm2e": true, "hbm3": true, "hbm3e": true,
	"gddr6": true, "gddr6x": true, "gddr7": true,
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
	//
	// Both the provider's spelling ("80GB") and ember's own ("-80g") are
	// recognised, which is what makes this function idempotent. It was not:
	// the separator stripping above turned "a100-80g" back into "a10080g",
	// and the rewrite only looked for "80gb", so normalising an
	// already-normalised name mangled it. That is not a theoretical concern,
	// because config.Filter runs an operator's typed string through here --
	// so `--gpu a100-80g`, the spelling every offer carries and the one
	// Accepts documents, produced a filter of "a10080g" and matched nothing
	// at all. `--gpu a100` worked, which is how it went unnoticed.
	//
	// Longest capacity first, so "141gb" is not read as a shorter suffix, and
	// CutSuffix rather than ReplaceAll because the capacity is the tail of
	// the name once the noise words are gone -- a replacement anywhere in the
	// string would rewrite a model number that happened to contain one.
	for _, c := range []string{"188", "141", "96", "80", "40"} {
		rest, ok := strings.CutSuffix(joined, c+"gb")
		if !ok {
			rest, ok = strings.CutSuffix(joined, c+"g")
		}
		if !ok {
			continue
		}
		rest = strings.TrimSuffix(rest, "-")
		if rest == "" {
			// A capacity with no model in front of it is not a model name.
			break
		}
		joined = rest + "-" + c + "g"
		break
	}
	return GPUModel(joined)
}

// Accepts reports whether an offer of model m satisfies a filter asking for
// want.
//
// Exact equality is not the right comparison, because the capacity suffix
// makes a model name *more specific*: NormalizeGPU deliberately keeps it, so
// "NVIDIA H100 80GB" becomes "h100-80g" while an operator types "--gpu h100".
// Comparing those as strings meant a filter for an H100 matched nothing on a
// provider whose catalogue is full of them, which looks exactly like a
// provider having no capacity -- the same failure mode as the substring bug
// noiseWords exists to prevent, arrived at from the other direction.
//
// So a filter matches a model it is a prefix of, but only at the capacity
// boundary. "a100" accepts "a100-40g" and "a100-80g"; "a100-80g" accepts only
// the 80G. A hypothetical "a1000" is not accepted by "a100", because the next
// character is a digit rather than the boundary -- which is what keeps this a
// parse of the structure rather than a substring search.
func (m GPUModel) Accepts(want GPUModel) bool {
	if want == "" || m == want {
		return true
	}
	rest, ok := strings.CutPrefix(string(m), string(want))
	return ok && strings.HasPrefix(rest, "-")
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

	// VCPU is how many virtual cores a CPU-only offer carries, and zero on a
	// GPU offer -- where the cores come with the card and are not chosen.
	//
	// It is both the figure the provider needs at create time and the thing
	// that tells the two kinds apart. A separate boolean would be a second
	// source of truth for the same fact, and the two could disagree.
	VCPU int

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
	if o.VCPU < 0 {
		return fmt.Errorf("offer %s has a negative vCPU count", o.Token)
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
	if o.IsCPU() {
		return fmt.Sprintf("%s/%s %d vCPU %v", o.Provider, o.Token, o.VCPU, o.Price)
	}
	return fmt.Sprintf("%s/%s %dx%s %v", o.Provider, o.Token, o.GPUCount, o.GPU, o.Price)
}

// IsCPU reports whether this offer is a CPU-only machine.
//
// There is no GPU on one, so nothing that needs a card may be scheduled
// against it -- which is the whole reason ember rents one: filling a store
// needs bandwidth and a disk, and renting a 4090 to download a file costs
// fifteen times as much as renting the cheapest thing that can.
func (o Offer) IsCPU() bool { return o.VCPU > 0 && o.GPUCount == 0 }
