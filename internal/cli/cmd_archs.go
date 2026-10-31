package cli

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/Legion112/ember/internal/node"
)

func cmdArchs(args []string, out io.Writer) error { return runArchs(args, out) }

// runArchs prints the CUDA architectures ember publishes an image for.
//
// It exists so the build matrix can read the list instead of repeating it. The
// architectures are a property of the cards ember can select, which lives in
// internal/node/compute.go beside the table that is the only reason any of
// them is in the list -- and GitHub Actions cannot read Go. A literal
// [75, 80, 86, ...] in the workflow would be the same defect PUBLISH_LABS was
// moved to the Makefile for: a list that drifted from the list it mirrored,
// where the one publish it guarded was the one place it did not run.
//
// Also useful by hand: it answers "which key does this card need" without
// renting anything.
func runArchs(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("archs", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		asJSON = fs.Bool("json", false, "print a JSON array, for a build matrix")
		only   = fs.String("only", "", "comma-separated subset to print, for rebuilding one architecture")
		cards  = fs.Bool("cards", false, "print the cards each architecture is for")
	)
	fs.Usage = func() {
		outf(out, "ember archs prints the CUDA architectures ember publishes an image for.\n\n")
		outf(out, "The build matrix reads this rather than repeating the list.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("archs takes no arguments, got %q", fs.Arg(0))}
	}

	want, err := archsToPrint(*only)
	if err != nil {
		return usageError{err}
	}

	if *asJSON {
		b, err := json.Marshal(want)
		if err != nil {
			return err
		}
		outf(out, "%s\n", b)
		return nil
	}

	for _, c := range want {
		if *cards {
			var names []string
			for _, m := range node.CardsFor(c) {
				names = append(names, string(m))
			}
			outf(out, "%d\t%s\t%s\n", int(c), c.Key(), strings.Join(names, " "))
			continue
		}
		outf(out, "%d\n", int(c))
	}
	return nil
}

// archsToPrint resolves --only against the known set.
//
// An architecture nobody can rent a card for is refused here, by Go, naming
// the set it could have been. A typo in a workflow's dispatch box would
// otherwise buy twenty minutes of nvcc and an image no card needs.
func archsToPrint(only string) ([]node.ComputeCap, error) {
	known := node.KnownCaps()
	if strings.TrimSpace(only) == "" {
		return known, nil
	}

	in := map[node.ComputeCap]bool{}
	for _, c := range known {
		in[c] = true
	}

	seen := map[node.ComputeCap]bool{}
	var out []node.ComputeCap
	for _, f := range strings.Split(only, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		// Both spellings, because the matrix passes bare numbers and an
		// operator reading a config types the key.
		n, err := strconv.Atoi(strings.TrimPrefix(f, "sm_"))
		if err != nil {
			return nil, fmt.Errorf("%q is not an architecture: write it as 90 or sm_90", f)
		}
		c := node.ComputeCap(n)
		if !in[c] {
			return nil, fmt.Errorf("no card ember can select is %s, so there is no image to build "+
				"for it; ember publishes %v", c, known)
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("--only named no architectures")
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out, nil
}
