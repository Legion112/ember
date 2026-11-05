// Package harness drives ember's real binary against a real Docker daemon.
//
// The labs here run the compiled `ember` as a subprocess rather than calling
// into internal packages. That costs a build and some milliseconds, and it
// buys three things no unit test reaches: the real Docker client in
// internal/dockerengine/dockerclient, which is excluded from the coverage gate
// as a branchless adapter and would otherwise never run at all; the real exit
// codes, which are the contract with cron; and the ability to SIGKILL ember
// mid-provision, which is the one way to produce the orphan that `ember ls`
// and `ember reap` exist to find.
//
// Nothing here skips. A missing daemon or a missing image is a hard failure
// naming the command that fixes it. A lab that skips itself reports success
// for something that never ran, which is worse than a red build: go-tun's
// AGENTS.md records the same rule and the reason it was written down.
package harness

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc64"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerimage "github.com/docker/docker/api/types/image"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/go-connections/nat"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/loopback"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/statestore"
	"github.com/Legion112/ember/test/integration/harness/labdaemon"
)

// LabImage is what ember's local provider starts as a node.
const LabImage = "ember:lab"

// EnvGate is the second of the two gates on these labs. The build tag keeps
// them out of an ordinary `go test ./...`; this keeps them out of a tagged run
// by someone who did not mean to start containers.
const EnvGate = "EMBER_INTEGRATION"

// Lab is one isolated ember installation plus the assertions about it.
type Lab struct {
	// Dir is the lab's whole world: ember.yaml, the config dir holding the
	// operator's key, and the state dir holding the journal.
	Dir string

	// ConfigPath is the ember.yaml every command is pointed at.
	ConfigPath string

	// Bin is the compiled ember under test.
	Bin string

	cli *client.Client
	t   *testing.T

	// last is the output of the most recently Started subprocess, so
	// WaitForLine can read it while that process is still running.
	last *syncBuffer
}

// Require fails the test unless this machine can actually run a lab.
//
// Each failure names the command that fixes it, because the alternative is a
// test that fails with a Docker API error forty lines deep in a provider.
func Require(t *testing.T) {
	t.Helper()
	if os.Getenv(EnvGate) != "1" {
		t.Fatalf("%s is not set to 1.\n"+
			"These labs start real containers, so they are gated twice: the integration build tag and this variable.\n"+
			"Run them with:\n  make test-integration", EnvGate)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("cannot construct a Docker client: %v", err)
	}
	defer func() { _ = cli.Close() }()

	if _, err := cli.Ping(ctx); err != nil {
		t.Fatalf("no Docker daemon reachable: %v\nThese labs need one; there is no fake at this layer.", err)
	}
	if _, err := cli.ImageInspect(ctx, LabImage); err != nil {
		t.Fatalf("the lab image %s is missing: %v\nBuild it with:\n  make docker-build", LabImage, err)
	}

	exclusive(t, cli)
}

// suite is this test binary's hold on the daemon and what was already on it.
//
// Both are process-wide and taken once, because the thing being protected is
// process-wide: a lab's cleanup removes every ember-labelled object the suite
// run produced, which is the breadth that catches the leaked bridge network
// nobody thought to look for, and which deleted another worktree's live nodes
// mid-test the first time two agents ran the suite at once.
//
// suiteLock also has to stay referenced for the life of the process. An
// *os.File that becomes unreachable is closed by the runtime's finalizer, and
// closing the descriptor releases the flock -- so a lock held in a local
// variable would be dropped at an unpredictable moment, mid-suite, with
// nothing to show what happened.
var (
	suiteOnce sync.Once
	suiteLock *labdaemon.Lock
	suiteErr  error

	// foreignObjects are the ember-labelled objects that were on the daemon
	// when this suite took the lock: a container somebody started by hand, a
	// leak from a lab that was SIGKILLed yesterday, anything at all. Every
	// listing below subtracts them, so they are not counted, not reported and
	// -- the point -- not removed. What is NOT in here is the kinds ember's
	// own listings report: those are refused up front by takeDaemon, because
	// the labs assert on that output and cannot pass while one is there.
	// Everything that is in here, a developer keeps: `make reap-labs` removes
	// them when they ask, which is a tool somebody runs on purpose rather than
	// an assertion that deletes what no lab created.
	foreignObjects labdaemon.Objects
)

// exclusive makes this test binary the only one acting on this daemon.
//
// THE DEFECT: Lab.Nodes matched containers on the presence of ember's run-id
// label with no value, and Reap force-removed everything it returned, so a
// lab's cleanup was daemon-wide. One checkout made that safe; agent worktrees
// put several on one machine against the one daemon, and two suites then
// deleted each other's live nodes -- "No such container" for a container that
// existed a moment earlier, 170s of wall clock against the usual 85, and every
// failing test passing when run alone.
//
// A lock rather than a per-run label, because a label can only scope what the
// *harness* looks at. ember's own leftover sweep is daemon-wide by design and
// the labs assert on its output -- TestLeftover_ReapRemovesANetworkNothingElseWould
// wants "removed 1 of 1 leftover" -- so a second suite's deliberately-expired
// network would still be swept by this one's `ember reap`, and the labelled
// listings would both have been right about nothing. Exclusivity is the only
// thing that makes those assertions true.
//
// Waiting, not refusing: "another run is in progress" is a reason to queue,
// and failing a suite because the daemon is busy would be the `--- SKIP`
// mistake in red. The loud failures are the two where waiting cannot help --
// a holder that never finishes, and an object already on the daemon that
// ember's own listings will report whatever this harness filters on.
func exclusive(t *testing.T, cli *client.Client) {
	t.Helper()

	// t.Fatalf inside a sync.Once would mark the Once done from a goroutine
	// that is being unwound, and every later lab would then sail past a gate
	// that never closed. The once only computes; the failing is out here.
	suiteOnce.Do(func() { suiteLock, foreignObjects, suiteErr = takeDaemon(t, cli) })

	if suiteErr != nil {
		t.Fatalf("%v", suiteErr)
	}
	if suiteLock == nil {
		t.Fatal("the lab lock was not taken and nothing says why; a lab must not run a daemon-wide cleanup unlocked")
	}
}

func takeDaemon(t *testing.T, cli *client.Client) (*labdaemon.Lock, labdaemon.Objects, error) {
	t.Helper()

	idCtx, cancelID := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelID()

	id, err := labdaemon.DaemonID(idCtx, cli)
	if err != nil {
		return nil, labdaemon.Objects{}, err
	}

	lock, err := labdaemon.Acquire(id, labdaemon.LockWait, func(waited time.Duration, holder string) {
		// Logged rather than silent: a suite that is queued behind another
		// agent's run must not look hung, because "it hangs sometimes" is how
		// a harness loses the trust these labs exist to have.
		t.Logf("waiting %v for the lab lock on this daemon, held by %s", waited.Round(time.Second), holder)
	})
	if err != nil {
		return nil, labdaemon.Objects{}, err
	}

	// The snapshot is taken AFTER the lock, and that ordering is the whole
	// meaning of it: with no other run allowed to touch the daemon, what is
	// here now is what was here before this suite run, so everything that
	// appears from this moment on belongs to it.
	//
	// On a context of its own, because the wait above can be minutes long and
	// a deadline started before it would already be spent -- which would read
	// as a daemon that stopped answering, in the one place where the message
	// has to be about the daemon's contents.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	foreign, err := labdaemon.List(ctx, cli)
	if err != nil {
		return nil, labdaemon.Objects{}, err
	}
	// What ember itself would report is a refusal, and the rest is tolerated.
	//
	// The split is measured. A stale `ember.kind=node` container on the daemon
	// makes `ember ls` report it `untracked` -- correctly, that is the whole
	// point of the command -- so TestLeftover_ReapRemovesANetworkNothingElseWould
	// fails ninety seconds in with "ls still reports findings after reap",
	// which says nothing about why. Deleting it was the old behaviour and is
	// the defect. Refusing here is the only answer left, and it is not the
	// hostile one it looks like: the labs cannot pass while the object is
	// there, so the choice is between one sentence now and a confusing red
	// suite later.
	//
	// "Another run is in progress" is NOT this failure. That one waits, above.
	if bad := foreign.Reported(); !bad.Empty() {
		return nil, labdaemon.Objects{}, fmt.Errorf(
			"%d ember-labelled object(s) on this daemon are not from this suite run:\n  %s\n"+
				"`ember ls` and `ember reap` look at the whole daemon by design, so the labs that assert on their output cannot pass while these are here -- and nothing here will remove them, because a leftover from a run that died and a live node somebody is using look exactly the same from outside. Clear them with:\n  make reap-labs",
			len(bad.Containers)+len(bad.Networks)+len(bad.Volumes), strings.Join(bad.Lines(), "\n  "))
	}
	if !foreign.Empty() {
		// Tolerated and named. These wear ember's run-id and nothing else, so
		// no ember listing looks at them and no lab can trip over them; every
		// listing in this file subtracts them, so none is reported or removed.
		t.Logf("%d ember-labelled object(s) were already on this daemon and will be left alone:\n  %s\n"+
			"Nothing here created them, so no lab reports or removes them. Clear them with:\n  make reap-labs",
			len(foreign.Containers)+len(foreign.Networks)+len(foreign.Volumes),
			strings.Join(foreign.Lines(), "\n  "))
	}
	return lock, foreign, nil
}

// scoped drops whatever was on the daemon before this suite run.
//
// Every listing in this file goes through it, so a cleanup cannot act on
// something this run did not produce -- and nothing in the lab code spells an
// unscoped listing of its own, which is the arrangement that makes the bug
// unrepresentable rather than tested for. labdaemon holds the one unscoped
// listing, and the one caller of it that removes anything is `make reap-labs`.
func scoped(o labdaemon.Objects) labdaemon.Objects { return o.Except(foreignObjects) }

// New returns a lab with its own config, state and operator key.
//
// The cleanup asserts that nothing wearing ember's label survived. That
// assertion is the point of every lab in this package: a passing lifecycle
// test that leaves a container behind has demonstrated the opposite of what it
// claims, and on a rented GPU it would still be billing.
func New(t *testing.T) *Lab {
	t.Helper()
	Require(t)

	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Fatalf("docker client: %v", err)
	}

	dir := t.TempDir()
	l := &Lab{
		Dir:        dir,
		ConfigPath: filepath.Join(dir, "ember.yaml"),
		Bin:        buildEmber(t),
		cli:        cli,
		t:          t,
	}

	l.WriteConfig(map[string]string{})

	t.Cleanup(func() {
		// Snapshot first, then clean up, then report. The order matters and
		// the previous one was wrong: Reap removed everything and the
		// assertion then looked at an empty daemon, so it could not fail --
		// for containers, networks or anything else. Every lab's final
		// assertion was passing because there was nothing left to see.
		//
		// Both properties are wanted. Reaping stops one failing lab from
		// cascading into every later one, and the report is the only thing
		// that says a lab leaked. Taking the snapshot before the reap is what
		// lets them coexist.
		//
		// These three lines are the whole of the check, which is why the
		// AssertNoStray that named it is gone rather than wired up: the
		// snapshot lands after a lab's own Reap -- the way the three labs that
		// leak on purpose declare it -- and before this one, so it reports
		// what a declared reap left as well as what a lab forgot. Measured by
		// reinstating a Reap that keeps its containers; see the note where
		// AssertNoStray was.
		stray := l.strayNow()
		l.Reap()
		l.reportStray(stray)
		_ = cli.Close()
	})
	return l
}

// WriteConfig writes ember.yaml with the lab's directories plus extra lines.
//
// config_dir and state_dir point inside the lab, so a lab never reads or
// writes the operator's real ~/.config/ember -- including their real journal,
// which a reap lab would otherwise be entitled to act on.
func (l *Lab) WriteConfig(extra map[string]string) {
	l.t.Helper()

	// extra OVERRIDES rather than appends. Appending emitted the key twice
	// -- `image:` from here and `image:` from the caller -- and yaml.v3
	// refuses a duplicate mapping key, so a lab that wanted a different
	// image got "mapping key already defined" instead of a config. The
	// obvious reading of a map of overrides is the one it should have.
	fields := map[string]string{
		"provider":   "local",
		"image":      LabImage,
		"ttl":        "10m",
		"config_dir": filepath.Join(l.Dir, "config"),
		"state_dir":  filepath.Join(l.Dir, "state"),
		"output_dir": l.OutputDir(),
	}
	for k, v := range extra {
		fields[k] = v
	}

	// Sorted, so two runs of one lab write the same file. A map's order is
	// random, and a config that differs between runs is one nobody can
	// compare when a lab fails on CI and passes here.
	keys := make([]string, 0, len(fields))
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, k+": "+fields[k])
	}
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(l.ConfigPath, []byte(body), 0o600); err != nil {
		l.t.Fatalf("write %s: %v", l.ConfigPath, err)
	}
}

// AppendConfig adds raw YAML to the lab's config.
//
// WriteConfig takes a map of scalars and joins them as "key: value", which
// cannot express a nested block -- and a map's iteration order is random, so
// it could not express one reliably even if the join allowed it. An `sd:`
// block is nested, so it arrives this way.
func (l *Lab) AppendConfig(raw string) {
	l.t.Helper()
	f, err := os.OpenFile(l.ConfigPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		l.t.Fatalf("open %s: %v", l.ConfigPath, err)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.WriteString(raw); err != nil {
		l.t.Fatalf("append to %s: %v", l.ConfigPath, err)
	}
}

// buildEmber compiles the binary under test once per package run.
func buildEmber(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(binDir(t), "ember")
	if _, err := os.Stat(bin); err == nil {
		return bin
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, goTool(), "build", "-o", bin, "./cmd/ember")
	cmd.Dir = repoRoot(t)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build ember: %v\n%s", err, out)
	}
	return bin
}

// binDir is shared across the package's tests so the binary is built once.
var sharedBin string

func binDir(t *testing.T) string {
	t.Helper()
	if sharedBin == "" {
		d, err := os.MkdirTemp("", "ember-lab-bin")
		if err != nil {
			t.Fatalf("temp dir: %v", err)
		}
		sharedBin = d
	}
	return sharedBin
}

// goTool prefers the same toolchain the Makefile selects, so the lab cannot
// compile ember with a different compiler than the one go.mod declares.
func goTool() string {
	sdk := filepath.Join(os.Getenv("HOME"), "sdk", "go1.27.0", "bin", "go")
	if _, err := os.Stat(sdk); err == nil {
		return sdk
	}
	return "go"
}

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// This package lives at test/integration, so the root is two up.
	return filepath.Join(wd, "..", "..")
}

// Result is one ember invocation.
type Result struct {
	Stdout   string
	ExitCode int
	Err      error
}

// Combined is for assertions that do not care which stream a line came from.
func (r Result) Combined() string { return r.Stdout }

// Run invokes ember with the lab's config and waits for it to finish.
func (l *Lab) Run(ctx context.Context, args ...string) Result {
	l.t.Helper()
	return l.run(ctx, nil, args...)
}

// RunSub runs a two-level subcommand, as `ember store create --config ...`.
//
// Run inserts --config after the first word, which is right for a flat
// command with positional arguments -- `ember down <handle>` needs its flags
// before the handle, because the standard flag package stops parsing at the
// first non-flag argument. For a two-level command that same insertion
// produces `ember store --config X create`, which the dispatcher reads as a
// store subcommand named "--config". Hence a separate entry point rather than
// a cleverer Run: the two shapes genuinely differ, and guessing which one is
// meant from the argument list is how a lab ends up asserting against a usage
// error.
func (l *Lab) RunSub(ctx context.Context, group, sub string, args ...string) Result {
	l.t.Helper()
	full := append([]string{group, sub, "--config", l.ConfigPath}, args...)
	return l.runRaw(ctx, full...)
}

func (l *Lab) run(ctx context.Context, before func(*exec.Cmd), args ...string) Result {
	l.t.Helper()

	full := append([]string{args[0], "--config", l.ConfigPath}, args[1:]...)
	return l.execEmber(ctx, before, full)
}

// runRaw runs ember with exactly these arguments.
func (l *Lab) runRaw(ctx context.Context, full ...string) Result {
	l.t.Helper()
	return l.execEmber(ctx, nil, full)
}

func (l *Lab) execEmber(ctx context.Context, before func(*exec.Cmd), full []string) Result {
	l.t.Helper()
	cmd := exec.CommandContext(ctx, l.Bin, full...)
	cmd.Dir = l.Dir
	// A deliberately small environment. HOME is inside the lab so that a bug
	// in path resolution writes to a temp dir and fails the lab, instead of
	// quietly touching the operator's real ~/.config/ember.
	cmd.Env = append(os.Environ(), "HOME="+l.Dir)
	if before != nil {
		before(cmd)
	}

	out, err := cmd.CombinedOutput()
	res := Result{Stdout: string(out), Err: err}

	var ee *exec.ExitError
	switch {
	case err == nil:
		res.ExitCode = 0
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
	default:
		res.ExitCode = -1
	}
	return res
}

// Start launches ember without waiting, for the labs that need to interrupt
// it. The returned stop function kills the process and collects its output.
func (l *Lab) Start(ctx context.Context, args ...string) (*exec.Cmd, func() string) {
	l.t.Helper()
	return l.StartRaw(ctx, append([]string{args[0], "--config", l.ConfigPath}, args[1:]...)...)
}

// StartRaw launches ember with exactly these arguments, inserting no --config.
//
// RunSub's reason, for a long-running command: Start inserts the lab's own
// config path, which is right for every lab that reads the one file and wrong
// for the one whose subject is a client naming a DIFFERENT one -- `ember up`
// over the socket, where the config path travels to the daemon. Passing both
// and relying on the last flag winning would work and would be a lab asserting
// a property of the standard flag package.
func (l *Lab) StartRaw(ctx context.Context, full ...string) (*exec.Cmd, func() string) {
	l.t.Helper()

	cmd := exec.CommandContext(ctx, l.Bin, full...)
	cmd.Dir = l.Dir
	cmd.Env = append(os.Environ(), "HOME="+l.Dir)

	var buf syncBuffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	if err := cmd.Start(); err != nil {
		l.t.Fatalf("start ember %v: %v", full, err)
	}
	l.last = &buf
	return cmd, func() string {
		_ = cmd.Wait()
		return buf.String()
	}
}

// Printed is everything the last-started subprocess has written so far.
//
// WaitForLine's SIBLING, FOR THE ASSERTION THAT IS ABOUT THE SECOND
// OCCURRENCE. WaitForLine returns the FIRST line matching its substring,
// which is the right answer for an address printed once at startup and the
// wrong one for a line a server writes per event: the progress socket records
// the cursor each watcher resumed from, and what the reconnect lab is about is
// the SECOND of those. A caller that needs to count or to parse polls this.
func (l *Lab) Printed() string {
	l.t.Helper()
	if l.last == nil {
		l.t.Fatal("Printed before Start; there is no subprocess to read")
	}
	return l.last.String()
}

// WaitForLine blocks until the last-started subprocess has printed sub.
//
// IT EXISTS BECAUSE Start's OWN READER WAITS FOR THE PROCESS. That works for
// every command this package ran until now -- they all run to completion --
// and cannot work for a server, whose address is printed while it keeps
// running. Polling the buffer Start already writes into is the whole of it.
//
// A test helper and nothing more: giving `ember serve` a --url-file so a test
// could read one would be a production interface shaped by a test's
// convenience, which is the mistake sshx.Runner was kept from making.
func (l *Lab) WaitForLine(ctx context.Context, sub string) string {
	l.t.Helper()
	if l.last == nil {
		l.t.Fatal("WaitForLine before Start; there is no subprocess to read")
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := l.last.String()
		if i := strings.Index(out, sub); i >= 0 {
			rest := out[i:]
			if nl := strings.IndexByte(rest, '\n'); nl >= 0 {
				return rest[:nl]
			}
			return rest
		}
		select {
		case <-ctx.Done():
			l.t.Fatalf("waiting for %q: %v\nwhat it printed:\n%s", sub, ctx.Err(), out)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("%q never appeared within 30s. What it printed:\n%s", sub, out)
		}
	}
}

// Nodes lists the containers wearing ember's run-id label that this suite run
// produced.
//
// It asks Docker directly rather than running `ember ls`, because a lab that
// used ember to check ember would pass whenever both sides were wrong in the
// same direction -- which is exactly what a label-matching bug looks like.
//
// The match is still on the presence of the label and never on its value: a
// listing that only found what the harness remembered creating is how the
// leaked bridge network stayed invisible. What it no longer includes is
// anything that was on the daemon before the suite took the lock, which is the
// difference between "nothing ember left behind in this run" and "nothing
// ember-labelled anywhere, whoever it belongs to".
func (l *Lab) Nodes(ctx context.Context) []container.Summary {
	l.t.Helper()
	cs, err := labdaemon.Containers(ctx, l.cli)
	if err != nil {
		l.t.Fatalf("%v", err)
	}
	return scoped(labdaemon.Objects{Containers: cs}).Containers
}

// AssertNoStray is GONE, and this note is where it was, because the labs
// here, eight comments in internal/ and an AGENTS.md entry all name it (#113).
//
// It was `l.reportStray(l.strayNow())` -- look now, fail on what is there --
// and for the life of the suite it had **no caller**: New's cleanup spells
// snapshot, reap, report itself, because reaping must not be able to empty the
// daemon before the snapshot is taken and a zero-argument method cannot
// express that ordering. Its own doc comment said "it is also run from the
// lab's own cleanup", which was never true. So the property was checked, by
// reportStray below, and the named mechanism was an orphan -- the
// written-covered-and-connected-to-nothing shape, in the one tree
// TestNoProductionDeadCode deliberately does not load.
//
// The `sdserver.Client.Cancel` precedent -- wire it up, because deleting the
// only mechanism deletes the mechanism -- was tried and does not apply, and
// the thing that decided it is a measurement rather than a reading. The gap it
// looked like it filled is the three labs that leak on purpose: they call Reap
// themselves to declare the orphan deliberate, and the cleanup's snapshot is
// taken afterwards, so it looks as though nothing can then say whether that
// Reap finished. It can, and it does: the snapshot is taken after the body's
// Reap and BEFORE the cleanup's own, so whatever the declared reap left is
// exactly what it sees. Reinstated -- a Reap that leaves its containers behind
// -- both orphan labs go red at reportStray with the mid-test assertion
// deleted, so an AssertNoStray() beside their Reap reported the same objects
// one line earlier and nothing else. That is the `queue.Cost` answer: a second
// path for one check, where the other path is the live one.
//
// Two judgements are kept here because whoever adds the next stray check makes
// them again: the snapshot is taken before the reap, or a working cleanup
// empties the daemon the assertion is about -- which is the defect this method
// was repaired for and could not fail over; and it is taken over this suite
// run's objects rather than the daemon's, or it accuses another checkout's
// live node of being a leak.

// stray is what one look at the daemon found.
type stray struct {
	networks []network.Summary
	volumes  []*volume.Volume
	nodes    []container.Summary
}

func (l *Lab) strayNow() stray {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return stray{
		networks: l.Networks(ctx),
		volumes:  l.Volumes(ctx),
		nodes:    l.Nodes(ctx),
	}
}

func (l *Lab) reportStray(s stray) {
	l.t.Helper()

	if ns := s.networks; len(ns) > 0 {
		var names []string
		for _, n := range ns {
			names = append(names, n.Name)
		}
		l.t.Errorf("%d network(s) wearing ember's label survived the lab: %s"+
			"\nNothing bills for these, which is why they went unnoticed until Docker ran out of address pools.",
			len(ns), strings.Join(names, ", "))
	}

	// Volumes outlive both. A store is the one resource in ember that
	// nothing destroys on a deadline, so a lab that creates one and does not
	// remove it leaks on the developer's daemon for good -- the same shape as
	// the networks above, which went unnoticed until Docker ran out of
	// address pools, except that these hold gigabytes.
	if vs := s.volumes; len(vs) > 0 {
		var names []string
		for _, v := range vs {
			names = append(names, v.Name)
		}
		l.t.Errorf("%d volume(s) wearing ember's label survived the lab: %s"+
			"\nOn a cloud provider each of these would still be billing by the month.",
			len(vs), strings.Join(names, ", "))
	}

	cs := s.nodes
	if len(cs) == 0 {
		return
	}
	var b strings.Builder
	for _, c := range cs {
		fmt.Fprintf(&b, "\n  %s  %s  %s  labels=%v", c.ID[:12], c.State, strings.Join(c.Names, ","), c.Labels)
	}
	l.t.Errorf("%d container(s) wearing ember's label survived the lab:%s"+
		"\nOn a rented GPU each of these would still be billing.", len(cs), b.String())
}

// Reap removes everything this suite run left on the daemon.
//
// It is called from the lab's own cleanup so that one failing lab does not
// cascade into every later one, and a lab that leaves something behind *on
// purpose* calls it itself once it has finished asserting on it -- the three
// labs that SIGKILL ember do exactly that. The cleanup snapshots the daemon
// before reaping, so calling this at the end of a test body is also how a
// deliberate leak is declared: anything still there when the snapshot is taken
// is reported.
//
// Which also means this function's own completeness is asserted: a declared
// reap runs BEFORE that snapshot, so whatever it leaves behind is reported
// against the lab that declared it. Measured, by making this one keep its
// containers.
//
// It still removes what no lab remembered creating -- networks and volumes
// beside the containers, found by the label rather than by a list the harness
// kept. Leaving them out leaked one network per SIGKILL lab: invisible,
// because nothing bills for a bridge network and nothing asserted on them,
// until Docker had subnetted its whole address pool and every later run failed
// with "all predefined address pools have been fully subnetted".
//
// What it no longer removes is another run's live node. The listings above
// subtract the objects that were on the daemon before this suite took the
// lock, and the lock is why there is no concurrent run to find: this function
// force-removed every ember-labelled container on the daemon, which was a
// whole-daemon delete executed by whichever worktree happened to finish a lab
// first.
func (l *Lab) Reap() {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Errors are deliberately dropped. This runs after the assertions have
	// been recorded, and the stray report -- not this -- is what says a lab
	// leaked; `make reap-labs` is the caller that needs to be told.
	_ = labdaemon.Remove(ctx, l.cli, labdaemon.Objects{
		Containers: l.Nodes(ctx),
		Networks:   l.Networks(ctx),
		Volumes:    l.Volumes(ctx),
	})
}

// CreateVolume makes a store volume out-of-band, through Docker.
//
// Seeded this way rather than through ember on purpose: a lab that creates
// the thing it is about to read is testing the writer, and the reader is what
// this is for. The labels are the ones the local provider writes, so what
// comes back through StoreList is indistinguishable from a store ember made.
func (l *Lab) CreateVolume(ctx context.Context, name string) {
	l.t.Helper()
	if _, err := l.cli.VolumeCreate(ctx, volume.CreateOptions{
		Name:   name,
		Labels: map[string]string{local.LabelKind: "store", local.LabelLabel: name},
	}); err != nil {
		l.t.Fatalf("create volume %s: %v", name, err)
	}
	// The helper owns what it made. Cleanups run last-registered-first, so
	// this one runs before the lab's own stray check and a seeded volume is
	// not reported as a leak -- while a volume ember itself left behind
	// still is. Leaving it to each lab to remember would mean the first one
	// that forgot turned a working check into a failing suite.
	l.t.Cleanup(func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = l.cli.VolumeRemove(rctx, name, true)
	})
}

// CreateNetwork makes a bridge network wearing ember's labels, with no node
// container beside it.
//
// This is what a destroy that could not find the run id leaves on the daemon:
// nothing bills for it, nothing is attached to it, and before `ember reap`
// learned to sweep, nothing in ember would ever look at it again. Seeded
// out-of-band through Docker rather than by running ember and interrupting it,
// because the thing under test is the sweep and a lab that created the object
// through the code under test would be asserting that ember agrees with
// itself.
//
// deadline goes into the ember label, so a lab can produce one that is already
// past without waiting for a clock -- the same trick NodeSpec.Deadline uses.
//
// The helper owns what it made, as CreateVolume does: cleanups run
// last-registered-first, so this one runs before the lab's own stray check and
// a seeded network is not reported as a leak, while a network ember itself
// left behind still is.
func (l *Lab) CreateNetwork(ctx context.Context, runID node.RunID, deadline time.Time) string {
	l.t.Helper()
	name := "ember-" + string(runID)
	resp, err := l.cli.NetworkCreate(ctx, name, network.CreateOptions{
		Labels: map[string]string{
			dockerengine.LabelRunID: string(runID),
			local.LabelKind:         "network",
			local.LabelLabel:        provider.Label(runID, deadline),
		},
	})
	if err != nil {
		l.t.Fatalf("create network %s: %v", name, err)
	}
	l.t.Cleanup(func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = l.cli.NetworkRemove(rctx, resp.ID)
	})
	return resp.ID
}

// CreateStore runs `ember store create` and registers its removal.
//
// Through ember, unlike CreateVolume, because some labs are about the command
// itself. The cleanup is registered here for the same reason CreateVolume
// registers one: cleanups run last-registered-first, so the store goes before
// the stray check looks, and the first lab that forgot would otherwise have
// turned a working check into a failing suite.
func (l *Lab) CreateStore(ctx context.Context, name string) {
	l.t.Helper()
	if out := l.RunSub(ctx, "store", "create"); out.ExitCode != 0 {
		l.t.Fatalf("store create exited %d: %s", out.ExitCode, out.Combined())
	}
	l.t.Cleanup(func() {
		rctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// Removed at the daemon rather than through ember: a lab whose
		// assertions already failed should not need ember to work in order
		// to clean up after itself.
		_ = l.cli.VolumeRemove(rctx, provider.StoreLabel(name), true)
	})
}

// SeedVolume writes a file into a store volume and returns its SHA-256.
//
// Out-of-band, through a throwaway container, because the thing under test is
// the verifying -- and a lab that used ember to put the file there would be
// asserting that ember agrees with itself.
//
// It is also what lets the asset lab run with no network at all: with the
// file already present and verified in place, the configured URL is never
// fetched, so pointing it somewhere unreachable proves the fetch did not go
// looking.
func (l *Lab) SeedVolume(ctx context.Context, vol, name, content string) string {
	l.t.Helper()

	resp, err := l.cli.ContainerCreate(ctx,
		&container.Config{
			Image: LabImage,
			// The image's entrypoint is embernode, which would try to be a
			// node. This container only has to write a file.
			Entrypoint: []string{"/bin/sh", "-c"},
			Cmd:        []string{"mkdir -p \"$(dirname /seed/" + name + ")\" && printf %s \"$CONTENT\" > /seed/" + name},
			Env:        []string{"CONTENT=" + content},
		},
		&container.HostConfig{Binds: []string{vol + ":/seed"}},
		nil, nil, "")
	if err != nil {
		l.t.Fatalf("create seed container: %v", err)
	}
	defer func() {
		_ = l.cli.ContainerRemove(context.Background(), resp.ID, container.RemoveOptions{Force: true})
	}()
	if err := l.cli.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		l.t.Fatalf("start seed container: %v", err)
	}
	l.WaitExited(ctx, resp.ID, 30*time.Second)

	j, err := l.cli.ContainerInspect(ctx, resp.ID)
	if err != nil {
		l.t.Fatalf("inspect seed container: %v", err)
	}
	if j.State.ExitCode != 0 {
		l.t.Fatalf("seeding %s exited %d", name, j.State.ExitCode)
	}

	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// Volumes lists the store volumes this suite run produced.
func (l *Lab) Volumes(ctx context.Context) []*volume.Volume {
	l.t.Helper()
	vs, err := labdaemon.Volumes(ctx, l.cli)
	if err != nil {
		l.t.Fatalf("%v", err)
	}
	return scoped(labdaemon.Objects{Volumes: vs}).Volumes
}

// Networks lists the networks wearing ember's run-id label that this suite run
// produced.
func (l *Lab) Networks(ctx context.Context) []network.Summary {
	l.t.Helper()
	ns, err := labdaemon.Networks(ctx, l.cli)
	if err != nil {
		l.t.Fatalf("%v", err)
	}
	return scoped(labdaemon.Objects{Networks: ns}).Networks
}

// StateDir is where the journal lives.
func (l *Lab) StateDir() string { return filepath.Join(l.Dir, "state") }

// OutputDir is where fetched outputs land, one directory per run.
func (l *Lab) OutputDir() string { return filepath.Join(l.Dir, "outputs") }

// FetchedFiles lists every file under the output directory, relative to it.
//
// Flattened rather than returning the run directory, because the run
// directory's name carries a timestamp a lab should not have to predict.
func (l *Lab) FetchedFiles() []string {
	l.t.Helper()
	var out []string
	err := filepath.WalkDir(l.OutputDir(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(l.OutputDir(), p)
		if err != nil {
			return err
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		l.t.Fatalf("walk the output directory: %v", err)
	}
	sort.Strings(out)
	return out
}

// Records returns the journal, read through statestore itself.
//
// Going through the real package rather than globbing a hand-spelled path is
// what keeps this honest. Two earlier versions got it wrong: one read a single
// nodes.json, the other read the state directory while records live in a
// "nodes" subdirectory of it. Both returned nothing, both reported an empty
// journal while ember was writing records perfectly well, and a lab asserting
// "a clean run leaves no records" passed without checking anything.
func (l *Lab) Records() []statestore.Record {
	l.t.Helper()
	store, err := statestore.Open(filepath.Join(l.StateDir(), statestore.DirName))
	if err != nil {
		l.t.Fatalf("open the journal: %v", err)
	}
	recs, err := store.List()
	if err != nil {
		l.t.Fatalf("read the journal: %v", err)
	}
	return recs
}

// RecordedNodeIDs are the provider node ids the journal knows about.
func (l *Lab) RecordedNodeIDs() []string {
	l.t.Helper()
	var out []string
	for _, r := range l.Records() {
		if h, ok := r.Handle(); ok {
			out = append(out, h.ID)
		}
	}
	return out
}

// WaitRecorded blocks until the journal holds a node id for some run.
//
// The labs that interrupt ember need this, or whether the record is complete
// depends on how fast the machine is and the lab asserts a different thing on
// a loaded CI runner than on a laptop. That is exactly what happened: the
// SIGKILL lab passed locally, where the kill landed before the id was
// written, and failed on CI, where it landed after.
func (l *Lab) WaitRecorded(within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if len(l.RecordedNodeIDs()) > 0 {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("no node id reached the journal within %v; it holds %d record(s)", within, len(l.Records()))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// FreePort asks the kernel for a port nothing is using.
//
// A hardcoded port would collide with whatever else a CI runner is doing, and
// the failure would read as "ember could not bind" rather than "two tests
// wanted the same number". There is a race between closing this listener and
// ember binding it; the alternative is a fixed port, whose collision window is
// the entire run rather than a microsecond.
func FreePort(t *testing.T) int {
	t.Helper()
	// Through loopback.Listen, not net.Listen. lab.go is a non-test file, so
	// TestNoListenOutsideLoopback covers it -- and it caught this exact line.
	// The right answer was to route it through the one sanctioned listener
	// rather than widen the guard: a rule with an exemption for the test
	// harness is a rule with an exemption for whoever needs one next.
	ln, err := loopback.Listen(context.Background(), loopback.Port(0))
	if err != nil {
		t.Fatalf("find a free port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("release the probe listener: %v", err)
	}
	return port
}

// WaitNode blocks until ember has created a node, or fails the lab.
func (l *Lab) WaitNode(ctx context.Context, within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if len(l.Nodes(ctx)) > 0 {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("no node wearing ember's label appeared within %v", within)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// WaitNoNodes blocks until nothing wearing ember's label is left.
//
// Separate from the cleanup's stray report because destroy is allowed to take
// time: it runs on a context that deliberately outlives an interrupt, with its
// own two minute budget, and asserting immediately after a signal would be a
// race the lab loses at random.
func (l *Lab) WaitNoNodes(ctx context.Context, within time.Duration, detail func() string) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		if len(l.Nodes(ctx)) == 0 {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("node(s) survived %v; on a rented GPU each would still be billing\n%s", within, detail())
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// NodeSpec describes a node the lab starts itself, bypassing ember.
//
// Used by the labs that need a node to still be there after ember has
// finished with it -- the host-key mismatch lab in particular, where the
// control under test only triggers on a second dial to the same node id, and
// ember's nodes are destroyed by design.
type NodeSpec struct {
	Name string

	// Image overrides LabImage.
	//
	// It exists so the listener assertion can be made about the image that
	// actually goes onto a public IP. ember-sd repeats the node image's
	// sshd configuration by hand, on a different base, with a different
	// package manager -- and until this field existed, the lab that proves
	// nothing listens outside port 22 could only be run against the one
	// image that never leaves the laptop.
	Image string

	// AuthorizedKey is the operator's public key, in authorized_keys form.
	AuthorizedKey string

	// HostKeyPEM is the private host key ember minted. It travels in the
	// Engine API's JSON body and never in argv, the same as in production:
	// /proc/<pid>/cmdline is world-readable.
	HostKeyPEM string

	// TTLSeconds arms the node's own self-destruct. Zero disarms it, which is
	// what a lab wants unless the self-destruct is the thing being tested.
	TTLSeconds int

	// EmberLabels, when set, makes the container look like one of ember's own
	// to the local provider -- so `ember ls` and `ember reap` classify it.
	// Deadline is encoded into the label, which lets a lab produce a node
	// that is already expired without waiting for a clock. The node's own
	// self-destruct stays disarmed unless TTLSeconds says otherwise, so reap
	// is the only thing that can end it and the lab tests reap rather than
	// racing it.
	EmberLabels bool

	// Deadline goes into the ember label when EmberLabels is set.
	Deadline time.Time

	// RunID goes into the ember label. Empty derives one from Name.
	RunID node.RunID

	// Binds is host:container directory pairs to mount into the node.
	//
	// It exists for the one thing a control-plane fake cannot pretend: a
	// store. `runpodfake` answers `mounts.network` by handing the directory
	// backing that volume through here, so the node really has weights at
	// /models and the S3 endpoint over the same directory really sees what
	// `embernode fetch` wrote. A fake that accepted the mount and started a
	// container with nothing at the path would make a full store and an empty
	// one the same lab.
	Binds []Bind
}

// Bind is one host directory mounted into a node.
type Bind struct {
	// Host is the directory on this machine. It is created by whoever owns
	// it, never here: a helper that made the directory would own what it did
	// not create, which is the rule CreateVolume follows from the other side.
	Host string

	// Path is where the container sees it.
	Path string
}

// StartNode runs the lab image directly and returns its loopback SSH address.
//
// The port is read back from the daemon rather than requested, because asking
// for host port 0 and then assuming you know which port you got is how a
// forward silently points at somebody else's service.
func (l *Lab) StartNode(ctx context.Context, spec NodeSpec) (id, addr string) {
	l.t.Helper()

	runID := spec.RunID
	if runID == "" {
		// The run id is a fixed-width hex token, so a lab name cannot be used
		// directly; derive one that still traces back to the lab.
		runID = node.RunID(fmt.Sprintf("%016x", crc64.Checksum([]byte(spec.Name), crc64.MakeTable(crc64.ECMA))))
	}

	env := []string{
		"EMBER_AUTHORIZED_KEY=" + spec.AuthorizedKey,
		"EMBER_HOST_KEY=" + spec.HostKeyPEM,
		"EMBER_RUN_ID=" + string(runID),
	}
	if spec.TTLSeconds > 0 {
		env = append(env, fmt.Sprintf("EMBER_TTL_SECONDS=%d", spec.TTLSeconds))
	}

	labels := map[string]string{dockerengine.LabelRunID: "lab-" + spec.Name}
	if spec.EmberLabels {
		deadline := spec.Deadline
		if deadline.IsZero() {
			deadline = time.Now().Add(time.Hour)
		}
		labels = map[string]string{
			dockerengine.LabelRunID: string(runID),
			local.LabelKind:         "node",
			local.LabelLabel:        provider.Label(runID, deadline),
			local.LabelUser:         "root",
		}
	}

	resp, err := l.cli.ContainerCreate(ctx,
		&container.Config{
			Image: imageOr(spec.Image),
			Env:   env,
			// Always labelled with ember's run-id key, so the cleanup's
			// stray report covers it and a lab that forgets to clean up
			// fails rather than leaking a container onto the developer's
			// machine.
			Labels:       labels,
			ExposedPorts: nat.PortSet{"22/tcp": struct{}{}},
		},
		&container.HostConfig{
			// Loopback, not Docker's 0.0.0.0 default: a lab must not publish
			// a key-only sshd to the whole LAN for the length of a test run.
			PortBindings: nat.PortMap{"22/tcp": []nat.PortBinding{{HostIP: "127.0.0.1"}}},
			Binds:        binds(spec.Binds),
		},
		nil, nil, "")
	if err != nil {
		l.t.Fatalf("create node container: %v", err)
	}
	id = resp.ID
	l.t.Cleanup(func() {
		_ = l.cli.ContainerRemove(context.Background(), id, container.RemoveOptions{Force: true})
	})

	if err := l.cli.ContainerStart(ctx, id, container.StartOptions{}); err != nil {
		l.t.Fatalf("start node container: %v", err)
	}

	j, err := l.cli.ContainerInspect(ctx, id)
	if err != nil {
		l.t.Fatalf("inspect node container: %v", err)
	}
	bs := j.NetworkSettings.Ports["22/tcp"]
	if len(bs) == 0 {
		l.t.Fatalf("the daemon published nothing for 22/tcp on %s", id[:12])
	}
	return id, net.JoinHostPort("127.0.0.1", bs[0].HostPort)
}

// binds renders the Docker Engine API's bind spelling.
//
// Read-write, because that is what a RunPod network volume is: the store is
// writable by every node that mounts it, which is exactly why every asset is
// verified by digest before any GPU time is spent on it. A read-only mount
// here would make the control that stands in for one unreachable.
func binds(bs []Bind) []string {
	if len(bs) == 0 {
		return nil
	}
	out := make([]string, 0, len(bs))
	for _, b := range bs {
		out = append(out, b.Host+":"+b.Path)
	}
	return out
}

// BridgeGateway is this machine's address as a node container sees it.
//
// Read from the daemon rather than assumed to be 172.17.0.1, which is the
// default and not a promise: a daemon configured with another `bip` answers
// something else, and a lab serving an asset at a guessed address fails with
// a connection timeout naming nothing.
//
// It is here because an asset fetch is the one lab traffic that originates
// INSIDE a container. Every other server a lab stands up is reached by the
// test process over loopback, which a container cannot see at all.
func (l *Lab) BridgeGateway(ctx context.Context) string {
	l.t.Helper()
	n, err := l.cli.NetworkInspect(ctx, "bridge", network.InspectOptions{})
	if err != nil {
		l.t.Fatalf("inspect the default bridge network: %v", err)
	}
	for _, c := range n.IPAM.Config {
		if c.Gateway != "" {
			return c.Gateway
		}
	}
	l.t.Fatalf("the default bridge network reports no gateway, so nothing in a container can reach this machine: %+v", n.IPAM)
	return ""
}

// Remove destroys one node container.
//
// Used by a fake control plane whose delete call has to actually take the
// machine away: a fake that marked a pod deleted while the container kept
// running would let a lab assert a clean teardown that never happened.
func (l *Lab) Remove(ctx context.Context, id string) {
	l.t.Helper()
	if err := l.cli.ContainerRemove(ctx, id, container.RemoveOptions{Force: true}); err != nil {
		l.t.Errorf("remove node %s: %v", id, err)
	}
}

// RotateHostKey replaces the node's host key and restarts sshd.
//
// This is what an attacker substituting a machine looks like from the client's
// side, and it is the only way to make a pin mismatch happen on purpose.
func (l *Lab) RotateHostKey(ctx context.Context, id string) {
	l.t.Helper()
	l.exec(ctx, id, "rm", "-f", "/etc/ssh/ssh_host_ed25519_key", "/etc/ssh/ssh_host_ed25519_key.pub")
	l.exec(ctx, id, "ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", "/etc/ssh/ssh_host_ed25519_key")
	// sshd reads its host keys at startup, so the new key only takes effect
	// once the running daemon is replaced.
	l.exec(ctx, id, "pkill", "-HUP", "sshd")
}

// Exec runs a command inside a node container over the Docker API.
//
// Out-of-band on purpose: a lab seeding files on a node should not go through
// ember's own SSH path, both because that is the thing under test and because
// widening sshx.Runner with stdin support to let a test write a file would be
// a production interface shaped by a test's convenience.
func (l *Lab) Exec(ctx context.Context, id string, args ...string) {
	l.t.Helper()
	l.exec(ctx, id, args...)
}

// ExecOut runs a command in a node container and returns its stdout.
func (l *Lab) ExecOut(ctx context.Context, id string, args ...string) string {
	l.t.Helper()
	return l.execOut(ctx, id, args...)
}

func (l *Lab) exec(ctx context.Context, id string, args ...string) {
	l.t.Helper()
	l.execOut(ctx, id, args...)
}

func (l *Lab) execOut(ctx context.Context, id string, args ...string) string {
	l.t.Helper()
	ex, err := l.cli.ContainerExecCreate(ctx, id, container.ExecOptions{
		Cmd: args, AttachStdout: true, AttachStderr: true,
	})
	if err != nil {
		l.t.Fatalf("exec create %v: %v", args, err)
	}
	_ = ex
	att, err := l.cli.ContainerExecAttach(ctx, ex.ID, container.ExecAttachOptions{})
	if err != nil {
		l.t.Fatalf("exec attach %v: %v", args, err)
	}
	var out, errb bytes.Buffer
	_, _ = stdcopy.StdCopy(&out, &errb, att.Reader)
	att.Close()

	ins, err := l.cli.ContainerExecInspect(ctx, ex.ID)
	if err != nil {
		l.t.Fatalf("exec inspect %v: %v", args, err)
	}
	if ins.ExitCode != 0 {
		l.t.Fatalf("exec %v exit %d: %s%s", args, ins.ExitCode, out.String(), errb.String())
	}
	return out.String()
}

// WaitSSH waits for a completed SSH handshake, not merely a TCP accept.
//
// sshd binds its listener before it is ready to negotiate, so a lab that
// waited on a successful dial would race the daemon and fail at random in the
// code under test rather than here.
func (l *Lab) WaitSSH(addr string, within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		b, err := l.readBannerAt(addr, 2*time.Second)
		if err == nil && strings.Contains(b, "SSH-2.0") {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("no sshd banner from %s within %v (last: %q, %v)", addr, within, b, err)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

func (l *Lab) readBannerAt(addr string, within time.Duration) (string, error) {
	d := net.Dialer{Timeout: within}
	conn, err := d.Dial("tcp", addr)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return "", err
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if n > 0 {
		return strings.TrimSpace(string(buf[:n])), nil
	}
	return "", err
}

// WaitExited blocks until the container is no longer running.
func (l *Lab) WaitExited(ctx context.Context, id string, within time.Duration) {
	l.t.Helper()
	deadline := time.Now().Add(within)
	for {
		j, err := l.cli.ContainerInspect(ctx, id)
		if err != nil {
			l.t.Fatalf("inspect %s: %v", id[:12], err)
		}
		if !j.State.Running {
			return
		}
		if time.Now().After(deadline) {
			l.t.Fatalf("container %s was still running after %v; its self-destruct did nothing", id[:12], within)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// DigestOf is the SHA-256 a lab should claim for this content.
func DigestOf(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// imageOr is the image a node runs, defaulting to the lab's own.
func imageOr(name string) string {
	if name != "" {
		return name
	}
	return LabImage
}

// HaveImage reports whether the daemon has this image locally.
//
// Used by the labs that assert about a published image: they must not pull
// one -- a lab that reached the network would be testing GitHub -- and they
// must not skip, because a silently skipped lab is worse than a red build.
// The caller fails with the command that builds it.
func (l *Lab) HaveImage(ctx context.Context, name string) bool {
	l.t.Helper()
	_, err := l.cli.ImageInspect(ctx, name)
	return err == nil
}

// TagImage points a second name at an image that already exists, and removes
// the name again when the lab finishes.
//
// For labs about which image ember *chose*, where what the bytes are does not
// matter. `docker tag` copies nothing and costs nothing, and ember:lab is
// already a hard prerequisite of every lab here -- so several architectures'
// worth of stand-ins is a few milliseconds rather than a CUDA compile.
//
// The cleanup removes the tag and nothing else. A forced remove would delete
// the underlying image, which is ember:lab, out from under every lab that runs
// after this one. It also has to be a cleanup rather than a reap: the stray
// report polices containers, networks and volumes by label -- labdaemon has no
// image kind at all -- and a tag inherits its source's labels, so a stray tag
// is indistinguishable from ember:lab by label and ownership is the only
// mechanism available. A lab helper owns what it creates.
func (l *Lab) TagImage(ctx context.Context, src, dst string) string {
	l.t.Helper()
	if _, err := l.cli.ImageInspect(ctx, src); err != nil {
		l.t.Fatalf("cannot tag %s as %s: the source is not on this daemon.\n"+
			"  make docker-build", src, dst)
	}
	if err := l.cli.ImageTag(ctx, src, dst); err != nil {
		l.t.Fatalf("tag %s as %s: %v", src, dst, err)
	}
	l.t.Cleanup(func() {
		// PruneChildren false and Force false: this removes the name, not the
		// image. The source keeps every other tag it had.
		if _, err := l.cli.ImageRemove(context.WithoutCancel(ctx), dst,
			dockerimage.RemoveOptions{Force: false, PruneChildren: false}); err != nil {
			l.t.Logf("could not untag %s: %v", dst, err)
		}
	})
	return dst
}

// ImageLabel reads one label off an image, or "" when it is not set.
//
// Read from the daemon rather than from a running container, because a label
// is a property of the image and travels with it into a registry -- which is
// what makes it worth comparing against what a build was asked for.
func (l *Lab) ImageLabel(ctx context.Context, image, key string) string {
	l.t.Helper()
	insp, err := l.cli.ImageInspect(ctx, image)
	if err != nil {
		l.t.Fatalf("inspect %s: %v", image, err)
	}
	if insp.Config == nil {
		return ""
	}
	return insp.Config.Labels[key]
}
