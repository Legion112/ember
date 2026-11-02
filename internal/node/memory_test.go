package node

import "testing"

// TestMemory_TellsNoLimitFromNobodyLooked is the whole rule in one table, and
// it is one table because the rule used to be two implementations with their
// tests split across them.
//
// `nodemem.Report.Limit` was tested for the knowability half and never for
// the fallback; `genstore.Entry.MemCeiling` for the fallback and never for the
// knowability half -- measured, by deleting the asset check from the live one
// and watching the whole suite stay green. Every quadrant is here so that
// neither half can go unproven again.
func TestMemory_TellsNoLimitFromNobodyLooked(t *testing.T) {
	const (
		gib       = int64(1) << 30
		hostTotal = 700 * gib
		weights   = 56 * gib
	)
	cases := []struct {
		name     string
		mem      Memory
		ceiling  int64
		headroom int64
		knowable bool
	}{
		// The two arms of the FLUX.2 measurement, which is what this is for:
		// the same card in the same datacentre, both nodes reporting hundreds
		// of gigabytes of HOST memory, and only one of them able to hold the
		// weights.
		{
			name:     "the node that held the model",
			mem:      Memory{CgroupLimit: 64 * gib, HostTotal: hostTotal, Assets: weights},
			ceiling:  64 * gib,
			headroom: 8 * gib,
			knowable: true,
		},
		{
			// NEGATIVE IS A REAL ANSWER and the interesting one: this node
			// re-read 56 GiB off the store every step, while `free` inside it
			// said there were 700 GiB spare.
			name:     "the node that re-read the model every step",
			mem:      Memory{CgroupLimit: 32 * gib, HostTotal: hostTotal, Assets: weights},
			ceiling:  32 * gib,
			headroom: -24 * gib,
			knowable: true,
		},
		{
			// Zero is "no limit", so the host's total is the honest ceiling.
			// Reading it as a container that cannot allocate anything is the
			// opposite claim, and it is the one that makes a bounded node
			// look like an unbounded one.
			name:     "an unlimited cgroup falls back to the host",
			mem:      Memory{HostTotal: 64 * gib, Assets: 8 * gib},
			ceiling:  64 * gib,
			headroom: 56 * gib,
			knowable: true,
		},
		{
			name:     "nothing was read at all",
			mem:      Memory{},
			ceiling:  0,
			knowable: false,
		},
		{
			// A ceiling with no weights to compare it against answers
			// nothing, and this is the half the live implementation's tests
			// did not cover: a baked image and a local container have no
			// asset set, so this is not a hypothetical row.
			name:     "a ceiling and no asset figure",
			mem:      Memory{CgroupLimit: 1 * gib},
			ceiling:  1 * gib,
			knowable: false,
		},
		{
			// And the mirror: weights read on a node that answered nothing
			// about its memory. A headroom of "56 GiB short of nothing" is a
			// number that would get believed.
			name:     "weights and no ceiling",
			mem:      Memory{Assets: weights},
			ceiling:  0,
			knowable: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.mem.Ceiling(); got != c.ceiling {
				t.Errorf("Ceiling = %d, want %d", got, c.ceiling)
			}
			h, ok := c.mem.Headroom()
			if ok != c.knowable {
				t.Fatalf("Headroom knowable = %v, want %v", ok, c.knowable)
			}
			if !ok {
				// Zero and not the subtraction, because a caller that
				// ignored the bool must not find a plausible figure there.
				if h != 0 {
					t.Errorf("Headroom = %d over a figure nobody read, want zero", h)
				}
				return
			}
			if h != c.headroom {
				t.Errorf("Headroom = %d, want %d", h, c.headroom)
			}
		})
	}
}

// The cgroup's limit wins over the host's total even when it is far smaller,
// which is the one assertion the swapped-argument version of this rule would
// fail -- and the reason the fields are named rather than positional.
//
// The figures are the capture in internal/nodemem/testdata: a container
// bounded at 512 MiB reporting 125.7 GiB of MemTotal, a factor of 252.
func TestMemory_TheCgroupsLimitWinsByAFactorOf252(t *testing.T) {
	m := Memory{CgroupLimit: 512 << 20, HostTotal: 131817620 * 1024, Assets: 56 << 30}
	if m.Ceiling() != 512<<20 {
		t.Fatalf("Ceiling = %d, want the container's 512 MiB and not the host's", m.Ceiling())
	}
	h, ok := m.Headroom()
	if !ok {
		t.Fatal("headroom is unknowable with every figure present")
	}
	if h >= 0 {
		t.Errorf("headroom = %d; a 56 GiB model does not fit in 512 MiB", h)
	}
}
