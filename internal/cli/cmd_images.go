package cli

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Legion112/ember/internal/node"
)

func cmdImages(args []string, out io.Writer) error { return runImages(args, out) }

// runImages assembles one pasteable `images:` block from per-architecture
// fragments, and refuses when an architecture produced none.
//
// A matrix job cannot write a whole map: GitHub Actions does not merge matrix
// job outputs, and a single output would be last-writer-wins -- six digests
// silently gone, and an operator pasting the survivor over a complete map
// takes six cards away from themselves. So each job writes its own fragment
// and this reads them back.
//
// The completeness check is the reason this is Go and not a shell loop in the
// workflow. A `for` loop that errors on a missing file is a branch that never
// runs until the day it matters, and a snippet in a YAML file cannot be
// tested at all. "Published nothing" and "never reported" are different
// answers, and the exit status is how the difference is said out loud -- the
// same reason `ember ls --all` exits non-zero when a provider could not be
// reached.
func runImages(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("images", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		from  = fs.String("from", "", "directory of per-architecture fragments")
		archs = fs.String("archs", "", "architectures that must be present (default: every one ember publishes)")
	)
	fs.Usage = func() {
		outf(out, "ember images assembles an images: block from per-architecture fragments.\n\n")
		outf(out, "Each fragment is a file containing one `sm_NN: <reference>` line, which is what\n")
		outf(out, "a build job writes. Exits non-zero naming any architecture that produced none.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("images takes no arguments, got %q", fs.Arg(0))}
	}
	if *from == "" {
		return usageError{fmt.Errorf("no fragment directory: pass -from")}
	}

	want, err := archsToPrint(*archs)
	if err != nil {
		return usageError{err}
	}

	found, err := readFragments(*from)
	if err != nil {
		return err
	}

	var missing []string
	for _, c := range want {
		if _, ok := found[c]; !ok {
			missing = append(missing, c.Key())
		}
	}

	// Printed before the refusal, not instead of it. A run where one
	// architecture failed still published the others, and their digests are
	// worth having -- the operator just must not mistake the block for a
	// complete one, which is what the error and the exit status are for.
	outf(out, "images:\n")
	for _, c := range want {
		ref, ok := found[c]
		if !ok {
			continue
		}
		outf(out, "  %s: %s\n", c.Key(), ref)
	}

	if len(missing) > 0 {
		return fmt.Errorf("no image was published for: %s.\n"+
			"The block above is incomplete, and a card with no entry is a card ember will refuse "+
			"to rent -- so pasting it over a complete map would take those cards away.\n"+
			"Rebuild just those with `make publish-sd-cuda CUDA_ARCH=<n>`, or dispatch the workflow "+
			"with archs set to them", strings.Join(missing, ", "))
	}
	return nil
}

// readFragments reads every `sm_NN: <reference>` line under dir.
//
// Every reference is validated the same way a config's would be, because this
// output is meant to be pasted into one: a digest that could not have come
// from a push, or a tag naming an architecture other than its key, is a
// mis-paste that is cheaper to catch here than in the config it ends up in.
func readFragments(dir string) (map[node.ComputeCap]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the fragment directory: %w", err)
	}

	out := map[node.ComputeCap]string{}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		raw, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a directory the operator named
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			key, ref, ok := strings.Cut(strings.TrimSpace(line), ":")
			if !ok {
				continue
			}
			key = strings.TrimSpace(key)
			if !strings.HasPrefix(key, "sm_") {
				continue
			}
			cap, err := node.ParseImageKey(key)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			ref = strings.TrimSpace(ref)
			if err := node.ValidateImage(ref); err != nil {
				return nil, fmt.Errorf("%s: %s: %w", name, key, err)
			}
			if err := node.ValidateImageForArch(ref, cap); err != nil {
				return nil, fmt.Errorf("%s: %w", name, err)
			}
			if was, dup := out[cap]; dup && was != ref {
				return nil, fmt.Errorf("two fragments disagree about %s: %q and %q", cap, was, ref)
			}
			out[cap] = ref
		}
	}
	return out, nil
}
