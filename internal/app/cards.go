package app

import (
	"context"
	"sort"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// Card is one GPU model a provider has, as a UI should offer it.
//
// WHY A LIST EXISTS AT ALL. The web UI's card field was a free-text input with
// the placeholder `any`, and an operator typing into it was guessing: nothing
// said which cards RunPod actually has, so the way to find out was to press
// `what is available`, read a table and type what it said. A field whose
// correct values are knowable and not shown is the same defect as a selector
// offering a kind this server cannot run, one input along.
//
// NORMALISED, BECAUSE THAT IS WHAT A REQUEST NAMES. The catalogue's own id is
// the provider's spelling -- "NVIDIA GeForce RTX 4090" -- and
// `catalogue.Sample.ID`'s comment is explicit that normalising it at the store
// would record ember's reading of the name instead of the name. Here is where a
// reader needs it: `POST /api/requests` normalises the `gpu` field on the way
// in, so a list offering the provider's spelling would offer values that do not
// match what gets stored, and `Compat` compares the stored one.
type Card struct {
	Provider node.ProviderID

	// ID is what a request puts in its `gpu` field.
	ID node.GPUModel

	// Name is the provider's own spelling, for a person to recognise.
	Name string

	// VRAMGiB is per card, where the catalogue quotes it. Zero means the
	// provider did not say, which is not the same as a card with no memory --
	// so a UI prints nothing rather than "0 GiB".
	VRAMGiB int
}

// CollectCards asks every provider that keeps a catalogue what cards it has.
//
// ONCE AT STARTUP AND NEVER PER PAGE LOAD, which is what makes this affordable
// at all. The set of card MODELS a provider sells does not change during an
// `ember serve`; what changes is availability and price, which is what
// `what is available` asks and why that one stays a press. One call per
// provider per process, and `CatalogueAPI`'s own comment settles the cost:
// "It is free to call: a catalogue read runs no machine and spends nothing."
//
// A PROVIDER THAT COULD NOT BE ASKED IS A FINDING AND NOT A SILENCE, which is
// this repository's rule everywhere a provider is reached. The error is
// returned per provider rather than failing the whole collection, because
// `ember serve` must still start when RunPod is unreachable -- the local
// provider needs no credential and a queue draining onto it is unaffected --
// and a card list that is empty because nobody asked must not look like a card
// list that is empty because the provider has nothing.
//
// A PROVIDER WITHOUT A CATALOGUE CONTRIBUTES NOTHING AND IS NOT AN ERROR. The
// local provider is the case: it has exactly one card, this machine's, which
// `ember offers` reports from nvidia-smi. `Caps.Catalogue` is the gate, the
// same one CollectCatalogue reads.
func CollectCards(ctx context.Context, reg *provider.Registry, now time.Time) ([]Card, map[node.ProviderID]error) {
	out := []Card{}
	errs := map[node.ProviderID]error{}

	for _, p := range reg.All() {
		if !p.Caps().Catalogue {
			continue
		}
		api, ok := p.(provider.CatalogueAPI)
		if !ok {
			continue
		}
		samples, err := api.CatalogueSnapshot(ctx, now)
		if err != nil {
			errs[p.Name()] = err
			continue
		}
		out = append(out, cardsFrom(p.Name(), samples)...)
	}

	// SORTED, so two runs of one unchanged provider publish one list. The
	// catalogue is one row per type per site and the sites arrive in whatever
	// order the API answered, so an unsorted dedup would reorder a `<select>`
	// between restarts for no reason an operator could see.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		return out[i].ID < out[j].ID
	})
	return out, errs
}

// cardsFrom reduces one provider's samples to one row per card.
//
// ONE ROW PER TYPE PER SITE IS WHAT THE CATALOGUE HOLDS, deliberately -- see
// catalogue.Sample, where a file holding only the global summary would record
// "unavailable" for cards that were rentable all along. A picker wants the
// opposite: one entry per card, because the sites are not something a request
// can name.
//
// AVAILABILITY IS NOT READ HERE, and that is the division of labour. Which
// cards exist is what this list is; which of them can be had right now, and
// for how much, is what `what is available` asks of the provider on the press.
// Filtering by availability at startup would publish a list that silently
// shrinks for the life of the process.
func cardsFrom(prov node.ProviderID, samples []catalogue.Sample) []Card {
	seen := map[node.GPUModel]int{}
	out := []Card{}
	for _, s := range samples {
		if s.Kind != catalogue.KindGPU || s.ID == "" {
			continue
		}
		id := node.NormalizeGPU(s.ID)
		if id == "" {
			continue
		}
		name := s.Name
		if name == "" {
			name = s.ID
		}
		if i, ok := seen[id]; ok {
			// THE LARGEST MEMORY WINS where two rows disagree, because the
			// rows that disagree are a summary row with no site beside a
			// site's own, and a zero there is "the provider did not say" --
			// which must not overwrite a figure it did say.
			if s.MemoryGiB > out[i].VRAMGiB {
				out[i].VRAMGiB = s.MemoryGiB
			}
			continue
		}
		seen[id] = len(out)
		out = append(out, Card{Provider: prov, ID: id, Name: name, VRAMGiB: s.MemoryGiB})
	}
	return out
}
