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
	"github.com/Legion112/ember/internal/report"
)

// The money gates in this package, and whether each of them can fail.
//
// IT WAS TWO AND IT IS FOUR. The two at the top are `AppPlanner.Plan`'s, on
// the rental a request ASKED for; the two at the bottom are
// `DirectRunner.Start`'s, on a run `ember up` submitted over the unix socket,
// which had --allow-paid and NO PERIOD CEILING AT ALL until #165. The fifth
// and sixth live in `internal/cli`, on the two shell paths, for the same
// reason and in the same change.
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

// submittedRunConfig is `paidRunConfig` over a whole hour, so the projection
// at $0.89/hr is exactly $0.89 and the figures in the assertions below are
// readable rather than a rounded fraction of an hour.
const submittedRunConfig = "provider: runpod\nimage: " + runImage + "\nttl: 1h\ncmd: [echo, hi]\n"

// aLoop runs the drain goroutine for as long as the test needs one and waits
// for its deferred destroy on the way out.
//
// The ACCEPTANCE arms below need one: `Submit` is a rendezvous on an
// unbuffered channel, so a run that got past both gates parks for ever with
// nobody receiving. The refusal arms never reach it. Nothing here asserts on a
// row's STATUS while this is running, for the reason
// TestDirectRunner_StartAdoptsTheRowAndHandsTheRunToTheLoop records: a rental
// that fails fast against the recording Docker engine writes a terminal status
// at a moment no assertion can order itself against. What they assert is the
// COUNT, which Adopt settles synchronously inside Start before Submit is
// reached.
func aLoop(t *testing.T, q *Queue) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done //nolint:errcheck // waiting for the destroy, not for an error
	})
	return ctx
}

// ledgerRows is how many requests the history holds.
func ledgerRows(t *testing.T, st genstore.Store) int {
	t.Helper()
	rs, err := st.List(context.Background(), genstore.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	return len(rs)
}

// TestDirectRunner_ProjectsASubmittedRunAgainstTheDaemonsPeriodCeilings.
//
// WHAT WAS WRONG (#165). Five paths write `requests.cost`, the column
// `genstore.Spend` sums with no status filter, and only two of them were
// projected against `max_day_usd` and `max_month_usd`. A run submitted over
// the socket consumed the queue's allowance and was bounded by nothing, so the
// key read as a bound on a day and was a bound on the queue -- measured
// structurally rather than by a test: `config.PeriodCeilings` had exactly two
// non-test callers, this package's one gate and a startup print.
//
// THE CEILING IS THE DAEMON'S AND THE LEDGER IS THE DAEMON'S, which is the
// pair the whole decision turns on: a ceiling is a question about a total, and
// the total a submitted run adds to is the one in `DirectRunner.Store`. See
// the Config field.
//
// THREE NUMBERS, CHOSEN SO THAT ONLY THE REAL GATE PASSES THEM -- #164's
// pattern, which is the "what would this assertion say if the bug were
// present?" question asked of a money figure. $19.45 spent today, a $20 day
// ceiling, a $0.89 projection:
//
//   - a gate that IGNORED the ledger compares $0.89 against $20 and allows;
//   - a gate that REPORTED rather than PROJECTED compares $19.45 against $20
//     and allows -- the "ceiling every period breaches by one rental" the
//     design refuses to be;
//   - the live one adds them, gets $20.34, and refuses.
//
// THE CEILING IS MATCHED AS "ceiling of $20," AND NOT AS "$20", because "$20"
// is a substring of the "$20.34" two fields later in the same message --
// measured in #164 as an assertion held up by the total beside it. The comma
// is what the message already puts after the figure, so the bound costs
// nothing.
//
// TWO WITNESSES THAT IT FIRED WHERE IT CLAIMS TO. The message is one; the
// other is that the history still holds ONE row -- the seeded rental -- so the
// refusal landed before `Adopt` wrote the run's own. A row written first would
// have told the ledger a rental was coming when none was.
//
// AND THE ACCEPTANCE IS IN THE SAME TEST, because a refusal alone is
// consistent with a gate that refuses every submitted run there is, which
// would make the channel useless rather than bounded.
func TestDirectRunner_ProjectsASubmittedRunAgainstTheDaemonsPeriodCeilings(t *testing.T) {
	cfg := writeRunConfig(t, submittedRunConfig)

	over := genstore.NewRecordingStore()
	spentOn(t, over, at(17, 9), 19_450_000)
	q := &Queue{Store: over, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
	ctx := aLoop(t, q)

	r := paidRunner(t, q, over)
	r.AllowPaid = true
	r.Config = config.Config{MaxDayUSD: "20"}
	r.Now = func() time.Time { return at(17, 12) }

	_, err := r.Start(ctx, app.RunAsk{Config: cfg})
	if err == nil {
		t.Fatal("a submitted run projected past the day's ceiling was accepted")
	}
	for _, want := range []string{"max_day_usd", "2026-11-17", "$19.45", "ceiling of $20,", "$0.89", "$20.34"} {
		if !strings.Contains(err.Error(), want) {
			// The price is the capture's, so a `gpus.json` whose RTX 4090 is
			// no longer $0.89/hr reads here as a gate that did not fire. Said
			// in the message rather than left to be worked out, because a
			// figure that moved and a gate that broke are otherwise the same
			// red line.
			t.Errorf("the refusal does not mention %q, so it is not the period ceiling's "+
				"(or testdata/gpus.json no longer prices the 4090 at $0.89/hr): %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "--allow-paid") {
		t.Errorf("the run stopped on the flag rather than on the ceiling: %v", err)
	}
	if n := ledgerRows(t, over); n != 1 {
		t.Errorf("the history holds %d row(s), want only the seeded rental: a refused run that "+
			"adopted a row told the ledger a rental was coming when none was", n)
	}

	room := genstore.NewRecordingStore()
	spentOn(t, room, at(17, 9), 18_950_000)
	q2 := &Queue{Store: room, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
	ctx2 := aLoop(t, q2)

	a := paidRunner(t, q2, room)
	a.AllowPaid = true
	a.Config = config.Config{MaxDayUSD: "20"}
	a.Now = func() time.Time { return at(17, 12) }

	if _, err := a.Start(ctx2, app.RunAsk{Config: cfg}); err != nil {
		t.Fatalf("a submitted run that fits inside the ceiling was refused, so the gate refuses "+
			"everything rather than refusing what crosses a ceiling: %v", err)
	}
	if n := ledgerRows(t, room); n != 2 {
		t.Errorf("the history holds %d row(s), want the seeded rental and the accepted run", n)
	}
}

// TestDirectRunner_TheTTLItProjectsOverIsTheRunsAndNotTheDaemons.
//
// The projection is the offer's price over the deadline THE NODE WILL GET,
// which on this path comes from the config the run named and the overrides it
// carried -- not from the config `ember serve` happens to have been started
// with. Those two files are both in scope here and only one of them says how
// long this rental lasts, so a gate that reached for `r.Config.TTL` would
// price a submitted run at the daemon's hours.
//
// Measured both ways rather than asserted in one: the daemon's config names
// TEN hours and the run's names one. At $0.89/hr with $18.95 already spent
// against a $20 day, the honest projection is $0.89 and the run goes ahead;
// the daemon's TTL would make it $8.90, take the day to $27.85 and refuse. So
// substituting it is red here and nowhere else, which is the half the test
// above cannot see because both of its configs agree about the hour.
func TestDirectRunner_TheTTLItProjectsOverIsTheRunsAndNotTheDaemons(t *testing.T) {
	st := genstore.NewRecordingStore()
	spentOn(t, st, at(17, 9), 18_950_000)
	q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
	ctx := aLoop(t, q)

	r := paidRunner(t, q, st)
	r.AllowPaid = true
	r.Config = config.Config{MaxDayUSD: "20", TTL: 10 * time.Hour}
	r.Now = func() time.Time { return at(17, 12) }

	if _, err := r.Start(ctx, app.RunAsk{Config: writeRunConfig(t, submittedRunConfig)}); err != nil {
		t.Fatalf("a one-hour run was priced at the daemon's ten hours: %v", err)
	}
}

// TestDirectRunner_ReChecksTheCeilingOnTheLoopWhereTheTotalIsComplete.
//
// THE GAP THE FIRST FORM OF #165 LEFT, found by review. `Start`'s gate runs on
// the CLIENT's goroutine and `Submit` then parks on the rendezvous until the
// drain loop is between iterations. `requests.cost` is written from
// `provision.WithNode`'s DEFERRED teardown, so a queued rental in flight when
// the gate read contributed ZERO -- it finishes and records while the
// submission is parked, and the submitted run then rented against a figure
// that was true when it was read and is not any more. Worked: $20 day, $19
// recorded, a $2 rental in flight, a $0.89 projection -- $19.89 admits, the day
// ends at $21.89.
//
// Bounded by one rental, and one rental is the magnitude this feature exists
// to refuse. Before #165 both gates lived on the drain loop, where "the
// completed cost of finished rentals" and "everything recorded" were the same
// set; moving one onto a client's goroutine is what made that insufficient.
//
// IT IS THE REAL SCENARIO AND NOT A SIMULATED ONE, and the first version was
// NEITHER -- it was a flake. That one started the drain loop AFTER the row was
// adopted, so `Run`'s startup `Interrupt` sweep raced the adopt and sometimes
// won, marking the submitted row `interrupted`; `waitForTerminal` then read
// that transient state, because `interrupted` is terminal, and the assertion
// saw a row with no reason on it. Caught by `make cover`, which runs the suite
// slower, and not by any ordinary run -- which is the frequency AGENTS.md
// records as the worst a flake can have. The sweep is a real and documented
// interaction with `Adopt` and it only happens in that ORDER, which production
// never has: `ember serve` sweeps once at startup, long before a client
// submits.
//
// So the loop is started FIRST, before anything is adopted, and is made busy
// the way the scenario says it is: a queued request, claimed, whose rental
// blocks. `Submit` then parks for certain, because the one goroutine that can
// rent is inside a rental -- and `Interrupt` has already run over an empty
// history.
//
// WHAT STANDS IN FOR THE TEARDOWN WRITE, said plainly: `fakeRental` emits no
// `run_cost`, so the cost of the rental in flight is written here, at the
// moment releasing it would have written it. That is the one thing this test
// hands itself, and it is the thing the whole defect is about -- a figure that
// lands between the two reads.
//
// WHAT IT ASSERTS IS THE ROW, because `Start` has already answered the client
// by then -- nil, with the id. So the refusal has to reach the operator
// through `requests.err`, which is what `runOutcome` prints to a client still
// following the stream, and `finish` is what writes it through `classify`.
// With the re-check removed the run reaches `plan.Do` instead and the row ends
// with a RunPod create failure, so the ceiling's own words are what
// discriminate.
func TestDirectRunner_ReChecksTheCeilingOnTheLoopWhereTheTotalIsComplete(t *testing.T) {
	st := genstore.NewRecordingStore()
	// $19 already recorded against a $20 day: the first read admits a $0.89
	// rental at $19.89.
	spentOn(t, st, at(17, 9), 19_000_000)

	// BUFFERED BY ONE for TestRun_ReturnsOnlyAfterTheRentalIsDestroyed's
	// reason: an unbuffered signal with a `default:` arm goes nowhere unless a
	// receiver is already parked, which under the whole suite in parallel is a
	// test that waits out its own timeout.
	inFlight := make(chan struct{}, 1)
	release := make(chan struct{})
	pl := &fakePlanner{served: &fakeServed{progress: func(report.Sink, int) {
		select {
		case inFlight <- struct{}{}:
		default:
		}
	}}, block: release}
	q := &Queue{Store: st, Planner: pl, Outputs: t.TempDir()}
	enqueue(t, st, "rtx2080ti", "a")

	// The loop first, so Interrupt runs over a history with no adopted row in
	// it, and then busy, so Submit has something to park behind.
	aLoop(t, q)
	select {
	case <-inFlight:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop never started the queued rental, so Submit would not have parked")
	}

	r := paidRunner(t, q, st)
	r.AllowPaid = true
	r.Config = config.Config{MaxDayUSD: "20"}
	r.Now = func() time.Time { return at(17, 12) }

	type res struct {
		id  genstore.ReqID
		err error
	}
	done := make(chan res, 1)
	go func() {
		id, err := r.Start(context.Background(), app.RunAsk{Config: writeRunConfig(t, submittedRunConfig)})
		done <- res{id, err}
	}()

	// Adopt happens inside Start and immediately before Submit, so the row
	// appearing is how this test knows the first gate has already passed --
	// a refusal there writes no row, which is the other test's witness.
	id := waitForAdopted(t, st)

	// The rental in flight records what it cost, which is what its teardown
	// would do. $19 + $1.50 is $20.50, so the honest total now refuses a
	// $0.89 rental and the stale one still admits it.
	spentOn(t, st, at(17, 10), 1_500_000)

	close(release)

	got := <-done
	if got.err != nil {
		t.Fatalf("Start refused the submission, so it never reached the loop: %v", got.err)
	}
	if got.id != id {
		t.Fatalf("Start answered %s and the row is %s", got.id, id)
	}

	row := waitForTerminal(t, st, id)
	if row.Status != genstore.StatusFailed {
		t.Errorf("the row ended as %q, want failed: a run admitted on a stale total rented "+
			"past the ceiling", row.Status)
	}
	for _, want := range []string{"max_day_usd", "ceiling of $20,", "$20.5"} {
		if !strings.Contains(row.Err, want) {
			t.Errorf("the recorded reason does not mention %q, so the ceiling was not re-read on "+
				"the loop (it says %q)", want, row.Err)
		}
	}
}

// waitForAdopted blocks until a row written by Adopt appears and answers its
// id.
//
// A BOUND AND A t.Fatal RATHER THAN A SLEEP: what is being waited for is a
// write another goroutine makes, and the thing that means it will never happen
// is the deadline -- `orExited`'s rule as the browser labs spell it. A skip
// here would report success for something that never ran.
//
// THE PROVIDER IS THE DISCRIMINATOR AND IT HAS TO BE SOMETHING: `ClaimNext`
// also writes `running`, so a wait that looked only at the status would pick
// up the queued rental this test deliberately has in flight. `enqueue` writes
// `local` and the submitted run names `runpod`, which tells them apart here.
// `run_id` would be the more principled answer -- Adopt leaves it empty by
// design -- and it is not available: `fakeRental` never mints one, so a
// claimed row's is empty too and the two would be indistinguishable.
func waitForAdopted(t *testing.T, st genstore.Store) genstore.ReqID {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		rs, err := st.List(context.Background(), genstore.Filter{})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range rs {
			if r.Status == genstore.StatusRunning && r.Provider == "runpod" {
				return r.ID
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no adopted row appeared, so Start never reached Submit")
	return ""
}

// waitForTerminal blocks until a request has ended and answers the row.
func waitForTerminal(t *testing.T, st genstore.Store, id genstore.ReqID) genstore.Request {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		r, err := st.Get(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status.Terminal() {
			return r
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("request %s never ended", id)
	return genstore.Request{}
}

// TestDirectRunner_TakesTheTighterOfTheTwoConfigsCeilings.
//
// BOTH FILES BIND AND NEITHER WIDENS THE OTHER, which is the second half of
// the decision on DirectRunner.Config. Reading only the daemon's would ship a
// money divergence between the two `up` paths -- an operator whose
// `ember.yaml` sets `max_day_usd` and who started the daemon with a different
// `--config` would get a bounded `ember up` in a shell and an unbounded one
// through a daemon, which is the one thing #146 exists to remove. Reading only
// the run's would leave #165 open, because the allowance being spent is the
// daemon's.
//
// So it is asserted in BOTH directions over the same $0.89 projection and the
// same $19.45 already spent: the daemon alone, and the run's config alone.
// Each arm is red if its own side is dropped, and the acceptance in the test
// above is what stops either being satisfied by a gate that refuses
// everything.
func TestDirectRunner_TakesTheTighterOfTheTwoConfigsCeilings(t *testing.T) {
	cases := []struct {
		name   string
		daemon config.Config
		run    string
	}{
		{
			// Only the daemon names one. This is #165 itself: the file
			// `ember serve` read is the one that owns the ledger being spent.
			name:   "the daemon's alone",
			daemon: config.Config{MaxDayUSD: "20"},
			run:    submittedRunConfig,
		},
		{
			// Only the run names one. Without this arm the socket path gives
			// LESS protection than the same command in a shell.
			name:   "the run's alone",
			daemon: config.Config{},
			run:    submittedRunConfig + "max_day_usd: \"20\"\n",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := genstore.NewRecordingStore()
			spentOn(t, st, at(17, 9), 19_450_000)
			q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
			ctx := aLoop(t, q)

			r := paidRunner(t, q, st)
			r.AllowPaid = true
			r.Config = c.daemon
			r.Now = func() time.Time { return at(17, 12) }

			_, err := r.Start(ctx, app.RunAsk{Config: writeRunConfig(t, c.run)})
			if err == nil {
				t.Fatal("the run was accepted, so this side's ceiling was not read")
			}
			if !strings.Contains(err.Error(), "ceiling of $20,") {
				t.Errorf("the refusal is not the period ceiling's: %v", err)
			}
			if n := ledgerRows(t, st); n != 1 {
				t.Errorf("the history holds %d row(s), want only the seeded rental", n)
			}
		})
	}
}

// TestDirectRunner_AMalformedCeilingNamesWhichFileItIsIn.
//
// Two configs reach this gate, so "max_day_usd: malformed price" on its own
// leaves an operator with two candidates and no way to tell which they just
// typed a path to. The parses are kept apart for exactly that, and both
// directions are asserted because a message that named one file whatever
// happened would read as working on the half it is right about.
//
// IT IS ALSO A PATH THE KEY HAD NEVER BEEN PARSED ON. `max_day_usd: "banana"`
// submitted to a daemon was accepted and run before this change -- not
// refused, not warned about -- because nothing between `config.Load` and the
// create call read the key at all.
func TestDirectRunner_AMalformedCeilingNamesWhichFileItIsIn(t *testing.T) {
	cases := []struct {
		name   string
		daemon config.Config
		run    string
		want   string
	}{
		{
			name:   "the daemon's",
			daemon: config.Config{MaxDayUSD: "banana"},
			run:    submittedRunConfig,
			want:   "the config ember serve was started with",
		},
		{
			name:   "the run's",
			daemon: config.Config{},
			run:    submittedRunConfig + "max_day_usd: \"banana\"\n",
			want:   "the config this run named",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := genstore.NewRecordingStore()
			q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
			ctx := aLoop(t, q)

			r := paidRunner(t, q, st)
			r.AllowPaid = true
			r.Config = c.daemon

			_, err := r.Start(ctx, app.RunAsk{Config: writeRunConfig(t, c.run)})
			if err == nil {
				t.Fatal("a ceiling that is not a price was accepted")
			}
			if !strings.Contains(err.Error(), "max_day_usd") {
				t.Errorf("the refusal does not name the key: %v", err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal does not say which file holds it (want %q): %v", c.want, err)
			}
			if n := ledgerRows(t, st); n != 0 {
				t.Errorf("a refused run wrote %d row(s) to the history", n)
			}
		})
	}
}

// TestCeilings_TightestNeverWidensAndTreatsZeroAsUnbounded.
//
// The composition the socket path rests on, as arithmetic. Zero has to mean
// unbounded on both sides or a config that sets neither narrows the other's to
// nothing and refuses every rental there is -- which is `Narrow`'s own reason
// for `smallerUSD`, and is why this delegates to it rather than spelling the
// comparison a second time.
func TestCeilings_TightestNeverWidensAndTreatsZeroAsUnbounded(t *testing.T) {
	cases := []struct {
		name string
		a, b Ceilings
		want Ceilings
	}{
		{"neither", Ceilings{}, Ceilings{}, Ceilings{}},
		{"only a", Ceilings{Day: 20_000_000}, Ceilings{}, Ceilings{Day: 20_000_000}},
		{"only b", Ceilings{}, Ceilings{Month: 250_000_000}, Ceilings{Month: 250_000_000}},
		{
			"a is tighter",
			Ceilings{Day: 20_000_000, Month: 100_000_000},
			Ceilings{Day: 50_000_000, Month: 250_000_000},
			Ceilings{Day: 20_000_000, Month: 100_000_000},
		},
		{
			// Per period and not per config: the tighter day and the tighter
			// month can come from different files, and taking one file whole
			// would widen the other's.
			"one period each",
			Ceilings{Day: 20_000_000, Month: 250_000_000},
			Ceilings{Day: 50_000_000, Month: 100_000_000},
			Ceilings{Day: 20_000_000, Month: 100_000_000},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.Tightest(c.b); got != c.want {
				t.Errorf("Tightest = %+v, want %+v", got, c.want)
			}
			// Symmetric, or "which config you name first" would be a money
			// decision.
			if got := c.b.Tightest(c.a); got != c.want {
				t.Errorf("Tightest reversed = %+v, want %+v", got, c.want)
			}
		})
	}
}
