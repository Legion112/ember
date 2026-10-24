//go:build integration

package integration

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/test/integration/harness"
)

// TestLifecycle_ForwardsRealBytesAndDestroys is the whole product in one test.
//
// ember rents a container, authenticates against a host key it minted and
// injected itself, forwards a port to loopback, and destroys the node. Every
// layer runs its production code: the real Docker client in dockerclient,
// which the coverage gate excludes as a branchless adapter and which therefore
// runs nowhere else; a real sshd; a real SSH handshake against a pinned host
// key; a real direct-tcpip forward.
//
// The forward is pointed at the node's own sshd so the lab can read a banner
// back. That is bytes which provably crossed the tunnel from inside the
// container -- a successful accept on the local listener would not show that,
// and a tunnel which completes a connect and then drops payload passes the
// weaker test and fails in production.
func TestLifecycle_ForwardsRealBytesAndDestroys(t *testing.T) {
	l := harness.New(t)
	port := harness.FreePort(t)

	cmd, wait := l.Start(t.Context(), "up",
		"--hold", "3m",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)

	l.WaitNode(t.Context(), 90*time.Second)

	// The forward is opened after the node is ready, so the banner read is
	// retried rather than attempted once against a listener that may not be
	// bound yet.
	var banner string
	deadline := time.Now().Add(90 * time.Second)
	for {
		b, err := l.ReadBanner(port, 5*time.Second)
		if err == nil && b != "" {
			banner = b
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("nothing came back through the forward within 90s: %v\n%s", err, wait())
		}
		time.Sleep(500 * time.Millisecond)
	}

	if !strings.Contains(banner, "SSH-2.0") {
		t.Errorf("the bytes through the tunnel are not an sshd banner: %q", banner)
	}

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
}

// TestLifecycle_CleanRunLeavesAnEmptyJournal. A record that outlives its node
// makes every later `ember ls` report an orphan that is not there, and an
// operator who learns to ignore that output is an operator who will ignore a
// real one.
func TestLifecycle_CleanRunLeavesAnEmptyJournal(t *testing.T) {
	l := harness.New(t)
	port := harness.FreePort(t)

	res := l.Run(t.Context(), "up",
		"--hold", "2s",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)
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
	port := harness.FreePort(t)

	res := l.Run(t.Context(), "up",
		"--hold", "2s",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)
	if res.ExitCode != 0 {
		t.Fatalf("ember up exited %d\n%s", res.ExitCode, res.Combined())
	}

	files := l.FetchedFiles()
	if len(files) != 1 {
		t.Fatalf("fetched %v, want exactly one file\n%s", files, res.Combined())
	}
	// The run directory's name is a timestamp plus the run id, so the
	// assertion is on the tail.
	if !strings.HasSuffix(files[0], filepath.Join("host", "hostname")) {
		t.Errorf("fetched %q, want it to end in host/hostname", files[0])
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

// TestLifecycle_AFailedOutputFetchStillDestroysTheNode. The fetch happens on
// a context that outlives the session precisely so it can run during
// teardown, and a path that was never written must not be able to keep a
// rented machine alive.
func TestLifecycle_AFailedOutputFetchStillDestroysTheNode(t *testing.T) {
	l := harness.New(t)
	l.WriteConfig(map[string]string{"outputs": "[/workspace/never-written:renders]"})
	port := harness.FreePort(t)

	res := l.Run(t.Context(), "up",
		"--hold", "2s",
		"--port", fmt.Sprintf("sshd:22:%d", port),
	)
	if res.ExitCode == 0 {
		t.Errorf("a missing output path was reported as a clean run\n%s", res.Combined())
	}
	l.WaitNoNodes(t.Context(), 2*time.Minute, res.Combined)
}
