package app

import (
	"errors"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/providertest"
)

func catalogueProvider(name node.ProviderID, samples ...catalogue.Sample) *providertest.CatalogueProvider {
	rp := providertest.NewRecordingProvider(name)
	rp.Capabilities = provider.Caps{Catalogue: true}
	return &providertest.CatalogueProvider{
		RecordingProvider:  rp,
		RecordingCatalogue: providertest.RecordingCatalogue{Samples: samples},
	}
}

// add registers a provider and fails loudly, because a registry that silently
// did not take one would make every assertion below about an empty list.
func add(t *testing.T, reg *provider.Registry, p provider.Provider) {
	t.Helper()
	if err := reg.Add(p); err != nil {
		t.Fatal(err)
	}
}

func gpu(id, name string, vram int, site string) catalogue.Sample {
	return catalogue.Sample{
		Kind: catalogue.KindGPU, ID: id, Name: name, MemoryGiB: vram, Datacentre: site,
	}
}

// TestCollectCards_IsOneRowPerCardAndNotPerSite.
//
// THE CATALOGUE IS ONE ROW PER TYPE PER SITE, deliberately -- catalogue.Sample
// records why: a file holding only the global summary reads "unavailable" for
// cards that were rentable all along. A picker wants the opposite, because a
// site is not something a request can name, so the same card arriving from
// four datacentres has to become one option and not four.
func TestCollectCards_IsOneRowPerCardAndNotPerSite(t *testing.T) {
	reg := provider.NewRegistry()
	add(t, reg, catalogueProvider("runpod",
		gpu("NVIDIA GeForce RTX 4090", "RTX 4090", 24, "EU-RO-1"),
		gpu("NVIDIA GeForce RTX 4090", "RTX 4090", 24, "US-KS-2"),
		gpu("NVIDIA A100-SXM4-80GB", "A100 80GB", 80, "EUR-IS-1"),
	))

	cards, errs := CollectCards(t.Context(), reg, time.Now())
	if len(errs) != 0 {
		t.Fatalf("errors from a provider that answered: %v", errs)
	}
	if len(cards) != 2 {
		t.Fatalf("got %d card(s), want 2: the 4090 arrived from two sites and is one option: %+v", len(cards), cards)
	}

	// NORMALISED, because that is what a request's `gpu` field carries and
	// what Compat compares. Offering "NVIDIA GeForce RTX 4090" would offer a
	// value that does not match the row posting it produces.
	for _, c := range cards {
		if c.ID != node.NormalizeGPU(string(c.ID)) {
			t.Errorf("card id %q is not normalised, so it does not match what a request stores",
				c.ID)
		}
		if c.Name == "" {
			t.Errorf("card %q has no name for a person to recognise", c.ID)
		}
	}
}

// TestCollectCards_KeepsTheMemoryTheProviderDidSay.
//
// THE ROWS THAT DISAGREE ARE A SUMMARY BESIDE A SITE'S OWN. A row with no site
// carries the global summary and, in the live catalogue, frequently no memory
// at all -- so a later zero must not overwrite a figure the provider gave. The
// cost of getting this wrong is a picker that prints a card with no memory
// beside one with 80 GiB, which is the one number that decides whether a model
// fits.
func TestCollectCards_KeepsTheMemoryTheProviderDidSay(t *testing.T) {
	reg := provider.NewRegistry()
	add(t, reg, catalogueProvider("runpod",
		gpu("NVIDIA A100-SXM4-80GB", "A100 80GB", 0, ""),
		gpu("NVIDIA A100-SXM4-80GB", "A100 80GB", 80, "EUR-IS-1"),
	))

	cards, _ := CollectCards(t.Context(), reg, time.Now())
	if len(cards) != 1 {
		t.Fatalf("got %d card(s), want 1", len(cards))
	}
	if cards[0].VRAMGiB != 80 {
		t.Errorf("vram = %d, want 80: a summary row's silence overwrote a site's figure",
			cards[0].VRAMGiB)
	}
}

// TestCollectCards_SkipsACPURowAndAProviderWithNoCatalogue.
//
// A CPU FAMILY IS NOT A CARD and `ember store sync` is what rents one, so a
// picker offering it would offer a machine no generation can run on. A
// provider with no catalogue is the local one: it has exactly one card, this
// machine's, reported through `offers` from nvidia-smi -- and that is an
// absence rather than an error, which is why it must not appear in the error
// map either.
func TestCollectCards_SkipsACPURowAndAProviderWithNoCatalogue(t *testing.T) {
	reg := provider.NewRegistry()
	add(t, reg, catalogueProvider("runpod",
		gpu("NVIDIA GeForce RTX 4090", "RTX 4090", 24, "EU-RO-1"),
		catalogue.Sample{Kind: catalogue.KindCPU, ID: "cpu3c", Name: "CPU 3rd gen"},
	))
	add(t, reg, providertest.NewRecordingProvider("local"))

	cards, errs := CollectCards(t.Context(), reg, time.Now())
	if len(cards) != 1 || cards[0].Provider != "runpod" {
		t.Fatalf("got %+v, want only the runpod card", cards)
	}
	if len(errs) != 0 {
		t.Errorf("a provider with no catalogue was reported as an error: %v; it is an absence "+
			"-- the local provider has one card and reports it through offers", errs)
	}
}

// TestCollectCards_AProviderThatCouldNotBeAskedIsAFinding.
//
// THE RULE EVERYWHERE A PROVIDER IS REACHED. `ember serve` has to start when
// RunPod is unreachable -- the local provider needs no credential and a queue
// draining onto it is unaffected -- so the collection cannot fail. What it
// must not do is answer with a short list and no reason, because a card list
// that is empty because nobody asked looks exactly like a provider that has
// nothing, and the page would offer a truncated catalogue as if it were whole.
func TestCollectCards_AProviderThatCouldNotBeAskedIsAFinding(t *testing.T) {
	bad := catalogueProvider("runpod", gpu("NVIDIA GeForce RTX 4090", "RTX 4090", 24, "EU-RO-1"))
	bad.Errors["CatalogueSnapshot"] = errors.New("dial tcp: lookup rest.runpod.io: no such host")
	reg := provider.NewRegistry()
	add(t, reg, bad)
	add(t, reg, catalogueProvider("other", gpu("NVIDIA L40S", "L40S", 48, "EU-SE-1")))

	cards, errs := CollectCards(t.Context(), reg, time.Now())
	if _, ok := errs["runpod"]; !ok {
		t.Errorf("the unreachable provider is not in the errors: %v", errs)
	}
	// AND THE OTHER PROVIDER'S CARDS STILL ARRIVE. One unreachable provider
	// must not hide what the others said, which is Collect's own rule one
	// route over.
	if len(cards) != 1 || cards[0].Provider != "other" {
		t.Errorf("got %+v, want the reachable provider's card", cards)
	}
}
