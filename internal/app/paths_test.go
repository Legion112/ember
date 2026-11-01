package app

import (
	"testing"

	"github.com/Legion112/ember/internal/config"
)

// TestResolvePaths_FallsBackToTheConfigWhenThereIsNoTestHome covers the branch
// every real invocation takes. The tests all inject HomeDir, so without this
// the production path is the only one never exercised.
func TestResolvePaths_FallsBackToTheConfigWhenThereIsNoTestHome(t *testing.T) {
	cfg := config.Config{ConfigDir: "/tmp/ember-c", StateDir: "/tmp/ember-s"}
	ps, err := ResolvePaths(cfg, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if ps.Config != "/tmp/ember-c" || ps.State != "/tmp/ember-s" {
		t.Fatalf("got %+v", ps)
	}
}

func TestResolvePaths_ReportsAnUnresolvableConfigDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := ResolvePaths(config.Default(), Deps{}); err == nil {
		t.Fatal("a home that cannot be resolved was accepted")
	}
}

func TestResolvePaths_ReportsAnUnresolvableStateDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	cfg := config.Default()
	cfg.ConfigDir = t.TempDir() // past the first hurdle, fail on the second
	if _, err := ResolvePaths(cfg, Deps{}); err == nil {
		t.Fatal("a state dir that cannot be resolved was accepted")
	}
}
