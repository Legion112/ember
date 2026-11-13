package queue

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/sdserver"
)

// TestDrivers_AreTheKindsTheStoreKnows holds the two lists of driver names
// against each other, in both directions.
//
// TWO LISTS AND NOT ONE, WHICH IS A COST AND NOT A DESIGN. `genstore.Kinds()`
// says of itself that it is "the one list", and `config.Drivers()` is the same
// set -- because `internal/config` cannot read it. Importing `internal/genstore`
// there would put modernc.org/sqlite into the dependency graph of
// `internal/app`, which is the one package that imports config and not
// genstore and is the package that rents machines; the reverse direction is a
// store that reads ember.yaml. And there is no package both already import
// that a driver name belongs in -- `internal/node` imports nothing from this
// module and holds machines, offers and money, which is the argument
// `node.Memory` won and this one would lose.
//
// SO IT IS CHECKED RATHER THAN DERIVED, which is the shape `make tidy-check`
// already uses for AGENTS.md's dependency list against `go.mod`: two lists
// nothing can derive from each other, and a gate that fails on either
// direction. HERE, because this is the package where an entry's driver becomes
// a stored kind -- `Narrow` compares the two -- so it is the one place that
// already imports both.
//
// BOTH DIRECTIONS, because each is a different defect. A driver the store does
// not know is a request `genstore.Request.validate` refuses after the config
// has accepted it, which is a row an operator wrote a file for and cannot
// queue. A kind no driver names is a workload nothing can declare, so the
// store would hold a column value `ember serve` could never produce.
func TestDrivers_AreTheKindsTheStoreKnows(t *testing.T) {
	drivers := config.Drivers()
	kinds := genstore.Kinds()

	if len(drivers) == 0 || len(kinds) == 0 {
		t.Fatalf("one of the lists is empty: drivers = %v, kinds = %v", drivers, kinds)
	}
	for _, d := range drivers {
		if !slices.Contains(kinds, d) {
			t.Errorf("config.Drivers() names %q and genstore.Kinds() does not; a request for "+
				"that workload is refused by the store after the config accepted it", d)
		}
	}
	for _, k := range kinds {
		if !slices.Contains(drivers, k) {
			t.Errorf("genstore.Kinds() names %q and config.Drivers() does not; it is a kind "+
				"the store will hold and no workload can declare", k)
		}
	}
	// AND THE TWO CONSTANTS, because the lists agreeing says nothing about
	// whether the spellings a switch dispatches on are the same strings. Both
	// packages switch on these literals, and a driver renamed on one side
	// alone would satisfy every assertion above while sending an sd request
	// down the speech arm.
	if config.DriverSD != genstore.KindSD || config.DriverTTS != genstore.KindTTS {
		t.Errorf("the spellings disagree: config has %q/%q and genstore has %q/%q",
			config.DriverSD, config.DriverTTS, genstore.KindSD, genstore.KindTTS)
	}
}

// TestNarrow_RefusesARowWhoseWorkloadChangedItsDriver.
//
// THE FILE CAN BE EDITED BETWEEN THE ENQUEUE AND THE CLAIM, which is the one
// window a per-entry driver opens. The row was validated against the driver it
// was queued as -- `genstore.Request.validate` holds the jobs' shape to it --
// and a `driver:` changed from `sd` to `tts` under an id that kept its name
// would hand a stored batch of image jobs to piper, after a card had been paid
// for.
//
// THE OPERATOR'S FILE WINS BY BEING REFUSED RATHER THAN BELIEVED: nothing is
// rented, the row carries the reason, and the remedy is in the sentence.
func TestNarrow_RefusesARowWhoseWorkloadChangedItsDriver(t *testing.T) {
	cfg := config.Config{
		Provider: "local",
		Workloads: []config.Workload{{
			ID: "speech", Driver: config.DriverTTS, Image: "ghcr.io/x/tts:v1",
			TTS: config.TTSConfig{Voices: []config.TTSVoice{
				{ID: "lessac", Voice: "/v.onnx"},
			}},
		}},
	}
	// Queued as an image generation against that id, which is what a row
	// written before the operator changed `driver:` looks like.
	r := genstore.Request{
		Kind: genstore.KindSD, Workload: "speech", Provider: "local",
	}
	_, _, err := Narrow(cfg, r)
	if err == nil {
		t.Fatal("a row queued as sd was planned against a workload whose driver is now tts")
	}
	for _, want := range []string{"speech", "sd", "tts"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}

	// AND THE MATCHING ROW IS PLANNED, because a refusal alone is consistent
	// with a Narrow that refuses every declared workload there is.
	ok := genstore.Request{Kind: genstore.KindTTS, Workload: "speech", Provider: "local"}
	got, _, err := Narrow(cfg, ok)
	if err != nil {
		t.Fatalf("a row whose driver still matches was refused: %v", err)
	}
	if got.Image != "ghcr.io/x/tts:v1" {
		t.Errorf("image = %q, want the entry's own", got.Image)
	}
	if len(got.TTS.Voices) != 1 {
		t.Errorf("the entry's tts block did not reach the plan: %+v", got.TTS)
	}
}

// TestOnce_TwoRequestsForOneDeclaredWorkloadRideOneRental.
//
// THE QUEUE'S OWN HALF OF Compat, AND IT HAD NO EXIT CODE UNTIL A
// REINSTATEMENT PASS SAID SO. `TestClaimNext_TwoWorkloadsSharingADriverAreTwoRentals`
// proves the STORE honours the entry id, and it builds the `Compat` itself --
// so nothing proved the DRAIN LOOP puts the entry in the one it hands over.
// Measured: with `Compat.Workload` dropped from `once` and only the driver
// left, `go test ./internal/... ./cmd/...` was exit 0.
//
// AND THE DEFECT POINTS THE OTHER WAY FROM THE OBVIOUS ONE, which is why the
// assertion is the RIDE rather than the refusal. A compat with no workload
// resolves through `WorkloadOf("", "sd")` to `"sd"`, which is an id no
// declared file has unless the operator wrote one -- so the rider is refused
// rather than wrongly admitted, and what is lost is the whole saving the
// resident server exists for: every request on a declared workload would rent
// its own node, paying for provisioning, the image pull, the asset verify and
// the model load again. On a rented A100 that is about three minutes against
// roughly a hundred seconds of generation.
//
// THE REFUSAL IS ASSERTED BESIDE IT AND IS NOT THE DISCRIMINATOR: it is
// satisfied by a compat that refuses every rider there is, which is exactly
// what the defect produces. Both halves are here because the pair is the rule;
// the first is the exit code.
func TestOnce_TwoRequestsForOneDeclaredWorkloadRideOneRental(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "flux2"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	enq := func(workload, prompt string) {
		t.Helper()
		if _, err := st.Enqueue(t.Context(), genstore.Request{
			Kind: genstore.KindSD, Workload: workload,
			Provider: "local", GPU: "rtx2080ti", TTL: time.Hour,
			Jobs: []genstore.Job{{Prompt: prompt, Width: 64, Height: 64, Steps: 1}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	enq("flux", "first")
	enq("flux", "second")
	// Same driver, same provider, same card, another image -- which is what a
	// declared file with two `sd` entries holds.
	enq("sd15", "third")

	// DRIVEN THROUGH `Run` AND COUNTED IN PLANS, which is this assertion
	// rewritten for the supervisor rather than weakened. `once` is the slot
	// goroutine's body now and takes the claim with it, so there is no
	// single-shot entry point to call -- and `served.batches` is a flat list
	// across every rental, so counting batches can no longer say which rental
	// served them. A PLAN is a rental: the two requests for one declared
	// workload must produce ONE, and `sd15` its own, so the number to assert
	// is two. With the entry id dropped from the Compat the rider is refused
	// and each request rents its own node, which is three.
	q.MaxNodes = 1
	stopLoop := runWorkloadLoop(t, q)
	defer stopLoop()

	waitFor(t, 30*time.Second, "all three requests to be served", func() bool {
		served.mu.Lock()
		defer served.mu.Unlock()
		return len(served.batches) == 3
	})

	pl.mu.Lock()
	plans := len(pl.planned)
	pl.mu.Unlock()
	if plans != 2 {
		t.Errorf("the loop planned %d rental(s) for three requests, want 2: two requests for "+
			"one declared workload ride one rental and `sd15` gets its own. A Compat that does "+
			"not carry the entry id refuses its own riders and rents a node per request, which "+
			"pays for provisioning, the image pull, the asset verify and the model load again",
			plans)
	}
}

// runWorkloadLoop drives the supervisor for one of these cases and stops it.
//
// THE SAME SHAPE AS runLoop IN maxnodes_test, and separate only because that
// one releases a fleetPlanner's blocked creates and these cases use the
// ordinary fake. A loop left running past the test holds a rental nothing
// accounts for, which is the thing its deferred stop exists for.
func runWorkloadLoop(t *testing.T, q *Queue) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			select {
			case <-done: //nolint:errcheck // the loop reports its own error
			case <-time.After(30 * time.Second):
				t.Error("the supervisor did not return after its context ended, so a rental " +
					"it holds is unaccounted for")
			}
		})
	}
}
