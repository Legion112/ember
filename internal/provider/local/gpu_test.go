package local

import (
	"context"
	"testing"

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
		"a datacentre card": {
			"NVIDIA A100-SXM4-80GB, 81920, 8.0\n",
			GPU{Model: "a100sxm480gb", VRAM: 81920, ComputeCap: "8.0"},
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

func TestNormaliseModel(t *testing.T) {
	for in, want := range map[string]node.GPUModel{
		"NVIDIA GeForce RTX 2080 Ti":    "rtx2080ti",
		"NVIDIA GeForce RTX 4090":       "rtx4090",
		"NVIDIA A100-SXM4-80GB":         "a100sxm480gb",
		"NVIDIA RTX A5000":              "rtxa5000",
		"Tesla V100-SXM2-16GB":          "v100sxm216gb",
		"NVIDIA H100 80GB HBM3":         "h10080gbhbm3",
		"Quadro RTX 8000":               "rtx8000",
		"NVIDIA RTX PRO 6000 Blackwell": "rtxpro6000blackwell",
	} {
		if got := NormaliseModel(in); got != want {
			t.Errorf("NormaliseModel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestComputeArch(t *testing.T) {
	for in, want := range map[string]int{
		"7.5":  75,
		"8.0":  80,
		"8.9":  89,
		"9.0":  90,
		"12.0": 120,
	} {
		got, err := ComputeArch(in)
		if err != nil {
			t.Errorf("ComputeArch(%q) = %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ComputeArch(%q) = %d, want %d", in, got, want)
		}
	}
	for _, bad := range []string{"", "7", "seven.five", "x.y"} {
		if _, err := ComputeArch(bad); err == nil {
			t.Errorf("ComputeArch(%q) accepted a bad capability", bad)
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
	g := DetectGPU(t.Context())
	if !g.IsZero() && g.Model == "" {
		t.Error("DetectGPU reported a present card with no model")
	}
	if !g.IsZero() && g.VRAM == 0 {
		t.Error("DetectGPU reported a present card with no VRAM")
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
	q := &Provider{Detect: func(context.Context) GPU {
		calls++
		return testGPU
	}}
	q.gpuNow(t.Context())
	q.gpuNow(t.Context())
	q.gpuNow(t.Context())
	if calls != 1 {
		t.Errorf("detected %d times, want once", calls)
	}
}
