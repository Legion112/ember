package queue

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// storeFakes serves a RunPod volume listing, the captured CPU catalogue and an
// S3 endpoint holding whatever objects the caller names, and NOTHING ELSE.
//
// NO GPU CATALOGUE, deliberately, for the reason the cli pre-flight tests
// give: a provider that cannot say what cards it has makes the ORDER
// observable. A planner that reached PickOffer failed there, and one that
// asked about the weights first failed differently.
//
// THE CPU ONE IS SERVED AND THAT WAS A GAP RATHER THAN A CHOICE. Without it
// the store fill could not get past planning its machine, so
// TestAppPlanner_AnEmptyStoreWithoutAllowPaidRentsNothingAtAll -- whose whole
// subject is that the cheap rental is gated like a rental -- was passing on a
// 404 from this handler and never reached refusePaid at all: deleting that
// gate left it green. Found by strengthening the assertion for #148 rather
// than by review, which is the same shape as the three refusal tests that
// could pass for either reason. It is the provider's own committed fixture,
// read rather than copied, because a second copy of a field shape is how
// `dataCenter` against `dataCenterId` stayed invisible; it has capacity in
// EU-RO-1, which is where storeConfig puts the store.
//
// There is still no `/v2/pods` route, so a sync that gets as far as creating a
// machine fails AT THE CREATE -- which is the rental most worth recording,
// because the row goes down before the call.
func storeFakes(t *testing.T, objects map[string]int64) app.Deps {
	t.Helper()

	cpus, err := os.ReadFile(filepath.Join("..", "provider", "runpod", "testdata", "cpus.json"))
	if err != nil {
		t.Fatal(err)
	}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.Contains(req.URL.Path, "network-volumes") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"networkVolumes":[{"id":"vol_abc","name":"ember-store-models",`+
				`"size":40,"dataCenter":"EU-RO-1","type":"STANDARD"}],`+
				`"pagination":{"hasNextPage":false}}`)
			return
		}
		if req.URL.Path == "/v2/catalog/cpus" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(cpus)
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
// The sync cannot complete here: there is no `/v2/pods` route, so the create
// call fails. What that proves is the branch taken, which is the whole claim.
// A planner that had simply returned the refusal would carry the remedy's name
// instead.
//
// AND THE ROW IS ASSERTED, WHICH IS #148. `fillStore` used to call SyncAssets
// with no sink and write nothing, so a rental the queue chose to make -- a CPU
// machine for as long as fifty-six gigabytes take to arrive -- was in no
// ledger: `ember history` and `ember spend` could not see it. The row goes
// down BEFORE the create call, which is the journal's own rule and is why a
// create that 404s is the sharpest case available without a node: the money
// window is exactly the one between asking a provider for a machine and
// reading the answer.
func TestAppPlanner_FillsAnEmptyStoreOnAMachineWithNoCardRatherThanRefusing(t *testing.T) {
	ledger := genstore.NewRecordingStore()
	p := AppPlanner{
		Config: storeConfig(), Deps: storeFakes(t, map[string]int64{}),
		AllowPaid: true, Runs: ledger,
	}

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
	// Positively, so none of this can pass because the run stopped earlier:
	// it got all the way to asking for the machine.
	if !strings.Contains(err.Error(), "create node") {
		t.Errorf("the queue did not get as far as creating the cheap machine: %v", err)
	}

	rows, err := ledger.List(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("the ledger holds %d row(s) after a rental the queue started on its own "+
			"initiative; a machine ember chose to pay for and did not record is money an "+
			"operator reconciling an invoice cannot account for", len(rows))
	}
	sync := rows[0]
	// No card, which is the whole point of the rental and the column
	// `ember spend --by gpu` groups on: a row claiming a card would say the
	// download was paid for at a card's rate.
	if sync.GPU != "" {
		t.Errorf("the recorded sync rental names the card %q; it asked for none", sync.GPU)
	}
	if len(sync.Jobs) != 0 {
		t.Errorf("the sync rental recorded %d job(s); it generates nothing", len(sync.Jobs))
	}
	if sync.Status != genstore.StatusFailed {
		t.Errorf("the sync rental ended %q, want failed: its create call 404'd, and a row left "+
			"running outlives the process that wrote it", sync.Status)
	}
	if !strings.Contains(sync.Err, "create node") {
		t.Errorf("the sync rental's row carries %q, which does not say the create failed", sync.Err)
	}
	if sync.Offer == "" {
		t.Error("the sync rental recorded no offer, so nothing says which machine was asked for")
	}
}

// TestAppPlanner_WithNoLedgerToWriteRentsNothingToFillAStore.
//
// A RENTAL NOBODY CAN ACCOUNT FOR IS NOT A RENTAL THIS MAKES. The store fill
// is the one rental `ember serve` starts on its own initiative, with nobody at
// the keyboard, and #148 is the hole it left in the ledger: `SyncAssets` took
// no sink, so `ember history` and `ember spend` were blind to a machine the
// queue chose to pay for. Wiring the recorder closed that; a nil recorder
// would reopen it silently, which is why a planner without one refuses rather
// than syncing unrecorded -- refuseOverPeriod's own rule ("a ceiling that
// silently does nothing is worse than one that is not configured") applied to
// a write.
//
// It asserts the message and not just an error, because an empty store refuses
// for three other reasons in this file and a bare `err != nil` would pass
// against any of them.
func TestAppPlanner_WithNoLedgerToWriteRentsNothingToFillAStore(t *testing.T) {
	p := AppPlanner{Config: storeConfig(), Deps: storeFakes(t, map[string]int64{}), AllowPaid: true}

	_, err := p.Plan(t.Context(), aRequest())
	if err == nil {
		t.Fatal("a planner with nowhere to record a rental went and made one")
	}
	if !strings.Contains(err.Error(), "nothing here can record what a machine rented to fetch them would cost") {
		t.Errorf("the refusal does not name the missing ledger: %v", err)
	}
	// And it refused BEFORE the catalogue read, which is what makes it free:
	// a refusal that first planned a machine would be one catalogue read per
	// request on a server that cannot record anything.
	if strings.Contains(err.Error(), "plan a machine to fill the store") {
		t.Errorf("the refusal came after planning the cheap machine: %v", err)
	}
}

// TestAppPlanner_AFullStoreStartsNoSecondRental is the negative control.
//
// Without it every assertion above is consistent with a planner that rents a
// CPU machine before every request there is -- which would be a second
// rental, every time, for nothing. This one must get past the weights and
// fail on the fake's missing catalogue, with no mention of filling anything.
// It is also the negative control for the LEDGER half, which #148 asked for
// by name: a request that starts no sync must not grow a row. Without it,
// "the ledger holds exactly one row" above is equally consistent with a
// planner that adopts a sync run for every request there is and then rents
// nothing -- a row in the money ledger for a rental that never happened.
func TestAppPlanner_AFullStoreStartsNoSecondRental(t *testing.T) {
	d := storeFakes(t, map[string]int64{"weights.gguf": 60306914804})
	ledger := genstore.NewRecordingStore()
	p := AppPlanner{Config: storeConfig(), Deps: d, AllowPaid: true, Runs: ledger}

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

	rows, err := ledger.List(t.Context(), genstore.Filter{})
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("a full store left %d row(s) in the ledger: %+v", len(rows), rows)
	}
}

// TestAppPlanner_AnEmptyStoreWithoutAllowPaidRentsNothingAtAll.
//
// The sync is a rental and is gated like one. Without this the cheap machine
// would be a hole in the one gate `ember serve` has: --allow-paid would stop
// the GPU and not the thing that goes and fetches fifty-six gigabytes.
// It asserts WHICH refusal, because an empty store now has more than one way
// to stop and a bare `err != nil` passed against the recorder's refusal as
// happily as against this one -- found by the #148 reinstatement pass rather
// than by review, which is the third time in this repository a refusal test
// has turned out to be consistent with the wrong refusal.
func TestAppPlanner_AnEmptyStoreWithoutAllowPaidRentsNothingAtAll(t *testing.T) {
	p := AppPlanner{
		Config: storeConfig(), Deps: storeFakes(t, map[string]int64{}),
		Runs: genstore.NewRecordingStore(),
	}

	_, err := p.Plan(t.Context(), aRequest())
	if err == nil {
		t.Fatal("a paid provider was planned without --allow-paid")
	}
	if !strings.Contains(err.Error(), "--allow-paid") {
		t.Errorf("the empty store stopped for some other reason than the money gate: %v", err)
	}
}
