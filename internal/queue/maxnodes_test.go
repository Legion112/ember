package queue

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/tts"
)

// max_nodes, fit-or-create and strict packing (#204), driven through Run.
//
// WHY THESE DRIVE `Run` AND NOT `claimAndRun`. Every other test in this
// package calls that helper, which is the supervisor's claim and a slot's
// rental in one synchronous call -- and that helper is the test file's own
// opinion about the ORDER the two happen in. The properties here are exactly
// the ones that opinion cannot stand in for: how many rentals exist at once,
// whether the compat of a rental that is still provisioning is already
// excluded, and whether a shutdown waits for a set. All three are about the
// supervisor, so the supervisor is what they run.

// fleetPlanner opens one rental per request, through one counter, and holds
// each open until the test lets it go.
//
// THE HOLD IS AFTER THE BATCH, where provision.WithNode's deferred destroy
// is: the request has finished and the node is still alive, which is the
// window a second rental is born in. Keyed by the card, because what every
// assertion here turns on is which rentals exist AT ONCE and a test that could
// only release all of them could not ask that.
type fleetPlanner struct {
	gate *rentalGate

	mu      sync.Mutex
	planned []string
	batches map[string]int
	holds   map[string]chan struct{}
}

func newFleetPlanner() *fleetPlanner {
	return &fleetPlanner{
		gate:    &rentalGate{},
		batches: map[string]int{},
		holds:   map[string]chan struct{}{},
	}
}

// hold is the channel a rental for this card waits on, created on demand so a
// test does not have to declare the fleet in advance.
func (p *fleetPlanner) hold(gpu string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.holds[gpu] == nil {
		p.holds[gpu] = make(chan struct{})
	}
	return p.holds[gpu]
}

// release lets one card's rental finish being torn down.
func (p *fleetPlanner) release(gpu string) {
	ch := p.hold(gpu)
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func (p *fleetPlanner) releaseAll() {
	p.mu.Lock()
	gpus := make([]string, 0, len(p.holds))
	for g := range p.holds {
		gpus = append(gpus, g)
	}
	p.mu.Unlock()
	for _, g := range gpus {
		p.release(g)
	}
}

func (p *fleetPlanner) plans() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.planned...)
}

// batchesOn is how many requests one card's rental generated, which is what
// makes packing an assertion rather than an absence.
func (p *fleetPlanner) batchesOn(gpu string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.batches[gpu]
}

func (p *fleetPlanner) Plan(_ context.Context, r genstore.Request) (Rental, error) {
	p.mu.Lock()
	p.planned = append(p.planned, r.GPU)
	p.mu.Unlock()
	// The hold is taken here rather than in the rental, so that a test can
	// release a card before its rental has even opened.
	return fleetRental{p: p, gpu: r.GPU, hold: p.hold(r.GPU)}, nil
}

type fleetRental struct {
	p    *fleetPlanner
	gpu  string
	hold chan struct{}
}

func (r fleetRental) WithServed(ctx context.Context, sink report.Sink, fn func(context.Context, app.Served) error) error {
	return r.rent(ctx, sink, func(ctx context.Context) error {
		return fn(ctx, &countingServed{p: r.p, gpu: r.gpu})
	})
}

// WithSpoken mirrors WithServed through the same counter.
//
// A MIRROR AND NOT A REFUSAL: a fake stricter than the thing it stands in for
// grows its callers a branch for an error nothing returns, and the counter is
// what every assertion in this file is about -- so a second method that
// skipped it would be a rental the max-nodes property cannot see. The
// incompatible-work tests use it, because `kind` is the one axis of Compat
// that cannot be two values on one node.
func (r fleetRental) WithSpoken(ctx context.Context, sink report.Sink, fn func(context.Context, app.Spoken) error) error {
	return r.rent(ctx, sink, func(ctx context.Context) error {
		return fn(ctx, recordingSpoken{})
	})
}

func (r fleetRental) rent(ctx context.Context, sink report.Sink, fn func(context.Context) error) error {
	r.p.gate.enter(r.gpu)
	defer r.p.gate.leave()
	sink.Emit(report.Event{Kind: report.KindNodeReady, Node: "local/" + r.gpu})
	err := fn(ctx)
	// AFTER the batch, which is where the deferred destroy is. A test
	// observing two rentals at once is observing this window.
	select {
	case <-r.hold:
	case <-time.After(20 * time.Second):
	}
	sink.Emit(report.Event{Kind: report.KindRunCost, Node: "local/" + r.gpu, Cost: 11333})
	return err
}

// countingServed records which card served how many batches.
type countingServed struct {
	p   *fleetPlanner
	gpu string
}

func (c *countingServed) Caps() sdserver.Capabilities {
	return sdserver.Capabilities{Model: "sd15"}
}

func (c *countingServed) Generate(_ context.Context, _ report.Sink, _ sdserver.Run, _ string, _ <-chan struct{}) error {
	c.p.mu.Lock()
	defer c.p.mu.Unlock()
	c.p.batches[c.gpu]++
	return nil
}

// fleet builds a queue over the planner above, with a node ceiling.
func fleet(t *testing.T, p *fleetPlanner, maxNodes int) (*Queue, genstore.Store) {
	t.Helper()
	st := genstore.NewRecordingStore()
	return &Queue{
		Store:    st,
		Planner:  p,
		Voices:   queueVoices(),
		Outputs:  filepath.Join(t.TempDir(), "outputs"),
		MaxNodes: maxNodes,
	}, st
}

// runLoop starts the supervisor and makes sure it is waited for.
//
// IT CARRIES ITS OWN DEADLINE, because a wedge prints no `--- FAIL`: `Run`
// does not return until every slot's destroy has, and a slot that never
// released would otherwise hang until the binary's own timeout printed a
// goroutine dump naming nothing a reviewer can act on.
func runLoop(t *testing.T, q *Queue, p *fleetPlanner) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			p.releaseAll()
			select {
			case <-done: //nolint:errcheck // the loop's error is reported by its own log
			case <-time.After(30 * time.Second):
				t.Error("the supervisor did not return after its context ended, so a rental " +
					"it holds is unaccounted for")
			}
		})
	}
}

// enqueueKind is enqueue with a kind, which is the one Compat axis that cannot
// be two values on one node.
func enqueueKind(t *testing.T, st genstore.Store, kind, gpu string, prompts ...string) genstore.ReqID {
	t.Helper()
	r := genstore.Request{
		Kind: kind, Provider: "local", GPU: gpu, TTL: time.Hour,
		Preview: "proj", PreviewInterval: 5,
	}
	for _, p := range prompts {
		r.Jobs = append(r.Jobs, genstore.Job{Prompt: p, Width: 512, Height: 512, Steps: 4})
	}
	if kind == genstore.KindTTS {
		// A REAL UTTERANCE, because genstore.Validate refuses a speech
		// request whose job carries none -- and a fake row that got past
		// that would be a row the drain loop cannot be handed.
		r.Jobs = []genstore.Job{{TTS: &tts.Ask{Text: "hello", VoiceID: "lessac", Output: "greeting"}}}
	}
	id, err := st.Enqueue(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestRun_PacksOntoALiveRentalRatherThanCreating is the scheduling rule, and
// the surprising half of what max_nodes buys.
//
// Two requests for the SAME card with TWO slots free is ONE node. A fitting
// rental skips the ~27s per-rental overhead and, for a resident model, a
// reload measured at 80-96s per image -- so packing beats spreading here even
// though it is the opposite of what a latency-oriented scheduler does, because
// what ember spends is wall-clock rental and not contention.
//
// THE WAIT RACES THE TWO OUTCOMES, which is `orExited`'s rule: the correct
// behaviour is two batches on one rental and the defect is a second plan, so
// waiting only for the first would spend the whole deadline reporting a
// timeout for a failure that is already decided.
func TestRun_PacksOntoALiveRentalRatherThanCreating(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	enqueue(t, st, "rtx2080ti", "first")
	enqueue(t, st, "rtx2080ti", "second")

	stop := runLoop(t, q, p)
	defer stop()

	deadline := time.Now().Add(20 * time.Second)
	for {
		if n := p.batchesOn("rtx2080ti"); n >= 2 {
			break
		}
		if plans := p.plans(); len(plans) > 1 {
			t.Fatalf("two compatible requests rented %d nodes (%v): a request a live rental "+
				"can serve must never be given a node of its own, because the second node "+
				"pays the whole per-rental overhead again for work the first was about to do",
				len(plans), plans)
		}
		if time.Now().After(deadline) {
			t.Fatalf("the live rental generated %d batch(es) in 20s, want 2; plans=%v",
				p.batchesOn("rtx2080ti"), p.plans())
		}
		time.Sleep(2 * time.Millisecond)
	}

	if max, who := p.gate.peak(); max != 1 {
		t.Errorf("%d rentals were open at once (%v) for two requests on one card, want 1", max, who)
	}
	if plans := p.plans(); len(plans) != 1 {
		t.Errorf("planned %d rental(s) (%v) for two compatible requests, want 1", len(plans), plans)
	}
}

// TestRun_CreatesASecondNodeForWorkTheFirstCannotServe is the other half, and
// without it the packing assertion above is satisfied by a supervisor that
// never creates anything.
//
// THE DEMO IS AN sd REQUEST BESIDE A tts ONE, which is the honest one: `kind`
// is the Compat axis that cannot be two values on one node, so this is what
// max_nodes>1 actually buys. Two images for one card buy nothing, which is the
// test above.
func TestRun_CreatesASecondNodeForWorkTheFirstCannotServe(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	enqueueKind(t, st, genstore.KindSD, "rtx2080ti", "an image")
	enqueueKind(t, st, genstore.KindTTS, "rtx2080ti")

	stop := runLoop(t, q, p)
	defer stop()

	waitFor(t, 20*time.Second, "two rentals to be open at once for incompatible work",
		func() bool { return p.gate.now() == 2 })

	if plans := p.plans(); len(plans) != 2 {
		t.Errorf("planned %d rental(s) (%v), want 2", len(plans), plans)
	}
}

// TestRun_HoldsNoMoreRentalsThanMaxNodes is the ceiling that replaced the
// invariant.
//
// Without it the new failure mode is runaway scale-out: a queue of fifty
// mutually incompatible requests is fifty Create calls, each individually
// inside every money ceiling, reachable from the web UI and expensive while
// somebody is asleep.
//
// THE THIRD RENTAL IS WHAT MAKES THE BOUND AN ASSERTION. Two open and a third
// claimable is the state the bound is for: the test waits for both, releases
// ONE, and only then expects the third -- so a supervisor that ignored the
// ceiling has already recorded a peak of three by the time the final check
// runs, and one that honours it cannot reach three until a slot is free.
func TestRun_HoldsNoMoreRentalsThanMaxNodes(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	// Three mutually incompatible requests: three cards, so no rental can
	// ride another's.
	for _, gpu := range []string{"a", "b", "c"} {
		enqueue(t, st, gpu, "on "+gpu)
	}

	stop := runLoop(t, q, p)
	defer stop()

	waitFor(t, 20*time.Second, "the node ceiling to be reached",
		func() bool { return p.gate.now() == 2 })

	// The third must not have started. Asserted through the PLAN count as
	// well as the gate, because a rental that was planned and is waiting to
	// enter the gate is a create call that has already been made.
	if plans := p.plans(); len(plans) != 2 {
		t.Fatalf("planned %d rental(s) (%v) with max_nodes 2, so the ceiling is not bounding "+
			"anything", len(plans), plans)
	}

	// One slot comes free, and the third starts -- which is what stops the
	// assertion above being satisfied by a supervisor that creates one node
	// and stops.
	p.release(p.plans()[0])
	waitFor(t, 20*time.Second, "the third rental to start once a slot was free",
		func() bool { return len(p.plans()) == 3 })

	if max, who := p.gate.peak(); max != 2 {
		t.Errorf("%d rentals were open at once (%v) with max_nodes 2", max, who)
	}
}

// TestRun_AtOneNodeNeverHoldsTwoRentals, because max_nodes: 1 is a real mode
// and not a degenerate one.
//
// It is the closest thing left to the invariant #204 traded away, it is what a
// cautious operator sets, and it is what every existing test in this package
// and every lab assumes -- a `queue.Queue{}` literal has no MaxNodes, so a
// zero has to mean one. If the supervisor only worked at 2 the regression
// would be found by somebody's invoice.
//
// BOTH SPELLINGS, because they are the same code path and that is the claim:
// an explicit 1 and the zero every literal written before the field has.
func TestRun_AtOneNodeNeverHoldsTwoRentals(t *testing.T) {
	for _, maxNodes := range []int{1, 0} {
		t.Run(map[bool]string{true: "explicit", false: "zero"}[maxNodes == 1], func(t *testing.T) {
			p := newFleetPlanner()
			q, st := fleet(t, p, maxNodes)
			for _, gpu := range []string{"a", "b", "c"} {
				enqueue(t, st, gpu, "on "+gpu)
			}

			stop := runLoop(t, q, p)
			defer stop()

			waitFor(t, 20*time.Second, "the one rental to be open",
				func() bool { return p.gate.now() == 1 })

			// It serves them one at a time, which is the mode: release the
			// first and the second starts.
			if plans := p.plans(); len(plans) != 1 {
				t.Fatalf("max_nodes %d planned %d rental(s) (%v), want 1", maxNodes, len(plans), plans)
			}
			p.release(p.plans()[0])
			waitFor(t, 20*time.Second, "the second rental to start once the first ended",
				func() bool { return len(p.plans()) == 2 })

			if max, who := p.gate.peak(); max != 1 {
				t.Errorf("max_nodes %d held %d rentals at once (%v)", maxNodes, max, who)
			}
		})
	}
}

// TestRun_RegistersWhatASlotWillServeBeforeTheNextClaim is the defect the
// supervisor's ORDER exists to refuse, and the one `claimAndRun` structurally
// cannot stand in for.
//
// The claim and the compat registration both happen on the supervisor, before
// the slot goroutine exists. Register from INSIDE the slot instead and there
// is a window the width of a provider's create call -- seconds on RunPod -- in
// which a second compatible request is not yet excluded and gets a card of its
// own. That is runaway scale-out for a queue that should have used one node.
//
// WHY IT IS A DIFFERENT TEST FROM THE PACKING ONE ABOVE. There, the second
// request arrives at a rental that is already up, so the exclusion is read
// from a registration that has had all the time in the world. Here BOTH are
// queued before the supervisor starts, so the second claim happens
// immediately after the first spawn -- which is the interleaving the defect
// lives in, and the planner blocks in `Plan` to widen it.
func TestRun_RegistersWhatASlotWillServeBeforeTheNextClaim(t *testing.T) {
	p := newFleetPlanner()
	planning := make(chan struct{})
	q, st := fleet(t, p, 2)
	// A planner that parks the first Plan call, so the supervisor's second
	// claim certainly happens while the first rental is still provisioning.
	q.Planner = slowFirstPlan{p: p, once: &sync.Once{}, parked: planning}

	enqueue(t, st, "rtx2080ti", "first")
	enqueue(t, st, "rtx2080ti", "second")

	stop := runLoop(t, q, p)
	defer stop()

	waitFor(t, 20*time.Second, "the first plan to be in flight", func() bool {
		select {
		case <-planning:
			return true
		default:
			return false
		}
	})

	// The supervisor has spawned slot 1 and has looped. Give it a kick so the
	// claim it would make is made now rather than at the next poll, then wait
	// for a second plan that must never come.
	q.Kick()
	waitFor(t, 20*time.Second, "the supervisor to settle after being kicked", func() bool {
		return len(p.plans()) >= 1
	})

	// A second plan while the first is still provisioning is the defect, and
	// it is checked after the kick has had a chance to produce one. The
	// positive half -- that the second request IS served, by the rental that
	// is coming up -- is the packing test above.
	if plans := p.plans(); len(plans) != 1 {
		t.Errorf("planned %d rental(s) (%v) while the first was still provisioning: the compat "+
			"of a rental that is coming up has to be registered before the supervisor's next "+
			"claim, or a second compatible request provokes a second card", len(plans), plans)
	}
}

// slowFirstPlan parks the first Plan call so a test can observe the
// supervisor's next decision while a rental is still provisioning.
type slowFirstPlan struct {
	p      *fleetPlanner
	once   *sync.Once
	parked chan struct{}
}

func (s slowFirstPlan) Plan(ctx context.Context, r genstore.Request) (Rental, error) {
	s.once.Do(func() { close(s.parked) })
	return s.p.Plan(ctx, r)
}

// TestRun_DoesNotReturnUntilEverySlotsDestroyHasRun is the shutdown sentence,
// which used to be about one node.
//
// `ember serve` waits for `queue.Run`, and `queue.Run` now waits for a SET. A
// process that returned while a slot still held a node would exit with a GPU
// rented, billing by the hour with nobody watching -- the most expensive bug
// available here, and the reason the wait has no bound.
//
// IT CARRIES ITS OWN DEADLINE, because a wedge prints no `--- FAIL`: the thing
// being tested is a wait, so the failure mode of the bug's absence and the
// failure mode of the test hanging look identical to CI.
func TestRun_DoesNotReturnUntilEverySlotsDestroyHasRun(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	enqueueKind(t, st, genstore.KindSD, "rtx2080ti", "an image")
	enqueueKind(t, st, genstore.KindTTS, "rtx2080ti")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()

	waitFor(t, 20*time.Second, "both rentals to be open",
		func() bool { return p.gate.now() == 2 })

	// The shutdown lands with two nodes alive. Neither teardown can complete
	// until the hold is released, so a Run that returned here returned over
	// two live nodes.
	cancel()
	select {
	case <-done: //nolint:errcheck // the loop's own error is reported by its log; the subject is whether it returned
		t.Fatal("Run returned while two rentals were still open: ember serve waits for this " +
			"goroutine and nothing else, so it would have exited with two GPUs rented")
	case <-time.After(250 * time.Millisecond):
	}

	p.releaseAll()
	select {
	case <-done: //nolint:errcheck // as above
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after every rental was released")
	}
	if open := p.gate.now(); open != 0 {
		t.Errorf("%d rental(s) were still open when Run returned", open)
	}
}

// TestRunning_NamesEveryRequestGeneratingNow, which is the one singular this
// change makes visible on the wire.
//
// At max_nodes>1 several requests generate at once, one per rental, so a
// single id would mark one row in the browser's queue table and leave the
// others looking queued -- on the page an operator reads to see what their
// cards are doing.
//
// SORTED, which is asserted rather than assumed: the field keys a CSS class on
// a keyed table, and Go's map iteration order is deliberately random, so an
// unsorted answer would churn the table between two polls for nothing.
func TestRunning_NamesEveryRequestGeneratingNow(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	// Two incompatible requests, so two rentals; each one's Generate blocks
	// until both are inside it, which is the only state this can be asked in.
	a := enqueueKind(t, st, genstore.KindSD, "rtx2080ti", "an image")
	b := enqueueKind(t, st, genstore.KindTTS, "rtx2080ti")

	both := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	var inside int
	var mu sync.Mutex
	// HELD INSIDE Generate UNTIL THE TEST HAS LOOKED, which is the only state
	// this question can be asked in: `current` is registered by generate and
	// deleted by its defer, so a hold that released as soon as both had
	// arrived is a race against the read. The first version of this test lost
	// it and reported an empty list over two correct registrations.
	gen := func() {
		mu.Lock()
		inside++
		n := inside
		mu.Unlock()
		if n == 2 {
			once.Do(func() { close(both) })
		}
		select {
		case <-proceed:
		case <-time.After(20 * time.Second):
		}
	}
	q.Planner = generatingPlanner{p: p, inGenerate: gen}

	stop := runLoop(t, q, p)
	defer stop()

	waitFor(t, 20*time.Second, "both requests to be generating at once", func() bool {
		select {
		case <-both:
			return true
		default:
			return false
		}
	})

	got := q.Running()
	close(proceed)
	want := []genstore.ReqID{a, b}
	if a > b {
		want = []genstore.ReqID{b, a}
	}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Running() = %v, want %v sorted: a single id would mark one generating row "+
			"and leave the other looking queued", got, want)
	}
}

// generatingPlanner holds each rental INSIDE its generation, which is where
// `current` is registered.
type generatingPlanner struct {
	p          *fleetPlanner
	inGenerate func()
}

func (g generatingPlanner) Plan(_ context.Context, r genstore.Request) (Rental, error) {
	return generatingRental{p: g.p, gpu: r.GPU, in: g.inGenerate, hold: g.p.hold(r.GPU)}, nil
}

type generatingRental struct {
	p    *fleetPlanner
	gpu  string
	in   func()
	hold chan struct{}
}

func (r generatingRental) WithServed(ctx context.Context, sink report.Sink, fn func(context.Context, app.Served) error) error {
	return fleetRental{p: r.p, gpu: r.gpu, hold: r.hold}.rent(ctx, sink, func(ctx context.Context) error {
		return fn(ctx, &blockingServed{in: r.in})
	})
}

func (r generatingRental) WithSpoken(ctx context.Context, sink report.Sink, fn func(context.Context, app.Spoken) error) error {
	return fleetRental{p: r.p, gpu: r.gpu, hold: r.hold}.rent(ctx, sink, func(ctx context.Context) error {
		return fn(ctx, blockingSpoken{in: r.in})
	})
}

type blockingServed struct{ in func() }

func (b *blockingServed) Caps() sdserver.Capabilities {
	return sdserver.Capabilities{Model: "sd15"}
}

func (b *blockingServed) Generate(_ context.Context, _ report.Sink, _ sdserver.Run, _ string, _ <-chan struct{}) error {
	b.in()
	return nil
}

type blockingSpoken struct{ in func() }

func (b blockingSpoken) Say(_ context.Context, _ report.Sink, _ tts.Run, _ string, _ <-chan struct{}) error {
	b.in()
	return nil
}

// TestHolding_ASlotHandsTheEntryOverRatherThanAccumulating.
//
// `held` is a SET across slots now, where it used to be one value the next
// claim overwrote. A rental serves its requests in turn, so a rider that
// simply ADDED to the set would leave every finished request on that rental
// reported as held for the life of the node -- and `Holding` is what webapi
// reads to choose between two 409s whose remediations are "wait, the rental is
// coming up" and "restart the server that is renting". A finished request
// answering the first would tell an operator to wait for a generation that
// ended.
//
// It is harmless TODAY, because that choice is only made for a row that says
// `running` and a finished row does not -- which is exactly the kind of
// harmless that stops being so when somebody reads the field's name instead of
// the paragraph beside it. The `holder` type is what makes one-entry-per-slot
// structural, and this is its exit code.
func TestHolding_ASlotHandsTheEntryOverRatherThanAccumulating(t *testing.T) {
	var held []bool
	served := &fakeServed{caps: sdserver.Capabilities{Model: "sd15"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	a := enqueue(t, st, "rtx2080ti", "first")
	b := enqueue(t, st, "rtx2080ti", "second")

	// Sampled from inside each batch, where exactly one of the two is the
	// request this slot is responsible for.
	served.progress = func(report.Sink, int) {
		held = append(held, q.Holding(a), q.Holding(b))
	}

	if _, err := q.claimAndRun(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(held) != 4 {
		t.Fatalf("sampled %d holdings over two batches, want 4", len(held))
	}
	if !held[0] || held[1] {
		t.Errorf("during the first batch Holding(a)=%v Holding(b)=%v, want true and false",
			held[0], held[1])
	}
	if held[2] || !held[3] {
		t.Errorf("during the second batch Holding(a)=%v Holding(b)=%v, want false and true: the "+
			"slot has to hand the entry over as it takes the next rider, or a finished request "+
			"is answered with \"the rental is coming up\" for the life of the node",
			held[2], held[3])
	}
	// AND BOTH ARE GIVEN BACK, which is the other end of the same rule.
	if q.Holding(a) || q.Holding(b) {
		t.Error("a request was still held after the rental ended, which makes the restart " +
			"advice unreachable for the state it IS true of")
	}
}

// TestRun_EachSlotsEventsLandOnItsOwnRequest, which `queue.sinks` makes true
// by construction and which nothing asserted.
//
// WHY IT NEEDED A TEST AT ALL. `sinks` is a map keyed by request id, so two
// slots emitting at once get two streams and the property holds without
// anybody arranging it -- which is exactly the kind of "holds by construction"
// this repository has been wrong about before. What makes it worth an exit
// code is what depends on it: `TestServe_UpOverTheSocket` asserts that a
// DETACHED operator's only carrier -- the node's own bytes, through
// `report.Writer` onto their request's event stream -- reaches them. At one
// rental there was one stream in flight and no way to cross them; at N there
// are N, and the failure would be an operator reading somebody else's node's
// output as their own, with their own missing.
//
// THE SEQUENCE NUMBERS ARE THE OTHER HALF. Two numberings for one request is
// what `sinks` is memoised against -- `genstore`'s INSERT OR REPLACE on
// `(request, seq)` makes each event overwrite the last, which once left a
// whole generation as one row -- so the assertion is that each stream is
// numbered from 1 with no gaps and no repeats, which a shared counter across
// two slots would break.
func TestRun_EachSlotsEventsLandOnItsOwnRequest(t *testing.T) {
	p := newFleetPlanner()
	q, st := fleet(t, p, 2)
	image := enqueueKind(t, st, genstore.KindSD, "rtx2080ti", "an image")
	speech := enqueueKind(t, st, genstore.KindTTS, "rtx2080ti")

	// Each rental emits a line naming the request it is for, which is what
	// `embernode fetch`'s stdout is on the real path: bytes out of the
	// container, onto this request's stream and no other.
	q.Planner = loggingPlanner{p: p}

	stop := runLoop(t, q, p)
	waitFor(t, 20*time.Second, "both rentals to be open",
		func() bool { return p.gate.now() == 2 })
	stop()

	for _, c := range []struct{ id, want, other genstore.ReqID }{
		{image, image, speech},
		{speech, speech, image},
	} {
		es, err := st.Events(t.Context(), c.id, 0)
		if err != nil {
			t.Fatal(err)
		}
		var text string
		for _, e := range es {
			if e.Kind == report.KindLog {
				text += e.Text
			}
		}
		if !strings.Contains(text, "for "+string(c.want)) {
			t.Errorf("request %s has none of its own node's output, so a detached operator's "+
				"only carrier is empty: %q", c.id, text)
		}
		if strings.Contains(text, "for "+string(c.other)) {
			t.Errorf("request %s carries the OTHER rental's node output, so an operator reads "+
				"somebody else's machine as their own: %q", c.id, text)
		}
		// Numbered from one, with no gaps and no repeats: a counter shared
		// across two slots would make each event overwrite the last.
		for i, e := range es {
			if e.Seq != int64(i+1) {
				t.Errorf("request %s event %d has seq %d, so the two slots are not numbering "+
					"their streams independently", c.id, i, e.Seq)
				break
			}
		}
	}
}

// loggingPlanner is the fleet planner with each rental emitting a line that
// names the request it is serving.
type loggingPlanner struct{ p *fleetPlanner }

func (l loggingPlanner) Plan(_ context.Context, r genstore.Request) (Rental, error) {
	return loggingRental{p: l.p, gpu: r.GPU, req: r.ID, hold: l.p.hold(r.GPU)}, nil
}

type loggingRental struct {
	p    *fleetPlanner
	gpu  string
	req  genstore.ReqID
	hold chan struct{}
}

func (r loggingRental) WithServed(ctx context.Context, sink report.Sink, fn func(context.Context, app.Served) error) error {
	sink.Emit(report.Event{Kind: report.KindLog, Text: "bytes for " + string(r.req) + "\n"})
	return fleetRental{p: r.p, gpu: r.gpu, hold: r.hold}.WithServed(ctx, sink, fn)
}

func (r loggingRental) WithSpoken(ctx context.Context, sink report.Sink, fn func(context.Context, app.Spoken) error) error {
	sink.Emit(report.Event{Kind: report.KindLog, Text: "bytes for " + string(r.req) + "\n"})
	return fleetRental{p: r.p, gpu: r.gpu, hold: r.hold}.WithSpoken(ctx, sink, fn)
}
