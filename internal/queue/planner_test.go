package queue

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/provider/providertest"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/logx"
	"github.com/Legion112/ember/internal/node"
)

// TestNarrow_ARequestMayOnlyEverAskForLess is the money rule.
//
// `ember up`'s applyFlags REPLACES: --max-hourly-usd 999 raises the ceiling,
// because the person typing it is the person who set it. A request is not that
// person. If this ever took the larger of the two, a browser -- or anything
// that reached the store -- could spend whatever it liked, and nothing
// downstream could tell that config apart from one read off disk.
func TestNarrow_ARequestMayOnlyEverAskForLess(t *testing.T) {
	cfg := config.Config{
		Provider: "local", TTL: time.Hour,
		MaxHourlyUSD: "1.00", MaxRunUSD: "2.00",
	}

	for _, c := range []struct {
		name       string
		req        genstore.Request
		wantTTL    time.Duration
		wantHourly node.Rate
		wantRun    node.MicroUSD
	}{
		{
			name:    "asking for less gets less",
			req:     genstore.Request{TTL: 30 * time.Minute, MaxHourly: node.Rate(500_000), MaxRun: node.MicroUSD(600_000)},
			wantTTL: 30 * time.Minute, wantHourly: node.Rate(500_000), wantRun: node.MicroUSD(600_000),
		},
		{
			name:    "asking for more gets the config's",
			req:     genstore.Request{TTL: 24 * time.Hour, MaxHourly: node.Rate(99_000_000), MaxRun: node.MicroUSD(99_000_000)},
			wantTTL: time.Hour, wantHourly: node.Rate(1_000_000), wantRun: node.MicroUSD(2_000_000),
		},
		{
			name: "asking for nothing gets the config's",
			req:  genstore.Request{},
			// Zero on the request means "no opinion", not "no ceiling": a
			// request that named no budget must not narrow the operator's to
			// nothing and refuse every offer.
			wantTTL: time.Hour, wantHourly: node.Rate(1_000_000), wantRun: node.MicroUSD(2_000_000),
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, budget, err := Narrow(cfg, c.req)
			if err != nil {
				t.Fatal(err)
			}
			if got.TTL != c.wantTTL {
				t.Errorf("ttl = %v, want %v", got.TTL, c.wantTTL)
			}
			if budget.MaxRate != c.wantHourly {
				t.Errorf("max_hourly = %v, want %v", budget.MaxRate, c.wantHourly)
			}
			if budget.MaxRun != c.wantRun {
				t.Errorf("max_run = %v, want %v", budget.MaxRun, c.wantRun)
			}
		})
	}
}

// TestNarrow_AConfigWithNoCeilingTakesTheRequests.
//
// Zero means unlimited on both sides, so a config that set no budget is
// narrowed by a request that did. That is still narrowing: the operator who
// wrote no ceiling allowed everything.
func TestNarrow_AConfigWithNoCeilingTakesTheRequests(t *testing.T) {
	cfg := config.Config{Provider: "local", TTL: time.Hour}
	_, budget, err := Narrow(cfg, genstore.Request{MaxHourly: node.Rate(250_000), MaxRun: node.MicroUSD(400_000)})
	if err != nil {
		t.Fatal(err)
	}
	if budget.MaxRate != node.Rate(250_000) {
		t.Errorf("max_hourly = %v, want the request's $0.25/hr", budget.MaxRate)
	}
	if budget.MaxRun != node.MicroUSD(400_000) {
		t.Errorf("max_run = %v, want the request's $0.40", budget.MaxRun)
	}
}

// TestNarrow_CarriesEveryAmountExactly.
//
// THIS TEST FOUND A REAL BUG, which is why the awkward values are in it. The
// first version of Narrow wrote the narrowed ceiling back into the config as
// a decimal string, through node.MicroUSD.String -- and that is "$%.6g",
// which renders one microdollar as "$1e-06". node.ParseUSD refuses scientific
// notation outright, so the smallest ceilings did not survive the round trip
// at all and 289999 was one bad render away from becoming 0.29.
//
// There is no round trip now: the budget comes back as integers. The values
// below are kept because a future change that reintroduces a string in this
// path fails here rather than in a budget that is quietly wrong.
func TestNarrow_CarriesEveryAmountExactly(t *testing.T) {
	cfg := config.Config{Provider: "local", TTL: time.Hour, MaxHourlyUSD: "99.00"}
	for _, want := range []node.Rate{
		1, 999, 289_999, 290_000, 340_000, 2_490_000, 999_999, 98_999_999,
	} {
		_, budget, err := Narrow(cfg, genstore.Request{MaxHourly: want})
		if err != nil {
			t.Fatalf("%v was refused: %v", want, err)
		}
		if budget.MaxRate != want {
			t.Errorf("max_hourly = %v, want %v exactly", budget.MaxRate, want)
		}
	}
}

// TestNarrow_RefusesANegativeBudget. Nothing can satisfy it, and a negative
// ceiling compared against a price refuses every offer -- which would read as
// "no capacity" rather than as the nonsense it is.
func TestNarrow_RefusesANegativeBudget(t *testing.T) {
	cfg := config.Config{Provider: "local", TTL: time.Hour}
	for _, r := range []genstore.Request{
		{MaxHourly: -1},
		{MaxRun: -1},
	} {
		if _, _, err := Narrow(cfg, r); err == nil {
			t.Errorf("Narrow accepted %+v", r)
		}
	}
}

// TestNarrow_ReportsTheOperatorsOwnBadBudget, before anything is rented, and
// as their error rather than the request's.
func TestNarrow_ReportsTheOperatorsOwnBadBudget(t *testing.T) {
	cfg := config.Config{Provider: "local", TTL: time.Hour, MaxHourlyUSD: "not a number"}
	_, _, err := Narrow(cfg, genstore.Request{})
	if err == nil {
		t.Fatal("an unparseable configured budget was accepted")
	}
	if !strings.Contains(err.Error(), "configured budget") {
		t.Errorf("err = %v, want it to say the configured budget is the problem", err)
	}
}

// TestNarrow_TheSelectionIsTheRequests. A provider and a GPU are not ceilings
// -- there is nothing to compare them against -- so a request chooses, and the
// provider either has the card or reports no capacity.
func TestNarrow_TheSelectionIsTheRequests(t *testing.T) {
	cfg := config.Config{Provider: "local", GPU: "rtx2080ti", TTL: time.Hour}
	got, _, err := Narrow(cfg, genstore.Request{Provider: "runpod", GPU: "h100-80g"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Provider != "runpod" || got.GPU != "h100-80g" {
		t.Errorf("selection = %q/%q, want the request's", got.Provider, got.GPU)
	}

	// An empty GPU is a real choice -- "anything" -- and must clear the
	// config's, or a UI offering "any card" would silently keep the file's.
	got, _, err = Narrow(cfg, genstore.Request{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}
	if got.GPU != "" {
		t.Errorf("gpu = %q, want it cleared when the request named none", got.GPU)
	}
}

// TestNarrow_LeavesEverythingARequestMayNotTouch.
//
// This is the list that keeps every existing seal upstream of anything a
// request can influence: node.MountAt's refusal of /root, ValidateImage's
// refusal of :latest, sdserver.ServeCommand's refusal of a listen flag. A
// request body cannot reach any of them because it is never consulted.
func TestNarrow_LeavesEverythingARequestMayNotTouch(t *testing.T) {
	cfg := config.Config{
		Provider: "local", TTL: time.Hour,
		Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
		Images:   map[string]string{"sm_75": "ghcr.io/example/sd:sd-cuda-sm75"},
		Ports:    []string{"ui:8188"},
		Store:    config.StoreConfig{Name: "models", Mount: "/models"},
		Assets:   []config.AssetConfig{{URL: "https://example.invalid/w", SHA256: strings.Repeat("a", 64), As: "w"}},
		Verify:   "present",
		GPUProbe: []string{"sh", "-c", "true"},
		SD:       config.SDConfig{Serve: []string{"sd-server", "--model", "/models/w"}},
	}
	got, _, err := Narrow(cfg, genstore.Request{Provider: "local"})
	if err != nil {
		t.Fatal(err)
	}

	if got.Image != cfg.Image {
		t.Error("the image was changed")
	}
	if len(got.Images) != 1 {
		t.Error("the images map was changed")
	}
	if len(got.Ports) != 1 {
		t.Error("the ports were changed")
	}
	if got.Store.Name != "models" || got.Store.Mount != "/models" {
		t.Error("the store or its mount was changed")
	}
	if len(got.Assets) != 1 || got.Verify != "present" {
		t.Error("the assets or the verify policy were changed")
	}
	if len(got.GPUProbe) != 3 {
		t.Error("the gpu probe was changed")
	}
	if len(got.SD.Serve) != 3 {
		t.Error("the serve argv was changed")
	}
}

func TestSmaller_ZeroIsUnlimited(t *testing.T) {
	if got := smaller(0, node.Rate(5)); got != 5 {
		t.Errorf("smaller(0, 5) = %v, want 5", got)
	}
	if got := smaller(node.Rate(5), 0); got != 5 {
		t.Errorf("smaller(5, 0) = %v, want 5", got)
	}
	if got := smaller(node.Rate(5), node.Rate(3)); got != 3 {
		t.Errorf("smaller(5, 3) = %v, want 3", got)
	}
	if got := smaller(node.Rate(3), node.Rate(5)); got != 3 {
		t.Errorf("smaller(3, 5) = %v, want 3", got)
	}
	if got := smallerUSD(0, 0); got != 0 {
		t.Errorf("smallerUSD(0, 0) = %v, want 0", got)
	}
	if got := smallerUSD(node.MicroUSD(7), node.MicroUSD(9)); got != 7 {
		t.Errorf("smallerUSD(7, 9) = %v, want 7", got)
	}
	if got := smallerUSD(node.MicroUSD(9), node.MicroUSD(7)); got != 7 {
		t.Errorf("smallerUSD(9, 7) = %v, want 7", got)
	}
}

// TestRefusePaid_DefaultsToRefusing.
//
// Caps().Free's zero value is false, which means "not free" -- so a provider
// added later is paid by default and an author who forgets the field gets the
// refusal rather than a bill. Same direction as Caps.PublicSSH and
// Caps.Stores, and for the same reason.
func TestRefusePaid_DefaultsToRefusing(t *testing.T) {
	paid := providertest.NewRecordingProvider("runpod")
	free := providertest.NewRecordingProvider("local")
	free.Capabilities.Free = true

	offer := node.Offer{Token: "offer-1", Price: node.Rate(2_490_000)}

	if err := refusePaid(false, paid, offer); err == nil {
		t.Error("a paid provider was accepted without --allow-paid")
	} else {
		// The refusal names the price, because "runpod is not free" is less
		// useful than the rate the operator was about to be charged.
		if !strings.Contains(err.Error(), "$2.49/hr") {
			t.Errorf("err = %v, want it to name the price it refused", err)
		}
		if !strings.Contains(err.Error(), "--allow-paid") {
			t.Errorf("err = %v, want it to name the flag that allows it", err)
		}
	}

	if err := refusePaid(true, paid, offer); err != nil {
		t.Errorf("--allow-paid still refused a paid provider: %v", err)
	}
	if err := refusePaid(false, free, offer); err != nil {
		t.Errorf("a free provider was refused without --allow-paid: %v", err)
	}
}

// TestAppPlanner_ResolvesWithoutRenting.
//
// The whole discipline app.NewPlan keeps: by the time Plan returns, the store
// is resolved, the offer is chosen, the image is picked for that card and
// every job has been checked -- and nothing has been created. The recording
// Docker engine is what proves the last part.
func TestAppPlanner_ResolvesWithoutRenting(t *testing.T) {
	engine := dockerengine.NewRecordingEngine()
	engine.GPURuntime = true

	p := AppPlanner{
		Config: config.Config{
			Provider: "local",
			Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
			TTL:      time.Hour,
			SD:       config.SDConfig{Serve: []string{"sd-server", "--model", "/m.gguf"}},
		},
		Deps: app.Deps{
			Engine:  engine,
			HomeDir: t.TempDir(),
			// Injected rather than resolved: left to read the environment,
			// every test here picks up the developer's own RUNPOD_API_KEY and
			// registers the real provider.
			Creds:     &config.Credentials{},
			DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
		},
	}

	rental, err := p.Plan(t.Context(), genstore.Request{
		Provider: "local", TTL: 30 * time.Minute,
		Jobs: []genstore.Job{{Prompt: "a cat", Width: 512, Height: 512, Steps: 4}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if rental == nil {
		t.Fatal("Plan returned no rental and no error")
	}
	if n := len(engine.Containers()); n != 0 {
		t.Errorf("%d container(s) were created while resolving a plan", n)
	}
}

// TestAppPlanner_RefusesAJobTheServerCouldNotHaveRun, before anything is
// rented. An output name that cleans away to nothing is a config error, and
// finding a config error after the create call is finding it after the billing
// started.
func TestAppPlanner_RefusesAJobTheServerCouldNotHaveRun(t *testing.T) {
	engine := dockerengine.NewRecordingEngine()
	p := AppPlanner{
		Config: config.Config{
			Provider: "local",
			Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
			TTL:      time.Hour,
			SD:       config.SDConfig{Serve: []string{"sd-server", "--model", "/m.gguf"}},
		},
		Deps: app.Deps{
			Engine: engine, HomeDir: t.TempDir(),
			Creds:     &config.Credentials{},
			DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
		},
	}

	_, err := p.Plan(t.Context(), genstore.Request{
		Provider: "local", TTL: time.Hour,
		Jobs: []genstore.Job{{Prompt: "a cat", Output: "../.."}},
	})
	if err == nil {
		t.Fatal("a job with an unusable output name was planned")
	}
	if n := len(engine.Containers()); n != 0 {
		t.Errorf("%d container(s) were created before the refusal", n)
	}
}

// TestQueue_DefaultsAreUsable: a Queue with only its collaborators set must
// not dereference a nil logger, clock or writer from inside a deferred
// teardown.
func TestQueue_DefaultsAreUsable(t *testing.T) {
	q := &Queue{}
	if q.log() == nil {
		t.Error("log() is nil")
	}
	if q.at().IsZero() {
		t.Error("at() returned the zero time")
	}
}

// TestAppPlanner_CarriesTheLogLevelIntoARental.
//
// `ember serve --log-level debug` is one flag and two destinations: the
// logger this process writes with, and the logger a rental's own lines come
// out of -- which is built inside app.BuildOptions, several layers below any
// handler. Without this field the flag would have covered the server and left
// the expensive half, the rental, at info; the symptom would have been an
// operator turning the logs up, seeing the HTTP side get louder and the
// provisioning stay quiet, and concluding that provisioning has no debug
// lines. It has four, and they are the ones a rented machine is debugged
// with.
//
// It asserts on the LOGGER THE PLAN HOLDS and not on the field it was given,
// because the field agreeing with itself is what a test of the struct would
// show: the gap this crosses is AppPlanner -> app.Request -> BuildOptions.
func TestAppPlanner_CarriesTheLogLevelIntoARental(t *testing.T) {
	engine := dockerengine.NewRecordingEngine()
	engine.GPURuntime = true

	cfg := config.Config{
		Provider: "local",
		Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
		TTL:      time.Hour,
		SD:       config.SDConfig{Serve: []string{"sd-server", "--model", "/m.gguf"}},
	}
	deps := app.Deps{
		Engine: engine, HomeDir: t.TempDir(),
		Creds:     &config.Credentials{},
		DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
	}
	req := genstore.Request{
		Provider: "local", TTL: 30 * time.Minute,
		Jobs: []genstore.Job{{Prompt: "a cat", Width: 512, Height: 512, Steps: 4}},
	}

	for _, tc := range []struct {
		name string
		opts logx.Options
		want bool
	}{
		{"the default is still info", logx.Options{}, false},
		{"--log-level debug reaches the rental", logx.Options{Level: slog.LevelDebug}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rental, err := AppPlanner{Config: cfg, Deps: deps, Log: tc.opts}.Plan(t.Context(), req)
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			ar, ok := rental.(appRental)
			if !ok {
				t.Fatalf("Plan returned %T, not the rental this test can read", rental)
			}
			if got := ar.plan.Options.Log.Enabled(t.Context(), slog.LevelDebug); got != tc.want {
				t.Errorf("debug enabled on the rental's logger = %v, want %v", got, tc.want)
			}
		})
	}
	if n := len(engine.Containers()); n != 0 {
		t.Errorf("%d container(s) were created by a plan", n)
	}
}
