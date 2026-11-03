package local

import (
	"context"
	"os/exec"
	"strconv"
	"strings"

	"github.com/Legion112/ember/internal/node"
)

// GPU is what this machine has, as the local provider reports it.
//
// Zero means none, and that is the common case: CI has no card, and a laptop
// running the labs does not need one. Everything about the local provider has
// to keep working when this is empty, because that is what every existing lab
// depends on.
type GPU struct {
	// Model is ember's normalised name, for example "rtx2080ti".
	Model node.GPUModel

	// VRAM is the card's memory.
	VRAM node.MiB

	// ComputeCap is "7.5" as nvidia-smi spells it. Carried because it is the
	// number that decides whether the image can run here at all: a binary
	// built without this architecture aborts on such a card, with "no kernel
	// image is available for execution on the device", once something launches
	// a kernel. node.Offer.MeasuredCap is where it goes.
	ComputeCap string
}

// IsZero reports whether no GPU was found.
func (g GPU) IsZero() bool { return g.Model == "" || g.VRAM == 0 }

// DetectGPU asks nvidia-smi what this machine has, and says whether it got an
// answer.
//
// A subprocess rather than a library, which is the same bargain ember already
// makes with the Docker CLI's daemon: the alternative is cgo and NVML, a
// build dependency on the CUDA toolkit, for one query.
//
// A FAILURE IS A FACT ABOUT THE MACHINE OR A FACT ABOUT THE CALL, AND THOSE
// ARE DIFFERENT ANSWERS. No card, no driver, no nvidia-smi at all, or an
// nvidia-smi that ran and complained are all facts about the machine: they
// answer "none", with no error, because that is the normal case for CI and for
// anyone running the labs, and a provider that refused to list offers over a
// hardware query would break every existing lab on every machine that never
// had a card. A context that ended first is a fact about the *caller* --
// exec.CommandContext kills the child, so Output fails for a reason that says
// nothing about the hardware -- and that answer is `false`.
//
// Two results rather than one, the shape node.Exposure() already uses for
// "published nothing" against "never reported": the collapse there was the
// same one, and the defect here was that gpuNow memoised a cancelled probe as
// "this machine has no GPU" for the life of an `ember serve`. The three
// reachable states are a card, no card, and nobody could ask.
func DetectGPU(ctx context.Context) (GPU, bool) {
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,compute_cap", "--format=csv,noheader,nounits").Output()
	if err != nil {
		// ctx and not the error's own shape. A cancelled exec.Cmd surfaces as
		// a kill signal, a Start failure or a context error depending on
		// where it was when the cancel landed, so classifying the error would
		// be three spellings of one question; the context answers it once.
		// Checked only here, because an Output that succeeded is an answer
		// whatever the context did afterwards.
		return GPU{}, ctx.Err() == nil
	}
	return parseSMI(string(out)), true
}

// parseSMI reads the first row of nvidia-smi's CSV.
//
// The first row only. A machine with several cards gets one offer for one
// card, because ember rents one node and sd-cli is pointed at one device;
// pretending otherwise would be an offer nothing can honour.
func parseSMI(out string) GPU {
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Split(line, ",")
		if len(parts) < 3 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		mib, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || mib <= 0 {
			continue
		}
		cap := strings.TrimSpace(parts[2])
		if name == "" {
			continue
		}
		// node.NormalizeGPU, and not a speller of this package's own.
		//
		// There used to be one here, and it produced a different answer for
		// the same card: "NVIDIA A100-SXM4-80GB" became "a100sxm480gb" where
		// every other provider calls it "a100-80g". GPUModel.Accepts requires
		// the next character after a prefix to be the capacity separator, so
		// `--gpu a100` matched nothing at all for a datacentre card in a
		// local machine -- and `--gpu h100` and `--gpu rtxpro6000` likewise,
		// five of the eleven real nvidia-smi names checked. The filter
		// compares one spelling, so there is one speller, and it is the one
		// that knows about noise words and capacity suffixes.
		return GPU{Model: node.NormalizeGPU(name), VRAM: node.MiB(mib), ComputeCap: cap}
	}
	return GPU{}
}
