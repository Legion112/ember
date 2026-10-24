package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/config"
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

	base := t.TempDir()
	var out bytes.Buffer
	if err := fetchOutputs(t.Context(), &out, r, "0123456789abcdef",
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, base); err != nil {
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
	err := fetchOutputs(t.Context(), &out, r, "0123456789abcdef", []node.OutputPath{
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
	if err := fetchOutputs(t.Context(), &out, sshx.NewRecordingRunner(), "0123456789abcdef", nil, t.TempDir()); err != nil {
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
	err := fetchOutputs(ctx, &out, sshx.NewRecordingRunner(), "0123456789abcdef",
		[]node.OutputPath{{Remote: "/workspace/out", Local: "renders"}}, t.TempDir())
	if err == nil {
		t.Fatal("a cancelled fetch reported success")
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0:                     "0 B",
		1:                     "1 B",
		1023:                  "1023 B",
		1024:                  "1.0 KiB",
		1536:                  "1.5 KiB",
		1 << 20:               "1.0 MiB",
		3*(1<<30) + (1 << 29): "3.5 GiB",
		1 << 40:               "1.0 TiB",
		// Beyond TiB the unit stops climbing rather than running off the end
		// of the table, which is the bug a bare index would have.
		1 << 50: "1024.0 TiB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ember.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestUp_ReportsABadOutputSpec before renting anything. A config error found
// after the create call is a config error that costs money.
func TestUp_ReportsABadOutputSpec(t *testing.T) {
	cfg := writeConfig(t, "provider: local\nimage: ghcr.io/example/wl:v1\nttl: 10m\noutputs:\n  - \":renders\"\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg}, &out, labDeps(t))
	if err == nil {
		t.Fatal("an output with no remote path was accepted")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit = %d, want a usage error (%v)", ExitCode(err), err)
	}
}

// TestUp_ReportsABadBudget. The budget is the thing standing between a typo
// and an eight-GPU node, so a figure ember cannot parse is a refusal rather
// than a default.
func TestUp_ReportsABadBudget(t *testing.T) {
	cfg := writeConfig(t, "provider: local\nimage: ghcr.io/example/wl:v1\nttl: 10m\nmax_hourly_usd: \"not a number\"\n")

	var out bytes.Buffer
	err := runUp([]string{"--config", cfg}, &out, labDeps(t))
	if err == nil {
		t.Fatal("an unparseable budget was accepted")
	}
	if ExitCode(err) != ExitUsage {
		t.Errorf("exit = %d, want a usage error (%v)", ExitCode(err), err)
	}
}

// TestUp_ReportsAnUnusableStateDirectory. The journal is how an orphan is
// found, so a run that cannot write one must not start a node it could then
// lose track of.
func TestUp_ReportsAnUnusableStateDirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := labDeps(t)
	d.HomeDir = home

	var out bytes.Buffer
	if err := runUp([]string{"--image", "ghcr.io/example/wl:v1"}, &out, d); err == nil {
		t.Fatal("a state directory that is a file was accepted")
	}
}

// TestUp_ReportsAnUnusableConfigDirectory, where the operator's SSH identity
// lives. Without a key there is no way to reach the node, so this must fail
// before anything is rented rather than after.
func TestUp_ReportsAnUnusableConfigDirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := labDeps(t)
	d.HomeDir = home

	var out bytes.Buffer
	if err := runUp([]string{"--image", "ghcr.io/example/wl:v1"}, &out, d); err == nil {
		t.Fatal("a config directory that is a file was accepted")
	}
}

func TestLs_ReportsAnUnresolvableHome(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	e := labDeps(t).Engine
	var out bytes.Buffer
	// No HomeDir, so the paths come from the config and cannot resolve.
	if err := runLs(nil, &out, deps{Engine: e, Creds: &config.Credentials{}}); err == nil {
		t.Fatal("a home that cannot be resolved was accepted")
	}
}
