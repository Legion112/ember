//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestLifecycle_RunsOverRealSSHAndDestroysOnCtrlC is the whole product in one
// test.
//
// ember rents a container, authenticates against a host key it minted and
// injected itself, runs a command on it over that connection, and destroys the
// node when the operator interrupts. Every layer runs its production code: the
// real Docker client in dockerclient, which the coverage gate excludes as a
// branchless adapter and which therefore runs nowhere else; a real sshd; a
// real SSH handshake against a pinned host key; a real remote exec.
//
// THE GPU PROBE IS WHAT HOLDS THE WINDOW OPEN, and it is not a test hook:
// `gpu_probe` is the config key an operator writes for a card that needs time
// to come up, doing exactly what it says, on a node that has no card. One that
// always exits non-zero is retried until `gpu_probe_timeout`, so the run stays
// alive with its connection established for as long as the lab needs -- and
// the debug line gpucheck writes per attempt is the proof a command ran on the
// node and came back with a status, which an accept on a socket would not show.
func TestLifecycle_RunsOverRealSSHAndDestroysOnCtrlC(t *testing.T) {
	l := harness.New(t)
	l.AppendConfig("gpu_probe: [sh, -c, \"exit 1\"]\ngpu_probe_timeout: 5m\n")

	cmd, wait := l.Start(t.Context(), "up", "--log-level", "debug")

	l.WaitNode(t.Context(), 90*time.Second)
	// A command that ran INSIDE the container and reported its status back:
	// gpucheck logs this per attempt, after the handshake against the pinned
	// key. A node that was merely created would never reach it.
	probed := l.WaitForLine(t.Context(), "gpu probe failed")

	if err := cmd.Process.Signal(interrupt()); err != nil {
		t.Fatalf("signal ember: %v", err)
	}
	out := wait()

	// Ctrl-C is the single most likely way to leak a rented GPU: the
	// operator's context is cancelled, and a destroy made on that same
	// context fails instantly. provision runs destroy on
	// context.WithoutCancel for exactly this reason, and this is where that
	// is proven against a real daemon rather than a fake.
	l.WaitNoNodes(t.Context(), 2*time.Minute, func() string { return out })

	if !strings.Contains(out, "node ready") {
		t.Errorf("ember never reported a ready node:\n%s", out)
	}
	if !strings.Contains(probed, "exit=1") {
		t.Errorf("the probe line does not carry the remote exit status, so nothing says a "+
			"command ran on the node: %q", probed)
	}
}

// TestLifecycle_CleanRunLeavesAnEmptyJournal. A record that outlives its node
// makes every later `ember ls` report an orphan that is not there, and an
// operator who learns to ignore that output is an operator who will ignore a
// real one.
func TestLifecycle_CleanRunLeavesAnEmptyJournal(t *testing.T) {
	l := harness.New(t)

	res := l.Run(t.Context(), "up")
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}

	if n := len(l.Nodes(t.Context())); n != 0 {
		t.Errorf("%d node(s) survived a clean run", n)
	}

	// With nothing running and nothing recorded, ls is clean and exits 0.
	ls := l.Run(t.Context(), "ls")
	if ls.ExitCode != 0 {
		t.Errorf("ls after a clean run exited %d:\n%s", ls.ExitCode, ls.Combined())
	}
	if !strings.Contains(ls.Combined(), "no nodes") {
		t.Errorf("ls does not report a clean slate:\n%s", ls.Combined())
	}
}

// TestLifecycle_UpFetchesOutputsBeforeDestroyingTheNode.
//
// The pieces are tested separately -- the extraction against in-memory
// archives, the tar command against a real node -- and this is the one that
// proves they are wired together in `ember up`. An output fetch that works
// perfectly in both halves and is never called is indistinguishable, from the
// operator's side, from one that does not work at all.
//
// /etc/hostname is the output path because it exists on any node without the
// lab having to put it there, which keeps this about the fetch rather than
// about seeding a file.
func TestLifecycle_UpFetchesOutputsBeforeDestroyingTheNode(t *testing.T) {
	l := harness.New(t)
	l.WriteConfig(map[string]string{"outputs": "[/etc/hostname:host]"})

	res := l.Run(t.Context(), "up")
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}

	// TWO FILES: the configured output, and embernode's own log.
	//
	// The second is not an output the operator asked for. It is brought home
	// by internal/app on every run because the node's logs used to die with
	// the node -- the component that installs the host key, starts sshd and
	// arms the self-destruct wrote to a stream nothing in ember read, on a
	// machine ember then destroyed. This assertion is also what caught the
	// collection working: this lab counted exactly one file and went red the
	// first time the log arrived beside it.
	files := l.FetchedFiles()
	if len(files) != 2 {
		t.Fatalf("fetched %v, want the configured output and the node's log\n%s", files, res.Combined())
	}
	// The run directory's name is a timestamp plus the run id, so the
	// assertions are on the tails.
	if !strings.HasSuffix(files[1], filepath.Join("host", "hostname")) {
		t.Errorf("fetched %q, want it to end in host/hostname", files[1])
	}
	if !strings.HasSuffix(files[0], "embernode.log") {
		t.Fatalf("fetched %q, want the node's own log", files[0])
	}

	// And it has to carry what embernode did, not merely exist. A file
	// created by the open and never written is what a logger pointed at the
	// wrong path leaves, and it would satisfy every assertion above.
	nodeLog := readFetched(t, l, files[0])
	for _, want := range []string{"keys installed", "sshd started", "deadline armed"} {
		if !strings.Contains(nodeLog, want) {
			t.Errorf("the node's log does not record %q:\n%s", want, nodeLog)
		}
	}
	// THE KEYS ARE FINGERPRINTS. This file is now on the operator's disk
	// beside the images, so a line that printed the material would be the
	// permanent-in-the-journal failure node.Secret exists to prevent, one hop
	// further out and durable.
	if strings.Contains(nodeLog, "PRIVATE KEY") {
		t.Errorf("the node's log carries key material:\n%s", nodeLog)
	}
	if !strings.Contains(nodeLog, "SHA256:") {
		t.Errorf("the node's log records no host key fingerprint, so a mismatch "+
			"cannot be told from a node that installed the wrong key:\n%s", nodeLog)
	}

	// And the operator is told, with a count and a size: "3 of 4 paths,
	// 1.2 GiB" is what says whether a run is worth keeping.
	if !strings.Contains(res.Combined(), "fetched 1 file") {
		t.Errorf("the fetch was not reported:\n%s", res.Combined())
	}

	// The node is still destroyed: the fetch runs before teardown, not
	// instead of it.
	if n := len(l.Nodes(t.Context())); n != 0 {
		t.Errorf("%d node(s) survived", n)
	}
}

// TestLifecycle_TheNodesOwnLogSurvivesAnInterruptedRun.
//
// Ctrl-C IS THE CASE, and it is why the collection runs on a context that
// survives cancellation. An interrupted run is the one whose evidence is
// worth most and the one ember is most likely to destroy before anybody has
// read anything -- the host-key incident, the trust-on-first-use fallback and
// the missing /workspace were all diagnosed by a person watching a terminal
// at the time, because nothing survived to be read later.
//
// The signal is SIGINT and not SIGKILL on purpose. SIGINT unwinds through
// provision.WithNode's deferred destroy, which is the path a collection on
// context.WithoutCancel is written for; a SIGKILL skips every defer in the
// process, so the log stays on a node nothing is left to fetch from -- which
// is the limit of this mechanism and is stated rather than tested for.
func TestLifecycle_TheNodesOwnLogSurvivesAnInterruptedRun(t *testing.T) {
	l := harness.New(t)
	l.WriteConfig(nil)
	l.AppendConfig("gpu_probe: [sh, -c, \"exit 1\"]\ngpu_probe_timeout: 5m\n")

	// --log-level debug through the real binary, because this is the only
	// place the flag, the handler and a line that was previously undisplayable
	// meet. Asserted below.
	cmd, wait := l.Start(t.Context(), "up", "--log-level", "debug")

	// WAIT FOR THE CONNECTION AND NOT FOR THE CONTAINER. The first version of
	// this lab waited for the node to exist and for its record to be
	// written, and then interrupted -- which landed inside session.Dial's
	// retry loop, before ember had a connection at all. The log could not
	// come home because there was nothing to fetch it over, and the lab was
	// asserting about a window this mechanism does not cover: an interrupt
	// before the handshake is the SIGKILL case with a different signal.
	// gpucheck's per-attempt debug line is written after the handshake, which
	// is the first moment a fetch is possible.
	l.WaitForLine(t.Context(), "gpu probe failed")

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("interrupt ember: %v", err)
	}
	out := wait()

	// The seventeen debug lines in this tree were unreachable: the handler
	// was built in two places and both pinned the level to info. This is one
	// of them, from internal/provision, displayed by a shipped binary.
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("--log-level debug printed no debug line:\n%s", out)
	}
	// And every line inside the rental names the rental, which is what makes
	// one filterable in a log store.
	assertEveryRentalLineNamesItsRun(t, out)

	// The node is gone, which is the property Ctrl-C must never cost.
	l.WaitNoNodes(t.Context(), 2*time.Minute, func() string { return out })

	files := l.FetchedFiles()
	if len(files) != 1 || !strings.HasSuffix(files[0], "embernode.log") {
		t.Fatalf("fetched %v after a Ctrl-C, want the node's own log\n%s", files, out)
	}
	if nodeLog := readFetched(t, l, files[0]); !strings.Contains(nodeLog, "sshd started") {
		t.Errorf("the log came home empty of what the node did:\n%s", nodeLog)
	}
}

// assertEveryRentalLineNamesItsRun checks the correlation convention against
// the output of a real `ember up`.
//
// The unit test in internal/provision asserts the same property against a
// buffer; this one asserts it about the bytes a person reads, which is where
// a logger rebuilt somewhere in the middle would show up. The run id is taken
// from the "node ready" line rather than being predicted, because it is
// minted per run.
func assertEveryRentalLineNamesItsRun(t *testing.T, out string) {
	t.Helper()

	var runID string
	for _, line := range strings.Split(out, "\n") {
		if i := strings.Index(line, " run="); i >= 0 {
			runID = strings.Fields(line[i+len(" run="):])[0]
			break
		}
	}
	if runID == "" {
		t.Fatalf("no line carries a run id at all:\n%s", out)
	}

	for _, line := range strings.Split(out, "\n") {
		// Only slog records: the operator-facing text around them -- the
		// forward list, "Ctrl-C to stop" -- is prose and not a log line.
		if !strings.HasPrefix(line, "time=") {
			continue
		}
		if !strings.Contains(line, "run="+runID) {
			t.Errorf("a line inside the rental cannot be filtered to it:\n  %s", line)
		}
	}
}

// readFetched reads one file the run brought home, named relative to the
// output directory the way FetchedFiles reports it.
func readFetched(t *testing.T, l *harness.Lab, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(l.OutputDir(), rel))
	if err != nil {
		t.Fatalf("read the fetched file: %v", err)
	}
	return string(b)
}

// TestLifecycle_AFailedOutputFetchStillDestroysTheNode. The fetch happens on
// a context that outlives the session precisely so it can run during
// teardown, and a path that was never written must not be able to keep a
// rented machine alive.
func TestLifecycle_AFailedOutputFetchStillDestroysTheNode(t *testing.T) {
	l := harness.New(t)
	l.WriteConfig(map[string]string{"outputs": "[/workspace/never-written:renders]"})

	res := l.Run(t.Context(), "up")
	if res.ExitCode == 0 {
		t.Errorf("a missing output path was reported as a clean run\n%s", res.Combined())
	}
	l.WaitNoNodes(t.Context(), 2*time.Minute, res.Combined)
}

// TestLifecycle_UpLeavesNoNetworkBehind.
//
// ember creates a bridge network per run and removes it on teardown. Nothing
// ever asserted that, because nothing bills for a network and `ls` does not
// report one -- so a leak here is invisible until Docker has subnetted its
// entire address pool and every later run fails with "all predefined address
// pools have been fully subnetted". That is what happened on the machine this
// was written on, after a day of runs.
func TestLifecycle_UpLeavesNoNetworkBehind(t *testing.T) {
	l := harness.New(t)

	before := len(l.Networks(t.Context()))

	res := l.Run(t.Context(), "up")
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}

	if after := len(l.Networks(t.Context())); after != before {
		var names []string
		for _, n := range l.Networks(t.Context()) {
			names = append(names, n.Name)
		}
		t.Errorf("networks went from %d to %d: %s", before, after, strings.Join(names, ", "))
	}
}

// TestLifecycle_UpReportsBothViewsOfTheNodesMemory.
//
// THE LINK NO UNIT TEST CAN MAKE. internal/nodemem parses what a node answers
// and internal/app emits it, and both have unit tests -- but whether
// Plan.prepare actually asks a real node is a question only a real node
// answers. Reinstating the removal of that one call left the whole unit suite
// green, which is the "feature built end to end and never connected" failure
// this repository has paid for three times, caught this time by the
// reinstatement pass rather than by a live run.
//
// It also exercises the script itself against a real container: `sh`, the
// cgroup files, /proc/meminfo and `stat` are all assumptions until something
// runs them somewhere other than in a fake.
func TestLifecycle_UpReportsBothViewsOfTheNodesMemory(t *testing.T) {
	l := harness.New(t)

	res := l.Run(t.Context(), "up")
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}
	out := res.Combined()

	if !strings.Contains(out, "node memory:") {
		t.Fatalf("ember never reported what the node said about its memory:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "node memory:") {
			t.Logf("against a real container: %s", line)
		}
	}
	// Both views, which is the whole point: the cgroup's limit is what bounds
	// the page cache and /proc/meminfo reports the HOST's memory, and a probe
	// that returned only one of them would be the figure that made eight
	// nodes look like they had hundreds of gigabytes spare.
	for _, want := range []string{"host reports", "weights are", "cgroup v"} {
		if !strings.Contains(out, want) {
			t.Errorf("the memory line does not mention %q:\n%s", want, out)
		}
	}
	// A lab container is unlimited, so the limit is reported as none rather
	// than as zero bytes -- which would be a container that cannot allocate
	// anything, the opposite claim.
	if !strings.Contains(out, "limit none") {
		t.Errorf("an unlimited container's limit is not reported as none:\n%s", out)
	}
	// And the host's total is a real figure rather than a zero, which is what
	// says /proc/meminfo was read rather than defaulted.
	if strings.Contains(out, "host reports 0 B total") {
		t.Errorf("the host's memory was not read:\n%s", out)
	}
}
