package local

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/node"
)

func TestParseSMI(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want GPU
	}{
		"the real machine": {
			"NVIDIA GeForce RTX 2080 Ti, 11264, 7.5\n",
			GPU{Model: "rtx2080ti", VRAM: 11264, ComputeCap: "7.5"},
		},
		// "a100-80g" and not "a100sxm480gb", which is what this package's own
		// normaliser used to answer. The form factor is a noise word and the
		// capacity is not; getting that wrong made the card unfilterable.
		"a datacentre card": {
			"NVIDIA A100-SXM4-80GB, 81920, 8.0\n",
			GPU{Model: "a100-80g", VRAM: 81920, ComputeCap: "8.0"},
		},
		// The same card under the other spelling RunPod uses for it, to the
		// same model: a local A100 and a rented one are one name.
		"the same card, named the other way": {
			"NVIDIA A100 80GB PCIe, 81920, 8.0\n",
			GPU{Model: "a100-80g", VRAM: 81920, ComputeCap: "8.0"},
		},
		// Several cards give one offer: ember rents one node and sd-cli is
		// pointed at one device, so advertising more would be an offer
		// nothing can honour.
		"two cards, first wins": {
			"NVIDIA GeForce RTX 4090, 24564, 8.9\nNVIDIA GeForce RTX 3090, 24576, 8.6\n",
			GPU{Model: "rtx4090", VRAM: 24564, ComputeCap: "8.9"},
		},
		"blank lines skipped": {
			"\n\nNVIDIA L40S, 46068, 8.9\n",
			GPU{Model: "l40s", VRAM: 46068, ComputeCap: "8.9"},
		},
		"no output":         {"", GPU{}},
		"garbage":           {"not a csv at all\n", GPU{}},
		"missing columns":   {"NVIDIA L40S, 46068\n", GPU{}},
		"unreadable memory": {"NVIDIA L40S, lots, 8.9\n", GPU{}},
		"zero memory":       {"NVIDIA L40S, 0, 8.9\n", GPU{}},
		"no name":           {", 46068, 8.9\n", GPU{}},
	} {
		got := parseSMI(tc.in)
		if got != tc.want {
			t.Errorf("%s: parseSMI = %+v, want %+v", name, got, tc.want)
		}
	}
}

func TestGPU_IsZero(t *testing.T) {
	if !(GPU{}).IsZero() {
		t.Error("the empty GPU is not zero")
	}
	// A name with no memory is not a card ember can reason about: VRAM is
	// what the filter compares, and zero would match a request for none.
	if !(GPU{Model: "rtx4090"}).IsZero() {
		t.Error("a card with no VRAM counted as present")
	}
	if !(GPU{VRAM: 11264}).IsZero() {
		t.Error("VRAM with no model counted as present")
	}
	if (GPU{Model: "rtx4090", VRAM: 24564}).IsZero() {
		t.Error("a real card counted as absent")
	}
}

// TestParseSMI_NamesACardTheWayEveryProviderDoes.
//
// This package used to carry its own normaliser, which stripped the vendor
// words and every separator and stopped there. It disagreed with
// node.NormalizeGPU for five of these eleven, and the disagreement was not
// cosmetic: GPUModel.Accepts requires the character after a prefix to be the
// capacity separator, so "a100sxm480gb" is not accepted by `--gpu a100` and
// the card could not be selected at all.
//
// Spelled out here rather than left to node's own tests, because what has to
// hold is that *this* path produces that spelling -- the bug was a second
// speller, and a test that only checked the first could not see it.
func TestParseSMI_NamesACardTheWayEveryProviderDoes(t *testing.T) {
	for in, want := range map[string]node.GPUModel{
		"NVIDIA GeForce RTX 2080 Ti":                   "rtx2080ti",
		"NVIDIA GeForce RTX 4090":                      "rtx4090",
		"NVIDIA RTX A5000":                             "rtxa5000",
		"NVIDIA L40S":                                  "l40s",
		"Quadro RTX 8000":                              "rtx8000",
		"NVIDIA A100-SXM4-80GB":                        "a100-80g",
		"NVIDIA A100 80GB PCIe":                        "a100-80g",
		"NVIDIA A100-PCIE-40GB":                        "a100-40g",
		"NVIDIA H100 80GB HBM3":                        "h100-80g",
		"NVIDIA B200":                                  "b200",
		"NVIDIA RTX PRO 6000 Blackwell Server Edition": "rtxpro6000",
	} {
		got := parseSMI(in + ", 11264, 7.5\n")
		if got.Model != want {
			t.Errorf("parseSMI(%q).Model = %q, want %q", in, got.Model, want)
		}
	}
}

// TestDetectGPU_IsQuietWhenThereIsNoCard.
//
// Every failure -- no driver, no card, no nvidia-smi at all -- has to answer
// "none" rather than an error. CI has no GPU, and a provider that refused to
// list offers because a hardware query failed would break every lab in this
// repository on every machine that never had one.
func TestDetectGPU_IsQuietWhenThereIsNoCard(t *testing.T) {
	// Not asserting the result: this test runs on machines with a card and
	// machines without. What must hold either way is that it answers at all.
	g, answered := DetectGPU(t.Context())
	if !answered {
		t.Error("DetectGPU could not answer on a live context")
	}
	if !g.IsZero() && g.Model == "" {
		t.Error("DetectGPU reported a present card with no model")
	}
	if !g.IsZero() && g.VRAM == 0 {
		t.Error("DetectGPU reported a present card with no VRAM")
	}
}

// TestDetectGPU_SaysWhenItCouldNotAskRatherThanThatThereIsNoCard.
//
// The second result is the whole point: `exec.CommandContext` kills the child
// when the context ends, so Output fails for a reason that says nothing about
// the hardware -- and the old signature returned the same zero GPU for that as
// for a machine with no card, which `Provider.gpuNow` then memoised for the
// life of an `ember serve`.
//
// Asserted on a real `nvidia-smi` lookup rather than through a fake, because
// what has to hold is that THIS function tells the two apart; and the
// acceptance above is beside it on purpose, since a `false` here is also what
// a function that could never answer would return.
func TestDetectGPU_SaysWhenItCouldNotAskRatherThanThatThereIsNoCard(t *testing.T) {
	dead, cancel := context.WithCancel(context.Background())
	cancel()

	g, answered := DetectGPU(dead)
	if answered {
		t.Error("a cancelled probe reported an answer about this machine's hardware")
	}
	if !g.IsZero() {
		t.Errorf("a probe that could not run reported a card: %+v", g)
	}
}

// TestProvider_DetectsOnceWithTheRealDetectorByDefault.
//
// A nil Detect is what production uses, and the result is cached because
// Offers runs on every invocation while a graphics card does not appear or
// vanish inside one.
func TestProvider_DetectsOnceWithTheRealDetectorByDefault(t *testing.T) {
	p := &Provider{}
	first := p.gpuNow(t.Context())
	second := p.gpuNow(t.Context())
	if first != second {
		t.Errorf("two calls disagreed: %+v then %+v", first, second)
	}

	calls := 0
	q := &Provider{Detect: func(context.Context) (GPU, bool) {
		calls++
		return testGPU, true
	}}
	q.gpuNow(t.Context())
	q.gpuNow(t.Context())
	q.gpuNow(t.Context())
	if calls != 1 {
		t.Errorf("detected %d times, want once", calls)
	}
}

// TestProvider_ACancelledProbeIsNotCachedAsNoCard.
//
// `ember serve` builds ONE registry at startup and `getOffers` calls the
// provider on the HTTP request's own context, so a browser reload that
// abandons the first `/api/offers` poll ran this probe against a dying
// context. With `sync.Once` and a detector that answered zero to every
// failure, that one cancelled listing was memoised as "this machine has no
// GPU" for the life of the process: the operator's card vanished from the
// listing they pick hardware from, `--gpu` read as sold out everywhere, and
// the "docker cannot hand it to a container" diagnosis could not fire because
// it is guarded on the probe having found something.
//
// THE CHEAP-WHEN-IT-ANSWERED PROPERTY IS ASSERTED BESIDE THE RECOVERY, and
// both halves are needed. Without the recovery this is the defect; without the
// probe count, the "fix" of never caching at all would pass -- and that costs
// an nvidia-smi on every listing on every machine that has no card, which is
// the cost the cache exists to avoid.
//
// TestProvider_DetectsOnceWithTheRealDetectorByDefault cannot see any of
// this: detecting exactly once is precisely what the defect does.
func TestProvider_ACancelledProbeIsNotCachedAsNoCard(t *testing.T) {
	calls := 0
	p := &Provider{Detect: func(ctx context.Context) (GPU, bool) {
		calls++
		// DetectGPU's own shape, which is what makes this a stand-in for it
		// rather than a fake arranged to fail.
		if ctx.Err() != nil {
			return GPU{}, false
		}
		return testGPU, true
	}}

	dead, cancel := context.WithCancel(context.Background())
	cancel()
	if g := p.gpuNow(dead); !g.IsZero() {
		t.Fatalf("a cancelled probe reported a card: %+v", g)
	}

	// Every later listing, on a healthy context, has to find the card.
	for i := range 3 {
		if g := p.gpuNow(t.Context()); g != testGPU {
			t.Fatalf("listing %d after the cancelled one reported %+v, want %+v",
				i+1, g, testGPU)
		}
	}
	// One probe for the cancelled listing, one that answered, and nothing
	// after that: the answer is cached, the failure is not.
	if calls != 2 {
		t.Errorf("nvidia-smi ran %d times, want 2 -- one abandoned probe and one answer", calls)
	}

	// And a probe that answered "no card at all" is an answer, so it is
	// cached too. A machine with no nvidia-smi must not re-exec per listing.
	none := 0
	q := &Provider{Detect: func(context.Context) (GPU, bool) {
		none++
		return GPU{}, true
	}}
	for range 3 {
		if g := q.gpuNow(t.Context()); !g.IsZero() {
			t.Fatalf("a machine with no card reported %+v", g)
		}
	}
	if none != 1 {
		t.Errorf("a cardless machine probed %d times, want once", none)
	}
}

// TestProvider_ConcurrentListingsStillProbeOnce.
//
// sync.Once gave this for free and a mutex has to be held across the probe to
// keep it: `ember serve` answers `/api/offers` from the HTTP server's
// goroutines, so two polls really can arrive together. Written because the
// obvious tidy-up -- probe first, lock only to store the answer -- is correct
// under the race detector and execs once per concurrent caller, which is the
// cost the cache exists to avoid.
func TestProvider_ConcurrentListingsStillProbeOnce(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	p := &Provider{Detect: func(context.Context) (GPU, bool) {
		mu.Lock()
		calls++
		mu.Unlock()
		// Long enough that a second caller is certainly inside gpuNow while
		// this one is still probing.
		time.Sleep(20 * time.Millisecond)
		return testGPU, true
	}}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if g := p.gpuNow(t.Context()); g != testGPU {
				t.Errorf("gpuNow = %+v, want %+v", g, testGPU)
			}
		}()
	}
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Errorf("eight concurrent listings ran nvidia-smi %d times, want once", calls)
	}
}
