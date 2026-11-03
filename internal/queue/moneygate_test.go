package queue

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/local"
)

// The two money gates on the rental a request ASKED for, which is the card.
//
// WHY THIS FILE EXISTS AT ALL. `AppPlanner.Plan` ends with refusePaid and then
// refuseOverPeriod -- the last two statements before anything is created --
// and until this file NEITHER could be shown to fire there. Measured, against
// the tree as it stood: deleting `refusePaid(p.AllowPaid, plan.Provider,
// plan.Offer)` outright left `go test ./...` at exit 0, and so did replacing
// `plan.Offer.Price` with a literal 0 in the ceiling call beside it -- a
// ceiling that projects nothing refuses nothing, which is the $54 case
// exactly. Found by sweeping the class for #157 rather than by review, which
// is the third time in this repository a money gate has turned out to be held
// in place by nothing: `AGENTS.md` records the --allow-paid gate on the store
// fill (#155) and this change records the period ceiling beside it.
//
// WHY NOTHING COULD SEE THEM. Both gates read `plan.Offer`, so they are on the
// far side of a successful `app.NewPlan` -- and no fake in this package served
// a GPU catalogue. `storeFakes` deliberately does not, because a provider that
// cannot say what cards it has is what makes the pre-flight's ORDER
// observable; `ceiling_test.go` reaches the ceiling through the local
// provider, which plans fine and is FREE, so refusePaid returns nil and the
// projection is zero. Every path to these two lines went through a provider
// that either could not choose an offer or could not be charged for one.
//
// SO THE FAKE HERE IS THE ONE THING MISSING: a paid provider that CAN choose
// an offer. It is the provider's own committed capture rather than a second
// copy of a field shape, which is the rule `dataCenter` against
// `dataCenterId` bought -- and it prices the RTX 4090 at $0.89/hr, which is
// the figure `AGENTS.md`'s own measurements quote for a 4090.
//
// IT IS NOT storeFakes WITH ONE MORE ROUTE, because three tests in
// preflight_test.go assert positively on failing at "list offers from runpod"
// and would stop asserting the order they exist for.
//
// THERE IS STILL NO /v2/pods ROUTE, so nothing here can create a node even if
// a gate let it through -- and a plan that got past both gates returns a
// rental nobody rents, which is what `TestAppPlanner_ResolvesWithoutRenting`
// already turns on.
func pricedFakes(t *testing.T) app.Deps {
	t.Helper()

	gpus, err := os.ReadFile(filepath.Join("..", "provider", "runpod", "testdata", "gpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/v2/catalog/gpus" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(gpus)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(api.Close)

	return app.Deps{
		Engine:       dockerengine.NewRecordingEngine(),
		HomeDir:      t.TempDir(),
		CloudBaseURL: api.URL,
		// Injected rather than resolved, so this test cannot pick up the
		// developer's own RUNPOD_API_KEY and make a live call.
		Creds:     &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))},
		DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
	}
}

// pricedConfig has no store and no assets, so Plan goes straight from the
// catalogue to the two gates: the store pre-flight is tested in
// preflight_test.go and would only put a second reason to stop in front of
// the thing being asserted here.
//
// A TWO-HOUR TTL THAT THE REQUEST NARROWS TO ONE, which is load-bearing for
// the ceiling test below rather than decoration -- see there.
func pricedConfig() config.Config {
	return config.Config{
		Provider: "runpod",
		Image:    "ghcr.io/example/sd@sha256:" + strings.Repeat("a", 64),
		TTL:      2 * time.Hour,
		SD:       config.SDConfig{Serve: []string{"sd-server", "--model", "/m.gguf"}},
	}
}

func aPricedRequest() genstore.Request {
	return genstore.Request{
		Provider: "runpod", TTL: time.Hour,
		Jobs: []genstore.Job{{Prompt: "a paper boat", Width: 512, Height: 512, Steps: 4}},
	}
}

// TestAppPlanner_ReachesTheAllowPaidGateOnTheCardItChose.
//
// The join, which is the half `TestRefusePaid_DefaultsToRefusing` cannot make:
// that one calls the function and proves the function refuses, and this one
// proves `Plan` ARRIVES there. A gate and a test that each agree with
// themselves across a gap nothing crosses is the failure this repository has
// paid for repeatedly -- the unrecorded preview frame, `capabilities.model`,
// and the --allow-paid gate on the store fill.
//
// THE REFUSAL NAMES THE CARD AND ITS PRICE, which is refusePaid's own stated
// reason for sitting after the offer is chosen rather than before: "runpod is
// not free" is less useful to an operator than the rate they were about to be
// charged.
//
// AND THE ACCEPTANCE IS IN THE SAME TEST, so neither half can be weakened
// without the other going red: a refusal on its own is equally consistent
// with a planner that refuses every paid provider whatever the flag says,
// which would make --allow-paid mean nothing.
func TestAppPlanner_ReachesTheAllowPaidGateOnTheCardItChose(t *testing.T) {
	deps := pricedFakes(t)

	_, err := AppPlanner{Config: pricedConfig(), Deps: deps}.Plan(t.Context(), aPricedRequest())
	if err == nil {
		t.Fatal("a paid card was planned through by a planner started without --allow-paid")
	}
	for _, want := range []string{"--allow-paid", "$0.89/hr", "runpod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it is not the money gate's: %v", want, err)
		}
	}

	rental, err := AppPlanner{Config: pricedConfig(), Deps: deps, AllowPaid: true}.
		Plan(t.Context(), aPricedRequest())
	if err != nil {
		t.Fatalf("--allow-paid still refused the card: %v", err)
	}
	if rental == nil {
		t.Fatal("Plan returned no rental and no error")
	}
}

// TestAppPlanner_ProjectsTheChosenCardsPriceAgainstThePeriodCeilings.
//
// `TestAppPlanner_ChecksThePeriodCeilingBeforeRenting` proves `Plan` reaches
// the ceiling, and it reaches it through the NO-LEDGER refusal over a free
// local offer -- so it says nothing about the arithmetic, because
// `refuseOverPeriod` answers a nil Ledger before it ever computes a
// projection. Measured: replacing `plan.Offer.Price` with 0, or `cfg.TTL` with
// 0, in that call left `go test ./...` at exit 0. A ceiling projecting nothing
// is a ceiling that refuses nothing.
//
// THREE NUMBERS, CHOSEN SO THAT ONLY THE REAL GATE PASSES THEM: $19.45 spent
// today, a $20 day ceiling, and a $0.89 projection.
//
//   - a ceiling that ignored the ledger compares $0.89 against $20 and allows;
//   - a ceiling that REPORTED rather than PROJECTED compares $19.45 against
//     $20 and allows -- the "ceiling every period breaches by one rental" this
//     exists not to be;
//   - the live one adds them, gets $20.34, and refuses.
//
// AND THE TTL IS THE NARROWED ONE, which is what the ACCEPTANCE arm pins.
// `pricedConfig` allows two hours and the request asks for one, so the honest
// projection is $0.89 and the config's own TTL would make it $1.78. With
// $18.95 already spent, the narrowed figure is $19.84 and goes ahead while the
// config's would be $20.73 and refuse -- so a ceiling reading
// `p.Config.TTL` in place of `cfg.TTL` fails here and nowhere else. That is
// the deadline the node will actually get, which is the whole reason the
// projection uses it.
func TestAppPlanner_ProjectsTheChosenCardsPriceAgainstThePeriodCeilings(t *testing.T) {
	cfg := pricedConfig()
	cfg.MaxDayUSD = "20"

	over := genstore.NewRecordingStore()
	spentOn(t, over, at(17, 9), 19_450_000)

	_, err := AppPlanner{
		Config: cfg, Deps: pricedFakes(t), AllowPaid: true, Ledger: over,
		Now: func() time.Time { return at(17, 12) },
	}.Plan(t.Context(), aPricedRequest())
	if err == nil {
		t.Fatal("a card projected past the day's ceiling was planned through")
	}
	for _, want := range []string{"max_day_usd", "2026-11-17", "$19.45", "$20", "$0.89", "$20.34"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q, so it is not the period ceiling's: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "--allow-paid") {
		t.Errorf("the card stopped on the flag rather than on the ceiling: %v", err)
	}

	room := genstore.NewRecordingStore()
	spentOn(t, room, at(17, 9), 18_950_000)

	rental, err := AppPlanner{
		Config: cfg, Deps: pricedFakes(t), AllowPaid: true, Ledger: room,
		Now: func() time.Time { return at(17, 12) },
	}.Plan(t.Context(), aPricedRequest())
	if err != nil {
		t.Fatalf("a rental that fits inside the ceiling over the TTL it will get was refused: %v", err)
	}
	if rental == nil {
		t.Fatal("Plan returned no rental and no error")
	}
}
