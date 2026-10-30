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

// storeConfig writes an ember.yaml naming a store, and returns its path.
func storeConfig(t *testing.T, d deps, extra string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, config.FileName)
	body := "provider: local\n" +
		"image: " + local.DefaultImage + "\n" +
		"ttl: 10m\n" +
		"config_dir: " + dir + "\n" +
		"state_dir: " + filepath.Join(dir, "state") + "\n" +
		"output_dir: " + filepath.Join(dir, "out") + "\n" +
		extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	_ = d
	return p
}

const storeBlock = "store:\n  name: models\n  size_gib: 40\n  region: EU-RO-1\n"

func TestStore_CreateThenRemove(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)

	var out bytes.Buffer
	if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
		t.Fatalf("store create = %v", err)
	}
	if !strings.Contains(out.String(), "created models") {
		t.Errorf("create said %q", out.String())
	}
	// Free storage says so rather than printing a cost of nothing.
	if !strings.Contains(out.String(), "charges nothing") {
		t.Errorf("a free store did not say so: %q", out.String())
	}

	// It shows up in ls, which is the only listing there is.
	var ls bytes.Buffer
	if err := runLs([]string{"-config", cfg}, &ls, d); err != nil {
		t.Fatalf("ls = %v", err)
	}
	if !strings.Contains(ls.String(), "models") || !strings.Contains(ls.String(), "store") {
		t.Errorf("ls did not report the store: %q", ls.String())
	}
	// A store is reported, never flagged: exit 2 here would teach the
	// operator to ignore exit 2.
	if ExitCode(nil) != ExitOK {
		t.Error("a store made ls exit non-zero")
	}

	out.Reset()
	if err := runStore([]string{"rm", "-config", cfg, "models"}, &out, d); err != nil {
		t.Fatalf("store rm = %v", err)
	}
	if !strings.Contains(out.String(), "destroyed models") {
		t.Errorf("rm said %q", out.String())
	}

	// Gone already is the state the caller asked for.
	out.Reset()
	if err := runStore([]string{"rm", "-config", cfg, "models"}, &out, d); err != nil {
		t.Errorf("a second rm = %v, want success", err)
	}
	if !strings.Contains(out.String(), "no store named models") {
		t.Errorf("rm said %q", out.String())
	}
}

func TestStore_CreateIsIdempotent(t *testing.T) {
	// Run twice on a fresh machine and on one that already has the store,
	// the config means the same thing. A create that failed the second time
	// would make `ember store create` something nobody could put in a setup
	// script.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	for i := range 2 {
		if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
			t.Fatalf("create %d = %v", i+1, err)
		}
	}
}

func TestStore_Usage(t *testing.T) {
	d := labDeps(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no subcommand", nil},
		{"unknown subcommand", []string{"list"}},
		{"create takes no arguments", []string{"create", "models"}},
		{"rm needs a name", []string{"rm"}},
		{"rm takes one name", []string{"rm", "a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out bytes.Buffer
			err := runStore(tc.args, &out, d)
			if err == nil {
				t.Fatal("accepted")
			}
			if ExitCode(err) != ExitUsage {
				t.Errorf("exit = %d, want %d", ExitCode(err), ExitUsage)
			}
		})
	}

	var out bytes.Buffer
	if err := runStore([]string{"help"}, &out, d); err != nil {
		t.Errorf("store help = %v", err)
	}
	if !strings.Contains(out.String(), "outlives") {
		t.Errorf("help said %q", out.String())
	}
}

func TestStore_RmNamesTheFlagOrder(t *testing.T) {
	// The standard flag package stops parsing at the first non-flag
	// argument, so `ember store rm models -config x` leaves three. The error
	// has to say that rather than count, or the operator reads it as "this
	// command is broken".
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	err := runStore([]string{"rm", "models", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("accepted")
	}
	if !strings.Contains(err.Error(), "flags come before the name") {
		t.Errorf("err = %q, want it to explain the flag order", err)
	}
}

func TestStore_CreateRefusesWithoutAStoreBlock(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, "")
	var out bytes.Buffer
	err := runStore([]string{"create", "-config", cfg}, &out, d)
	if err == nil {
		t.Fatal("created a store the config does not name")
	}
	if !strings.Contains(err.Error(), "no store is named") {
		t.Errorf("err = %q", err)
	}
}

func TestStore_RefusesAProviderWithoutStorage(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	if err := runStore([]string{"rm", "-config", cfg, "-provider", "nosuch", "models"}, &out, d); err == nil {
		t.Error("an unknown provider was accepted")
	}
}

func TestUp_RefusesWhenTheNamedStoreDoesNotExist(t *testing.T) {
	// The refusal that keeps `ember up` from bringing a monthly charge into
	// existence because a config mentioned a name. It must also happen
	// before anything is rented: a run that discovers this after creating a
	// node has already started paying.
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)

	var out bytes.Buffer
	err := runUp([]string{"-config", cfg, "--cmd", "/bin/true"}, &out, d)
	if err == nil {
		t.Fatal("up ran with a store that does not exist")
	}
	if !strings.Contains(err.Error(), "ember store create") {
		t.Errorf("err = %q, want it to name the command that makes one", err)
	}
	e := d.Engine.(*dockerengine.RecordingEngine)
	for _, c := range e.Calls {
		if strings.HasPrefix(c, "ContainerCreate") {
			t.Errorf("a node was created before the store was resolved: %v", e.Calls)
			break
		}
	}
}

func TestUp_MountsTheStoreItWasToldAbout(t *testing.T) {
	d := labDeps(t)
	cfg := storeConfig(t, d, storeBlock)
	var out bytes.Buffer
	if err := runStore([]string{"create", "-config", cfg}, &out, d); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := runUp([]string{"-config", cfg, "--dry-run"}, &out, d); err != nil {
		t.Fatalf("dry-run up = %v", err)
	}

	// The dry run creates nothing, so the assertion is on the spec reaching
	// the provider rather than on a container. Exercised against a real
	// container in the labs.
	e := d.Engine.(*dockerengine.RecordingEngine)
	for _, c := range e.Calls {
		if strings.HasPrefix(c, "ContainerCreate") {
			t.Errorf("--dry-run created a container: %v", e.Calls)
		}
	}
}
