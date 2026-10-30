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

func TestFilter_CPUOnlyIsExclusiveBothWays(t *testing.T) {
	// A run that wanted a card and silently got a CPU box fails at the
	// workload; one that wanted a cheap downloader and got a 4090 costs
	// fifteen times what it meant to. Neither may happen by default.
	cpu := Offer{Provider: "runpod", Token: "cpu3c", GPU: "cpu3c", VCPU: 2, Price: 60000}
	gpu := Offer{Provider: "runpod", Token: "t", GPU: NormalizeGPU("rtx4090"), GPUCount: 1, VRAM: GiB(24), Price: 890000}

	if (Filter{}).Match(cpu) {
		t.Error("an ordinary filter matched a CPU machine")
	}
	if !(Filter{}).Match(gpu) {
		t.Error("an ordinary filter did not match a GPU")
	}
	if !(Filter{CPUOnly: true}).Match(cpu) {
		t.Error("CPUOnly did not match a CPU machine")
	}
	if (Filter{CPUOnly: true}).Match(gpu) {
		t.Error("CPUOnly matched a GPU")
	}
}

func TestFilter_CPUOnlyStillHonoursPriceRegionAndInterruptible(t *testing.T) {
	cpu := Offer{Provider: "runpod", Token: "cpu3c", VCPU: 2, Price: 60000, Region: "EU-RO-1"}

	if (Filter{CPUOnly: true, MaxPrice: 50000}).Match(cpu) {
		t.Error("a CPU machine over the ceiling matched")
	}
	if !(Filter{CPUOnly: true, MaxPrice: 70000}).Match(cpu) {
		t.Error("a CPU machine under the ceiling did not match")
	}
	if (Filter{CPUOnly: true, Region: "US-KS-2"}).Match(cpu) {
		t.Error("a CPU machine in the wrong datacentre matched; a store pins placement")
	}
	spot := cpu
	spot.Interruptible = true
	if (Filter{CPUOnly: true}).Match(spot) {
		t.Error("an interruptible CPU machine matched without AllowInterruptible")
	}
}

func TestFilter_CPUOnlyIgnoresGPUCriteria(t *testing.T) {
	// A CPU offer has no card, no VRAM and no count. Comparing them would
	// mean a GPU model name matched against a processor family, which is how
	// a filter stops matching anything for a reason nobody can see.
	cpu := Offer{Provider: "runpod", Token: "cpu3c", GPU: "cpu3c", VCPU: 2, Price: 60000}
	f := Filter{CPUOnly: true, GPU: NormalizeGPU("rtx4090"), MinVRAM: GiB(80), MinCount: 4}
	if !f.Match(cpu) {
		t.Error("CPU matching was blocked by GPU criteria that cannot apply to it")
	}
}
