package queue

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// fakeServed is a rental whose weights are resident, with no card.
//
// It records each batch it was asked for, which is what makes "one rental
// served both" an assertion rather than a hope.
type fakeServed struct {
	mu       sync.Mutex
	batches  []sdserver.Run
	dirs     []string
	failNth  map[int]error
	progress func(sink report.Sink, n int)
	caps     sdserver.Capabilities
}

func (f *fakeServed) Caps() sdserver.Capabilities { return f.caps }

func (f *fakeServed) Generate(_ context.Context, sink report.Sink, jobs sdserver.Run, dir string) error {
	f.mu.Lock()
	n := len(f.batches)
	f.batches = append(f.batches, jobs)
	f.dirs = append(f.dirs, dir)
	fail := f.failNth[n]
	prog := f.progress
	f.mu.Unlock()

	if prog != nil {
		prog(sink, n)
	}
	return fail
}

func (f *fakeServed) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.batches)
}

// fakeRental is a plan that has not been rented, and counts how often it was.
//
// RENTALS is the number every money assertion here turns on: two rentals for
// what should have been one is twice the bill, and the whole shape exists to
// make that not happen.
type fakeRental struct {
	served    *fakeServed
	rentals   *int
	destroyed *int
	rentErr   error
	mu        *sync.Mutex
	// block is closed by a test that wants to observe a rental in flight.
	block chan struct{}
}

func (r fakeRental) WithServed(ctx context.Context, sink report.Sink, fn func(context.Context, app.Served) error) error {
	r.mu.Lock()
	*r.rentals++
	r.mu.Unlock()

	// The node exists from here. The deferred decrement is what
	// provision.WithNode's deferred destroy stands for: there is no path out
	// that leaves it running.
	defer func() {
		r.mu.Lock()
		*r.destroyed++
		r.mu.Unlock()
	}()

	if r.rentErr != nil {
		return r.rentErr
	}
	sink.Emit(report.Event{Kind: report.KindNodeReady, Node: "local/n1"})
	err := fn(ctx, r.served)
	if r.block != nil {
		// Held open so a test can see the rental alive while it cancels.
		select {
		case <-r.block:
		case <-time.After(5 * time.Second):
		}
	}
	sink.Emit(report.Event{Kind: report.KindRunCost, Node: "local/n1", Cost: 11333})
	return err
}

type fakePlanner struct {
	served    *fakeServed
	mu        sync.Mutex
	rentals   int
	destroyed int
	planErr   error
	rentErr   error
	block     chan struct{}
	planned   []genstore.Request
}

func (p *fakePlanner) Plan(_ context.Context, r genstore.Request) (Rental, error) {
	p.mu.Lock()
	p.planned = append(p.planned, r)
	err := p.planErr
	p.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return fakeRental{
		served: p.served, rentals: &p.rentals, destroyed: &p.destroyed,
		rentErr: p.rentErr, mu: &p.mu, block: p.block,
	}, nil
}

func (p *fakePlanner) counts() (rentals, destroyed int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rentals, p.destroyed
}

func newQueue(t *testing.T, pl *fakePlanner) (*Queue, genstore.Store) {
	t.Helper()
	st := genstore.NewRecordingStore()
	return &Queue{
		Store:   st,
		Planner: pl,
		Outputs: filepath.Join(t.TempDir(), "outputs"),
	}, st
}

func enqueue(t *testing.T, st genstore.Store, gpu string, prompts ...string) genstore.ReqID {
	t.Helper()
	r := genstore.Request{Provider: "local", GPU: gpu, TTL: time.Hour, Preview: "proj", PreviewInterval: 5}
	for _, p := range prompts {
		r.Jobs = append(r.Jobs, genstore.Job{Prompt: p, Width: 512, Height: 512, Steps: 4})
	}
	id, err := st.Enqueue(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// TestOnce_OneRentalServesEveryCompatibleRequest is the behaviour the whole
// package exists for.
//
// Two requests for the same card are two batches on ONE node. The alternative
// -- a rental each -- pays for provisioning, the image pull, the asset verify
// and a 56 GiB model load twice, which on a rented A100 is about three minutes
// against roughly a hundred seconds of generation.
func TestOnce_OneRentalServesEveryCompatibleRequest(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "sd15"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	a := enqueue(t, st, "rtx2080ti", "first")
	b := enqueue(t, st, "rtx2080ti", "second")

	worked, err := q.once(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !worked {
		t.Fatal("the queue reported it had nothing to do")
	}

	if rentals, destroyed := pl.counts(); rentals != 1 || destroyed != 1 {
		t.Errorf("rented %d node(s) and destroyed %d, want 1 and 1", rentals, destroyed)
	}
	if n := served.count(); n != 2 {
		t.Errorf("generated %d batch(es) on the one rental, want 2", n)
	}

	for _, id := range []genstore.ReqID{a, b} {
		got, err := st.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != genstore.StatusDone {
			t.Errorf("request %s ended as %q, want done", id, got.Status)
		}
	}

	// Each into its own directory, which is also its blob key prefix: two
	// requests sharing one would overwrite each other the moment both used
	// the same output stem.
	served.mu.Lock()
	dirs := append([]string(nil), served.dirs...)
	served.mu.Unlock()
	if dirs[0] == dirs[1] {
		t.Errorf("both requests wrote into %s", dirs[0])
	}
}

// TestOnce_ARequestForAnotherCardWaitsForItsOwnRental.
//
// Serving it on the node that is up would generate on hardware the operator
// did not ask for and bill them for it. So the first rental finishes and is
// destroyed, and the second request gets its own.
func TestOnce_ARequestForAnotherCardWaitsForItsOwnRental(t *testing.T) {
	served := &fakeServed{}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	enqueue(t, st, "rtx2080ti", "this card")
	other := enqueue(t, st, "h100-80g", "another card")

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rentals, _ := pl.counts(); rentals != 1 {
		t.Fatalf("rented %d node(s) for the first request, want 1", rentals)
	}
	if n := served.count(); n != 1 {
		t.Errorf("the rental generated %d batch(es), want 1 -- the other needs a different card", n)
	}
	if got, err := st.Get(t.Context(), other); err != nil {
		t.Fatal(err)
	} else if got.Status != genstore.StatusQueued {
		t.Errorf("the incompatible request became %q, want it still queued", got.Status)
	}

	// And now it gets its own.
	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if rentals, destroyed := pl.counts(); rentals != 2 || destroyed != 2 {
		t.Errorf("rented %d and destroyed %d, want 2 and 2", rentals, destroyed)
	}
}

// TestOnce_AnEmptyQueueDestroysAtOnce. No idle timeout: a node with nothing to
// do is a node billing for nothing, which is the failure mode ember exists to
// prevent.
func TestOnce_AnEmptyQueueDestroysAtOnce(t *testing.T) {
	served := &fakeServed{}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	enqueue(t, st, "rtx2080ti", "only one")

	start := time.Now()
	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("the rental was held for %v after its last batch", took)
	}
	if _, destroyed := pl.counts(); destroyed != 1 {
		t.Error("the node was not destroyed when the queue emptied")
	}
}

// TestOnce_NothingToClaimRentsNothing. The loop must not rent speculatively:
// a node started "in case" is a node billing with no work.
func TestOnce_NothingToClaimRentsNothing(t *testing.T) {
	pl := &fakePlanner{served: &fakeServed{}}
	q, _ := newQueue(t, pl)

	worked, err := q.once(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if worked {
		t.Error("the queue claimed it did something with an empty queue")
	}
	if rentals, _ := pl.counts(); rentals != 0 {
		t.Errorf("rented %d node(s) with nothing queued", rentals)
	}
}

// TestOnce_AFailedBatchStopsTheRental.
//
// A rental that carried on after one request failed would bill for work whose
// premise the operator is now going to re-examine. The requests behind it are
// still queued and get a fresh node, so one bad request does not poison the
// queue.
func TestOnce_AFailedBatchStopsTheRental(t *testing.T) {
	boom := errors.New("the server refused it")
	served := &fakeServed{failNth: map[int]error{0: boom}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	bad := enqueue(t, st, "rtx2080ti", "fails")
	good := enqueue(t, st, "rtx2080ti", "never reached on this rental")

	_, err := q.once(t.Context())
	if err == nil {
		t.Fatal("a failed batch was reported as success")
	}
	if n := served.count(); n != 1 {
		t.Errorf("generated %d batch(es) after a failure, want 1", n)
	}
	if _, destroyed := pl.counts(); destroyed != 1 {
		t.Error("the node outlived the failure")
	}

	got, err := st.Get(t.Context(), bad)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusFailed {
		t.Errorf("the failing request ended as %q, want failed", got.Status)
	}
	if got.Err == "" {
		t.Error("the failing request does not say why")
	}

	q2, err := st.Get(t.Context(), good)
	if err != nil {
		t.Fatal(err)
	}
	if q2.Status != genstore.StatusQueued {
		t.Errorf("the request behind it became %q; one bad request must not poison the queue", q2.Status)
	}
}

// TestOnce_APlanThatFailsRentsNothing. Every refusal app.NewPlan makes happens
// before the create call, so a request ember cannot plan costs nothing at all
// -- and carries the reason, which is the point of resolving before renting.
func TestOnce_APlanThatFailsRentsNothing(t *testing.T) {
	pl := &fakePlanner{served: &fakeServed{}, planErr: errors.New("no image for that card")}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "a")

	worked, err := q.once(t.Context())
	if err == nil {
		t.Fatal("a failed plan was reported as success")
	}
	if !worked {
		t.Error("the queue claimed it did nothing, but it claimed a request")
	}
	if rentals, _ := pl.counts(); rentals != 0 {
		t.Errorf("rented %d node(s) for a request it could not plan", rentals)
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusFailed {
		t.Errorf("status = %q, want failed", got.Status)
	}
	if got.Err == "" || !strings.Contains(got.Err, "no image") {
		t.Errorf("err = %q, want the planner's own reason", got.Err)
	}
}

// TestOnce_CancellationIsNotAFailure.
//
// The operator pressed Ctrl-C, or the node reached its deadline. Recording
// that as a failed generation would send them looking for a bug that is not
// there -- and it is the difference between "your prompt was wrong" and "you
// stopped it".
func TestOnce_CancellationIsNotAFailure(t *testing.T) {
	served := &fakeServed{failNth: map[int]error{0: context.Canceled}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "a")

	if _, err := q.once(t.Context()); err == nil {
		t.Fatal("want the cancellation reported")
	}
	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusCancelled {
		t.Errorf("status = %q, want cancelled rather than failed", got.Status)
	}
}

// TestOnce_RecordsTheRentalAgainstTheRequestThatOpenedIt.
//
// Which request gets the rental's cost has no tidy answer, so the answer is
// written down and tested: the one that opened it, because it is the one that
// paid for the node to exist. The others get their own per-job card time, and
// the two figures differ by exactly the per-rental overhead this shape pays
// once.
func TestOnce_RecordsTheRentalAgainstTheRequestThatOpenedIt(t *testing.T) {
	served := &fakeServed{}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	opener := enqueue(t, st, "rtx2080ti", "first")
	rider := enqueue(t, st, "rtx2080ti", "second")

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}

	got, err := st.Get(t.Context(), opener)
	if err != nil {
		t.Fatal(err)
	}
	if got.Node != "local/n1" {
		t.Errorf("node = %q, want the rental's handle so `ember ls` can be joined to it", got.Node)
	}
	if got.Cost == 0 {
		t.Error("the request that opened the rental carries no cost")
	}

	second, err := st.Get(t.Context(), rider)
	if err != nil {
		t.Fatal(err)
	}
	if second.Cost != 0 {
		t.Errorf("the rider carries the rental's cost too (%v); it would be counted twice", second.Cost)
	}
}

// TestOnce_PersistsProgressSoAReconnectCanReplayIt.
func TestOnce_PersistsProgressSoAReconnectCanReplayIt(t *testing.T) {
	served := &fakeServed{
		// NO Seq SET, deliberately. Nothing real sets one -- report.Stream
		// stamps it -- and the first version of this fake numbered its own
		// events, which hid a bug where nothing was numbering them at all:
		// every event arrived as Seq 0 and overwrote the last, leaving one
		// row for a whole generation. A fake tidier than the thing it stands
		// in for tests a program that does not exist.
		progress: func(sink report.Sink, _ int) {
			for i := 1; i <= 4; i++ {
				sink.Emit(report.Event{
					Kind: report.KindJobProgress, Unit: report.UnitJob,
					Index: 1, Total: 1, Pass: 1, Step: i, TotalSteps: 4,
				})
			}
		},
	}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "a")

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}

	es, err := st.Events(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var progress int
	for _, e := range es {
		if e.Kind == report.KindJobProgress {
			progress++
		}
	}
	if progress != 4 {
		t.Errorf("recorded %d progress event(s), want 4 -- a client that reconnects reads these", progress)
	}

	// EVERY EVENT NUMBERED, DISTINCTLY AND IN ORDER. This is the assertion
	// the first version of this test could not make, because its fake set Seq
	// itself: nothing was stamping them, so every event arrived as Seq 0 and
	// genstore's INSERT OR REPLACE on (request, seq) meant each overwrote the
	// last. A whole generation left one row, and only running ember found it.
	seen := map[int64]bool{}
	var prev int64
	for _, e := range es {
		if e.Seq == 0 {
			t.Fatalf("an event reached the store unnumbered: %+v", e)
		}
		if seen[e.Seq] {
			t.Fatalf("Seq %d was used twice, so one event overwrote another", e.Seq)
		}
		if e.Seq <= prev {
			t.Fatalf("Seq went backwards: %d after %d", e.Seq, prev)
		}
		seen[e.Seq], prev = true, e.Seq
		if e.At.IsZero() {
			t.Errorf("event %d carries no timestamp", e.Seq)
		}
	}
	// The lifecycle events share the numbering, because they belong to the
	// same request: node_ready before the progress, run_cost after it.
	if len(es) < 6 {
		t.Errorf("recorded %d event(s); the rental's own node_ready and run_cost should share this stream", len(es))
	}
}

// TestOnce_JoinsTheFilesAJobWroteToTheRequest.
//
// The names come off the event, which got them from sdserver.WriteImages --
// the one place that decides what an image is called. Without the join a UI
// would have to list a directory and guess which request owned what.
func TestOnce_JoinsTheFilesAJobWroteToTheRequest(t *testing.T) {
	served := &fakeServed{
		progress: func(sink report.Sink, _ int) {
			sink.Emit(report.Event{
				Kind: report.KindJobImages, Unit: report.UnitJob,
				Index: 1, Total: 1, Files: 2, Bytes: 4096,
				Wrote: []report.File{
					{Name: "cat-00.png", Bytes: 2048},
					{Name: "cat-01.png", Bytes: 2048},
				},
			})
		},
	}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "a cat")

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}

	ims, err := st.Images(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ims) != 2 {
		t.Fatalf("recorded %d image(s), want 2", len(ims))
	}
	// The key is the request's own prefix plus the name sdserver chose, so a
	// row and a file cannot disagree about where something is.
	want := string(id) + "/cat-00.png"
	if ims[0].Key != want {
		t.Errorf("key = %q, want %q", ims[0].Key, want)
	}
	if ims[0].Bytes != 2048 {
		t.Errorf("bytes = %d, want each file's own length", ims[0].Bytes)
	}
}

// TestRun_ReturnsOnlyAfterTheRentalIsDestroyed is the shutdown property, and
// the most expensive one to get wrong.
//
// A server that exits while a node is alive leaks a GPU. Because the rental
// lives inside one iteration of this loop, waiting for Run to return is a
// complete shutdown -- there is no set of goroutines to track.
func TestRun_ReturnsOnlyAfterTheRentalIsDestroyed(t *testing.T) {
	inFlight := make(chan struct{})
	release := make(chan struct{})
	served := &fakeServed{progress: func(report.Sink, int) {
		select {
		case inFlight <- struct{}{}:
		default:
		}
	}}
	pl := &fakePlanner{served: served, block: release}
	q, st := newQueue(t, pl)
	enqueue(t, st, "rtx2080ti", "a")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- q.Run(ctx) }()

	select {
	case <-inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop never started generating")
	}

	// Cancel while the rental is up, then let the teardown proceed.
	cancel()
	select {
	case err := <-done:
		t.Fatalf("Run returned (%v) while its rental was still being torn down", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run = %v, want nil: a cancellation is the ordinary way out", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the rental was released")
	}

	if rentals, destroyed := pl.counts(); rentals != destroyed {
		t.Errorf("rented %d and destroyed %d", rentals, destroyed)
	}
}

// TestRun_MarksWhatWasRunningWhenTheProcessDied.
//
// Its rental is gone, so it cannot be resumed -- and re-queueing it would rent
// a second card for work the operator may no longer want.
func TestRun_MarksWhatWasRunningWhenTheProcessDied(t *testing.T) {
	pl := &fakePlanner{served: &fakeServed{}}
	q, st := newQueue(t, pl)

	id := enqueue(t, st, "rtx2080ti", "was in flight")
	if _, ok, err := st.ClaimNext(t.Context(), "an-older-lease", genstore.Compat{}); err != nil || !ok {
		t.Fatalf("setup: ok=%v err=%v", ok, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // one pass of the startup work and straight out
	if err := q.Run(ctx); err != nil {
		t.Fatal(err)
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusInterrupted {
		t.Errorf("status = %q, want interrupted", got.Status)
	}
	if rentals, _ := pl.counts(); rentals != 0 {
		t.Errorf("rented %d node(s) for an interrupted request; it must not be resumed", rentals)
	}
}

// TestRun_AKickIsNotRequiredAndIsNotBlocking. Kick is how the API avoids a
// poll's latency; it must never block the handler that sends it.
func TestRun_AKickIsNotRequiredAndIsNotBlocking(t *testing.T) {
	pl := &fakePlanner{served: &fakeServed{}}
	q, _ := newQueue(t, pl)

	// Nothing is reading, and several kicks in a row must still return at
	// once: a second kick before the loop wakes is the same information.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			q.Kick()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Kick blocked")
	}
}

// TestRunning_NamesWhatIsGeneratingNow, which is what a UI shows as "in
// progress" and what the lifecycle routing uses.
func TestRunning_NamesWhatIsGeneratingNow(t *testing.T) {
	seen := make(chan genstore.ReqID, 1)
	served := &fakeServed{progress: func(report.Sink, int) {}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	served.progress = func(report.Sink, int) {
		id, ok := q.Running()
		if ok {
			seen <- id
		}
	}
	id := enqueue(t, st, "rtx2080ti", "a")

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-seen:
		if got != id {
			t.Errorf("Running() = %q while generating %q", got, id)
		}
	default:
		t.Fatal("Running() reported nothing while a batch was generating")
	}

	// And nothing once it is over: a UI showing a request as in progress
	// after its node is gone is a UI nobody believes.
	if _, ok := q.Running(); ok {
		t.Error("Running() still names a request after the rental ended")
	}
}

// TestOnce_CachesTheServersCapabilities, so a mistyped sampler is refused when
// it is typed rather than a minute into a rented card.
func TestOnce_CachesTheServersCapabilities(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{
		Model: "sd15.gguf", Samplers: []string{"euler"},
	}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	enqueue(t, st, "rtx2080ti", "a")

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	caps, ok, err := st.Caps(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("the capabilities were not cached, so the next request costs a rental to validate")
	}
	if caps.Model != "sd15.gguf" || len(caps.Samplers) != 1 {
		t.Errorf("cached %+v", caps)
	}
}

// TestOnce_SurvivesAStoreThatCannotRecordProgress.
//
// Losing a progress line is not a reason to abandon a generation that is
// already paid for. The images still land and the request still finishes.
func TestOnce_SurvivesAStoreThatCannotRecordProgress(t *testing.T) {
	served := &fakeServed{progress: func(sink report.Sink, _ int) {
		sink.Emit(report.Event{Kind: report.KindJobProgress, Step: 1, TotalSteps: 4})
	}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "a")

	st.(*genstore.RecordingStore).FailOn = "AppendEvents"

	if _, err := q.once(t.Context()); err != nil {
		t.Errorf("a store that could not record progress failed the generation: %v", err)
	}
	st.(*genstore.RecordingStore).FailOn = ""

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusDone {
		t.Errorf("status = %q, want done", got.Status)
	}
}

func TestCost_IsTheLastRunningTotal(t *testing.T) {
	es := []report.Event{
		{Kind: report.KindUnitCost, Cost: 10, Spent: 10},
		{Kind: report.KindJobProgress},
		{Kind: report.KindUnitCost, Cost: 15, Spent: 25},
	}
	if got := Cost(es); got != 25 {
		t.Errorf("Cost = %v, want the last running total, 25", got)
	}
	if got := Cost(nil); got != 0 {
		t.Errorf("Cost(nil) = %v, want 0", got)
	}
}

// TestOnce_AStoreThatBreaksDoesNotTakeTheNodeWithIt.
//
// Every write the drain loop makes can fail, and what matters is which of
// them is fatal. A claim that fails has to stop -- the loop cannot know what
// it would have got -- while a record that fails must not: the node is up, the
// generation is paid for, and abandoning it to preserve a row would be the
// wrong trade. These paths run from deferred teardowns, where a panic takes
// the destroy with it and leaks the node.
func TestOnce_AStoreThatBreaksDoesNotTakeTheNodeWithIt(t *testing.T) {
	for _, c := range []struct {
		failOn  string
		wantErr bool
		why     string
	}{
		{"ClaimNext", false, "nothing was claimed, so nothing was rented"},
		{"Update", false, "the generation finished; only the record of it is lost"},
		{"AddImage", false, "the files are on disk; only the join to them is lost"},
	} {
		t.Run(c.failOn, func(t *testing.T) {
			served := &fakeServed{progress: func(sink report.Sink, _ int) {
				sink.Emit(report.Event{
					Kind: report.KindJobImages, Unit: report.UnitJob,
					Index: 1, Total: 1, Files: 1, Bytes: 2048,
					Wrote: []report.File{{Name: "a-00.png", Bytes: 2048}},
				})
			}}
			pl := &fakePlanner{served: served}
			q, st := newQueue(t, pl)
			enqueue(t, st, "rtx2080ti", "a")

			rec := st.(*genstore.RecordingStore)
			// The setup claim has to succeed, so the injection is armed after
			// the request is in and cleared by the assertion below.
			if c.failOn != "ClaimNext" {
				rec.FailOn = c.failOn
			}

			_, err := q.once(t.Context())
			if c.failOn == "ClaimNext" {
				rec.FailOn = "ClaimNext"
				_, err = q.once(t.Context())
				if err == nil {
					t.Fatal("a claim that failed was reported as success")
				}
				rec.FailOn = ""
				return
			}
			rec.FailOn = ""

			if (err != nil) != c.wantErr {
				t.Errorf("err = %v, want err != nil to be %v (%s)", err, c.wantErr, c.why)
			}
			// Whatever broke, the node went away.
			if rentals, destroyed := pl.counts(); rentals != destroyed {
				t.Errorf("rented %d and destroyed %d", rentals, destroyed)
			}
		})
	}
}

// TestDrain_AClaimThatFailsMidRentalStopsThatRental.
//
// The loop cannot know what it would have been handed, so carrying on would
// mean either destroying a node with work outstanding or serving a request
// twice. Stopping leaves the queue intact and the next iteration rents again.
func TestDrain_AClaimThatFailsMidRentalStopsThatRental(t *testing.T) {
	served := &fakeServed{}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	enqueue(t, st, "rtx2080ti", "first")
	second := enqueue(t, st, "rtx2080ti", "second")

	rec := st.(*genstore.RecordingStore)
	served.progress = func(report.Sink, int) {
		// Armed during the first batch, so the claim for the second fails.
		rec.FailOn = "ClaimNext"
	}

	_, err := q.once(t.Context())
	rec.FailOn = ""
	if err == nil {
		t.Fatal("a claim that failed mid-rental was reported as success")
	}
	if n := served.count(); n != 1 {
		t.Errorf("generated %d batch(es), want 1", n)
	}
	if _, destroyed := pl.counts(); destroyed != 1 {
		t.Error("the node outlived the failure")
	}
	// Still queued: it was never claimed, so it gets a fresh rental.
	got, err := st.Get(t.Context(), second)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusQueued {
		t.Errorf("status = %q, want it still queued", got.Status)
	}
}

// TestRun_SurvivesAStoreThatCannotMarkInterruptedRequests. The history is
// wrong, which is a nuisance; refusing to start would mean the queue never
// runs again.
func TestRun_SurvivesAStoreThatCannotMarkInterruptedRequests(t *testing.T) {
	pl := &fakePlanner{served: &fakeServed{}}
	q, st := newQueue(t, pl)
	st.(*genstore.RecordingStore).FailOn = "Interrupt"

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := q.Run(ctx); err != nil {
		t.Errorf("Run = %v, want it to start anyway", err)
	}
}

// TestQueue_UsesItsInjectedCollaborators, so a recorded timestamp is
// comparable and the node's own output goes where the caller said.
func TestQueue_UsesItsInjectedCollaborators(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	var buf strings.Builder
	q := &Queue{
		Now: func() time.Time { return at },
		Out: &buf,
		Log: slog.New(slog.DiscardHandler),
	}
	if !q.at().Equal(at) {
		t.Errorf("at() = %v, want the injected %v", q.at(), at)
	}
	if q.out() != &buf {
		t.Error("out() is not the injected writer")
	}
	if q.log() == nil {
		t.Error("log() is nil")
	}
}
