// Package gpucheck waits for a rented GPU to become usable.
//
// It exists because of a run that cost $0.37 and produced nothing. ember
// rented an A100-SXM4-80GB, the node came up, the weights verified, and
// sd-cli reported:
//
//	ggml_cuda_init: failed to initialize CUDA: system not yet initialized
//	[W] auto-fit: no GPU devices; using the default backend
//	[V] Using CPU backend
//
// It then began a 32-billion-parameter diffusion model on 128 CPU threads. The
// workload was running, so nothing in ember thought anything was wrong; the
// text encoder alone took 122 seconds where a working card would have taken
// about one. Only a human reading the log noticed, twelve minutes in.
//
// Two lessons are encoded here, and the second one is the reason this is a
// loop rather than a check.
//
// The first is that "the workload started" is not the same as "the hardware
// works". A GPU that cannot initialize does not fail: it silently becomes a
// slow CPU, at the GPU's price.
//
// The second is that the driver said "not yet". On an SXM board the GPU sits
// on an NVSwitch fabric that nvidia-fabricmanager brings up on the host, and
// CUDA returns CUDA_ERROR_SYSTEM_NOT_READY while that is still in progress.
// ember probed once, at the earliest possible moment, and concluded the host
// was broken -- which it may well not have been. A board that is not ready
// yet is not a board that will never be ready, and the difference between
// those two is a retry.
//
// So: probe, retry until a deadline, and only then refuse. A host that is
// genuinely faulty costs the probe timeout instead of the whole run, and a
// host that was merely still booting its fabric is used rather than thrown
// away.
package gpucheck

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/workload"
)

// DefaultTimeout is how long a card gets to become usable.
//
// Two minutes because the thing being waited for is a host daemon settling,
// which takes seconds when it happens at all -- and because the wait is billed
// at the GPU's rate, so a generous default is a standing charge on every run
// that hits a dead host. Long enough to cover a fabric coming up, short enough
// that giving up costs about six cents on an A100.
const DefaultTimeout = 2 * time.Minute

// Interval between attempts.
//
// Fixed rather than exponential. The window being waited on is tens of
// seconds, so backoff would spend most of the budget asleep after two
// failures and turn a card that became ready at 40 seconds into a refusal.
const Interval = 5 * time.Second

// maxOutput caps what is kept from the probe to report on failure. A probe
// prints a device list; anything that prints megabytes is misconfigured, and
// holding it in memory to maybe print it later would be the bug.
const maxOutput = 8 << 10

// Wait runs probe until it exits zero, or until timeout elapses.
//
// An empty probe is not an error and does nothing: a config that names no
// probe gets the behaviour it had before this package existed. The decision to
// require one belongs to whoever writes the config, not here -- a CPU-only run
// has no card to check, and an image this package has never heard of has no
// command it could guess.
//
// The probe must exit zero when the GPU is usable and non-zero when it is not.
// ember deliberately does not parse its output: what counts as a working card
// depends on the image, and a tool whose contract is an exit status can be
// anything the operator can write, including a pipeline through grep.
func Wait(
	ctx context.Context,
	r sshx.Runner,
	probe []string,
	timeout time.Duration,
	out io.Writer,
	log *slog.Logger,
) error {
	if len(probe) == 0 {
		return nil
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}

	start := time.Now()
	deadline := start.Add(timeout)

	var (
		attempts int
		lastErr  error
		lastCode int
		lastOut  string
	)

	for {
		attempts++

		// Buffered rather than streamed. A probe that is retried would
		// otherwise print its complaint once per attempt, and the one time the
		// output matters is the last one -- so it is kept and printed only if
		// the card never comes up.
		var buf bytes.Buffer
		res, err := workload.Run(ctx, r, probe, &limitedWriter{w: &buf, n: maxOutput})
		switch {
		case err == nil && res.ExitCode == 0:
			if attempts > 1 {
				// Said out loud, because a run that waited is a run whose
				// bill includes the wait, and because a host that needed four
				// attempts today is worth knowing about.
				say(out, "the GPU became usable after %s (%d attempts)\n",
					time.Since(start).Round(time.Second), attempts)
			}
			log.Debug("gpu probe passed", "attempts", attempts, "took", time.Since(start))
			return nil
		case err != nil && res.ExitCode == 0:
			// The command never left the laptop: workload.Run returns a zero
			// Result when sshx.Command.Validate refuses the argv. That is a
			// malformed probe, not a card that is warming up, and no amount
			// of waiting will fix it -- so it fails now rather than burning
			// the whole timeout on a GPU that was probably fine.
			return fmt.Errorf("the gpu probe cannot be sent: %w", err)
		case res.ExitCode > 0:
			// A probe that ran and said no. workload.Run reports a non-zero
			// exit as an error *and* an exit code, so the code is what
			// distinguishes this from the transport case below -- reading
			// only the error would report every refusal as a broken
			// connection.
			lastErr, lastCode, lastOut = nil, res.ExitCode, buf.String()
		default:
			// ExitCode is -1: the command never produced a status. sshd on a
			// node that is still settling refuses a channel, which is
			// indistinguishable here from a card that is not ready yet, so it
			// is retried the same way.
			lastErr, lastCode, lastOut = err, res.ExitCode, buf.String()
		}

		log.Debug("gpu probe failed", "attempt", attempts, "exit", lastCode, "err", lastErr)

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &NotReadyError{
				Probe:    workload.Describe(probe),
				Attempts: attempts,
				Waited:   time.Since(start).Round(time.Second),
				ExitCode: lastCode,
				Err:      lastErr,
				Output:   lastOut,
			}
		}

		wait := Interval
		if remaining < wait {
			wait = remaining
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
}

// NotReadyError is returned when the card never became usable.
//
// A type rather than a formatted string so a caller can tell this apart from
// everything else that can go wrong between renting and running -- this one
// specifically means the hardware was no good, which is the case where trying
// again lands on a different host and may simply work.
type NotReadyError struct {
	Probe    string
	Attempts int
	Waited   time.Duration
	ExitCode int
	Err      error
	Output   string
}

func (e *NotReadyError) Error() string {
	var why string
	switch {
	case e.Err != nil:
		why = fmt.Sprintf("the probe could not be run (%v)", e.Err)
	default:
		why = fmt.Sprintf("the probe exited %d", e.ExitCode)
	}

	msg := fmt.Sprintf("the GPU never became usable: %s after %d attempt(s) over %s.\n"+
		"Nothing was run against it, so this run has cost the wait and no more.\n"+
		"A card that cannot initialize does not slow a workload down, it silently runs it on the CPU "+
		"at the GPU's price, which is why ember refuses here instead.\n"+
		"This is usually the host rather than the card -- destroying and renting again lands elsewhere.\n"+
		"  probe: %s", why, e.Attempts, e.Waited, e.Probe)
	if out := trimOutput(e.Output); out != "" {
		msg += "\n  last output:\n" + out
	}
	return msg
}

func (e *NotReadyError) Unwrap() error { return e.Err }

// trimOutput keeps the tail, which is where a failure says why.
func trimOutput(s string) string {
	const keep = 1 << 10
	if len(s) > keep {
		s = "..." + s[len(s)-keep:]
	}
	return s
}

// limitedWriter writes at most n bytes and discards the rest without error.
//
// Discards rather than fails: the probe's output is diagnostic, and a probe
// that passed must not be reported as failed because it was chatty.
type limitedWriter struct {
	w io.Writer
	n int
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	if l.n <= 0 {
		return len(p), nil
	}
	if len(p) > l.n {
		if _, err := l.w.Write(p[:l.n]); err != nil {
			return 0, err
		}
		l.n = 0
		return len(p), nil
	}
	n, err := l.w.Write(p)
	l.n -= n
	if err != nil {
		return n, err
	}
	return len(p), nil
}

// say writes progress, tolerating a nil writer.
//
// The same shape as internal/assets.say, and for the same reason: a progress
// line that cannot be written is not a reason to fail a run, and a nil out is
// what every unit test passes.
func say(out io.Writer, format string, a ...any) {
	if out == nil {
		return
	}
	_, _ = fmt.Fprintf(out, format, a...)
}
