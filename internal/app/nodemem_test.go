package app

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/nodemem"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
)

// memOpts is a session.Options with a logger, because reportMemory reports
// every failure through one and a nil Log would panic rather than warn.
func memOpts() session.Options {
	return session.Options{Log: slog.New(slog.DiscardHandler)}
}

// memLog is a session.Options whose warnings can be read back.
//
// WHAT IT IS FOR: the two ways a probe learns nothing -- a node that would not
// answer, and a node that answered with nothing in it -- both emit no event,
// so nothing downstream can tell them apart. The warning is the only place the
// difference exists, and an operator diagnosing an empty column needs the
// right one. Without this, reinstating the removal of either path left the
// suite green.
func memLog() (session.Options, *strings.Builder) {
	var b strings.Builder
	h := slog.NewTextHandler(&b, &slog.HandlerOptions{Level: slog.LevelWarn})
	return session.Options{Log: slog.New(h)}, &b
}

// memSpec is a spec with a store and one asset, which is the shape where the
// asset figure is knowable.
func memSpec(t *testing.T) node.Spec {
	t.Helper()
	mount, err := node.MountAt("/models")
	if err != nil {
		t.Fatal(err)
	}
	return node.Spec{
		Mount: mount,
		Assets: []node.Asset{{
			As: "flux2.gguf", URL: "https://example.test/f",
			SHA256: strings.Repeat("a", 64),
		}},
	}
}

// answer scripts the recording runner for the probe this spec would send.
func answer(t *testing.T, spec node.Spec, out string) *sshx.RecordingRunner {
	t.Helper()
	paths, err := nodemem.AssetPaths(spec.Mount, spec.Assets)
	if err != nil {
		t.Fatal(err)
	}
	cmd := nodemem.Command(paths)
	r := sshx.NewRecordingRunner()
	r.Outputs[sshx.Cmd(cmd[0], cmd[1:]...).String()] = []byte(out)
	return r
}

// TestReportMemory_EmitsBothViews.
//
// THE LINK A UNIT TEST OF THE PARSER CANNOT MAKE: that the run actually asks
// the node and puts the answer on the event stream. A parser and a column
// that each agree with themselves across a gap no test crosses is the failure
// this repository has paid for three times.
func TestReportMemory_EmitsBothViews(t *testing.T) {
	spec := memSpec(t)
	r := answer(t, spec, strings.Join([]string{
		"cgroup2_max 34359738368",
		"cgroup2_current 33285996544",
		"mem_total_kb 741343232",
		"mem_cached_kb 31138512896",
		"assets 60129542144",
	}, "\n"))

	var c collect
	reportMemory(t.Context(), &c, r, spec, memOpts())

	if len(c.es) != 1 {
		t.Fatalf("emitted %d event(s), want one: %+v", len(c.es), c.es)
	}
	e := c.es[0]
	if e.Kind != report.KindNodeMemory {
		t.Errorf("kind = %q, want node_memory", e.Kind)
	}
	if e.MemCgroup != 2 || e.MemLimit != 32<<30 || e.MemCurrent != 31<<30 {
		t.Errorf("the cgroup's view was not emitted: %+v", e)
	}
	if e.MemTotal != 741343232*1024 {
		t.Errorf("the host's view was not emitted: %+v", e)
	}
	// The asset figure travels on Bytes, which is what that field means
	// everywhere else: how many bytes this event is about.
	if e.Bytes != 56<<30 {
		t.Errorf("asset bytes = %d, want the weights the run was about to read", e.Bytes)
	}

	// And the probe asked for the asset by its path on the store, which is
	// what makes the figure "this model" rather than "everything anybody left
	// on the volume".
	if calls := r.Calls(); len(calls) != 1 || !strings.Contains(calls[0], "/models/flux2.gguf") {
		t.Errorf("the probe did not stat this run's weights: %v", calls)
	}
}

// IT CANNOT FAIL A RUN. Every failure is swallowed, because these are reads of
// four files and a stat: a node that will not answer them is a node whose
// generation should still happen. A probe that could refuse would be a
// diagnostic with the authority of a safety check, over a value nothing yet
// acts on.
func TestReportMemory_SwallowsEveryFailure(t *testing.T) {
	spec := memSpec(t)

	cases := []struct {
		name  string
		warns string
		run   func(t *testing.T) (report.Sink, sshx.Runner, node.Spec)
	}{
		{"no sink to report to", "", func(t *testing.T) (report.Sink, sshx.Runner, node.Spec) {
			return nil, sshx.NewRecordingRunner(), spec
		}},
		{"a node that would not answer", "could not read the node's memory facts",
			func(t *testing.T) (report.Sink, sshx.Runner, node.Spec) {
				r := sshx.NewRecordingRunner()
				r.FailOn = "sh"
				return &collect{}, r, spec
			}},
		{"an asset ember cannot name", "could not name this run's assets",
			func(t *testing.T) (report.Sink, sshx.Runner, node.Spec) {
				bad := spec
				bad.Assets = []node.Asset{{As: "../etc/passwd"}}
				return &collect{}, sshx.NewRecordingRunner(), bad
			}},
		{"a node that answered nothing at all", "reported no memory facts at all",
			func(t *testing.T) (report.Sink, sshx.Runner, node.Spec) {
				return &collect{}, answer(t, spec, ""), spec
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sink, r, s := c.run(t)
			so, logged := memLog()
			// No panic, no error to return: the only contract is that the run
			// carries on.
			reportMemory(t.Context(), sink, r, s, so)
			if got, ok := sink.(*collect); ok && len(got.es) != 0 {
				t.Errorf("recorded %+v from a probe that learned nothing", got.es)
			}
			if c.warns == "" {
				return
			}
			// And it said which kind of nothing it learned, because a probe
			// that stopped working otherwise leaves an empty column that
			// reads as "this node had no limit" -- the answer that makes a
			// slow node look fine.
			if !strings.Contains(logged.String(), c.warns) {
				t.Errorf("the warning does not say %q:\n%s", c.warns, logged.String())
			}
		})
	}
}

// A run with no store has no asset figure, and the memory facts are still
// worth having: the limit is the limit whether or not weights are read
// through it.
func TestReportMemory_RecordsAMemoryLimitWithNoAssets(t *testing.T) {
	spec := node.Spec{}
	r := answer(t, spec, "cgroup2_max 1073741824\nmem_total_kb 1024\nassets 0\n")

	var c collect
	reportMemory(t.Context(), &c, r, spec, memOpts())

	if len(c.es) != 1 {
		t.Fatalf("emitted %d event(s), want one: %+v", len(c.es), c.es)
	}
	if c.es[0].MemLimit != 1<<30 {
		t.Errorf("the limit was not recorded without assets: %+v", c.es[0])
	}
	if c.es[0].Bytes != 0 {
		t.Errorf("asset bytes = %d, want zero for a run with no store", c.es[0].Bytes)
	}
}
