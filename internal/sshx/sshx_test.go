package sshx

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestCommand_ValidateAcceptsOrdinaryPathsAndFlags(t *testing.T) {
	ok := []Command{
		Cmd("tar", "-czf", "-", "-C", "/workspace", "--", "out"),
		Cmd("tar", "-czf", "-", "-C", "/work-space_1/a.b", "--", "frame0001.png"),
		Cmd("true"),
		Cmd("env", "FOO=bar"),
		Cmd("x", "a:b", "a,b", "a+b"),
	}
	for _, c := range ok {
		if err := c.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", c.String(), err)
		}
	}
}

// TestQuote_NeutralisesHostileArguments.
//
// An SSH exec request is handed to the remote shell, so every one of these
// would run something if it reached the command line bare. Inside single
// quotes every character except the single quote itself is literal: no
// expansion, no substitution, no word splitting, no backslash escapes.
//
// ember used to refuse these instead, which was fine while every remote
// command was tar over config-file paths. A workload command cannot work that
// way -- a prompt is arbitrary text -- so the quoting exists now and this is
// where it is exercised. The lab in test/integration round-trips the same
// inputs through a real sshd and compares what arrived, which is the half a
// unit test cannot do.
func TestQuote_NeutralisesHostileArguments(t *testing.T) {
	for in, want := range map[string]string{
		// Nothing special: left bare, so ordinary commands stay readable.
		"tar":          "tar",
		"-czf":         "-czf",
		"/workspace/a": "/workspace/a",

		// Everything else is wrapped.
		"a b":            "'a b'",
		"$(touch pwned)": "'$(touch pwned)'",
		"`id`":           "'`id`'",
		"a;rm -rf /":     "'a;rm -rf /'",
		"a|b":            "'a|b'",
		"a&b":            "'a&b'",
		"a>b":            "'a>b'",
		"$HOME":          "'$HOME'",
		"a\nb":           "'a\nb'",
		"a*b":            "'a*b'",
		`a"b`:            `'a"b'`,
		"~/secrets":      "'~/secrets'",
		"":               "''",

		// The one character single quotes cannot contain: end the quote,
		// escape it, reopen.
		"a'b":    `'a'\''b'`,
		"'":      `''\'''`,
		"it's a": `'it'\''s a'`,
	} {
		if got := Quote(in); got != want {
			t.Errorf("Quote(%q) = %s, want %s", in, got, want)
		}
	}
}

// TestCommand_StringQuotesEveryPart, so an argument cannot escape into being
// a second command.
func TestCommand_StringQuotesEveryPart(t *testing.T) {
	got := Cmd("sd-cli", "-p", "a lovely cat; rm -rf /", "-o", "/workspace/out/cat.png").String()
	want := `sd-cli -p 'a lovely cat; rm -rf /' -o /workspace/out/cat.png`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// TestCommand_ValidateRefusesOnlyWhatCannotBeSent. A NUL byte cannot appear
// in an argument at all; everything else survives quoting, including a
// newline, which inside single quotes is a literal newline.
func TestCommand_ValidateRefusesOnlyWhatCannotBeSent(t *testing.T) {
	if err := Cmd("").Validate(); err == nil {
		t.Error("a command with no program was accepted")
	}
	if err := Cmd("tar\x00evil").Validate(); err == nil {
		t.Error("a NUL in the path was accepted")
	}
	if err := Cmd("tar", "a\x00b").Validate(); err == nil {
		t.Error("a NUL in an argument was accepted")
	}
	for _, ok := range []string{"a b", "a;b", "$(id)", "a\nb", "a'b"} {
		if err := Cmd("tar", ok).Validate(); err != nil {
			t.Errorf("Validate rejected %q, which quoting handles: %v", ok, err)
		}
	}
}

func TestExitError_Message(t *testing.T) {
	e := &ExitError{Cmd: "tar -czf -", Code: 2, Stderr: "tar: /nope: No such file or directory\n"}
	got := e.Error()
	// The stderr is the only thing that explains the exit: an error saying
	// "tar exited 2" without it sends the operator looking in the wrong place.
	for _, want := range []string{"tar -czf -", "2", "No such file or directory"} {
		if !strings.Contains(got, want) {
			t.Errorf("message %q is missing %q", got, want)
		}
	}
	if got := (&ExitError{Cmd: "x", Code: 1}).Error(); !strings.Contains(got, "exited 1") {
		t.Errorf("got %q", got)
	}
}

func TestRecordingRunner_ReturnsScriptedOutput(t *testing.T) {
	r := NewRecordingRunner()
	cmd := Cmd("tar", "-czf", "-")
	r.Outputs[cmd.String()] = []byte("archive bytes")

	stdout, wait, err := r.Run(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "archive bytes" {
		t.Errorf("got %q", got)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if calls := r.Calls(); len(calls) != 1 || calls[0] != cmd.String() {
		t.Errorf("calls = %v", calls)
	}
}

// TestRecordingRunner_RefusesWhatProductionWouldRefuse. A fake more permissive
// than the real thing is a fake that hides the bug.
func TestRecordingRunner_RefusesWhatProductionWouldRefuse(t *testing.T) {
	r := NewRecordingRunner()
	if _, _, err := r.Run(t.Context(), Cmd("tar\x00evil")); err == nil {
		t.Fatal("the fake accepted a command the real runner would refuse")
	}
	if len(r.Calls()) != 0 {
		t.Error("a refused command was recorded as having run")
	}
}

func TestRecordingRunner_Errors(t *testing.T) {
	r := NewRecordingRunner()
	cmd := Cmd("tar", "-czf", "-")
	want := errors.New("connection reset")
	r.Errors[cmd.String()] = want

	if _, _, err := r.Run(t.Context(), cmd); !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
}

// TestRecordingRunner_WaitsReproducesAStreamThatThenFails, which is the case
// that loses data quietly: a valid prefix of an archive plus a non-zero exit.
func TestRecordingRunner_WaitsReproducesAStreamThatThenFails(t *testing.T) {
	r := NewRecordingRunner()
	cmd := Cmd("tar", "-czf", "-")
	r.Outputs[cmd.String()] = []byte("partial")
	r.Waits[cmd.String()] = &ExitError{Cmd: cmd.String(), Code: 2}

	stdout, wait, err := r.Run(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(stdout); string(got) != "partial" {
		t.Errorf("got %q", got)
	}
	var exit *ExitError
	if err := wait(); !errors.As(err, &exit) {
		t.Fatalf("got %v", err)
	}
}

func TestRecordingRunner_FailOn(t *testing.T) {
	r := NewRecordingRunner()
	r.FailOn = "tar"
	if _, _, err := r.Run(t.Context(), Cmd("tar", "-czf", "-")); err == nil {
		t.Fatal("FailOn was ignored")
	}
	if _, _, err := r.Run(t.Context(), Cmd("true")); err != nil {
		t.Fatalf("an unrelated command failed: %v", err)
	}
}

func TestRecordingRunner_UnscriptedCommandReturnsNothing(t *testing.T) {
	r := NewRecordingRunner()
	stdout, wait, err := r.Run(t.Context(), Cmd("true"))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(stdout); len(got) != 0 {
		t.Errorf("got %q", got)
	}
	if err := wait(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordingRunner_HonoursCancellation(t *testing.T) {
	r := NewRecordingRunner()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := r.Run(ctx, Cmd("true")); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestContains(t *testing.T) {
	if contains("tar -czf", "tar") != true {
		t.Error("contains missed a substring")
	}
	if contains("tar", "") != false {
		t.Error("an empty needle should not match")
	}
	if contains("a", "abc") != false {
		t.Error("a longer needle matched a shorter string")
	}
}

var _ Runner = (*RecordingRunner)(nil)
