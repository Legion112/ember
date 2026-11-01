package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/Legion112/ember/internal/config"
)

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
	// See TestUp_RefusesAMalformedPort: the status is what is at risk now
	// that the refusal is raised in another package.
	if got := ExitCode(err); got != ExitUsage {
		t.Errorf("exit = %d, want %d for a config error", got, ExitUsage)
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
	if got := ExitCode(err); got != ExitUsage {
		t.Errorf("exit = %d, want %d for a config error", got, ExitUsage)
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
