package queue

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// rentalGate counts rentals that are open RIGHT NOW, across both kinds.
//
// THE MAXIMUM IS THE ASSERTION. A queued request's rental and a submitted
// run's go through the same counter, so "two rentals at once" is a number
// rather than an argument -- and the number is what a mechanism built out of a
// boolean would get wrong under exactly the interleaving a boolean cannot see.
type rentalGate struct {
	mu   sync.Mutex
	open int
	max  int
	in   []string
}

func (g *rentalGate) enter(who string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open++
	g.in = append(g.in, who)
	if g.open > g.max {
		g.max = g.open
	}
}

func (g *rentalGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.open--
}

func (g *rentalGate) now() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.open
}

func (g *rentalGate) peak() (int, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.max, append([]string(nil), g.in...)
}

// gatedPlanner rents through the gate and holds the rental open until hold is
// closed.
//
// The hold is AFTER the batch, where provision.WithNode's deferred destroy is:
// the request finishes and the node is still alive, which is the window a
// second rental would be born in.
type gatedPlanner struct {
	gate *rentalGate
	hold chan struct{}

	// order records each rental this planner opens, so a test can compare the
	// sequence against what a submitted run recorded.
	//
	// IT IS WRITTEN BY BOTH SIDES OR IT SAYS NOTHING. The first version of
	// TestSubmit_TakesASubmittedRunBeforeAClaim had only the submitted run
	// writing here, so the channel held exactly one value whichever order the
	// loop chose -- and the assertion passed against a loop that reads the
	// handoff only when idle, which is the defect it was written for. The
	// expected value was what the bug produces, which is the trap AGENTS.md
	// names.
	order chan string
}

func (p gatedPlanner) Plan(context.Context, genstore.Request) (Rental, error) {
	return gatedRental(p), nil
}

type gatedRental struct {
	gate  *rentalGate
	hold  chan struct{}
	order chan string
}

// WithSpoken mirrors WithServed through the same gate.
//
// A MIRROR AND NOT A REFUSAL, although no test here drives speech through it:
// a fake stricter than the thing it stands in for grows its callers a branch
// for an error nothing returns, which AGENTS.md records finding in
// `genstore.RecordingStore.JobRuns` -- and the gate is what every assertion in
// this file is about, so a second method that skipped it would be a rental the
// one-at-a-time property cannot see.
func (r gatedRental) WithSpoken(ctx context.Context, sink report.Sink, fn func(context.Context, app.Spoken) error) error {
	r.gate.enter("queued")
	defer r.gate.leave()
	if r.order != nil {
		r.order <- "queued"
	}
	sink.Emit(report.Event{Kind: report.KindNodeReady, Node: "local/n1"})
	return fn(ctx, recordingSpoken{})
}

// Card is a fixed card, because every request in this file names one provider
// and the gate is what the tests are about.
func (r gatedRental) Card() node.GPUModel { return "rtx2080ti" }

func (r gatedRental) WithServed(ctx context.Context, sink report.Sink, fn func(context.Context, app.Served) error) error {
	r.gate.enter("queued")
	defer r.gate.leave()
	if r.order != nil {
		r.order <- "queued"
	}
	sink.Emit(report.Event{Kind: report.KindNodeReady, Node: "local/n1"})
	err := fn(ctx, &fakeServed{caps: sdserver.Capabilities{Model: "sd15"}})
	if r.hold != nil {
		select {
		case <-r.hold:
		case <-time.After(10 * time.Second):
		}
	}
	return err
}

// waitFor spins until want is true, or fails.
//
// A bound rather than a sleep: what is being waited for is a goroutine
// reaching a line, and a fixed sleep is either slower than it needs to be or
// a flake.
func waitFor(t *testing.T, within time.Duration, what string, want func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if want() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for %s", within, what)
}

// TestSubmit_ARunAndAQueuedRequestCannotHoldRentalsAtOnce is the invariant
// #146 turns on, as three assertions rather than one.
//
// WHY THREE. "One rental is structural" is a claim about a mechanism, and this
// repository has been wrong about those -- the trust-on-first-use regression
// tests asserted on a string no code logs. So the three are chosen for what
// each one rules out:
//
//  1. Submit DOES NOT RETURN while a queued rental is in flight. A mechanism
//     that refused instead -- a 409, the obvious answer -- returns an error
//     immediately, and this asserts the error is a DEADLINE rather than a
//     refusal, so a refusing implementation is red.
//  2. THE MAXIMUM NUMBER OF OPEN RENTALS IS ONE, counted by the thing that
//     opens them. A mechanism that started the run beside the loop is red
//     whether its locking was right or not, because with the queued rental
//     deliberately held open the two overlap.
//  3. The run RUNS once the queued rental has ended, which is what stops 1
//     and 2 being satisfied by a mechanism that simply never runs anything.
//     A refusal alone is consistent with refusing everything, which is the
//     zero-Pin lesson.
//
// WHAT IT CANNOT SEE, said rather than implied: it proves nothing about code
// that does not go through this Queue at all. A handler that built a plan and
// called Plan.Do itself would be a second place a rental can start and would
// not appear here -- which is why cmd/check-coverage's
// TestNothingRentsOutsideInternalApp bounds the other half by asking which
// packages may name provision.WithNode.
func TestSubmit_ARunAndAQueuedRequestCannotHoldRentalsAtOnce(t *testing.T) {
	gate := &rentalGate{}
	hold := make(chan struct{})
	st := genstore.NewRecordingStore()
	q := &Queue{
		Store:   st,
		Planner: gatedPlanner{gate: gate, hold: hold},
		Outputs: filepath.Join(t.TempDir(), "outputs"),
	}
	enqueue(t, st, "rtx2080ti", "a queued batch")

	ctx, cancel := context.WithCancel(context.Background())
	loop := make(chan error, 1)
	go func() { loop <- q.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-loop: //nolint:errcheck // Run's error is the loop's own, and the tests above read the row
		case <-time.After(15 * time.Second):
			t.Error("the drain loop did not return")
		}
	})

	waitFor(t, 10*time.Second, "the queued request's rental to be open",
		func() bool { return gate.now() == 1 })

	// 1. The submission is offered while that rental is open, with a deadline
	// of its own. It must be the deadline that ends the wait and not a
	// refusal: queueing it would be a backlog `up` does not mean, and
	// refusing it would be a message rather than a wait.
	ranEarly := make(chan struct{})
	early, cancelEarly := context.WithTimeout(context.Background(), 300*time.Millisecond)
	err := q.Submit(early, Direct{Req: "early", Run: func(context.Context) error {
		gate.enter("submitted-early")
		defer gate.leave()
		close(ranEarly)
		return nil
	}})
	cancelEarly()
	switch {
	case err == nil:
		t.Fatal("Submit was taken while a queued request's rental was still open; " +
			"that is the second GPU this whole shape exists to make unrepresentable")
	case !errors.Is(err, context.DeadlineExceeded):
		t.Fatalf("Submit ended with %v, want the caller's own deadline: a refusal here would "+
			"mean the loop had answered rather than been busy", err)
	}
	select {
	case <-ranEarly:
		t.Fatal("the submitted run executed while a queued rental was open")
	default:
	}

	// 2 and 3. The queued rental ends, and the same submission is taken and
	// runs.
	close(hold)
	ran := make(chan struct{})
	late, cancelLate := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelLate()
	if err := q.Submit(late, Direct{Req: "late", Run: func(context.Context) error {
		gate.enter("submitted-late")
		defer gate.leave()
		close(ran)
		return nil
	}}); err != nil {
		t.Fatalf("Submit after the rental ended: %v", err)
	}
	select {
	case <-ran:
	case <-time.After(15 * time.Second):
		t.Fatal("the drain loop took the run and never ran it")
	}

	if max, who := gate.peak(); max != 1 {
		t.Errorf("%d rentals were open at once (%v), want 1", max, who)
	}
}

// TestSubmit_TheRunExecutesOnASlotsOwnGoroutine is the assertion that
// goes red for a CHECK where a structure would be green.
//
// The three above are satisfied by any mechanism that serialises correctly,
// including the one this repository refuses: a goroutine per submission with a
// mutex or a condition variable saying no to the second. That is a counter
// with better manners, and "a counter that can be wrong costs two GPUs" is the
// recorded reason it is not the answer.
//
// So this asserts the structure itself: the run's own goroutine stack contains
// (*Queue).Run, because the loop calls it inline, in the same iteration a
// claimed request's rental lives in. Move the run beside the loop and the
// frame is gone however correct the locking is.
//
// THE HONEST LIMIT. A stack is evidence about one goroutine, so this says the
// run executed on the loop's and not that nothing else in the program can
// rent. cmd/check-coverage's TestNothingRentsOutsideInternalApp is the other
// half, and the two together are what "structural" means here.
func TestSubmit_TheRunExecutesOnASlotsOwnGoroutine(t *testing.T) {
	st := genstore.NewRecordingStore()
	q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}

	ctx, cancel := context.WithCancel(context.Background())
	loop := make(chan error, 1)
	go func() { loop <- q.Run(ctx) }()
	defer func() {
		cancel()
		<-loop //nolint:errcheck // waiting for the destroy, not for an error
	}()

	// Handed over on a channel rather than written to a variable the test
	// reads: two goroutines and one string is a data race, which -race
	// reports and which would be a flake rather than a finding.
	stacks := make(chan string, 1)
	sub, cancelSub := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancelSub()
	if err := q.Submit(sub, Direct{Req: "onloop", Run: func(context.Context) error {
		buf := make([]byte, 32<<10)
		stacks <- string(buf[:runtime.Stack(buf, false)])
		return nil
	}}); err != nil {
		t.Fatal(err)
	}

	var stack string
	select {
	case stack = <-stacks:
	case <-time.After(15 * time.Second):
		t.Fatal("the run never executed")
	}

	// THE SUBJECT IS `slot` AND NOT `Run`, AND THE OLD SPELLING PASSED THE
	// MOMENT IT STOPPED MEANING ANYTHING. This looked for
	// `internal/queue.(*Queue).Run`, which was the frame while the loop ran
	// the rental inline. Under the supervisor the rental is on a slot
	// goroutine -- and the assertion stayed GREEN, because the closure the
	// supervisor hands to `spawn` is declared inside `Run` and is therefore
	// named `internal/queue.(*Queue).Run.func1`: the old substring matched a
	// closure's NAME rather than a caller's frame. Measured, which is the only
	// reason this comment exists -- the first run of the supervisor against
	// the unchanged assertion passed.
	//
	// `(*Queue).slot` is the frame that cannot be faked that way. It is a
	// method the supervisor calls through `spawn`, so it is in the stack of
	// every rental this process counted and in the stack of nothing else -- a
	// run on a goroutine somebody started beside the supervisor has no `slot`
	// frame however correct its locking is, which is the mechanism this
	// assertion exists to refuse.
	const frame = "internal/queue.(*Queue).slot"
	if !strings.Contains(stack, frame) {
		t.Errorf("the submitted run did not execute on a rental slot: %s is not in its stack, "+
			"so a rental can now start somewhere other than the one place that counts "+
			"them.\n%s", frame, stack)
	}
}

// TestSubmit_TakesASubmittedRunBeforeAClaim.
//
// A submission that has been waiting for a rental to end must not be overtaken
// by a request queued while it waited: it is a command somebody typed and the
// queue is a backlog. The loop reads the handoff before ClaimNext for that
// reason, and this is the exit code behind the comment that says so.
func TestSubmit_TakesASubmittedRunBeforeAClaim(t *testing.T) {
	gate := &rentalGate{}
	hold := make(chan struct{})
	// BOTH SIDES WRITE HERE OR THE SEQUENCE SAYS NOTHING. The first version of
	// this test had only the submitted run recording, so the channel held one
	// value whichever order the loop chose and it passed against the very
	// defect it names -- the expected value was what the bug produces.
	order := make(chan string, 8)
	st := genstore.NewRecordingStore()
	q := &Queue{
		Store:   st,
		Planner: gatedPlanner{gate: gate, hold: hold, order: order},
		Outputs: t.TempDir(),
	}
	enqueue(t, st, "rtx2080ti", "the first batch")

	ctx, cancel := context.WithCancel(context.Background())
	loop := make(chan error, 1)
	go func() { loop <- q.Run(ctx) }()
	defer func() {
		cancel()
		<-loop //nolint:errcheck // waiting for the destroy, not for an error
	}()

	waitFor(t, 10*time.Second, "the first rental", func() bool { return gate.now() == 1 })
	if got := <-order; got != "queued" {
		t.Fatalf("the first rental recorded %q", got)
	}

	// Parked on the handoff while the rental is held, from its own goroutine,
	// so the test can queue a second request behind it.
	submitted := make(chan error, 1)
	go func() {
		submitted <- q.Submit(ctx, Direct{Req: "typed", Run: func(context.Context) error {
			order <- "submitted"
			return nil
		}})
	}()
	// Long enough for the send above to be parked. If it has not parked, the
	// claim below wins and the sequence assertion reports it rather than the
	// test passing for the wrong reason.
	time.Sleep(100 * time.Millisecond)
	enqueue(t, st, "rtx2080ti", "queued while it waited")

	close(hold)
	if err := <-submitted; err != nil {
		t.Fatalf("Submit: %v", err)
	}

	// TWO MORE, IN ORDER: the submission that was already waiting, then the
	// request queued behind it. A loop that reads the handoff only when idle
	// claims first and this reads "queued" here.
	var got []string
	for range 2 {
		select {
		case s := <-order:
			got = append(got, s)
		case <-time.After(15 * time.Second):
			t.Fatalf("only %v ran after the first rental ended", got)
		}
	}
	if got[0] != "submitted" || got[1] != "queued" {
		t.Errorf("the loop ran %v after the first rental, want [submitted queued]: a command "+
			"somebody typed must not be overtaken by a request queued while it waited", got)
	}
}

// TestSubmit_RefusesARunNobodyFilledIn, rather than panicking in the drain
// goroutine.
//
// AppPlanner.out()'s reason: a nil dereference there unwinds past
// provision.WithNode's deferred destroy with a node running, which is the one
// failure this package exists to prevent.
func TestSubmit_RefusesARunNobodyFilledIn(t *testing.T) {
	q := &Queue{Store: genstore.NewRecordingStore()}
	for _, d := range []Direct{
		{},
		{Req: "a"},
		{Run: func(context.Context) error { return nil }},
	} {
		if err := q.Submit(context.Background(), d); err == nil {
			t.Errorf("Submit accepted %+v", d)
		}
	}
}

// TestDirect_HoldsTheRequestWhileItRuns.
//
// The row has read `running` since Adopt wrote it, so a cancel arriving now
// must be answered by a queue that knows this process holds the request --
// which is the difference between webapi's two 409s. Cleared when the run
// returns, which is after the teardown.
func TestDirect_HoldsTheRequestWhileItRuns(t *testing.T) {
	q := &Queue{Store: genstore.NewRecordingStore()}
	const id genstore.ReqID = "held"

	seen := make(chan bool, 1)
	err := q.direct(context.Background(), Direct{Req: id, Run: func(context.Context) error {
		seen <- q.Holding(id)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !<-seen {
		t.Error("the queue did not report holding the request while its run was in flight")
	}
	if q.Holding(id) {
		t.Error("the queue still holds the request after the run returned")
	}
}

// TestDirect_ReportsWhatTheRunReturned, so queue.Run can log it rather than
// swallowing it.
func TestDirect_ReportsWhatTheRunReturned(t *testing.T) {
	q := &Queue{Store: genstore.NewRecordingStore()}
	boom := errors.New("the card aborted")
	err := q.direct(context.Background(), Direct{Req: "r", Run: func(context.Context) error { return boom }})
	if !errors.Is(err, boom) {
		t.Errorf("direct returned %v, want it to wrap %v", err, boom)
	}
	if !strings.Contains(err.Error(), "r") {
		t.Errorf("the error should name the request, got %v", err)
	}
}

// --- DirectRunner ---

// directDeps is a runner that can plan against the local provider and rents
// nothing, because the Docker engine is a recording fake.
func directRunner(t *testing.T, q *Queue, st genstore.Store) DirectRunner {
	r, _ := directRunnerEngine(t, q, st)
	return r
}

// directRunnerEngine hands back the Docker fake as well, for the tests that
// have to make a step of the rental fail in order to observe an event.
func directRunnerEngine(t *testing.T, q *Queue, st genstore.Store) (DirectRunner, *dockerengine.RecordingEngine) {
	t.Helper()
	e := dockerengine.NewRecordingEngine()
	e.AddImage(local.DefaultImage)
	e.AddImage(runImage)
	return DirectRunner{
		Deps: app.Deps{
			// Credentials are injected EMPTY rather than resolved, for
			// internal/cli's labDeps reason: a machine with RUNPOD_API_KEY set
			// would otherwise register the real provider and these tests would
			// make live API calls.
			Engine: e, HomeDir: t.TempDir(), Creds: &config.Credentials{},
			// Injected for the reason internal/cli's labDeps injects it: a
			// developer's own card would make the offer -- and therefore the
			// recorded GPU column -- differ from one desk to the next.
			DetectGPU: func(context.Context) (local.GPU, bool) { return local.GPU{}, true },
		},
		Store: st, Queue: q,
	}, e
}

// writeRunConfig puts a config an `ember up` would run on disk and answers
// with its absolute path, which is what the channel requires.
func writeRunConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ember.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

const (
	runImage       = "ghcr.io/example/wl:v1"
	localRunConfig = "provider: local\nimage: " + runImage + "\nttl: 10m\n"
)

// TestDirectRunner_StartAdoptsTheRowAndHandsTheRunToTheLoop.
//
// The two halves that make this `ember up` rather than an enqueue: the row is
// StatusRunning, which no ClaimNext will take -- an enqueued one would be
// claimed by this very loop and generate the batch a second time -- and the
// run goes to the drain goroutine rather than starting anywhere else.
//
// NO DRAIN LOOP RUNS HERE, AND THAT IS WHAT MAKES IT DETERMINISTIC. The first
// version started one, which meant the rental could fail against the recording
// Docker engine and write a terminal status BEFORE the assertion read the row
// -- an intermittent failure caught by the control of a reinstatement pass
// rather than by any run of the suite, which is the only reason it was found at
// all. A flaky assertion is worse than none: it teaches people to re-run
// rather than to read, which this repository already paid for when two lab
// suites destroyed each other's nodes.
//
// With nobody receiving, Submit parks on the rendezvous and the row stays in
// exactly the state Adopt wrote it in -- so the parking is asserted too, which
// is a property the old shape could not see at all. The handoff's other half,
// that the loop takes it and runs it, is TestSubmit_* and
// TestAdoptRun_SendsEveryEventToTheExtraSinksAndToTheRow.
func TestDirectRunner_StartAdoptsTheRowAndHandsTheRunToTheLoop(t *testing.T) {
	st := genstore.NewRecordingStore()
	q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
	r := directRunner(t, q, st)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan error, 1)
	go func() {
		_, err := r.Start(ctx, app.RunAsk{Config: writeRunConfig(t, localRunConfig)})
		started <- err
	}()

	var row genstore.Request
	waitFor(t, 15*time.Second, "the run's row to be adopted", func() bool {
		rows, err := st.List(context.Background(), genstore.Filter{})
		if err != nil || len(rows) != 1 {
			return false
		}
		row = rows[0]
		return true
	})

	if row.Status != genstore.StatusRunning {
		t.Errorf("the adopted row is %q, want running: a queued row is exactly what ClaimNext "+
			"takes, and a second claim of this run is a second card", row.Status)
	}
	if row.Provider != "local" {
		t.Errorf("provider = %q, want local", row.Provider)
	}
	if row.TTL != 10*time.Minute {
		t.Errorf("ttl = %v, want the config's 10m", row.TTL)
	}
	if len(row.Jobs) != 0 {
		t.Errorf("a `cmd` run recorded %d job row(s), want none: a step is not a job", len(row.Jobs))
	}

	// AND IT IS NOT CLAIMABLE, which is the assertion the status is a proxy
	// for: Adopt's whole reason is that a queued row would be taken.
	if _, ok, cerr := st.ClaimNext(context.Background(), "lease", genstore.Compat{}); cerr != nil || ok {
		t.Errorf("ClaimNext took the adopted row (ok=%v, err=%v)", ok, cerr)
	}

	// AND IT IS STILL PARKED: a Submit that something had taken would have
	// returned by now, and the row would be whatever the run left.
	select {
	case err := <-started:
		t.Fatalf("Start returned with no drain loop to take the run (%v), so a rental can "+
			"begin somewhere other than the one place that serialises them", err)
	default:
	}

	// AND AN ABANDONED SUBMISSION ENDS THE ROW rather than leaving it reading
	// `running`. Nothing was rented, and a phantom row that only a restart
	// clears is a defect this repository has already paid for once.
	cancel()
	if err := <-started; err == nil {
		t.Error("Start reported success after its caller went away")
	}
	waitFor(t, 15*time.Second, "the abandoned row to be ended", func() bool {
		got, err := st.Get(context.Background(), row.ID)
		return err == nil && got.Status.Terminal()
	})
}

// TestDirectRunner_StartReplacesTheConfigsCeilingRatherThanNarrowingIt.
//
// THE MONEY HALF OF THE WHOLE CHANGE. `ember up --max-hourly-usd 999` raises
// the ceiling because the operator typing it is the operator who set it, and
// queue.Narrow takes the SMALLER of the two because a queued request is not
// that person. This channel is the first thing that is `up` and arrives over a
// socket, so getting it wrong would be the one rule in this repository where
// two opposite answers are both written down.
//
// The recorded ceiling is the assertion rather than the config value, because
// that is the number that bounded the rental: AdoptRun reads it off the
// resolved Options, so a Narrow on this path would show up here as the
// config's 0.10 instead of the flag's 999.
func TestDirectRunner_StartReplacesTheConfigsCeilingRatherThanNarrowingIt(t *testing.T) {
	st := genstore.NewRecordingStore()
	q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
	r := directRunner(t, q, st)

	ctx, cancel := context.WithCancel(context.Background())
	loop := make(chan error, 1)
	go func() { loop <- q.Run(ctx) }()
	defer func() {
		cancel()
		<-loop //nolint:errcheck // waiting for the destroy, not for an error
	}()

	cfg := writeRunConfig(t, localRunConfig+"max_hourly_usd: \"0.10\"\n")
	id, err := r.Start(ctx, app.RunAsk{
		Config:    cfg,
		Overrides: app.Overrides{MaxHourlyUSD: "999"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	row, err := st.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	want := int64(999_000_000)
	if int64(row.MaxHourly) != want {
		t.Errorf("the run was bounded at %v (%d micro-USD/hr), want the flag's $999 (%d): "+
			"a flag REPLACES on this path, and narrowing it against the file would make "+
			"--max-hourly-usd mean the opposite of what it means in a shell",
			row.MaxHourly, int64(row.MaxHourly), want)
	}
}

// TestDirectRunner_StartRefusesWhatItCannotRun, before a row exists.
//
// Every one of these is a refusal `ember up` makes itself, and every one is
// free: nothing has been rented and -- for the first three -- nothing has been
// written either, so a client gets the operator's own sentence as a 400 rather
// than having to read a database to find out.
func TestDirectRunner_StartRefusesWhatItCannotRun(t *testing.T) {
	st := genstore.NewRecordingStore()
	q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}

	cases := []struct {
		name   string
		runner func() DirectRunner
		ask    app.RunAsk
		want   string
	}{
		{
			name:   "no rental loop",
			runner: func() DirectRunner { r := directRunner(t, q, st); r.Queue = nil; return r },
			ask:    app.RunAsk{Config: writeRunConfig(t, localRunConfig)},
			want:   "rental loop",
		},
		{
			name:   "no history",
			runner: func() DirectRunner { r := directRunner(t, q, st); r.Store = nil; return r },
			ask:    app.RunAsk{Config: writeRunConfig(t, localRunConfig)},
			want:   "generation history",
		},
		{
			name:   "a relative config path",
			runner: func() DirectRunner { return directRunner(t, q, st) },
			ask:    app.RunAsk{Config: "ember.yaml"},
			want:   "relative",
		},
		{
			// A CONFIG THAT IS NOT THERE IS NOT AN ERROR, and that is
			// config.Load's own decision rather than this channel's: `ember up
			// -image x` works in a directory with no ember.yaml, so an absent
			// file is the defaults. What refuses is therefore the defaults
			// being incomplete, which is the same sentence the shell path
			// gives -- asserted here because the obvious expectation is the
			// other one and this is the answer an operator gets.
			name:   "a config that is not there, so the defaults name no image",
			runner: func() DirectRunner { return directRunner(t, q, st) },
			ask:    app.RunAsk{Config: filepath.Join(t.TempDir(), "absent.yaml")},
			want:   "no image",
		},
		{
			name:   "an unpinned image",
			runner: func() DirectRunner { return directRunner(t, q, st) },
			ask:    app.RunAsk{Config: writeRunConfig(t, "provider: local\nimage: wl:latest\nttl: 10m\n")},
			want:   "latest",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			id, err := c.runner().Start(context.Background(), c.ask)
			if err == nil {
				t.Fatalf("Start accepted it as %s", id)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("the refusal should name %q, got %v", c.want, err)
			}
			if id != "" {
				t.Errorf("a refused run was given the id %s", id)
			}
		})
	}
}

// TestDirectRunner_StartRefusesAPaidProviderWithoutAllowPaid.
//
// THE ONE PLACE THIS CHANNEL IS NOT SIMPLY "THE OPERATOR'S OWN SHELL".
// Everything else about it rests on connect(2) having proved who is asking, so
// the config path travels and the ceilings replace -- and --allow-paid is
// different because it is a property of the long-running process rather than
// of the request. An `ember serve` left running without it is one the operator
// has said must not spend, and a door that spent anyway would make the flag
// mean "unless you came in the other way".
// It is ASSERTED IN BOTH DIRECTIONS, because a refusal alone is consistent
// with a runner that refuses everything -- the zero-Pin lesson -- and the
// direction that would cost money is the accept.
func TestDirectRunner_StartRefusesAPaidProviderWithoutAllowPaid(t *testing.T) {
	st := genstore.NewRecordingStore()
	q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}

	cfg := writeRunConfig(t, paidRunConfig)
	ctx, cancel := context.WithCancel(context.Background())
	loop := make(chan error, 1)
	go func() { loop <- q.Run(ctx) }()
	defer func() {
		cancel()
		<-loop //nolint:errcheck // waiting for the destroy, not for an error
	}()

	refusing := paidRunner(t, q, st)
	_, err := refusing.Start(ctx, app.RunAsk{Config: cfg})
	if err == nil {
		t.Fatal("a paid provider was accepted by a runner started without --allow-paid")
	}
	if !strings.Contains(err.Error(), "--allow-paid") {
		t.Errorf("the refusal should name the flag, got %v", err)
	}
	// And it is refused BEFORE a row exists, so a refused submission leaves no
	// phantom in the ledger.
	rows, lerr := st.List(ctx, genstore.Filter{})
	if lerr != nil {
		t.Fatal(lerr)
	}
	if len(rows) != 0 {
		t.Errorf("a refused run wrote %d row(s) to the history", len(rows))
	}

	allowing := paidRunner(t, q, st)
	allowing.AllowPaid = true
	if _, err := allowing.Start(ctx, app.RunAsk{Config: cfg}); err != nil {
		t.Fatalf("the same ask was refused with --allow-paid, so the gate refuses everything "+
			"rather than refusing a paid provider: %v", err)
	}
}

// paidRunConfig names a provider that costs money.
const paidRunConfig = "provider: runpod\nimage: " + runImage + "\nttl: 10m\n"

// paidRunner is a runner that can plan against RunPod and reach an OFFER.
//
// THE CATALOGUE BODY IS THE PROVIDER'S OWN COMMITTED FIXTURE, served verbatim
// rather than copied here, which is the choice runpodfake makes for the same
// reason: two copies of a field shape is how `dataCenter` against
// `dataCenterId` stayed invisible for a release. Nothing else is served, so a
// run that got past the refusal fails at the create call -- which is what the
// accept half wants, because what it has to prove is that refusePaid let it
// through and not that a pod was made.
func paidRunner(t *testing.T, q *Queue, st genstore.Store) DirectRunner {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "provider", "runpod", "testdata", "gpus.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/catalog/gpus" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	r := directRunner(t, q, st)
	r.Deps.CloudBaseURL = srv.URL
	r.Deps.Creds = &config.Credentials{RunPod: node.NewSecret([]byte("rp-token"))}
	return r
}

// TestAdoptRun_ReportsAnAdoptThatFailed rather than handing back a sink
// nothing can write through.
//
// A store that will not take the row is a diagnostic problem on the shell
// path -- recordRental says so and runs anyway -- and on this one it has to stop
// the run: the client was promised a request id to stream, and a run with no
// row is a rental nothing records. So the error travels and Start returns it
// as a 400 rather than renting against a history it cannot write.
func TestAdoptRun_ReportsAnAdoptThatFailed(t *testing.T) {
	st := genstore.NewRecordingStore()
	st.FailOn = "Adopt"
	q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
	r := directRunner(t, q, st)

	id, err := r.Start(context.Background(), app.RunAsk{Config: writeRunConfig(t, localRunConfig)})
	if err == nil {
		t.Fatalf("Start ran against a history that refused the row, as %s", id)
	}
	if id != "" {
		t.Errorf("Start returned the id %s for a row that was never written", id)
	}
}

// TestAdoptRun_SendsEveryEventToTheExtraSinksAndToTheRow.
//
// The `also` sinks are the whole difference between the two callers: the shell
// passes report.Text so the operator's terminal gets the lines, and the socket
// route passes none because its operator reads them through the client. A
// numbering bug here is the one that cost a whole generation's event stream,
// so the sequence is asserted too.
func TestAdoptRun_SendsEveryEventToTheExtraSinksAndToTheRow(t *testing.T) {
	st := genstore.NewRecordingStore()
	q := &Queue{Store: st, Planner: &fakePlanner{served: &fakeServed{}}, Outputs: t.TempDir()}
	r, e := directRunnerEngine(t, q, st)
	// destroy_failed is the event this uses for the reason
	// TestUp_RecordsTheRentalsOwnEvents does: it is reachable with no node and
	// no sshd -- an inspect that fails leaves the rental unconfirmed and the
	// destroy unable to complete -- and it is the one an operator must not
	// miss, because the node may still be running and running means billing.
	e.FailOn = "ContainerInspect"

	ctx, cancel := context.WithCancel(context.Background())
	loop := make(chan error, 1)
	go func() { loop <- q.Run(ctx) }()
	defer func() {
		cancel()
		<-loop //nolint:errcheck // waiting for the destroy, not for an error
	}()

	id, err := r.Start(ctx, app.RunAsk{Config: writeRunConfig(t, localRunConfig)})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, "the run to end", func() bool {
		got, gerr := st.Get(context.Background(), id)
		return gerr == nil && got.Status.Terminal()
	})

	es, err := st.Events(ctx, id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var destroy *report.Event
	for i := range es {
		if es[i].Kind == report.KindDestroyFailed {
			destroy = &es[i]
		}
	}
	if destroy == nil {
		t.Fatalf("the run's events reached no row, so a submitted run reports to nothing a "+
			"client can read: %+v", es)
	}
	if destroy.Err == "" {
		t.Error("destroy_failed was recorded without the reason it failed")
	}
	// NUMBERED, because an unnumbered event arrives as Seq 0 and genstore's
	// INSERT OR REPLACE on (request, seq) makes every one overwrite the last
	// -- a whole generation collapsing into one row is how that bug presented
	// in the queue, and the stream is what prevents it on both paths.
	for i, ev := range es {
		if ev.Seq != int64(i+1) {
			t.Errorf("event %d has seq %d: the recorder is not behind a report.Stream", i, ev.Seq)
		}
	}
}
