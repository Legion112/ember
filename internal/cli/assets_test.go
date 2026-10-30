package cli

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/assets"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
)

func assetSpec(t *testing.T, as ...node.Asset) node.Spec {
	t.Helper()
	mount, err := node.MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	s, err := node.NewSpec(node.SpecOptions{
		Image:  "ghcr.io/x/y:v1",
		TTL:    time.Hour,
		Store:  node.StoreHandle{Provider: "local", ID: "ember-store-models"},
		Mount:  mount,
		Assets: as,
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func quietOptions() session.Options {
	return session.Options{Log: slog.New(slog.DiscardHandler)}
}

const digestA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestEnsureAssets_RunsOneFetchPerAssetInOrder(t *testing.T) {
	// Sequential, not concurrent: these are gigabytes over one link, so
	// parallel fetches share the same bandwidth and arrive no sooner, while
	// making a failure impossible to attribute.
	spec := assetSpec(t,
		node.Asset{URL: "https://h/a", SHA256: digestA, As: "a.gguf"},
		node.Asset{URL: "https://h/b", SHA256: digestB, As: "b.gguf"},
	)
	r := sshx.NewRecordingRunner()

	var out bytes.Buffer
	if err := ensureAssets(t.Context(), &out, r, spec, quietOptions()); err != nil {
		t.Fatalf("ensureAssets = %v", err)
	}

	calls := r.Calls()
	if len(calls) != 2 {
		t.Fatalf("made %d calls, want one per asset: %v", len(calls), calls)
	}
	for i, want := range []string{"a.gguf", "b.gguf"} {
		if !strings.Contains(calls[i], want) {
			t.Errorf("call %d = %q, want it to name %s", i, calls[i], want)
		}
		// The node binary, not a shell: an asset name is config text and a
		// concatenated command would be an injection with the operator's own
		// file as the input.
		if !strings.HasPrefix(calls[i], assets.NodeBinary+" fetch") {
			t.Errorf("call %d = %q", i, calls[i])
		}
	}
}

func TestEnsureAssets_NothingToDo(t *testing.T) {
	r := sshx.NewRecordingRunner()
	if err := ensureAssets(t.Context(), nil, r, assetSpec(t), quietOptions()); err != nil {
		t.Fatalf("ensureAssets with no assets = %v", err)
	}
	if n := len(r.Calls()); n != 0 {
		t.Errorf("made %d calls for no assets", n)
	}
}

func TestEnsureAssets_ANodeRefusalStopsTheRun(t *testing.T) {
	// The whole point of fetching before the workload. A non-zero exit here
	// means the digest did not match, and nothing may run against those
	// bytes.
	spec := assetSpec(t, node.Asset{URL: "https://h/a", SHA256: digestA, As: "a.gguf"})
	r := sshx.NewRecordingRunner()
	r.FailOn = "a.gguf"

	err := ensureAssets(t.Context(), nil, r, spec, quietOptions())
	if err == nil {
		t.Fatal("a refused asset did not stop the run")
	}
	if !strings.Contains(err.Error(), "a.gguf") {
		t.Errorf("err = %q, want it to name the asset", err)
	}
}

func TestEnsureAssets_StopsAtTheFirstFailure(t *testing.T) {
	// The second asset is not attempted. Fetching the rest after one failed
	// would spend minutes of a rented machine on work that is about to be
	// thrown away.
	spec := assetSpec(t,
		node.Asset{URL: "https://h/a", SHA256: digestA, As: "a.gguf"},
		node.Asset{URL: "https://h/b", SHA256: digestB, As: "b.gguf"},
	)
	r := sshx.NewRecordingRunner()
	r.FailOn = "a.gguf"

	if err := ensureAssets(t.Context(), nil, r, spec, quietOptions()); err == nil {
		t.Fatal("expected a failure")
	}
	for _, c := range r.Calls() {
		if strings.Contains(c, "b.gguf") {
			t.Errorf("the second asset was fetched after the first failed: %v", r.Calls())
		}
	}
}

func TestEnsureAssets_ReportsATransportFailure(t *testing.T) {
	// The connection died rather than the node refusing. Distinguished from
	// a refusal because the remedy differs: one is a bad digest, the other
	// is a node that cannot be reached.
	spec := assetSpec(t, node.Asset{URL: "https://h/a", SHA256: digestA, As: "a.gguf"})
	r := sshx.NewRecordingRunner()
	cmd, err := assets.Command(spec.Mount, spec.Assets[0], assets.DefaultPolicy)
	if err != nil {
		t.Fatal(err)
	}
	r.Errors[cmd.String()] = errors.New("connection lost")

	if err := ensureAssets(t.Context(), nil, r, spec, quietOptions()); err == nil {
		t.Error("a transport failure was treated as success")
	}
}

func TestEnsureAssets_RefusesAPolicyItCannotRead(t *testing.T) {
	// Unreachable through buildSpec, which parses the policy before anything
	// is rented, and checked anyway: a misspelling must never quietly become
	// the weaker setting.
	spec := assetSpec(t, node.Asset{URL: "https://h/a", SHA256: digestA, As: "a.gguf"})
	spec.Verify = "alwyas"
	if err := ensureAssets(t.Context(), nil, sshx.NewRecordingRunner(), spec, quietOptions()); err == nil {
		t.Error("a misspelled policy was accepted")
	}
}

func TestEnsureAssets_RefusesAssetsWithNoMount(t *testing.T) {
	// Unreachable through NewSpec, and checked because what it guards is a
	// fetch writing gigabytes to the node's own disk and throwing them away.
	spec := assetSpec(t, node.Asset{URL: "https://h/a", SHA256: digestA, As: "a.gguf"})
	spec.Mount = node.Mount{}
	if err := ensureAssets(t.Context(), nil, sshx.NewRecordingRunner(), spec, quietOptions()); err == nil {
		t.Error("assets with nowhere to land were accepted")
	}
}
