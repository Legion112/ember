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
// is how the --allow-paid gate on the store fill was found (#155) and how the
// period ceiling beside it was found here. NO RUNNING TOTAL: the first
// wording of this comment called it "the third time", and this change alone
// adds two or three to whatever the count was -- a tally in prose is the
// thing `AGENTS.md` records itself having been wrong about, so the instances
// are named rather than numbered.
//
// WHY NOTHING COULD SEE THEM. Both gates read `plan.Offer`, so they are on the
// far side of a successful `app.NewPlan` -- and no fake in this package could
// get one. `ceiling_test.go` reaches the ceiling through the local provider,
// which plans fine and is FREE, so refusePaid returns nil and the projection
// is zero. `storeFakes` served no GPU catalogue. Every path to these two lines
// went through a provider that either could not choose an offer or could not
// be charged for one.
//
// SO THE FAKE HERE IS THE ONE THING MISSING: a paid provider that CAN choose
// an offer. It is the provider's own committed capture rather than a second
// copy of a field shape, which is the rule `dataCenter` against
// `dataCenterId` bought -- and it prices the RTX 4090 at $0.89/hr, which is
// the figure `AGENTS.md`'s own measurements quote for a 4090.
//
// IT IS NOT storeFakes WITH ONE MORE ROUTE, AND THE FIRST REASON GIVEN FOR
// THAT WAS FALSE. This comment used to read "because three tests in
// preflight_test.go assert positively on failing at 'list offers from
// runpod'". Measured: adding a /v2/catalog/gpus route to `storeFakes` breaks
// NOTHING -- `go test ./internal/queue/` exit 0 -- there is ONE such
// assertion in this package, at preflight_test.go:255, and it survives the
// route; the three are in internal/cli/preflight_test.go, a different package
// with its own fake, and one of those three is a negative. A comment wrong
// about why a seal holds is how the seal gets removed by somebody being
// helpful, and that one would have sent the next reader to delete a helper
// they believed was redundant.
//
// THE REAL REASON IS THE FIXTURE AND IT IS STRONGER: `storeConfig` asks for
// EU-RO-1 and the fake's volume listing reports it -- the region that reaches
// Filter.Region is the PROVIDER's answer, which AttachStore reads off
// `storeFakes`'s own network-volumes handler, and a store's datacentre
// OVERRIDES the configured placement -- while the committed `gpus.json`
// carries capacity only in CA-MTL-3, US-MO-1 and US-IL-1. So `storeFakes` can
// never produce a GPU offer at all: the gates behind plan.Offer are out of
// its reach by FIXTURE and not by the catalogue route it was missing.
// Reaching them from there would mean moving that region on both sides or
// re-capturing the catalogue, perturbing five existing tests to serve one.
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
		// S3BaseURL IS SET ALTHOUGH NOTHING HERE READS IT, because
		// app.CheckStoreAssets falls back to s3.EndpointFor(store.Region) --
		// a real RunPod endpoint -- whenever this field is empty. It is
		// unreachable today and TWO conditions short rather than one:
		// `pricedConfig` names no store and no assets, and the check also
		// returns at preflight.go's creds.S3.IsZero() arm, BEFORE the
		// endpoint is built, which is where these Creds land it. So the hole
		// needs a store AND S3 credentials in this literal, and it opens the
		// day somebody adds either half for a reason that has nothing to do
		// with the other. The rule it would break then is the one `AGENTS.md`
		// states by name: a test that registers a cloud provider must not be
		// able to reach it. The same handler, whose default branch is a 404,
		// so an object call reads as an empty store -- packets do leave, over
		// loopback to this test's own httptest listener, and none of them
		// reaches a RunPod endpoint.
		S3BaseURL: api.URL,
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
			t.Errorf("the refusal does not mention %q, so it is not the money gate's "+
				"(or testdata/gpus.json no longer prices the 4090 at $0.89/hr): %v", want, err)
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
// THE CEILING IS MATCHED AS "ceiling of $20," AND NOT AS "$20", because "$20"
// is a SUBSTRING of the $20.34 two fields later. Measured: with the ceiling
// figure misprinted, `go test -run AgainstThePeriodCeilings ./internal/queue/`
// is exit 0 against the bare "$20" -- the assertion was held up by the total
// beside it and the property by a different test entirely. That is
// `AGENTS.md`'s "a mention is a whole token" rule, which the config-key guard
// learned over `--as`, reached one test over; the comma is what the message
// puts after the figure, so the bound costs nothing.
//
// AND THE TTL IS THE NARROWED ONE. `pricedConfig` allows two hours and the
// request asks for one, so the honest projection is $0.89 and the config's own
// TTL would make it $1.78 -- that is the deadline the node will actually get,
// which is the whole reason the projection uses it. Measured: substituting
// `p.Config.TTL` for `cfg.TTL` is red in BOTH arms -- at the refusal's `want`
// list, because $0.89 and $20.34 both move, and at the acceptance, because
// $18.95 + $1.78 is $20.73 and refuses where $19.84 goes ahead. The first
// wording of this comment said the acceptance arm caught it "and nowhere
// else", which understated the test it was describing; the acceptance arm is
// a second witness rather than the only one, and it is the one that would
// still fire if the refusal's figures were ever loosened.
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
	for _, want := range []string{"max_day_usd", "2026-11-17", "$19.45", "ceiling of $20,", "$0.89", "$20.34"} {
		if !strings.Contains(err.Error(), want) {
			// The price figures are the capture's, so a `gpus.json` whose
			// RTX 4090 is no longer $0.89/hr reads here as a gate that did
			// not fire. Said in the message rather than left to be worked
			// out, because a figure that moved and a gate that broke are the
			// same red line otherwise.
			t.Errorf("the refusal does not mention %q, so it is not the period ceiling's "+
				"(or testdata/gpus.json no longer prices the 4090 at $0.89/hr): %v", want, err)
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
