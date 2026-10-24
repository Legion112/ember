package node

import "testing"

func TestFilter_ValidateRefusesNegatives(t *testing.T) {
	for name, f := range map[string]Filter{
		"count": {MinCount: -1},
		"vram":  {MinVRAM: -1},
		"price": {MaxPrice: -1},
	} {
		if err := f.Validate(); err == nil {
			t.Errorf("negative %s: Validate() = nil, want an error", name)
		}
	}
	if err := (Filter{}).Validate(); err != nil {
		t.Errorf("the zero filter should be valid (it matches anything non-interruptible): %v", err)
	}
}

// TestFilter_PriceCeilingIsInclusive: an offer priced at exactly the ceiling is
// within budget. Integer money is what makes that statement true rather than
// approximately true.
func TestFilter_PriceCeilingIsInclusive(t *testing.T) {
	f := Filter{MaxPrice: 340000}
	at := validOffer()
	if !f.Match(at) {
		t.Error("an offer priced exactly at the ceiling must match")
	}
	over := validOffer()
	over.Price = 340001
	if f.Match(over) {
		t.Error("one microdollar over the ceiling must not match")
	}
}

// TestFilter_InterruptibleIsOptIn: losing a node halfway through a render has
// paid for half a render, and that should be a choice.
func TestFilter_InterruptibleIsOptIn(t *testing.T) {
	o := validOffer()
	o.Interruptible = true
	if (Filter{}).Match(o) {
		t.Fatal("the zero Filter must not match an interruptible offer")
	}
	if !(Filter{AllowInterruptible: true}).Match(o) {
		t.Fatal("AllowInterruptible must admit it")
	}
}

func TestFilter_Match(t *testing.T) {
	for name, tc := range map[string]struct {
		filter Filter
		mut    func(*Offer)
		want   bool
	}{
		"zero filter matches":      {Filter{}, nil, true},
		"gpu matches":              {Filter{GPU: "rtx4090"}, nil, true},
		"gpu mismatch":             {Filter{GPU: "h100"}, nil, false},
		"vram floor met":           {Filter{MinVRAM: 24576}, nil, true},
		"vram floor unmet":         {Filter{MinVRAM: 24577}, nil, false},
		"count floor met":          {Filter{MinCount: 1}, nil, true},
		"count floor unmet":        {Filter{MinCount: 2}, nil, false},
		"zero count means one":     {Filter{}, func(o *Offer) { o.GPUCount = 0 }, false},
		"region matches":           {Filter{Region: "eu"}, func(o *Offer) { o.Region = "eu" }, true},
		"region mismatch":          {Filter{Region: "eu"}, func(o *Offer) { o.Region = "us" }, false},
		"no ceiling admits a cost": {Filter{}, func(o *Offer) { o.Price = MaxRate }, true},
	} {
		o := validOffer()
		if tc.mut != nil {
			tc.mut(&o)
		}
		if got := tc.filter.Match(o); got != tc.want {
			t.Errorf("%s: Match() = %v, want %v", name, got, tc.want)
		}
	}
}
