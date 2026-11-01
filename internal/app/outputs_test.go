package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/workload"
)

func tarOne(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o600, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetchOutputs_ReportsWhatArrived(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd, err := transfer.TarCommand("/workspace/out", "")
	if err != nil {
		t.Fatal(err)
	}
	r.Outputs[cmd.String()] = tarOne(t, "out/a.png", "pixels")

	// The caller decides the directory, so every step of a batch lands in
	// one place. fetchOutputs used to compute it from time.Now() on each
	// call, which put a three-step run in three directories.
	dir := transfer.RunDir(t.TempDir(), "0123456789abcdef", time.Now())
	var out bytes.Buffer
	if err := fetchOutputs(t.Context(), &out, r,
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, dir, ""); err != nil {
		t.Fatal(err)
	}

	// A count and a size, because "3 of 4 paths, 1.2 GiB" is what tells the
	// operator whether the run is worth keeping.
	got := out.String()
	for _, want := range []string{"fetching 1 output path", "fetched 1 file", "6 B"} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q is missing %q", got, want)
		}
	}
	// The run id is in the directory name, so two runs cannot overwrite each
	// other.
	if !strings.Contains(got, "0123456789abcdef") {
		t.Errorf("the run directory is not named after the run: %q", got)
	}
}

// TestFetchOutputs_ReportsWhatArrivedEvenWhenItFailed. A run that failed
// halfway is often exactly the one whose partial output is worth having, so
// the count is printed before the error is returned.
func TestFetchOutputs_ReportsWhatArrivedEvenWhenItFailed(t *testing.T) {
	r := sshx.NewRecordingRunner()
	good, _ := transfer.TarCommand("/workspace/out", "")
	bad, _ := transfer.TarCommand("/workspace/missing", "")
	r.Outputs[good.String()] = tarOne(t, "out/a.png", "pixels")
	r.Errors[bad.String()] = &sshx.ExitError{Cmd: bad.String(), Code: 2, Stderr: "No such file or directory"}

	base := t.TempDir()
	var out bytes.Buffer
	err := fetchOutputs(t.Context(), &out, r, []node.OutputPath{
		{Remote: "/workspace/out", Local: "renders"},
		{Remote: "/workspace/missing", Local: "logs"},
	}, base, "")

	if err == nil {
		t.Fatal("a missing output was reported as success")
	}
	if !strings.Contains(out.String(), "fetched 1 file") {
		t.Errorf("what did arrive was not reported: %q", out.String())
	}
	// And the file really is on disk, not merely counted.
	var found bool
	_ = filepath.Walk(base, func(p string, fi os.FileInfo, _ error) error {
		if fi != nil && !fi.IsDir() && strings.HasSuffix(p, filepath.Join("renders", "out", "a.png")) {
			found = true
		}
		return nil
	})
	if !found {
		t.Error("the output that succeeded was not written")
	}
}

func TestFetchOutputs_NoOutputsIsSilent(t *testing.T) {
	var out bytes.Buffer
	if err := fetchOutputs(t.Context(), &out, sshx.NewRecordingRunner(), nil, t.TempDir(), ""); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Errorf("a run with no outputs printed %q", out.String())
	}
}

func TestFetchOutputs_HonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	var out bytes.Buffer
	err := fetchOutputs(ctx, &out, sshx.NewRecordingRunner(),
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, t.TempDir(), "")
	if err == nil {
		t.Fatal("a cancelled fetch reported success")
	}
}

// TestRunWorkload_RunsTheCommandAndReportsIt.
//
// A node rented to run something stops when it has run it. Holding afterwards
// would bill for a machine with nothing left to do, which is the failure mode
// ember exists to prevent.
func TestRunWorkload_RunsTheCommandAndReportsIt(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd := []string{"sd-cli", "-p", "a lovely cat", "-o", "/workspace/out/cat.png"}
	r.Outputs[workload.Describe(cmd)] = []byte("generating\ndone\n")

	var out bytes.Buffer
	if err := runWorkload(t.Context(), &out, tunnel.NewRecordingDialer(), r, cmd, nil, session.Options{Log: discardLog()}); err != nil {
		t.Fatal(err)
	}

	got := out.String()
	// The command is echoed exactly as sent, quoting included, so an operator
	// whose prompt did not arrive as intended can see what the remote shell
	// was handed.
	if !strings.Contains(got, "sd-cli -p 'a lovely cat'") {
		t.Errorf("the command was not shown as sent: %q", got)
	}
	if !strings.Contains(got, "generating") {
		t.Errorf("the workload output was not streamed: %q", got)
	}
	if !strings.Contains(got, "workload finished in") {
		t.Errorf("the run was not reported: %q", got)
	}
	if calls := r.Calls(); len(calls) != 1 {
		t.Errorf("calls = %v", calls)
	}
}

// TestRunWorkload_ForwardsPortsAlongsideTheCommand. A workload may want to
// reach a service on the node -- a ComfyUI run is exactly that -- so the
// forwards run beside it rather than instead of it.
func TestRunWorkload_ForwardsPortsAlongsideTheCommand(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd := []string{"true"}
	ports := []node.PortMap{{Name: "ui", Remote: 8188, Local: uint16(freePort(t))}}

	var out bytes.Buffer
	if err := runWorkload(t.Context(), &out, tunnel.NewRecordingDialer(), r, cmd, ports, session.Options{Log: discardLog()}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "ui") {
		t.Errorf("the forward was not announced: %q", out.String())
	}
}

// TestRunWorkload_AFailingCommandIsAnError, carried out of the session so the
// run's exit code reflects it.
func TestRunWorkload_AFailingCommandIsAnError(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd := []string{"sd-cli"}
	r.Waits[workload.Describe(cmd)] = &sshx.ExitError{Cmd: "sd-cli", Code: 1, Stderr: "no model"}

	var out bytes.Buffer
	err := runWorkload(t.Context(), &out, tunnel.NewRecordingDialer(), r, cmd, nil, session.Options{Log: discardLog()})
	if err == nil {
		t.Fatal("a failing workload was reported as success")
	}
	// And the duration is still printed: a run that failed after four
	// minutes is a different problem from one that failed instantly.
	if !strings.Contains(out.String(), "workload finished in") {
		t.Errorf("got %q", out.String())
	}
}

func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := loopback.Listen(context.Background(), loopback.Port(0))
	if err != nil {
		t.Fatal(err)
	}
	p := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return p
}

// stepSpec is a batch of n commands writing to one output path.
func stepSpec(t *testing.T, cmds ...[]string) node.Spec {
	t.Helper()
	s, err := node.NewSpec(node.SpecOptions{
		Image:   "ghcr.io/example/x@sha256:" + strings.Repeat("a", 64),
		TTL:     time.Hour,
		Cmds:    cmds,
		Outputs: []node.OutputPath{{Remote: "/workspace/out", Local: "renders"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestRunSteps_FetchesAfterEachStep is the behaviour the whole change exists
// for. A ten-step batch is over an hour on a rented card; fetching only at
// the end means an expired deadline or a closed laptop loses all of it.
// stepRate is what the tests price a step at. A real rate rather than a round
// one, because the per-step figure is reported in microdollars and a rate of
// zero would make every assertion about it pass for the wrong reason.
const stepRate = node.Rate(2_000_000) // $2.00/hr
func TestRunSteps_FetchesAfterEachStep(t *testing.T) {
	r := sshx.NewRecordingRunner()
	spec := stepSpec(t, []string{"sd-cli", "-p", "one"}, []string{"sd-cli", "-p", "two"})

	primeFetches(t, r)

	var out bytes.Buffer
	if err := runSteps(t.Context(), &out, report.Text(&out), tunnel.NewRecordingDialer(), r,
		spec, session.Options{Log: discardLog()}, t.TempDir(), stepRate); err != nil {
		t.Fatal(err)
	}

	var tars, touches int
	for _, c := range r.Calls() {
		if strings.HasPrefix(c, "tar ") {
			tars++
		}
		if strings.HasPrefix(c, "touch ") {
			touches++
		}
	}
	if tars != 2 {
		t.Errorf("fetched %d time(s) for a 2-step batch, want 2 — results must come home as they are produced", tars)
	}
	// One marker, set between the steps. Not before the first: that step
	// fetches everything, which is what a single-command run always did.
	if touches != 1 {
		t.Errorf("touched the marker %d time(s), want 1", touches)
	}
}

// TestRunSteps_LaterFetchesAreBounded. Re-sending a file already home costs
// little for a 2 MB image and a great deal for the minutes of video the store
// was built for.
func TestRunSteps_LaterFetchesAreBounded(t *testing.T) {
	r := sshx.NewRecordingRunner()
	spec := stepSpec(t, []string{"a"}, []string{"b"}, []string{"c"})

	primeFetches(t, r)

	var out bytes.Buffer
	if err := runSteps(t.Context(), &out, report.Text(&out), tunnel.NewRecordingDialer(), r,
		spec, session.Options{Log: discardLog()}, t.TempDir(), stepRate); err != nil {
		t.Fatal(err)
	}

	var tars []string
	for _, c := range r.Calls() {
		if strings.HasPrefix(c, "tar ") {
			tars = append(tars, c)
		}
	}
	if len(tars) != 3 {
		t.Fatalf("got %d fetches, want 3", len(tars))
	}
	if strings.Contains(tars[0], "--newer") {
		t.Errorf("the first fetch was bounded; it must send everything:\n%s", tars[0])
	}
	for i, c := range tars[1:] {
		if !strings.Contains(c, "--newer="+fetchMarker) {
			t.Errorf("fetch %d re-sends what is already home:\n%s", i+2, c)
		}
	}
}

// TestRunSteps_StopsAtTheFirstFailure. A batch where step 2 failed and 3
// ran anyway is a batch nobody can trust, and the rest would bill for work
// the operator no longer wants.
func TestRunSteps_StopsAtTheFirstFailure(t *testing.T) {
	r := sshx.NewRecordingRunner()
	bad := []string{"sd-cli", "-p", "two"}
	r.Waits[workload.Describe(bad)] = &sshx.ExitError{Cmd: "sd-cli", Code: 1, Stderr: "out of memory"}
	spec := stepSpec(t, []string{"sd-cli", "-p", "one"}, bad, []string{"sd-cli", "-p", "three"})

	primeFetches(t, r)

	var out bytes.Buffer
	err := runSteps(t.Context(), &out, report.Text(&out), tunnel.NewRecordingDialer(), r,
		spec, session.Options{Log: discardLog()}, t.TempDir(), stepRate)
	if err == nil {
		t.Fatal("a failed step was reported as success")
	}
	if !strings.Contains(err.Error(), "step 2/3") {
		t.Errorf("the error does not say which step failed: %v", err)
	}
	for _, c := range r.Calls() {
		if strings.Contains(c, "three") {
			t.Error("step 3 ran after step 2 failed")
		}
	}

	// And the failed step's output still comes home: two fetches, not one.
	// A step that failed is often the one whose output explains why.
	var tars int
	for _, c := range r.Calls() {
		if strings.HasPrefix(c, "tar ") {
			tars++
		}
	}
	if tars != 2 {
		t.Errorf("fetched %d time(s); the failing step's output was not collected", tars)
	}
}

// TestRunSteps_ASingleStepLooksExactlyLikeItAlwaysDid.
func TestRunSteps_ASingleStepLooksExactlyLikeItAlwaysDid(t *testing.T) {
	r := sshx.NewRecordingRunner()
	spec := stepSpec(t, []string{"sd-cli", "-p", "only"})

	primeFetches(t, r)

	var out bytes.Buffer
	if err := runSteps(t.Context(), &out, report.Text(&out), tunnel.NewRecordingDialer(), r,
		spec, session.Options{Log: discardLog()}, t.TempDir(), stepRate); err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Calls() {
		if strings.HasPrefix(c, "touch ") {
			t.Error("a single-command run touched a marker it does not need")
		}
		if strings.Contains(c, "--newer") {
			t.Errorf("a single-command fetch was bounded: %s", c)
		}
	}
	// No "step 1/1" noise for the overwhelmingly common case, and no per-step
	// price either: a single command's output is what it was before `cmds`
	// existed, because somebody is parsing it.
	if strings.Contains(out.String(), "step 1/1") {
		t.Errorf("a single-command run announced itself as a batch: %q", out.String())
	}
	if strings.Contains(out.String(), "of card time") {
		t.Errorf("a single-command run priced itself per step: %q", out.String())
	}
}

// TestRunSteps_ReportsWhatEachStepCostAndTheRunningTotal covers the one thing
// an operator watching a ten-step batch needs that a step number does not
// give them: whether to stop paying for the rest.
//
// The figures are asserted as properties and not as values. A step's duration
// is real wall-clock -- workload.Run measures it, and a recording runner
// returns in microseconds -- so the exact amount differs every run. What must
// hold is that the total accumulates: it is at least the step that was just
// reported, and it never goes down. The arithmetic itself belongs to
// node.Rate.Cost, which is tested against its own overflow and rounding
// cases.
//
// One property is deliberately not covered here: that the figure spans the
// fetch and not just the workload. A recording runner returns in microseconds
// and Cost rounds up to one microdollar, so a timer that stopped at the
// workload prints the same thing as one that did not. Making it observable
// would mean a Runner that sleeps -- a production seam shaped by a test's
// convenience, which this repository refuses elsewhere for the same reason.
func TestRunSteps_ReportsWhatEachStepCostAndTheRunningTotal(t *testing.T) {
	r := sshx.NewRecordingRunner()
	spec := stepSpec(t,
		[]string{"sd-cli", "-p", "one"},
		[]string{"sd-cli", "-p", "two"},
		[]string{"sd-cli", "-p", "three"},
	)

	primeFetches(t, r)

	var out bytes.Buffer
	if err := runSteps(t.Context(), &out, report.Text(&out), tunnel.NewRecordingDialer(), r,
		spec, session.Options{Log: discardLog()}, t.TempDir(), stepRate); err != nil {
		t.Fatal(err)
	}

	line := regexp.MustCompile(`step (\d+)/(\d+) took \$([0-9.e+-]+) of card time, \$([0-9.e+-]+) so far`)
	ms := line.FindAllStringSubmatch(out.String(), -1)
	if len(ms) != len(spec.Cmds) {
		t.Fatalf("priced %d step(s) of %d:\n%s", len(ms), len(spec.Cmds), out.String())
	}

	prev := -1.0
	for i, m := range ms {
		if m[1] != strconv.Itoa(i+1) || m[2] != strconv.Itoa(len(spec.Cmds)) {
			t.Errorf("step %d reported itself as %s/%s", i+1, m[1], m[2])
		}
		step, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Fatalf("step %d price %q: %v", i+1, m[3], err)
		}
		total, err := strconv.ParseFloat(m[4], 64)
		if err != nil {
			t.Fatalf("step %d total %q: %v", i+1, m[4], err)
		}
		if total < step {
			t.Errorf("step %d: running total %v is below the step's own %v", i+1, total, step)
		}
		if total < prev {
			t.Errorf("step %d: running total went down, %v after %v", i+1, total, prev)
		}
		prev = total
	}
}

// primeFetches gives the recording runner a real archive for both the
// unbounded fetch and the marker-bounded one, so a step loop can run end to
// end without a node.
func primeFetches(t *testing.T, r *sshx.RecordingRunner) {
	t.Helper()
	plain, err := transfer.TarCommand("/workspace/out", "")
	if err != nil {
		t.Fatal(err)
	}
	bounded, err := transfer.TarCommand("/workspace/out", fetchMarker)
	if err != nil {
		t.Fatal(err)
	}
	r.Outputs[plain.String()] = tarOne(t, "out/a.png", "pixels")
	r.Outputs[bounded.String()] = tarOne(t, "out/b.png", "pixels")
}

// TestRunSteps_AMissingMarkerIsWastefulNotFatal.
//
// If the marker cannot be touched, the next fetch sends everything again.
// That costs bandwidth and nothing else, so it must not fail a batch that is
// otherwise working — the results matter more than the optimisation.
func TestRunSteps_AMissingMarkerIsWastefulNotFatal(t *testing.T) {
	for name, breakIt := range map[string]func(*sshx.RecordingRunner){
		"touch cannot run": func(r *sshx.RecordingRunner) {
			r.Errors["touch "+fetchMarker] = errors.New("ssh: channel refused")
		},
		"touch exits non-zero": func(r *sshx.RecordingRunner) {
			r.Waits["touch "+fetchMarker] = &sshx.ExitError{Cmd: "touch", Code: 1, Stderr: "read-only file system"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := sshx.NewRecordingRunner()
			primeFetches(t, r)
			breakIt(r)
			spec := stepSpec(t, []string{"a"}, []string{"b"})

			var out bytes.Buffer
			if err := runSteps(t.Context(), &out, report.Text(&out), tunnel.NewRecordingDialer(), r,
				spec, session.Options{Log: discardLog()}, t.TempDir(), stepRate); err != nil {
				t.Fatalf("a batch failed because a marker could not be set: %v", err)
			}

			var tars []string
			for _, c := range r.Calls() {
				if strings.HasPrefix(c, "tar ") {
					tars = append(tars, c)
				}
			}
			if len(tars) != 2 {
				t.Fatalf("got %d fetches, want 2", len(tars))
			}
			// Both unbounded: without a marker there is nothing to bound by,
			// and sending too much is the safe direction.
			for i, c := range tars {
				if strings.Contains(c, "--newer") {
					t.Errorf("fetch %d was bounded by a marker that was never set:\n%s", i+1, c)
				}
			}
		})
	}
}

// TestRunSteps_AFailedFetchIsReportedButDoesNotStopTheBatch.
//
// The results of later steps are still worth having, and a fetch that failed
// once may well succeed next time -- the node is the same one.
func TestRunSteps_AFailedFetchIsReportedButDoesNotStopTheBatch(t *testing.T) {
	r := sshx.NewRecordingRunner()
	primeFetches(t, r)
	plain, err := transfer.TarCommand("/workspace/out", "")
	if err != nil {
		t.Fatal(err)
	}
	// The first fetch is the unbounded one, so this breaks step 1's.
	r.Errors[plain.String()] = errors.New("tar: Cannot open: No such file or directory")

	spec := stepSpec(t, []string{"a"}, []string{"b"})
	var out bytes.Buffer
	runErr := runSteps(t.Context(), &out, report.Text(&out), tunnel.NewRecordingDialer(), r,
		spec, session.Options{Log: discardLog()}, t.TempDir(), stepRate)
	if runErr == nil {
		t.Fatal("a failed fetch was not reported")
	}
	// But step 2 still ran, and its output still came home.
	var ran bool
	for _, c := range r.Calls() {
		if c == "b" {
			ran = true
		}
	}
	if !ran {
		t.Error("step 2 was skipped because step 1's fetch failed")
	}
}

// TestRunSteps_PutsEveryStepInOneDirectory.
//
// A three-step run used to produce three directories a minute apart, because
// the fetch computed its own path from time.Now() on every call. One run is
// one directory.
func TestRunSteps_PutsEveryStepInOneDirectory(t *testing.T) {
	r := sshx.NewRecordingRunner()
	primeFetches(t, r)
	spec := stepSpec(t, []string{"a"}, []string{"b"}, []string{"c"})

	dir := transfer.RunDir(t.TempDir(), "0123456789abcdef", time.Now())
	var out bytes.Buffer
	if err := runSteps(t.Context(), &out, report.Text(&out), tunnel.NewRecordingDialer(), r,
		spec, session.Options{Log: discardLog()}, dir, stepRate); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out.String(), "fetching 1 output path(s) into "+dir); n != 3 {
		t.Errorf("announced %d fetches into %s, want 3:\n%s", n, dir, out.String())
	}
}
