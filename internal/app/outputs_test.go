package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/logx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/transfer"
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
	cmd, err := transfer.TarCommand("/workspace/out")
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
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, dir); err != nil {
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
	good, _ := transfer.TarCommand("/workspace/out")
	bad, _ := transfer.TarCommand("/workspace/missing")
	r.Outputs[good.String()] = tarOne(t, "out/a.png", "pixels")
	r.Errors[bad.String()] = &sshx.ExitError{Cmd: bad.String(), Code: 2, Stderr: "No such file or directory"}

	base := t.TempDir()
	var out bytes.Buffer
	err := fetchOutputs(t.Context(), &out, r, []node.OutputPath{
		{Remote: "/workspace/out", Local: "renders"},
		{Remote: "/workspace/missing", Local: "logs"},
	}, base)

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
	if err := fetchOutputs(t.Context(), &out, sshx.NewRecordingRunner(), nil, t.TempDir()); err != nil {
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
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, t.TempDir())
	if err == nil {
		t.Fatal("a cancelled fetch reported success")
	}
}

// TestRunWorkload_RunsTheCommandAndReportsIt.
//
// A node rented to run something stops when it has run it. Holding afterwards
// would bill for a machine with nothing left to do, which is the failure mode
// ember exists to prevent.
func discardLog() *slog.Logger { return slog.New(slog.DiscardHandler) }

func TestCollectNodeLog_BringsEmbernodesOwnLogHome(t *testing.T) {
	r := sshx.NewRecordingRunner()
	cmd, err := transfer.TarCommand(logx.NodeLogPath)
	if err != nil {
		t.Fatal(err)
	}
	r.Outputs[cmd.String()] = tarOne(t, "embernode.log", "level=INFO msg=\"sshd started\"\n")

	dir := transfer.RunDir(t.TempDir(), "0123456789abcdef", time.Now())
	collectNodeLog(t.Context(), r, dir, slog.New(slog.DiscardHandler))

	b, err := os.ReadFile(filepath.Join(dir, "embernode.log"))
	if err != nil {
		t.Fatalf("the node's log did not come home: %v", err)
	}
	if !strings.Contains(string(b), "sshd started") {
		t.Errorf("the log arrived empty of what the node did: %q", b)
	}
}

// TestCollectNodeLog_CannotFailARun is the rule that is not negotiable.
//
// Logging is the one subsystem that must never be load-bearing, and the ways
// this fails are ordinary: a node image whose embernode predates the file, an
// sshd already gone, a read-only /tmp. GNU tar over a missing path exits
// non-zero and writes a valid *empty* archive, so "there is no log" arrives
// here as an error rather than as an empty result -- which is why the
// swallowing has to be tested rather than assumed from the shape of the code.
//
// There is no return value to assert on, deliberately: a function that
// reported a failure would be a function a caller could decide to propagate.
// So what is checked is that it does not panic, writes nothing, and says so
// at debug -- the level that exists for exactly this.
func TestCollectNodeLog_CannotFailARun(t *testing.T) {
	r := sshx.NewRecordingRunner()
	r.FailOn = "tar"

	var logged bytes.Buffer
	// A test reading what was logged, which is why seal_test.go exempts
	// _test.go files from the one-construction-site rule.
	log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

	dir := transfer.RunDir(t.TempDir(), "0123456789abcdef", time.Now())
	collectNodeLog(t.Context(), r, dir, log)

	if !strings.Contains(logged.String(), "could not bring the node's own log home") {
		t.Errorf("a failed collection was not reported at debug:\n%s", logged.String())
	}
	if _, err := os.Stat(filepath.Join(dir, "embernode.log")); err == nil {
		t.Error("a failed collection left a file behind")
	}
}

// TestBuildOptions_HonoursTheLevelItWasGiven.
//
// This is the whole of `--log-level debug` arriving where it has to arrive.
// The level was hard-coded to info at this call site, so seventeen debug
// lines in this tree could not be displayed by any invocation of ember that
// shipped -- and the plumbing between the flag and the handler is the half a
// test can hold still.
func TestBuildOptions_HonoursTheLevelItWasGiven(t *testing.T) {
	ps := Paths{State: t.TempDir(), Config: t.TempDir(), Outputs: t.TempDir()}

	for _, tc := range []struct {
		name string
		opts logx.Options
		want bool
	}{
		{"the default is still info", logx.Options{}, false},
		{"debug is reachable", logx.Options{Level: slog.LevelDebug}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o, err := BuildOptions(ps, config.Config{}, io.Discard, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			if got := o.Log.Enabled(t.Context(), slog.LevelDebug); got != tc.want {
				t.Errorf("debug enabled = %v, want %v", got, tc.want)
			}
		})
	}
}
