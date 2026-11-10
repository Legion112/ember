package app

import (
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/node"
)

// TestResolveImage_DependsOnTheOfferThroughTheCardAndNothingElse.
//
// WHAT THIS IS LOAD-BEARING FOR, AND IT IS NOT ABOUT IMAGES. The scheduler
// packs a queued request onto a live rental when the kind, the provider and
// the card agree -- see genstore.Compat -- and nothing anywhere compares the
// IMAGE the two would run. That is sound only because the image is a function
// of this process's config and the card: a request cannot name an image (the
// request seal has no field for one and genstore.Request has none either), and
// `ember serve` loads its config once, so two requests the compat calls
// interchangeable resolve the same reference by construction rather than by
// agreement.
//
// SO THIS IS THE ASSERTION THAT KEEPS IT TRUE. Make the image depend on
// anything else an offer carries -- the region, the price, the provider, how
// many cards the box has -- and the compat becomes a match on three fields
// over a rental that differs in a fourth: a request packed onto a node running
// an image ember would not have chosen for it, discovered after the card was
// paid for. There is no test of "the images agree" anywhere, because the
// property is meant to be structural; this is what makes the structure
// checkable.
//
// THE CARD IS GPU AND MeasuredCap TOGETHER, which is the one honest
// complication. Offer.ComputeCap prefers a capability a provider read off the
// silicon itself over ember's own table, and only the local provider fills it
// in -- so "the card" for this purpose is the name plus that reading, and the
// map is keyed on the capability either produces. A disagreement between
// nvidia-smi and internal/node/compute.go would make two offers of one GPU
// name resolve differently; that is written down in the report rather than
// guarded here, because the compat has no access to MeasuredCap and the local
// provider is one machine.
func TestResolveImage_DependsOnTheOfferThroughTheCardAndNothingElse(t *testing.T) {
	// A per-architecture map, because the scalar `image:` answers every offer
	// identically and would make this test pass against anything.
	cfg := config.Config{
		Images: map[string]string{
			"sm_75": "ghcr.io/example/sd:sm_75@sha256:" + strings.Repeat("a", 64),
			"sm_89": "ghcr.io/example/sd:sm_89@sha256:" + strings.Repeat("b", 64),
		},
	}

	// rtx2080ti is sm_75 and rtx4090 is sm_89, so the map really does
	// distinguish them -- asserted, because a fixture whose two cards share an
	// entry would make every comparison below trivially true.
	base := node.Offer{
		Provider: "runpod", Token: "t1", GPU: "rtx2080ti", GPUCount: 1,
		VRAM: 11 * 1024, Price: 200000, Region: "EU-RO-1",
	}
	want, from, err := ResolveImage(cfg, base)
	if err != nil {
		t.Fatalf("the fixture does not resolve: %v", err)
	}
	if !strings.HasPrefix(from, "images.") {
		t.Fatalf("resolved from %q, want the per-architecture map: this test cannot see a "+
			"dependency on anything if the scalar image: answered", from)
	}
	other := base
	other.GPU = "rtx4090"
	if ref, _, err := ResolveImage(cfg, other); err != nil {
		t.Fatalf("the second card does not resolve: %v", err)
	} else if ref == want {
		t.Fatalf("both fixture cards resolve to %q, so nothing below could detect a change", ref)
	}

	// Every other field an offer carries, varied one at a time. The image must
	// not move.
	for _, c := range []struct {
		field string
		mut   func(*node.Offer)
	}{
		{"provider", func(o *node.Offer) { o.Provider = "vast" }},
		{"token", func(o *node.Offer) { o.Token = "a-different-listing" }},
		{"gpu count", func(o *node.Offer) { o.GPUCount = 8 }},
		{"vram", func(o *node.Offer) { o.VRAM = 80 * 1024 }},
		{"price", func(o *node.Offer) { o.Price = 2_490_000 }},
		{"vcpu", func(o *node.Offer) { o.VCPU = 64 }},
		{"region", func(o *node.Offer) { o.Region = "US-KS-2" }},
		{"reclaim", func(o *node.Offer) { o.Reclaim = node.ReclaimAnyTime }},
		{"label", func(o *node.Offer) { o.Label = "a line for `ember offers`" }},
	} {
		t.Run(c.field, func(t *testing.T) {
			o := base
			c.mut(&o)
			got, _, err := ResolveImage(cfg, o)
			if err != nil {
				t.Fatalf("changing the %s stopped an image resolving at all: %v", c.field, err)
			}
			if got != want {
				t.Errorf("the image moved from %q to %q when the %s changed; the scheduler packs "+
					"a request onto a live rental on kind, provider and card alone, so an image "+
					"that depends on the %s means a request can be packed onto a node running an "+
					"image ember would not have chosen for it", want, got, c.field, c.field)
			}
		})
	}
}

// TestBuildSpec_TakesTheStoreAndItsMountFromTheConfigAndNotFromTheOffer.
//
// THE SECOND HALF OF THE SAME ARGUMENT, for the dimension the scheduler also
// does not compare: the store a rental has mounted. A request cannot name one
// -- `/workspace` being a refused mount point is a rule about the config, and
// genstore.Request has no field for a store, a mount or a volume -- so every
// rental this process opens attaches what one config says, and two requests
// the compat calls interchangeable mount the same thing by construction.
//
// ASSERTED THROUGH THE SIGNATURE AS WELL AS THE VALUES, because the structural
// claim is "nothing on this path reads an offer": BuildSpec takes a config and
// a Resolved, and an offer is not in either. What is checked here is that the
// mount and the image that come out are the ones the config named, so a later
// change that threaded an offer in would have to move these too.
func TestBuildSpec_TakesTheStoreAndItsMountFromTheConfigAndNotFromTheOffer(t *testing.T) {
	cfg := config.Config{
		Image: "ghcr.io/example/sd@sha256:" + strings.Repeat("c", 64),
		Store: config.StoreConfig{Name: "weights", SizeGiB: 100, Mount: "/data"},
		TTL:   time.Hour,
	}
	mount, err := node.MountAt("/data")
	if err != nil {
		t.Fatal(err)
	}
	spec, err := BuildSpec(cfg, Resolved{
		Image: cfg.Image,
		Store: Attached{Handle: node.StoreHandle{Provider: "runpod", ID: "s1"}, Mount: mount},
	})
	if err != nil {
		t.Fatalf("the spec does not build: %v", err)
	}
	if spec.Mount.Path() != "/data" {
		t.Errorf("mount = %q, want the config's own /data", spec.Mount)
	}
	if spec.Image != cfg.Image {
		t.Errorf("image = %q, want the config's own %q", spec.Image, cfg.Image)
	}
}
