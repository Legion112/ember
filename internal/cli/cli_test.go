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

func TestNotYet_FailsButNamesTheSubcommand(t *testing.T) {
	for _, c := range commands() {
		if !c.stub {
			continue
		}
		var out bytes.Buffer
		err := run([]string{c.name}, &out)
		if err == nil || !strings.Contains(err.Error(), c.name) {
			t.Errorf("%s: got %v, want an error naming the subcommand", c.name, err)
		}
		if ExitCode(err) != ExitError {
			t.Errorf("%s: an unimplemented subcommand is an ordinary error, not a usage error", c.name)
		}

		out.Reset()
		if err := run([]string{c.name, "-h"}, &out); err != nil {
			t.Errorf("%s: -h on an unimplemented subcommand should not fail: %v", c.name, err)
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
