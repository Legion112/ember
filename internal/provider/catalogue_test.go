package provider_test

import (
	"testing"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/provider/providertest"
)

func TestAssertCatalogueCapsMatch_CatchesBothDirections(t *testing.T) {
	// Both directions are silent in production and only one is loud: a cap
	// with no implementation fails a type assertion from an hourly timer,
	// while an implementation with no cap is simply never sampled -- and the
	// hours that pass meanwhile cannot be sampled later. So both are
	// asserted here against a recording *testing.T rather than trusted.
	claimsOnly := providertest.NewRecordingProvider("claims")
	claimsOnly.Capabilities.Catalogue = true

	var fake testing.T
	providertest.AssertCatalogueCapsMatch(&fake, claimsOnly)
	if !fake.Failed() {
		t.Error("a provider claiming Caps.Catalogue without implementing CatalogueAPI was accepted")
	}

	silent := providertest.WithCatalogue("silent")
	silent.Capabilities.Catalogue = false
	var fake2 testing.T
	providertest.AssertCatalogueCapsMatch(&fake2, silent)
	if !fake2.Failed() {
		t.Error("a provider implementing CatalogueAPI without claiming it was accepted")
	}
}

func TestCatalogueProvider_StampsTheCallersClock(t *testing.T) {
	p := providertest.WithCatalogue("fake", catalogue.Sample{
		Kind: catalogue.KindGPU, ID: "card", Datacentre: "EU-RO-1",
		Availability: catalogue.LevelHigh,
	})
	providertest.AssertCatalogueCapsMatch(t, p)

	now := time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)
	got, err := p.CatalogueSnapshot(t.Context(), now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d sample(s)", len(got))
	}
	// The fake stamps what it was handed, because a fake that returned its
	// own timestamps would let a caller that never passed one look correct.
	if !got[0].ObservedAt.Equal(now) {
		t.Errorf("observed_at = %s, want the instant the caller passed", got[0].ObservedAt)
	}
	if got[0].Provider != "fake" {
		t.Errorf("provider = %q", got[0].Provider)
	}
	if err := got[0].Validate(); err != nil {
		t.Errorf("the fake produced a sample that cannot be recorded: %v", err)
	}

	p.FailOn = "CatalogueSnapshot"
	if _, err := p.CatalogueSnapshot(t.Context(), now); err == nil {
		t.Error("CatalogueSnapshot ignored FailOn")
	}
}
