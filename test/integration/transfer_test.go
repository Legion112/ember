//go:build integration

package integration

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/sshx/sshexec"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/test/integration/harness"
)

// TestTransfer_FetchesOutputsOverARealSSHConnection.
//
// The extraction is unit-tested against in-memory archives, which is where the
// containment rules belong. What only a real node can show is the other half:
// that GNU tar invoked exactly the way TarCommand spells it produces an
// archive whose member names are what the extractor expects. The two halves
// being tested separately is how a fetch passes every unit test and still
// writes files in the wrong place.
func TestTransfer_FetchesOutputsOverARealSSHConnection(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "outputs")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	runner := sshexec.New(client.SSH())

	// Seeded over the Docker API rather than over SSH: the fetch is what is
	// under test, and the setup should not share its code path.
	//
	// A directory and a file, the two shapes an output path can be.
	l.Exec(t.Context(), n.ID, "mkdir", "-p", "/workspace/out/nested")
	l.Exec(t.Context(), n.ID, "sh", "-c", "printf pixels > /workspace/out/frame0001.png")
	l.Exec(t.Context(), n.ID, "sh", "-c", "printf 'more pixels' > /workspace/out/nested/frame0002.png")
	l.Exec(t.Context(), n.ID, "sh", "-c", "printf 'it worked' > /workspace/run.log")

	dir := filepath.Join(t.TempDir(), "run")
	res, err := transfer.Fetch(t.Context(), runner, []node.OutputPath{
		{Remote: "/workspace/out", Local: "renders"},
		{Remote: "/workspace/run.log", Local: "logs"},
	}, dir, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if res.Files != 3 {
		t.Errorf("files = %d, want 3", res.Files)
	}

	// The member names GNU tar produces for `-C /workspace -- out` are
	// "out/..." relative paths, which is the whole reason TarCommand changes
	// directory instead of tarring an absolute path.
	for rel, want := range map[string]string{
		"renders/out/frame0001.png":        "pixels",
		"renders/out/nested/frame0002.png": "more pixels",
		"logs/run.log":                     "it worked",
	} {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// TestTransfer_AMissingPathIsAnErrorAndNotAnEmptyArchive. GNU tar exits
// non-zero and writes a valid empty archive, so without reading the exit
// status this would look like a successful fetch of nothing -- which is the
// specific way an output fetch loses a whole run quietly.
func TestTransfer_AMissingPathIsAnErrorAndNotAnEmptyArchive(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "missing")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	dir := filepath.Join(t.TempDir(), "run")
	_, err = transfer.Fetch(t.Context(), sshexec.New(client.SSH()),
		[]node.OutputPath{{Remote: "/workspace/never-written", Local: "renders"}}, dir, "")
	if err == nil {
		t.Fatal("tar over a path that does not exist was reported as a successful fetch")
	}

	var exit *sshx.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("got %T (%v), want an sshx.ExitError carrying tar's status", err, err)
	}
	// tar's own stderr is the only thing that says which path was missing.
	if !strings.Contains(err.Error(), "never-written") {
		t.Errorf("the error does not name the path: %v", err)
	}
}

// TestTransfer_RefusesASymlinkTheNodeWrote. The containment rules are
// unit-tested, but this proves the kinds survive a real tar: GNU tar stores a
// symlink as TypeSymlink, and ember must decline to recreate it rather than
// follow it into the operator's own filesystem.
func TestTransfer_RefusesASymlinkTheNodeWrote(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "symlink")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	runner := sshexec.New(client.SSH())
	l.Exec(t.Context(), n.ID, "mkdir", "-p", "/workspace/out")
	l.Exec(t.Context(), n.ID, "sh", "-c", "printf pixels > /workspace/out/real.png")
	l.Exec(t.Context(), n.ID, "ln", "-s", "/etc/shadow", "/workspace/out/stolen")

	dir := filepath.Join(t.TempDir(), "run")
	res, err := transfer.Fetch(t.Context(), runner,
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, dir, "")

	// The real file still arrives.
	if res.Files != 1 {
		t.Errorf("files = %d, want the one real file", res.Files)
	}
	var skipped *transfer.SkippedError
	if !errors.As(err, &skipped) {
		t.Fatalf("got %v, want a SkippedError naming the symlink", err)
	}
	if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("the report does not say what was refused: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "renders", "out", "stolen")); err == nil {
		t.Error("the symlink was recreated on the operator's filesystem")
	}
}

// TestTransfer_SurvivesALargePayload is go-tun's MTU lesson applied here: a
// transfer that carries a few bytes and drops a megabyte passes every small
// test and fails on the first real run.
func TestTransfer_SurvivesALargePayload(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "bulk")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	runner := sshexec.New(client.SSH())
	l.Exec(t.Context(), n.ID, "mkdir", "-p", "/workspace/out")
	// 16 MiB of incompressible bytes, so gzip cannot make the test vacuous.
	l.Exec(t.Context(), n.ID, "dd", "if=/dev/urandom", "of=/workspace/out/big.bin", "bs=1M", "count=16", "status=none")

	dir := filepath.Join(t.TempDir(), "run")
	start := time.Now()
	res, err := transfer.Fetch(t.Context(), runner,
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, dir, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	const want = 16 << 20
	if res.Bytes != want {
		t.Errorf("fetched %d bytes, want %d", res.Bytes, want)
	}
	fi, err := os.Stat(filepath.Join(dir, "renders", "out", "big.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != want {
		t.Errorf("the file on disk is %d bytes, want %d", fi.Size(), want)
	}
	t.Logf("16 MiB in %v", time.Since(start).Round(time.Millisecond))
}

// TestTransfer_AFetchLeavesTheOutputsOnTheNode is a property `README.md` now
// tells operators to rely on, so it is asserted rather than hoped for.
//
// A `cmds` chain works -- entry n+1 can read the file entry n wrote -- for
// exactly one reason: the fetch is a read. `tar -czf -` streams the files out
// and removes nothing, and the `--newer` marker bounds what is *sent* and not
// what *exists*. Both halves are checked here, because only one of them can
// be seen from inside the extractor: that the second fetch carries only the
// new file (the marker working) and that the first file is still on the node
// afterwards (the fetch being non-destructive).
//
// The tidy-up that would break it is obvious and would otherwise land green:
// `tar --remove-files` to spare a rented node's disk, which looks like a pure
// saving once the marker means nothing is re-sent anyway. Verified by doing
// exactly that and watching this fail.
//
// The files are written in order and the marker touched between them, which
// is what runSteps does, rather than having their mtimes set to fixed dates.
// GNU tar's --newer compares **ctime as well as mtime**, so `touch -d` on an
// old date leaves a file that tar still considers new -- the first version of
// this test did exactly that and reported two files on the second fetch,
// blaming the marker for the test's own setup. Each Docker exec is
// milliseconds apart and the node's filesystem stores nanoseconds, so the
// natural ordering is both sufficient and the one ember actually relies on.
func TestTransfer_AFetchLeavesTheOutputsOnTheNode(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "chain")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	runner := sshexec.New(client.SSH())

	// Seeded over the Docker API, not over SSH: the fetch is what is under
	// test and the setup must not share its code path.
	//
	// The marker is the lab's own path and deliberately not a second spelling
	// of internal/app's `fetchMarker`. Fetch takes `since` as a parameter, so
	// nothing here depends on which file ember picks -- and a copy of that
	// literal is the "one name for a path, not one per call site" drift that
	// made the lab harness read the wrong journal directory once.
	const mark = "/tmp/.lab-fetch-mark"
	l.Exec(t.Context(), n.ID, "mkdir", "-p", "/workspace/out")
	l.Exec(t.Context(), n.ID, "sh", "-c", "printf clip01 > /workspace/out/clip01.mp4")

	dir := filepath.Join(t.TempDir(), "run")
	outputs := []node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}

	res, err := transfer.Fetch(t.Context(), runner, outputs, dir, "")
	if err != nil {
		t.Fatalf("first fetch: %v", err)
	}
	if res.Files != 1 {
		t.Fatalf("first fetch brought %d file(s), want 1", res.Files)
	}

	// Asserted here, before anything else uses it, so that a destructive
	// fetch fails on the sentence it broke rather than further down as a
	// shell complaining it cannot write into a directory that is gone --
	// which is what `tar --remove-files` actually produces.
	if ls := l.ExecOut(t.Context(), n.ID, "sh", "-c", "ls /workspace/out || true"); !strings.Contains(ls, "clip01.mp4") {
		t.Fatalf("the fetch removed the output from the node, so no cmds chain can work: ls said %q", ls)
	}

	// What runSteps does between steps, and then the next step's work: the
	// second clip is derived from the first, which is only possible because
	// the first is still there.
	l.Exec(t.Context(), n.ID, "touch", mark)
	out := l.ExecOut(t.Context(), n.ID, "sh", "-c",
		"cat /workspace/out/clip01.mp4 > /workspace/out/clip02.mp4 && printf +clip02 >> /workspace/out/clip02.mp4 && cat /workspace/out/clip02.mp4")
	if !strings.Contains(out, "clip01+clip02") {
		t.Fatalf("the second step could not read the first step's output: %q", out)
	}

	res, err = transfer.Fetch(t.Context(), runner, outputs, dir, mark)
	if err != nil {
		t.Fatalf("second fetch: %v", err)
	}
	// One file, not two: the marker is what stops a clip already home from
	// crossing the operator's link again, which for minutes of video is the
	// whole reason it exists.
	if res.Files != 1 {
		t.Errorf("second fetch brought %d file(s), want only the new one", res.Files)
	}

	// And the first clip is still on the node after both fetches, which is
	// the property a chain depends on.
	if out := l.ExecOut(t.Context(), n.ID, "ls", "/workspace/out"); !strings.Contains(out, "clip01.mp4") {
		t.Errorf("clip01.mp4 is gone from the node after the fetch; a cmds chain cannot work: %q", out)
	}

	for rel, want := range map[string]string{
		"renders/out/clip01.mp4": "clip01",
		"renders/out/clip02.mp4": "clip01+clip02",
	} {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", rel, got, want)
		}
	}
}

// TestSSHX_QuotingSurvivesARealShell is the half a unit test cannot do.
//
// sshx.Quote claims that inside single quotes a POSIX shell treats every
// character literally. A unit test can only check that the string ember built
// looks right; whether the remote shell agrees is a question for a remote
// shell. So these go over a real SSH connection to a real sshd, which hands
// the line to a real /bin/sh, and the arguments are printed back one per line
// for comparison.
//
// Each input would do something if it escaped quoting: run a command, expand
// a variable, split into two words, or truncate the argument list.
func TestSSHX_QuotingSurvivesARealShell(t *testing.T) {
	l := harness.New(t)
	n := mintAndStart(t, l, "quoting")

	trust, err := hostkey.Pinned(fingerprintOf(t, n.Pair))
	if err != nil {
		t.Fatal(err)
	}
	client, err := dial(t, l, n, trust)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()
	runner := sshexec.New(client.SSH())

	hostile := []string{
		"a lovely cat",
		"$(touch /tmp/pwned)",
		"`touch /tmp/pwned2`",
		"a;touch /tmp/pwned3",
		"$HOME",
		"${PATH}",
		"a|b",
		"a&b",
		"a>/tmp/pwned4",
		"it's a cat",
		`a"b`,
		"*",
		"~",
		"back\\slash",
		"a\tb",
	}

	// printf '%s\n' prints each argument on its own line, so the comparison
	// is exact and a dropped or split argument shows up immediately.
	stdout, wait, err := runner.Run(t.Context(), sshx.Cmd("printf", append([]string{"%s\n"}, hostile...)...))
	if err != nil {
		t.Fatal(err)
	}
	raw, rerr := io.ReadAll(stdout)
	_ = stdout.Close()
	if rerr != nil {
		t.Fatal(rerr)
	}
	if err := wait(); err != nil {
		t.Fatalf("printf: %v", err)
	}

	got := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(got) != len(hostile) {
		t.Fatalf("got %d arguments back, sent %d: %q", len(got), len(hostile), got)
	}
	for i, want := range hostile {
		if got[i] != want {
			t.Errorf("argument %d came back changed:\n  sent %q\n  got  %q", i, want, got[i])
		}
	}

	// And nothing ran: every substitution above would have left a file.
	for _, marker := range []string{"/tmp/pwned", "/tmp/pwned2", "/tmp/pwned3", "/tmp/pwned4"} {
		_, w, lerr := runner.Run(t.Context(), sshx.Cmd("test", "-e", marker))
		if lerr != nil {
			t.Fatal(lerr)
		}
		if w() == nil {
			t.Errorf("%s exists: an argument escaped quoting and ran", marker)
		}
	}
}
