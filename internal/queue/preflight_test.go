package queue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/awssig"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/local"
)

// storeFakes serves a RunPod volume listing and an S3 endpoint holding
// whatever objects the caller names, and NOTHING ELSE.
//
// No catalogue route, deliberately, for the reason the cli pre-flight tests
// give: a provider that cannot say what cards it has makes the ORDER
// observable. A planner that reached PickOffer failed there, and one that
// asked about the weights first failed differently.
func storeFakes(t *testing.T, objects map[string]int64) app.Deps {
	t.Helper()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "network-volumes") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"vol_abc","name":"ember-store-models",`+
				`"size":40,"dataCenter":"EU-RO-1","type":"STANDARD"}],`+
				`"pagination":{"hasNextPage":false}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)

	s3srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.URL.Path, "/vol_abc/")
		size, ok := objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(size))
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s3srv.Close)

	engine := dockerengine.NewRecordingEngine()
	return app.Deps{
		Engine:       engine,
		HomeDir:      t.TempDir(),
		CloudBaseURL: api.URL,
		S3BaseURL:    s3srv.URL,
		Creds: &config.Credentials{
			RunPod: node.NewSecret([]byte("rp-token")),
			S3: awssig.Credentials{
				AccessKey: "user_test", Secret: node.NewSecret([]byte("rps_secret")),
			},
		},
		DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
	}
}

func storeConfig() config.Config {
	return config.Config{
		Provider: "runpod",
		Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
		TTL:      time.Hour,
		Store:    config.StoreConfig{Name: "models", SizeGiB: 40, Region: "EU-RO-1", Mount: "/models"},
		Assets: []config.AssetConfig{{
			URL:    "https://example.invalid/weights.gguf",
			SHA256: "09d005300dd8dcbbd489bb75ada6254145c84c2c9c3d7cc1829e3c5dedcb42ce",
			As:     "weights.gguf",
		}},
		Verify: "always",
		SD:     config.SDConfig{Serve: []string{"sd-server", "--model", "/models/weights.gguf"}},
	}
}

func aRequest() genstore.Request {
	return genstore.Request{
		Provider: "runpod", TTL: time.Hour,
		Jobs: []genstore.Job{{Prompt: "a paper boat", Width: 512, Height: 512, Steps: 4}},
	}
}

// TestAppPlanner_FillsAnEmptyStoreOnAMachineWithNoCardRatherThanRefusing.
//
// THE DIVERGENCE IS THE POINT. The identical config and the identical empty
// store make `ember up` refuse with "ember store sync" -- see
// internal/cli/preflight_test.go -- and make the queue go and do it, because
// a queue drains unattended and is the case where nobody is watching to type
// the command.
//
// The sync cannot complete here: the fake has no catalogue, so planning the
// CPU machine fails. What that proves is the branch taken, which is the whole
// claim. A planner that had simply returned the refusal would carry the
// remedy's name instead.
func TestAppPlanner_FillsAnEmptyStoreOnAMachineWithNoCardRatherThanRefusing(t *testing.T) {
	p := AppPlanner{Config: storeConfig(), Deps: storeFakes(t, map[string]int64{}), AllowPaid: true}

	_, err := p.Plan(t.Context(), aRequest())
	if err == nil {
		t.Fatal("a request planned against a store with none of its weights on it")
	}
	if !strings.Contains(err.Error(), "fill the store") {
		t.Errorf("the queue refused instead of filling the store: %v", err)
	}
	if strings.Contains(err.Error(), "ember store sync") {
		t.Errorf("the queue told the operator to run the command it should have run itself: %v", err)
	}
}

// TestAppPlanner_AFullStoreStartsNoSecondRental is the negative control.
//
// Without it every assertion above is consistent with a planner that rents a
// CPU machine before every request there is -- which would be a second
// rental, every time, for nothing. This one must get past the weights and
// fail on the fake's missing catalogue, with no mention of filling anything.
func TestAppPlanner_AFullStoreStartsNoSecondRental(t *testing.T) {
	d := storeFakes(t, map[string]int64{"weights.gguf": 60306914804})
	p := AppPlanner{Config: storeConfig(), Deps: d, AllowPaid: true}

	_, err := p.Plan(t.Context(), aRequest())
	if err == nil {
		t.Fatal("the request planned against a fake with no catalogue route")
	}
	if strings.Contains(err.Error(), "fill the store") {
		t.Fatalf("a store holding every asset started a sync rental anyway: %v", err)
	}
	// Positively: it reached the catalogue, so it got past the weights
	// rather than failing the check some other way.
	if !strings.Contains(err.Error(), "list offers from runpod") {
		t.Fatalf("a full store did not get as far as choosing an offer: %v", err)
	}
}

// TestAppPlanner_AnEmptyStoreWithoutAllowPaidRentsNothingAtAll.
//
// The sync is a rental and is gated like one. Without this the cheap machine
// would be a hole in the one gate `ember serve` has: --allow-paid would stop
// the GPU and not the thing that goes and fetches fifty-six gigabytes.
func TestAppPlanner_AnEmptyStoreWithoutAllowPaidRentsNothingAtAll(t *testing.T) {
	p := AppPlanner{Config: storeConfig(), Deps: storeFakes(t, map[string]int64{})}

	_, err := p.Plan(t.Context(), aRequest())
	if err == nil {
		t.Fatal("a paid provider was planned without --allow-paid")
	}
}
