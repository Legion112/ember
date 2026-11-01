package app

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// planDeps is an injection seam with nothing real behind it.
//
// Every field is here for the reason its own comment on Deps gives: left to
// resolve itself, this reads the developer's own Docker daemon, graphics card
// and RUNPOD_API_KEY, and a test asserting "nothing matches --gpu h100" would
// pass on CI and fail on a workstation.
func planDeps(t *testing.T) Deps {
	t.Helper()
	e := dockerengine.NewRecordingEngine()
	return Deps{
		Engine:    e,
		HomeDir:   t.TempDir(),
		Creds:     &config.Credentials{},
		DetectGPU: func(context.Context) local.GPU { return local.GPU{} },
	}
}

func servedConfig() config.Config {
	return config.Config{
		Provider: "local",
		Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
		TTL:      time.Hour,
		SD:       config.SDConfig{Serve: []string{"sd-server", "--model", "/m.gguf"}},
	}
}

// TestNewPlan_ARequestsJobsReplaceTheFiles.
//
// A queued batch did not come from the config, and routing it through
// cfg.SD.Jobs would mean converting through config.SDJob -- which carries
// Extra, the field that reaches every untyped part of the generation API
// including the ones naming file paths on the node. A request may not set it,
// so it never travels through the type that can.
func TestNewPlan_ARequestsJobsReplaceTheFiles(t *testing.T) {
	cfg := servedConfig()
	cfg.SD.Jobs = []config.SDJob{{Prompt: "from the file"}}

	plan, err := NewPlan(t.Context(), Request{
		Config: cfg, Deps: planDeps(t),
		Jobs:    []sdserver.Job{{Prompt: "from the request"}},
		Preview: "proj", PreviewInterval: 5,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Jobs.Jobs) != 1 {
		t.Fatalf("got %d job(s), want 1", len(plan.Jobs.Jobs))
	}
	if got := plan.Jobs.Jobs[0].Prompt; got != "from the request" {
		t.Errorf("prompt = %q, want the request's", got)
	}
	if plan.Jobs.Preview != sdserver.PreviewProj || plan.Jobs.PreviewInterval != 5 {
		t.Errorf("preview = %q every %d, want proj every 5", plan.Jobs.Preview, plan.Jobs.PreviewInterval)
	}
	if plan.Jobs.Jobs[0].Extra != nil {
		t.Error("a request's job carries an Extra map, which it may not set")
	}
}

// TestNewPlan_TheConfigIsUsedWhenARequestBringsNoJobs -- which is `ember up`.
func TestNewPlan_TheConfigIsUsedWhenARequestBringsNoJobs(t *testing.T) {
	cfg := servedConfig()
	cfg.SD.Jobs = []config.SDJob{{Prompt: "from the file"}}

	plan, err := NewPlan(t.Context(), Request{Config: cfg, Deps: planDeps(t)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Jobs.Jobs) != 1 || plan.Jobs.Jobs[0].Prompt != "from the file" {
		t.Errorf("got %+v, want the config's job", plan.Jobs.Jobs)
	}
}

// TestNewPlan_ARequestsBudgetWins, and only because whoever set it narrowed
// it already.
//
// It is not written into the config as a decimal string, because
// node.MicroUSD.String is "$%.6g" -- scientific notation for small values,
// which node.ParseUSD refuses. Money stays an integer the whole way.
func TestNewPlan_ARequestsBudgetWins(t *testing.T) {
	cfg := servedConfig()
	cfg.MaxHourlyUSD = "1.00"

	want := provision.Budget{MaxRate: node.Rate(1), MaxRun: node.MicroUSD(999_999)}
	plan, err := NewPlan(t.Context(), Request{
		Config: cfg, Deps: planDeps(t),
		Jobs:   []sdserver.Job{{Prompt: "a"}},
		Budget: &want,
	}, nil)
	// The local offer is free, so a one-microdollar ceiling does not refuse
	// it: the point here is which budget reached the provisioner.
	if err != nil {
		t.Fatal(err)
	}
	if plan.Options.Budget != want {
		t.Errorf("budget = %+v, want the request's %+v", plan.Options.Budget, want)
	}
}

// TestJobsFromRequest_RefusesEveryJobBeforeAnythingIsRented.
//
// Every refusal here is config.ParseSD's, reached through the same function,
// so a batch that arrived over a wire and one that came from a file are held
// to the same rules. A job checked after the node was up is a job refused
// about a minute into a rented card.
func TestJobsFromRequest_RefusesEveryJobBeforeAnythingIsRented(t *testing.T) {
	for _, c := range []struct {
		name string
		req  Request
		want string
	}{
		{
			name: "a preview mode that does not exist",
			req:  Request{Preview: "magic", Jobs: []sdserver.Job{{Prompt: "a"}}},
			want: "preview",
		},
		{
			name: "a negative preview interval",
			req:  Request{PreviewInterval: -1, Jobs: []sdserver.Job{{Prompt: "a"}}},
			want: "preview_interval",
		},
		{
			name: "an output name that cleans away to nothing",
			req:  Request{Jobs: []sdserver.Job{{Prompt: "a", Output: "../.."}}},
			want: "jobs[0]",
		},
		{
			// A seed resolved the wrong way makes two runs that claim to be
			// the same measurement produce different images, so a collision
			// is refused rather than resolved. Seed is set as well as named
			// in extra, because ember only owns the keys it actually sets --
			// an extra naming a field ember left alone is not a collision.
			name: "an extra that shadows a typed field",
			req: Request{Jobs: []sdserver.Job{
				{Prompt: "a"},
				{Prompt: "b", Seed: 7, Extra: map[string]any{"seed": 1}},
			}},
			// Named by index, because the ninth job of a batch is where this
			// is least obvious and most expensive.
			want: "jobs[1]",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := jobsFromRequest(c.req)
			if err == nil {
				t.Fatal("accepted a batch nothing could run")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestWithServed_RefusesAConfigWithNoServer.
//
// A queued request has nowhere to run without one, and the refusal has to come
// before anything is rented: a node up with no server to start is a node
// billing for a mistake in a file.
func TestWithServed_RefusesAConfigWithNoServer(t *testing.T) {
	plan := Plan{}
	err := plan.WithServed(t.Context(), nil, report.Discard,
		func(context.Context, Served) error {
			t.Fatal("the closure ran for a config with no sd.serve block")
			return nil
		})
	if err == nil {
		t.Fatal("accepted a plan with no generation server")
	}
	if !strings.Contains(err.Error(), "sd.serve") {
		t.Errorf("err = %v, want it to name the missing block", err)
	}
}

// TestServed_GenerateRefusesEveryBadJobBeforeGeneratingAnything.
//
// Checking job n just before submitting it would mean n-1 images already paid
// for when the typo in the last one surfaces. On a rented A100 that is a
// dollar a mistake.
func TestServed_GenerateRefusesEveryBadJobBeforeGeneratingAnything(t *testing.T) {
	f := newFakeSD(t)
	c, _ := f.start(t)
	s := served{client: c, caps: sdserver.Capabilities{Samplers: []string{"euler"}}}

	err := s.Generate(t.Context(), report.Discard, testRun(
		sdserver.Job{Prompt: "fine", Sampler: "euler"},
		sdserver.Job{Prompt: "typo", Sampler: "eular"},
	), t.TempDir())
	if err == nil {
		t.Fatal("a sampler the server does not have was accepted")
	}
	if !strings.Contains(err.Error(), "jobs[1]") {
		t.Errorf("err = %v, want the index named", err)
	}
	// And it is the operator's to fix, which is what makes internal/cli
	// exit 3.
	if !errorsIsUsage(err) {
		t.Errorf("err = %v, want it to be an ErrUsage", err)
	}
	if len(f.submitted) != 0 {
		t.Errorf("%d job(s) were generated before the refusal", len(f.submitted))
	}
}

// TestServed_GenerateRunsTheBatchAndReportsIt.
func TestServed_GenerateRunsTheBatchAndReportsIt(t *testing.T) {
	shrinkPoll(t)
	f := newFakeSD(t)
	c, _ := f.start(t)
	s := served{client: c, caps: sdserver.Capabilities{Samplers: []string{"euler"}}, rate: node.Rate(2_000_000)}

	dir := t.TempDir()
	events := &collect{}
	if err := s.Generate(t.Context(), events, testRun(
		sdserver.Job{Prompt: "a cat", Output: "cat"},
		sdserver.Job{Prompt: "a dog", Output: "dog"},
	), dir); err != nil {
		t.Fatal(err)
	}

	if len(f.submitted) != 2 {
		t.Errorf("submitted %d job(s), want 2", len(f.submitted))
	}
	// The names travel on the event, from sdserver.WriteImages, so a reader
	// recording where the bytes went does not re-derive the naming.
	var named int
	for _, e := range events.es {
		if e.Kind == report.KindJobImages {
			if len(e.Wrote) == 0 {
				t.Error("an images event named no files")
			}
			named += len(e.Wrote)
		}
	}
	if named != 2 {
		t.Errorf("%d file(s) were named across the batch, want 2", named)
	}
	if s.Caps().Samplers[0] != "euler" {
		t.Error("Caps() does not report what the server said")
	}
}

func errorsIsUsage(err error) bool { return errors.Is(err, ErrUsage) }
