package cli

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRun_VersionPrintsIt(t *testing.T) {
	var out bytes.Buffer
	if err := run([]string{"version"}, &out); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(out.String()); got != Version {
		t.Errorf("version printed %q, want %q", got, Version)
	}
}

func TestRun_NoSubcommandIsAUsageError(t *testing.T) {
	var out bytes.Buffer
	err := run(nil, &out)
	if err == nil {
		t.Fatal("running with no subcommand should fail")
	}
	if got := ExitCode(err); got != ExitUsage {
		t.Errorf("ExitCode = %d, want %d", got, ExitUsage)
	}
	if !strings.Contains(out.String(), "usage: ember") {
		t.Error("usage should be printed when no subcommand is given")
	}
}

func TestRun_UnknownSubcommandNamesIt(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"frobnicate"}, &out)
	if err == nil || !strings.Contains(err.Error(), "frobnicate") {
		t.Fatalf("error should name the unknown subcommand, got %v", err)
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("an unknown subcommand is a usage error")
	}
}

func TestRun_HelpIsNotAnError(t *testing.T) {
	for _, arg := range []string{"-h", "--help", "help"} {
		var out bytes.Buffer
		if err := run([]string{arg}, &out); err != nil {
			t.Errorf("%s returned %v, want nil", arg, err)
		}
		if !strings.Contains(out.String(), "usage: ember") {
			t.Errorf("%s did not print usage", arg)
		}
	}
}

// TestUsage_ListsEveryCommand: the dispatch table is the documentation, so a
// verb that exists but is undocumented is a bug in one of the two.
func TestUsage_ListsEveryCommand(t *testing.T) {
	var out bytes.Buffer
	usage(&out)
	for _, c := range commands() {
		if !strings.Contains(out.String(), c.name) {
			t.Errorf("usage does not mention %q", c.name)
		}
		if c.summary == "" {
			t.Errorf("%q has no summary", c.name)
		}
	}
}

// TestExitCode_DistinguishesFindingsFromFailure is the contract with cron:
// "there are orphaned nodes" and "ember itself broke" must not look the same.
func TestExitCode_DistinguishesFindingsFromFailure(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"nil":           {nil, ExitOK},
		"plain":         {errors.New("boom"), ExitError},
		"findings":      {&FindingsError{N: 3}, ExitFindings},
		"wrapped find":  {fmt.Errorf("listing: %w", &FindingsError{N: 1}), ExitFindings},
		"usage":         {usageError{errors.New("bad flag")}, ExitUsage},
		"wrapped usage": {fmt.Errorf("parse: %w", usageError{errors.New("x")}), ExitUsage},
	} {
		if got := ExitCode(tc.err); got != tc.want {
			t.Errorf("%s: ExitCode = %d, want %d", name, got, tc.want)
		}
	}
}

func TestFindingsError_CountsThem(t *testing.T) {
	if got := (&FindingsError{N: 2}).Error(); !strings.Contains(got, "2 finding") {
		t.Errorf("Error() = %q", got)
	}
}

func TestVersionFlagParseFailureIsUsage(t *testing.T) {
	var out bytes.Buffer
	err := run([]string{"version", "-nope"}, &out)
	if err == nil || ExitCode(err) != ExitUsage {
		t.Fatalf("a bad flag should be a usage error, got %v", err)
	}
}

func TestQuote(t *testing.T) {
	if got := Quote("a b"); got != `"a b"` {
		t.Errorf("Quote = %s", got)
	}
}

// TestRun_WrapsRunWithStdout covers the one-line adapter. It is trivial, and
// it is exactly the kind of line that gets changed to os.Stderr by accident
// and then silently sends ember's output to the wrong stream in a pipeline.
func TestRun_WrapsRunWithStdout(t *testing.T) {
	if err := Run([]string{"version"}); err != nil {
		t.Fatalf("Run(version) = %v", err)
	}
	if err := Run([]string{"definitely-not-a-subcommand"}); ExitCode(err) != ExitUsage {
		t.Fatalf("Run with a bad subcommand returned %v", err)
	}
}

// TestCommands_EveryEntryReachesItsCommand.
//
// The table is the only thing connecting a name an operator types to the code
// that runs. Every test elsewhere calls runDown/runLs/runUp directly with
// injected deps, so a table entry pointing at the wrong function -- or at a
// function that panics before parsing -- would ship unnoticed.
//
// `-h` is used because it is answered by the flag parser before any provider
// is contacted, so this touches no network and rents nothing.
func TestCommands_EveryEntryReachesItsCommand(t *testing.T) {
	for _, c := range commands() {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			err := c.run([]string{"-h"}, &out)

			// version takes no flags and simply prints; everything else
			// answers -h with a usage error. Either is a command that ran.
			if err != nil && ExitCode(err) != ExitUsage {
				t.Errorf("%s -h = %v (exit %d), want success or a usage error", c.name, err, ExitCode(err))
			}
			if out.Len() == 0 {
				t.Errorf("%s -h printed nothing, so the table entry may not reach a command", c.name)
			}
			if c.summary == "" {
				t.Errorf("%s has no summary, so `ember` with no subcommand cannot describe it", c.name)
			}
		})
	}
}
