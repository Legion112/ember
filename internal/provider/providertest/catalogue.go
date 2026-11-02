package providertest

import (
	"context"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// RecordingCatalogue is a provider.CatalogueAPI that does nothing and
// remembers everything, embedded in a RecordingProvider to give it a
// catalogue.
//
// Separate from RecordingProvider's own fields for the reason RecordingStores
// and RecordingLeftovers are: a fake *without* a catalogue has to stay
// expressible, because the disagreement between Caps.Catalogue and the
// interface is exactly what AssertCatalogueCapsMatch exists to catch in a
// real provider.
type RecordingCatalogue struct {
	// Samples is what CatalogueSnapshot returns, with ObservedAt overwritten
	// by whatever instant the caller passed.
	//
	// Overwritten rather than returned as written, because that is what a
	// real provider's mapper does: the clock arrives as an argument, and a
	// fake that ignored it would let a caller which forgot to pass one still
	// look correct. A fake tidier than the thing it stands in for tests a
	// program that does not exist.
	Samples []catalogue.Sample

	// Failures go through the embedded RecordingProvider's Errors and
	// FailOn, keyed as Calls records them ("CatalogueSnapshot").
}

// WithCatalogue returns a RecordingProvider that claims and implements a
// catalogue.
//
// The pairing is the point, as it is for WithStores and WithLeftovers: a
// caller cannot get the capability without the implementation.
func WithCatalogue(name node.ProviderID, samples ...catalogue.Sample) *CatalogueProvider {
	p := NewRecordingProvider(name)
	p.Capabilities.Catalogue = true
	return &CatalogueProvider{
		RecordingProvider:  p,
		RecordingCatalogue: RecordingCatalogue{Samples: samples},
	}
}

// CatalogueProvider is a RecordingProvider that also has a catalogue.
type CatalogueProvider struct {
	*RecordingProvider
	RecordingCatalogue
}

func (p *CatalogueProvider) CatalogueSnapshot(_ context.Context, now time.Time) ([]catalogue.Sample, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := p.record("CatalogueSnapshot", ""); err != nil {
		return nil, err
	}
	out := make([]catalogue.Sample, 0, len(p.Samples))
	for _, s := range p.Samples {
		s.ObservedAt = now
		if s.Provider == "" {
			s.Provider = p.ProviderName
		}
		out = append(out, s)
	}
	return out, nil
}

// AssertCatalogueCapsMatch checks that Caps.Catalogue and
// provider.CatalogueAPI agree, in both directions.
//
// The same shape as AssertStoreCapsMatch and AssertLeftoverCapsMatch, and
// here for the reason the second one exists: `ember catalogue snapshot` asks
// a provider for a sample only when the cap says to, so a provider that
// implements the interface and forgets the flag is never sampled -- and what
// that costs is invisible, because the series simply has no rows for it and
// nothing anywhere says a row was expected. A collector that silently
// collects nothing is worse than one that fails: the data it did not take
// cannot be taken again.
//
// The other direction fails loudly instead -- a cap with no implementation is
// a nil type assertion from an hourly timer -- which is why both are checked
// rather than just the quiet one.
func AssertCatalogueCapsMatch(t *testing.T, p provider.Provider) {
	t.Helper()
	_, implements := p.(provider.CatalogueAPI)
	claims := p.Caps().Catalogue
	switch {
	case claims && !implements:
		t.Errorf("%s says Caps.Catalogue but does not implement provider.CatalogueAPI; "+
			"an hourly snapshot would fail the type assertion", p.Name())
	case implements && !claims:
		t.Errorf("%s implements provider.CatalogueAPI but does not say Caps.Catalogue, "+
			"so nothing will ever sample it and the series it could have had cannot be collected later", p.Name())
	}
}

var (
	_ provider.Provider     = (*CatalogueProvider)(nil)
	_ provider.CatalogueAPI = (*CatalogueProvider)(nil)
)
