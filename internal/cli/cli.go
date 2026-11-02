// Package cli is ember's subcommand dispatch.
//
// Dispatch is a hand-written switch over flag.NewFlagSet, as in go-tun and
// transcription. No cobra: the whole surface is a handful of verbs, and a
// framework would add a dependency, a code-generation step and a help format
// nobody chose, to save about forty lines.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/Legion112/ember/internal/app"
)

// outf writes to the command's output stream.
//
// The error is discarded deliberately: a failed write to stdout means the pipe
// is gone — `ember ls | head` is the ordinary way that happens — and the exit
// status is about to be the only thing anyone sees. Returning the error would
// turn a closed pager into a non-zero exit, which in this program means
// "there are findings".
func outf(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

// ExitCodes. These are part of ember's contract with cron and CI, not
// decoration: `ember ls` is meant to be run from a timer, and "there are
// findings" has to be distinguishable from "ember itself broke" without
// parsing output.
const (
	ExitOK       = 0
	ExitError    = 1
	ExitFindings = 2
	ExitUsage    = 3
)

// FindingsError means ember worked and found something the operator must look
// at — an orphaned node, a node past its deadline, a provider that could not
// be reached.
type FindingsError struct{ N int }

func (e *FindingsError) Error() string {
	return fmt.Sprintf("%d finding(s); see the listing above", e.N)
}

type usageError struct{ error }

// ExitCode maps an error from Run onto a process exit status.
func ExitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var f *FindingsError
	if errors.As(err, &f) {
		return ExitFindings
	}
	var u usageError
	if errors.As(err, &u) {
		return ExitUsage
	}
	// And the same answer for a refusal raised inside internal/app, which
	// cannot construct the unexported type above.
	//
	// Without this line every refusal that moved out of this package -- "no
	// image", "that is not a port", "a serve command chose its own listen
	// address" -- would silently become exit 1, and exit 3 is a documented
	// contract with cron and CI rather than decoration. Five of the tests
	// covering those paths assert only that an error came back, so they
	// would not have noticed.
	if errors.Is(err, app.ErrUsage) {
		return ExitUsage
	}
	return ExitError
}

type command struct {
	name    string
	summary string
	run     func(args []string, out io.Writer) error
}

// commands is the dispatch table. Output goes through an injected io.Writer so
// that every subcommand is testable without capturing os.Stdout.
func commands() []command {
	return []command{
		{"version", "print the ember version", cmdVersion},
		{"up", "rent a node, forward its ports to loopback, destroy it", cmdUp},
		{"down", "destroy a node by handle", cmdDown},
		{"ls", "list nodes ember created, across every configured provider", cmdLs},
		{"serve", "run the web UI and a queue that drains onto one rental", cmdServe},
		{"reap", "destroy every node past its deadline", cmdReap},
		{"spend", "report what the recorded generations cost", cmdSpend},
		{"history", "list the generations ember has run, and measure or prune what they left", cmdHistory},
		{"offers", "list rentable offers matching a filter", cmdOffers},
		{"store", "create or destroy persistent storage that outlives a node", cmdStore},
		{"catalogue", "record what a provider has available, and read the series back", cmdCatalogue},
		{"archs", "print the CUDA architectures ember publishes an image for", cmdArchs},
		{"images", "assemble an images: block from the digests a build published", cmdImages},
	}
}

// Run dispatches args. It returns nil on success and an error whose ExitCode
// the caller uses as the process status.
func Run(args []string) error {
	return run(args, os.Stdout)
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return usageError{errors.New("no subcommand given")}
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage(out)
		return nil
	}
	for _, c := range commands() {
		if c.name == args[0] {
			return c.run(args[1:], out)
		}
	}
	usage(out)
	return usageError{fmt.Errorf("unknown subcommand %q", args[0])}
}

func usage(out io.Writer) {
	outf(out, "ember rents a GPU node, runs a container on it, and destroys it.\n\nusage: ember <subcommand> [flags]\n\n")
	cs := commands()
	sort.Slice(cs, func(i, j int) bool { return cs[i].name < cs[j].name })
	// Width from the longest name rather than a literal. It was %-8s, and
	// `catalogue` is nine characters: the summary column jumped a space out
	// on that one line and lined up with nothing. Found by reading the
	// output, which is also the only way it could have been found -- every
	// test here asserts on substrings.
	width := 0
	for _, c := range cs {
		if len(c.name) > width {
			width = len(c.name)
		}
	}
	for _, c := range cs {
		outf(out, "  %-*s %s\n", width, c.name, c.summary)
	}
	outf(out, "\nRun ember <subcommand> -h for the flags of one subcommand.\n")
}

// Quote is a small helper for error messages that embed user input.
func Quote(s string) string { return strings.TrimSpace(fmt.Sprintf("%q", s)) }
