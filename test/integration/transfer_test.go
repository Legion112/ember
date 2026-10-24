//go:build integration

package integration

import (
	"errors"
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
	}, dir)
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
		[]node.OutputPath{{Remote: "/workspace/never-written", Local: "renders"}}, dir)
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
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, dir)

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
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, dir)
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
