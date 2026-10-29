package workload

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
)

func TestRun_StreamsOutputAndSucceeds(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd := []string{"sd-cli", "-p", "a lovely cat", "-o", "/workspace/out/cat.png"}
	line := Describe(cmd)
	r.Outputs[line] = []byte("loading model\ngenerating\ndone\n")

	var out bytes.Buffer
	res, err := Run(t.Context(), r, cmd, &out)
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit = %d", res.ExitCode)
	}
	if !strings.Contains(out.String(), "generating") {
		t.Errorf("the output was not streamed: %q", out.String())
	}
	if calls := r.Calls(); len(calls) != 1 || calls[0] != line {
		t.Errorf("calls = %v", calls)
	}
}

// TestRun_AFailedWorkloadCarriesItsExitCode. A workload that fails is not a
// transport failure, and the two need different responses: one is the
// operator's command, the other is the connection.
func TestRun_AFailedWorkloadCarriesItsExitCode(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd := []string{"sd-cli", "-p", "x"}
	line := Describe(cmd)
	r.Outputs[line] = []byte("loading model\n")
	r.Waits[line] = &sshx.ExitError{Cmd: line, Code: 2, Stderr: "no such model"}

	var out bytes.Buffer
	res, err := Run(t.Context(), r, cmd, &out)
	if err == nil {
		t.Fatal("a failing workload was reported as success")
	}
	if res.ExitCode != 2 {
		t.Errorf("exit = %d, want 2", res.ExitCode)
	}
	// What it managed to print before failing is kept: a workload that spends
	// four minutes loading a model and then fails deserves to have shown what
	// it was doing.
	if !strings.Contains(out.String(), "loading model") {
		t.Errorf("the partial output was dropped: %q", out.String())
	}
	if !strings.Contains(err.Error(), "no such model") {
		t.Errorf("stderr is missing from the error: %v", err)
	}
}

// TestRun_ATransportFailureIsNotAnExitCode. Minus one, because the command
// never produced a status at all.
func TestRun_ATransportFailureIsNotAnExitCode(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd := []string{"sd-cli", "-p", "x"}
	r.Errors[Describe(cmd)] = errors.New("connection reset")

	res, err := Run(t.Context(), r, cmd, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a transport failure was reported as success")
	}
	if res.ExitCode != -1 {
		t.Errorf("exit = %d, want -1 for a command that never ran", res.ExitCode)
	}
}

func TestRun_RefusesAnEmptyCommand(t *testing.T) {
	if _, err := Run(t.Context(), sshx.NewRecordingRunner(), nil, &bytes.Buffer{}); err == nil {
		t.Fatal("an empty command was accepted")
	}
}

func TestRun_HonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Run(ctx, sshx.NewRecordingRunner(), []string{"sd-cli"}, &bytes.Buffer{}); err == nil {
		t.Fatal("a cancelled run reported success")
	}
}

// TestDescribe_ShowsTheCommandExactlyAsSent, quoting included, so an operator
// debugging a prompt that did not arrive as intended can see what the remote
// shell was handed.
func TestDescribe(t *testing.T) {
	got := Describe([]string{"sd-cli", "-p", "a lovely cat", "--steps", "4"})
	want := "sd-cli -p 'a lovely cat' --steps 4"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
	if Describe(nil) != "" {
		t.Error("an empty command described itself")
	}
}

// TestValidate_RefusesWhatCannotBeSent, before a node is rented. A config
// error found after the create call is one found after the billing started.
func TestValidate(t *testing.T) {
	if err := Validate(nil); err != nil {
		t.Errorf("no command is not an error: %v", err)
	}
	if err := Validate([]string{"sd-cli", "-p", "a cat; rm -rf /"}); err != nil {
		t.Errorf("a prompt with metacharacters is quoted, not refused: %v", err)
	}
	for _, bad := range [][]string{
		{""},
		{"  "},
		{"sd-cli\x00evil"},
		{"sd-cli", "a\x00b"},
	} {
		if err := Validate(bad); err == nil {
			t.Errorf("Validate(%q) was accepted", bad)
		}
	}
}

func TestOutputs(t *testing.T) {
	if Outputs(node.Spec{}) {
		t.Error("a spec with no outputs reported some")
	}
	if !Outputs(node.Spec{Outputs: []node.OutputPath{{Remote: "/a", Local: "b"}}}) {
		t.Error("a spec with outputs reported none")
	}
}

// failingReader is a stdout that breaks partway, which is what a dropped
// connection looks like to the copy.
type failingReader struct{ read bool }

func (f *failingReader) Read(p []byte) (int, error) {
	if !f.read {
		f.read = true
		n := copy(p, []byte("partial output\n"))
		return n, nil
	}
	return 0, errors.New("connection reset mid-stream")
}

func (f *failingReader) Close() error { return nil }

// brokenRunner hands back a stdout that fails partway through.
type brokenRunner struct{ wait error }

func (b brokenRunner) Run(context.Context, sshx.Command) (io.ReadCloser, func() error, error) {
	return &failingReader{}, func() error { return b.wait }, nil
}

// TestRun_AnOutputItCouldNotReadIsAFailureEvenOnAZeroExit.
//
// The remote command succeeded and ember did not see all of what it printed.
// Reporting success would mean an operator believing they have the whole log
// of a run they cannot repeat, which is worse than a clear failure.
func TestRun_AnOutputItCouldNotReadIsAFailureEvenOnAZeroExit(t *testing.T) {
	var out bytes.Buffer
	res, err := Run(t.Context(), brokenRunner{}, []string{"sd-cli"}, &out)
	if err == nil {
		t.Fatal("a truncated output stream was reported as success")
	}
	if res.ExitCode != -1 {
		t.Errorf("exit = %d, want -1", res.ExitCode)
	}
	// What did arrive is still shown.
	if !strings.Contains(out.String(), "partial output") {
		t.Errorf("got %q", out.String())
	}
}

// TestRun_AFailureExplainsATruncatedStream. When the command also failed, the
// failure is the explanation for the short read, so that is what is reported.
func TestRun_AFailureExplainsATruncatedStream(t *testing.T) {
	r := brokenRunner{wait: &sshx.ExitError{Cmd: "sd-cli", Code: 137, Stderr: "killed"}}
	res, err := Run(t.Context(), r, []string{"sd-cli"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a killed workload was reported as success")
	}
	if res.ExitCode != 137 {
		t.Errorf("exit = %d, want the command's own status", res.ExitCode)
	}
}

// TestRun_AnUnexpectedWaitFailureIsNotAnExitCode.
func TestRun_AnUnexpectedWaitFailureIsNotAnExitCode(t *testing.T) {
	r := brokenRunner{wait: errors.New("session closed")}
	res, err := Run(t.Context(), r, []string{"sd-cli"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("a broken session was reported as success")
	}
	if res.ExitCode != -1 {
		t.Errorf("exit = %d, want -1", res.ExitCode)
	}
}
