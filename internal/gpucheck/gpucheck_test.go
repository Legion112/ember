package gpucheck

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Legion112/ember/internal/sshx"
)

// scriptedRunner answers differently on each attempt.
//
// sshx.RecordingRunner is keyed by the rendered command, so it gives the same
// answer every time -- which cannot express the behaviour this package exists
// for: a card that refuses and then, moments later, works.
type scriptedRunner struct {
	mu sync.Mutex

	// exits is the exit code per attempt. The last entry repeats forever, so
	// a script of [1] is "always refuses".
	exits []int

	// transport, when true at an index, makes that attempt fail the way a
	// refused SSH channel does: no exit status at all.
	transport []bool

	stderr string
	calls  []string
}

func (s *scriptedRunner) Run(_ context.Context, cmd sshx.Command) (io.ReadCloser, func() error, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	i := len(s.calls)
	s.calls = append(s.calls, cmd.String())

	at := func(n int) int {
		if n >= len(s.exits) {
			n = len(s.exits) - 1
		}
		return s.exits[n]
	}
	isTransport := i < len(s.transport) && s.transport[i]

	if cmd.Stderr != nil && s.stderr != "" {
		_, _ = io.WriteString(cmd.Stderr, s.stderr)
	}
	if isTransport {
		return nil, nil, errors.New("ssh: channel refused")
	}

	code := at(i)
	body := io.NopCloser(strings.NewReader(""))
	if code == 0 {
		return body, func() error { return nil }, nil
	}
	return body, func() error {
		return &sshx.ExitError{Cmd: cmd.String(), Code: code, Stderr: s.stderr}
	}, nil
}

func (s *scriptedRunner) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

var probe = []string{"sh", "-c", "sd-cli --list-devices | grep -qi cuda"}

func TestWait_NoProbeDoesNothing(t *testing.T) {
	r := &scriptedRunner{exits: []int{1}}
	if err := Wait(t.Context(), r, nil, time.Minute, io.Discard, nil); err != nil {
		t.Fatalf("Wait with no probe = %v, want nil", err)
	}
	if n := r.count(); n != 0 {
		t.Errorf("ran %d command(s) for an empty probe; a config that names no probe must be left alone", n)
	}
}

func TestWait_PassesFirstTime(t *testing.T) {
	r := &scriptedRunner{exits: []int{0}}
	var out strings.Builder
	if err := Wait(t.Context(), r, probe, time.Minute, &out, nil); err != nil {
		t.Fatalf("Wait = %v, want nil", err)
	}
	if n := r.count(); n != 1 {
		t.Errorf("probed %d times, want 1", n)
	}
	// Nothing is said when nothing was waited for: a line on every run would
	// train the operator to ignore it.
	if out.String() != "" {
		t.Errorf("printed %q for a card that was ready immediately", out.String())
	}
}

// TestWait_RetriesUntilTheCardComesUp is the behaviour this package exists
// for. The driver said "not yet", and ember used to give up on the first
// refusal.
func TestWait_RetriesUntilTheCardComesUp(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &scriptedRunner{exits: []int{1, 1, 0}}
		var out strings.Builder
		if err := Wait(t.Context(), r, probe, time.Minute, &out, nil); err != nil {
			t.Fatalf("Wait = %v, want nil once the probe passes", err)
		}
		if n := r.count(); n != 3 {
			t.Errorf("probed %d times, want 3", n)
		}
		if got := out.String(); !strings.Contains(got, "became usable") {
			t.Errorf("a run that waited said nothing about it: %q", got)
		}
	})
}

func TestWait_RetriesATransportFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// sshd on a node that is still settling refuses the channel, which
		// must not be mistaken for a card that will never work.
		r := &scriptedRunner{exits: []int{0}, transport: []bool{true, true}}
		if err := Wait(t.Context(), r, probe, time.Minute, io.Discard, nil); err != nil {
			t.Fatalf("Wait = %v, want nil", err)
		}
		if n := r.count(); n != 3 {
			t.Errorf("probed %d times, want 3", n)
		}
	})
}

func TestWait_GivesUpAndSaysWhy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &scriptedRunner{exits: []int{1}, stderr: "Using CPU backend"}
		err := Wait(t.Context(), r, probe, 30*time.Second, io.Discard, nil)
		if err == nil {
			t.Fatal("a card that never came up was accepted")
		}

		var nr *NotReadyError
		if !errors.As(err, &nr) {
			t.Fatalf("got %T, want *NotReadyError so a caller can tell this from every other failure", err)
		}
		if nr.ExitCode != 1 {
			t.Errorf("ExitCode = %d, want 1; workload.Run reports a non-zero exit as both an error and a "+
				"code, and reading only the error loses the code", nr.ExitCode)
		}
		if nr.Attempts < 2 {
			t.Errorf("Attempts = %d; it gave up without retrying", nr.Attempts)
		}
		// A probe that ran and said no is not a probe that could not be run.
		// Conflating them sends the operator looking at their SSH connection
		// when the answer was "this card does not work".
		if nr.Err != nil {
			t.Errorf("Err = %v for a probe that exited cleanly with status %d; "+
				"a refusal is not a transport failure", nr.Err, nr.ExitCode)
		}

		msg := err.Error()
		for _, want := range []string{
			"never became usable",
			"the probe exited 1",
			"Nothing was run against it",
			"renting again lands elsewhere",
			"Using CPU backend",
		} {
			if !strings.Contains(msg, want) {
				t.Errorf("the refusal does not mention %q:\n%s", want, msg)
			}
		}
	})
}

func TestWait_ReportsATransportFailureAsOne(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &scriptedRunner{exits: []int{0}, transport: []bool{true, true, true, true, true, true, true, true}}
		err := Wait(t.Context(), r, probe, 20*time.Second, io.Discard, nil)
		var nr *NotReadyError
		if !errors.As(err, &nr) {
			t.Fatalf("got %v, want *NotReadyError", err)
		}
		if nr.Err == nil {
			t.Error("a transport failure was reported with no underlying error")
		}
		if !strings.Contains(err.Error(), "could not be run") {
			t.Errorf("a transport failure reads like a refusal: %v", err)
		}
	})
}

func TestWait_StopsOnCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		r := &scriptedRunner{exits: []int{1}}

		done := make(chan error, 1)
		go func() { done <- Wait(ctx, r, probe, time.Hour, io.Discard, nil) }()

		synctest.Wait()
		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Wait = %v, want context.Canceled; an operator pressing Ctrl-C must not wait out the timeout", err)
		}
	})
}

func TestWait_ZeroTimeoutUsesTheDefault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &scriptedRunner{exits: []int{1}}
		start := time.Now()
		if err := Wait(t.Context(), r, probe, 0, io.Discard, nil); err == nil {
			t.Fatal("want a refusal")
		}
		// Not an exact equality: the last sleep is clamped to the remaining
		// time, so the total is the default and not a multiple of Interval.
		if waited := time.Since(start); waited < DefaultTimeout {
			t.Errorf("waited %s for a zero timeout, want at least the %s default", waited, DefaultTimeout)
		}
	})
}

func TestWait_NegativeTimeoutUsesTheDefault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &scriptedRunner{exits: []int{1}}
		start := time.Now()
		if err := Wait(t.Context(), r, probe, -time.Minute, io.Discard, nil); err == nil {
			t.Fatal("want a refusal")
		}
		if waited := time.Since(start); waited < DefaultTimeout {
			t.Errorf("waited %s for a negative timeout, want the %s default", waited, DefaultTimeout)
		}
	})
}

func TestWait_SendsTheProbeExactly(t *testing.T) {
	r := &scriptedRunner{exits: []int{0}}
	if err := Wait(t.Context(), r, probe, time.Minute, io.Discard, nil); err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	got := r.calls[0]
	r.mu.Unlock()

	// The pipeline must survive intact: the whole point of taking a command
	// rather than a built-in check is that an operator can write one.
	if !strings.Contains(got, "sd-cli --list-devices") || !strings.Contains(got, "grep -qi cuda") {
		t.Errorf("the probe was mangled on the way out: %s", got)
	}
}

func TestWait_RefusesAnUnquotableProbe(t *testing.T) {
	// sshx.Command.Validate refuses what the real runner would refuse. A
	// probe that cannot be sent must fail at once rather than be retried for
	// the whole timeout: this test used to take a real minute, which is how
	// the bug was found.
	r := &scriptedRunner{exits: []int{0}}
	start := time.Now()
	err := Wait(t.Context(), r, []string{"sh", "-c", "echo \x00"}, time.Minute, io.Discard, nil)
	if err == nil {
		t.Fatal("an unsendable probe was accepted")
	}
	if waited := time.Since(start); waited > 5*time.Second {
		t.Errorf("took %s to reject a malformed probe; it was retried instead of refused", waited)
	}
	var nr *NotReadyError
	if errors.As(err, &nr) {
		t.Error("a malformed probe was blamed on the hardware; NotReadyError means the card was no good")
	}
	if n := r.count(); n != 0 {
		t.Errorf("sent %d command(s) for a probe that cannot be rendered", n)
	}
}

func TestLimitedWriter_CapsWithoutFailing(t *testing.T) {
	var b strings.Builder
	w := &limitedWriter{w: &b, n: 4}

	// Reports the full length each time: a probe that passed must not be
	// reported as failed because its output was long.
	if n, err := w.Write([]byte("abcdef")); n != 6 || err != nil {
		t.Errorf("Write = %d, %v; want 6, nil", n, err)
	}
	if n, err := w.Write([]byte("ghi")); n != 3 || err != nil {
		t.Errorf("Write past the cap = %d, %v; want 3, nil", n, err)
	}
	if b.String() != "abcd" {
		t.Errorf("kept %q, want the first 4 bytes", b.String())
	}
}

func TestTrimOutput_KeepsTheTail(t *testing.T) {
	// A failure says why at the end, so the tail is what survives.
	long := strings.Repeat("x", 4096) + "the actual reason"
	got := trimOutput(long)
	if !strings.Contains(got, "the actual reason") {
		t.Error("trimming dropped the end of the output, which is where the reason is")
	}
	if !strings.HasPrefix(got, "...") {
		t.Errorf("a trimmed output does not say it was trimmed: %q", got[:8])
	}
	if short := trimOutput("brief"); short != "brief" {
		t.Errorf("trimOutput(%q) = %q", "brief", short)
	}
}

// failingWriter is the case where the place we are keeping diagnostics breaks.
type failingWriter struct{ err error }

func (f failingWriter) Write([]byte) (int, error) { return 0, f.err }

func TestLimitedWriter_ReportsAnUnderlyingFailure(t *testing.T) {
	boom := errors.New("disk went away")

	// Under the cap: the error is the caller's to see.
	w := &limitedWriter{w: failingWriter{err: boom}, n: 16}
	if _, err := w.Write([]byte("abc")); !errors.Is(err, boom) {
		t.Errorf("Write = %v, want %v", err, boom)
	}

	// Straddling the cap takes the truncating branch, which writes a slice
	// and must report that failure too rather than claim success.
	w2 := &limitedWriter{w: failingWriter{err: boom}, n: 2}
	if _, err := w2.Write([]byte("abcdef")); !errors.Is(err, boom) {
		t.Errorf("Write across the cap = %v, want %v", err, boom)
	}
}

func TestNotReadyError_UnwrapsTheTransportFailure(t *testing.T) {
	// So a caller can ask errors.Is about the thing that actually went wrong
	// underneath, rather than matching on message text.
	boom := errors.New("ssh: channel refused")
	e := &NotReadyError{Probe: "p", Attempts: 3, Waited: time.Minute, ExitCode: -1, Err: boom}
	if !errors.Is(e, boom) {
		t.Error("NotReadyError does not unwrap to the error it wrapped")
	}

	// And a refusal with no underlying error unwraps to nothing, rather than
	// to some stand-in that a caller could mistake for a transport problem.
	if errors.Unwrap(&NotReadyError{ExitCode: 1}) != nil {
		t.Error("a probe that merely exited non-zero should wrap no error")
	}
}

func TestWait_DoesNotOvershootTheDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// 7s is deliberately not a multiple of the 5s Interval: the last
		// sleep has to be clamped to the 2s that remain, or the probe sleeps
		// past its own deadline and the operator pays for the overshoot.
		r := &scriptedRunner{exits: []int{1}}
		start := time.Now()
		if err := Wait(t.Context(), r, probe, 7*time.Second, io.Discard, nil); err == nil {
			t.Fatal("want a refusal")
		}
		waited := time.Since(start)
		if waited > 7*time.Second {
			t.Errorf("waited %s against a 7s timeout; the final sleep was not clamped", waited)
		}
		if n := r.count(); n != 3 {
			t.Errorf("probed %d times over 7s at a %s interval, want 3 (t=0, t=5, t=7)", n, Interval)
		}
	})
}

func TestWait_ToleratesANilWriter(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// The waited-and-recovered path prints; a nil writer must not panic,
		// because a progress line is not worth failing a run over.
		r := &scriptedRunner{exits: []int{1, 0}}
		if err := Wait(t.Context(), r, probe, time.Minute, nil, nil); err != nil {
			t.Fatalf("Wait with a nil writer = %v", err)
		}
	})
}
