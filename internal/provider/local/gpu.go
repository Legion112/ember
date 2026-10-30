package local

import (
	"context"
	"fmt"
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
	// built without this architecture and without PTX below it does not fail
	// on such a card, it falls back to the CPU backend.
	ComputeCap string
}

// IsZero reports whether no GPU was found.
func (g GPU) IsZero() bool { return g.Model == "" || g.VRAM == 0 }

// DetectGPU asks nvidia-smi what this machine has.
//
// A subprocess rather than a library, which is the same bargain ember already
// makes with the Docker CLI's daemon: the alternative is cgo and NVML, a
// build dependency on the CUDA toolkit, for one query.
//
// Every failure returns a zero GPU and no error. A machine with no card, no
// driver, or no nvidia-smi is not a broken machine -- it is the normal case
// for CI and for anyone running the labs -- and a provider that refused to
// list offers because a GPU query failed would break every existing lab on
// every machine that never had a card.
func DetectGPU(ctx context.Context) GPU {
	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,memory.total,compute_cap", "--format=csv,noheader,nounits").Output()
	if err != nil {
		return GPU{}
	}
	return parseSMI(string(out))
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
		return GPU{Model: NormaliseModel(name), VRAM: node.MiB(mib), ComputeCap: cap}
	}
	return GPU{}
}

// NormaliseModel turns nvidia-smi's name into the spelling an operator types.
//
// "NVIDIA GeForce RTX 2080 Ti" becomes "rtx2080ti". Lowercased, with the
// vendor words and all separators removed, because those are the parts that
// differ between a card's marketing name and what anybody would put in a
// config file.
func NormaliseModel(name string) node.GPUModel {
	s := strings.ToLower(name)
	for _, drop := range []string{"nvidia", "geforce", "quadro", "tesla"} {
		s = strings.ReplaceAll(s, drop, "")
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return node.GPUModel(b.String())
}

// ComputeArch renders a compute capability as the number CUDA_ARCHS wants:
// "7.5" becomes 75.
//
// Used only to tell an operator what to build, never to decide anything. The
// authority on what a binary carries is the binary, which the image lab reads
// directly.
func ComputeArch(cap string) (int, error) {
	maj, min, err := node.ParseCUDAVersion(cap)
	if err != nil {
		return 0, fmt.Errorf("compute capability %q: %w", cap, err)
	}
	return maj*10 + min, nil
}
