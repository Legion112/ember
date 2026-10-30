package node

import "fmt"

// Filter narrows the offers ember will consider.
//
// Providers pass what their API can express server-side, but every result is
// re-filtered through Match on our side regardless. That is a cost control,
// not tidiness: a marketplace that answers a max_price query with a dearer
// offer — because the field meant something else, or because the ask changed
// between query and answer — cannot cause a spend if the ceiling is also
// enforced here by a pure function with full test coverage.
type Filter struct {
	GPU      GPUModel // empty matches any
	MinVRAM  MiB      // per GPU; zero matches any
	MinCount int      // zero means one
	MaxPrice Rate     // whole offer; zero means no ceiling

	// AllowInterruptible must be set explicitly for ember to bid on a
	// preemptible instance. The default is the safe one: a run that loses its
	// node halfway through a 20-minute render has paid for 10 minutes of
	// nothing, and that should be a choice rather than a surprise.
	AllowInterruptible bool

	Region string // empty matches any

	// CPUOnly asks for a machine with no GPU at all.
	//
	// A filter rather than a flag on the spec, because it is a statement
	// about what to rent. It is also exclusive in both directions: with it
	// set, only CPU offers match; without it, only GPU offers do. A run that
	// wanted a card and silently got a CPU box would fail at the workload,
	// and one that wanted a cheap downloader and got a 4090 would cost
	// fifteen times what it meant to.
	CPUOnly bool
}

// Validate rejects a Filter that cannot be satisfied by construction.
func (f Filter) Validate() error {
	if f.MinCount < 0 {
		return fmt.Errorf("filter has negative gpu count")
	}
	if f.MinVRAM < 0 {
		return fmt.Errorf("filter has negative vram floor")
	}
	if f.MaxPrice < 0 {
		return fmt.Errorf("filter has negative price ceiling")
	}
	return nil
}

// Match reports whether o satisfies f.
func (f Filter) Match(o Offer) bool {
	if f.CPUOnly != o.IsCPU() {
		return false
	}
	if f.CPUOnly {
		// Nothing below applies: a CPU offer has no card, no VRAM and no
		// count to compare, and the GPU model it would be matched against is
		// the processor family.
		if f.MaxPrice > 0 && o.Price > f.MaxPrice {
			return false
		}
		if o.Interruptible && !f.AllowInterruptible {
			return false
		}
		return f.Region == "" || o.Region == f.Region
	}
	// Accepts, not equality: the capacity suffix makes an offer's model name
	// more specific than what an operator types. See GPUModel.Accepts.
	if !o.GPU.Accepts(f.GPU) {
		return false
	}
	if f.MinVRAM > 0 && o.VRAM < f.MinVRAM {
		return false
	}
	want := f.MinCount
	if want == 0 {
		want = 1
	}
	if o.GPUCount < want {
		return false
	}
	if f.MaxPrice > 0 && o.Price > f.MaxPrice {
		return false
	}
	if o.Interruptible && !f.AllowInterruptible {
		return false
	}
	if f.Region != "" && o.Region != f.Region {
		return false
	}
	return true
}
