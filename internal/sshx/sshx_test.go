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

// TestCommand_ValidateRefusesShellMetacharacters. An SSH exec request is
// handed to the remote shell, so these are refused rather than quoted: a
// quoting routine never exercised on a hostile input is one nobody knows is
// wrong, and the input here is the operator's own config, which is exactly the
// kind of thing that ends up holding a path with a space in it.
func TestCommand_ValidateRefusesShellMetacharacters(t *testing.T) {
	for _, arg := range []string{
		"", "a b", "a;b", "a&b", "a|b", "a>b", "a<b", "$(id)", "`id`",
		"a'b", `a"b`, "a\nb", "a\tb", "a*b", "a?b", "a(b", "a{b", "a~b",
		"a#b", "a!b", "a\\b", "a$b",
	} {
		if err := Cmd("tar", arg).Validate(); err == nil {
			t.Errorf("argument %q was accepted", arg)
		}
	}
	for _, path := range []string{"", "ta r", "tar;id", "/bin/$SHELL"} {
		if err := Cmd(path).Validate(); err == nil {
			t.Errorf("path %q was accepted", path)
		}
	}
}

func TestCommand_String(t *testing.T) {
	if got := Cmd("tar", "-czf", "-").String(); got != "tar -czf -" {
		t.Errorf("got %q", got)
	}
	if got := Cmd("true").String(); got != "true" {
		t.Errorf("got %q", got)
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
	if _, _, err := r.Run(t.Context(), Cmd("tar", "a;rm -rf /")); err == nil {
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
