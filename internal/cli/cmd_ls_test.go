package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/provider/local"
)

func TestLs_EmptyIsCleanAndExitsZero(t *testing.T) {
	var out bytes.Buffer
	if err := runLs(nil, &out, labDeps(t)); err != nil {
		t.Fatalf("nothing running should exit 0: %v", err)
	}
	if !strings.Contains(out.String(), "no nodes") {
		t.Errorf("got %q", out.String())
	}
}

// TestLs_AnUntrackedNodeExitsTwo is the contract with cron: an orphan and a
// broken ember must not look the same to a timer that only sees a status.
func TestLs_AnUntrackedNodeExitsTwo(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	// A node wearing ember's label that no local record mentions: what a lost
	// state directory looks like, and it is still billing.
	id, err := e.ContainerCreate(t.Context(), dockerengine.ContainerSpec{
		Name:  "ember-node-stray",
		Image: local.DefaultImage,
		Labels: map[string]string{
			dockerengine.LabelRunID: "0123456789abcdef",
			local.LabelKind:         "node",
			local.LabelLabel:        "ember-0123456789abcdef-99999999999",
			local.LabelUser:         "root",
		},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ContainerStart(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	err = runLs(nil, &out, d)
	if err == nil {
		t.Fatal("an untracked running node exited 0")
	}
	if ExitCode(err) != ExitFindings {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitFindings)
	}
	if !strings.Contains(out.String(), "expired") && !strings.Contains(out.String(), "untracked") {
		t.Errorf("the finding was not printed: %q", out.String())
	}
}

func TestLs_ReportsAProviderThatCouldNotBeAsked(t *testing.T) {
	d := labDeps(t)
	d.Engine.(*dockerengine.RecordingEngine).FailOn = "ContainerList"

	var out bytes.Buffer
	err := runLs(nil, &out, d)
	if err == nil {
		t.Fatal("ls reported a clean bill of health without being able to check")
	}
	if ExitCode(err) != ExitFindings {
		t.Errorf("exit = %d, want %d", ExitCode(err), ExitFindings)
	}
	if !strings.Contains(out.String(), "unreachable") {
		t.Errorf("got %q", out.String())
	}
}

func TestLs_ReportsABadFlag(t *testing.T) {
	var out bytes.Buffer
	if err := runLs([]string{"--nope"}, &out, labDeps(t)); ExitCode(err) != ExitUsage {
		t.Fatalf("got %v", err)
	}
}

func TestLs_HelpExplainsTheExitCode(t *testing.T) {
	var out bytes.Buffer
	_ = runLs([]string{"-h"}, &out, labDeps(t))
	if !strings.Contains(out.String(), "Exit 2") {
		t.Errorf("help should explain the exit code: %q", out.String())
	}
}

func TestReap_NothingToDo(t *testing.T) {
	var out bytes.Buffer
	if err := runReap(nil, &out, labDeps(t)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "nothing to reap") {
		t.Errorf("got %q", out.String())
	}
}

// TestReap_DestroysAnExpiredNode and clears nothing else.
func TestReap_DestroysAnExpiredNode(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	// Deadline in 1970: comfortably expired.
	id, err := e.ContainerCreate(t.Context(), dockerengine.ContainerSpec{
		Name:  "ember-node-old",
		Image: local.DefaultImage,
		Labels: map[string]string{
			dockerengine.LabelRunID: "0123456789abcdef",
			local.LabelKind:         "node",
			local.LabelLabel:        "ember-0123456789abcdef-1",
			local.LabelUser:         "root",
		},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ContainerStart(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runReap(nil, &out, d); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !strings.Contains(out.String(), "destroyed 1") {
		t.Errorf("got %q", out.String())
	}
	if len(e.Containers()) != 0 {
		t.Error("the expired node survived reap")
	}
}

// TestReap_DryRunDestroysNothing.
func TestReap_DryRunDestroysNothing(t *testing.T) {
	d := labDeps(t)
	e := d.Engine.(*dockerengine.RecordingEngine)

	id, err := e.ContainerCreate(t.Context(), dockerengine.ContainerSpec{
		Name:  "ember-node-old",
		Image: local.DefaultImage,
		Labels: map[string]string{
			dockerengine.LabelRunID: "0123456789abcdef",
			local.LabelKind:         "node",
			local.LabelLabel:        "ember-0123456789abcdef-1",
			local.LabelUser:         "root",
		},
		PublishTCP: []uint16{22},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ContainerStart(t.Context(), id); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := runReap([]string{"--dry-run"}, &out, d); err != nil {
		t.Fatal(err)
	}
	if len(e.Containers()) != 1 {
		t.Fatal("a dry run destroyed something")
	}
	if !strings.Contains(out.String(), "would destroy") {
		t.Errorf("got %q", out.String())
	}
}

func TestReap_ReportsABadFlag(t *testing.T) {
	var out bytes.Buffer
	if err := runReap([]string{"--nope"}, &out, labDeps(t)); ExitCode(err) != ExitUsage {
		t.Fatalf("got %v", err)
	}
}

func TestReap_HelpExplainsWhyUntrackedNodesSurvive(t *testing.T) {
	var out bytes.Buffer
	_ = runReap([]string{"-h"}, &out, labDeps(t))
	if !strings.Contains(out.String(), "another machine") {
		t.Errorf("help should explain the deliberate restraint: %q", out.String())
	}
}

// TestResolvePaths_FallsBackToTheConfigWhenThereIsNoTestHome covers the branch
// every real invocation takes. The tests all inject HomeDir, so without this
// the production path is the only one never exercised.
func TestResolvePaths_FallsBackToTheConfigWhenThereIsNoTestHome(t *testing.T) {
	cfg := config.Config{ConfigDir: "/tmp/ember-c", StateDir: "/tmp/ember-s"}
	ps, err := resolvePaths(cfg, deps{})
	if err != nil {
		t.Fatal(err)
	}
	if ps.config != "/tmp/ember-c" || ps.state != "/tmp/ember-s" {
		t.Fatalf("got %+v", ps)
	}
}

func TestResolvePaths_ReportsAnUnresolvableConfigDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := resolvePaths(config.Default(), deps{}); err == nil {
		t.Fatal("a home that cannot be resolved was accepted")
	}
}

func TestResolvePaths_ReportsAnUnresolvableStateDir(t *testing.T) {
	t.Setenv("HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	cfg := config.Default()
	cfg.ConfigDir = t.TempDir() // past the first hurdle, fail on the second
	if _, err := resolvePaths(cfg, deps{}); err == nil {
		t.Fatal("a state dir that cannot be resolved was accepted")
	}
}

// TestLs_ReportsAMalformedConfig. An absent ember.yaml is fine -- the defaults
// are a valid configuration -- but one that exists and does not parse is not,
// because the operator believes it is in force.
func TestLs_ReportsAMalformedConfig(t *testing.T) {
	p := filepath.Join(t.TempDir(), "ember.yaml")
	if err := os.WriteFile(p, []byte("provider: [unterminated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runLs([]string{"--config", p}, &out, labDeps(t)); err == nil {
		t.Fatal("a config that does not parse was accepted")
	}
}

// TestLs_ReportsAnUnusableStateDirectory: the journal is how an orphan is
// found, so "I could not open it" has to surface rather than scan as empty.
func TestLs_ReportsAnUnusableStateDirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := dockerengine.NewRecordingEngine()
	e.AddImage(local.DefaultImage)

	var out bytes.Buffer
	if err := runLs(nil, &out, deps{Engine: e, HomeDir: home}); err == nil {
		t.Fatal("a state directory that is a file was accepted")
	}
}

func TestReap_ReportsAnUnusableStateDirectory(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "state"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	e := dockerengine.NewRecordingEngine()
	e.AddImage(local.DefaultImage)

	var out bytes.Buffer
	if err := runReap(nil, &out, deps{Engine: e, HomeDir: home}); err == nil {
		t.Fatal("a state directory that is a file was accepted")
	}
}
