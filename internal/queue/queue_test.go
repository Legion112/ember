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

	// onStop, when set, is called with the stop channel of each batch
	// instead of returning at once.
	//
	// IT TAKES THE CHANNEL RATHER THAN A BOOL, because what has to be
	// asserted is that the channel the queue handed over is the one closing
	// a cancel of THAT request closes -- a fake that reported "I was
	// cancelled" out of its own state would agree with itself whether or not
	// the signal ever arrived. The same reason the preview fake repeats a
	// frame: a fake tidier than the thing it stands in for tests a program
	// that does not exist.
	onStop func(stop <-chan struct{}, n int) error
}

func (f *fakeServed) Caps() sdserver.Capabilities { return f.caps }

func (f *fakeServed) Generate(_ context.Context, sink report.Sink, jobs sdserver.Run, dir string, stop <-chan struct{}) error {
	f.mu.Lock()
	n := len(f.batches)
	f.batches = append(f.batches, jobs)
	f.dirs = append(f.dirs, dir)
	fail := f.failNth[n]
	prog := f.progress
	onStop := f.onStop
	f.mu.Unlock()

	if prog != nil {
		prog(sink, n)
	}
	if onStop != nil {
		if err := onStop(stop, n); err != nil {
			return err
		}
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
	// What app.Plan.prepare emits before the workload, so the drain loop's
	// write-through of it is exercised by every rental here rather than by
	// one test that remembered to send it.
	sink.Emit(report.Event{
		Kind: report.KindNodeMemory, MemCgroup: 2,
		MemLimit: 32 << 30, MemCurrent: 31 << 30,
		MemTotal: 700 << 30, MemCached: 29 << 30,
		Bytes: 56 << 30,
	})
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
	// BUFFERED BY ONE, and the non-blocking send below is why. An unbuffered
	// channel with a `default:` arm drops the signal unless a receiver is
	// already parked -- so under the load of the whole suite running in
	// parallel, the loop started generating, the signal went nowhere, and
	// this test waited out its own timeout. It failed about one run in
	// twenty, which is the worst frequency a flake can have.
	inFlight := make(chan struct{}, 1)
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

// TestRecorder_WritesWhatEachJobDidThroughToItsRow.
//
// This replaces TestCost_IsTheLastRunningTotal, which tested a function
// nothing but itself had ever called: queue.Cost summed one request's card
// time by scanning its whole event stream, and the coverage floor was all
// that kept it alive. The facts are columns now, so the thing worth asserting
// is that they are written -- a schema, a query layer and a command over
// figures nothing records is the "built end to end and never connected"
// failure this repository has already paid for twice.
func TestRecorder_WritesWhatEachJobDidThroughToItsRow(t *testing.T) {
	q, st := newQueue(t, nil)
	id := enqueue(t, st, "rtx2080ti", "first", "second")

	snk := q.sinkFor(id)
	// One job, start to finish, as app.served emits it: started, two
	// sampling passes, the images, then the cost.
	snk.Emit(report.Event{Kind: report.KindUnitStarted, Unit: report.UnitJob, Index: 1, Total: 2})
	snk.Emit(report.Event{Kind: report.KindJobProgress, Index: 1, Pass: 1, Step: 4, TotalSteps: 20})
	snk.Emit(report.Event{Kind: report.KindJobProgress, Index: 1, Pass: 1, Step: 20, TotalSteps: 20})
	snk.Emit(report.Event{Kind: report.KindJobProgress, Index: 1, Pass: 2, Step: 6, TotalSteps: 8})
	snk.Emit(report.Event{
		Kind: report.KindJobImages, Unit: report.UnitJob, Index: 1, Total: 2,
		Files: 1, Bytes: 4096,
		Wrote: []report.File{{Name: "001.png", Bytes: 4096}},
	})
	snk.Emit(report.Event{
		Kind: report.KindUnitCost, Unit: report.UnitJob, Index: 1, Total: 2,
		Cost: 31_000, Spent: 31_000,
	})

	runs, err := st.JobRuns(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 {
		t.Fatalf("JobRuns = %+v, want one row per job", runs)
	}
	got := runs[0]
	if got.Cost != 31_000 {
		t.Errorf("cost = %v, want 31000", got.Cost)
	}
	if got.Images != 1 || got.Bytes != 4096 {
		t.Errorf("images/bytes = %d/%d, want 1/4096", got.Images, got.Bytes)
	}
	if got.Started.IsZero() || got.Ended.IsZero() {
		t.Errorf("span = %s..%s, want both ends", got.Started, got.Ended)
	}

	// Two passes of 20 and 8, so 28 steps -- not 20, which is what one pass
	// reported, and not 3, which is how many frames arrived. A pass restarts
	// for each batch item and each highres pass, and ember dedupes progress
	// on the step, so neither of those is the iteration count.
	if got.Passes != 2 || got.Steps != 28 {
		t.Errorf("passes/steps = %d/%d, want 2/28", got.Passes, got.Steps)
	}

	// The second job was never touched, and reads as a job that did nothing
	// rather than being absent: a half-finished request must not look like a
	// shorter one.
	if runs[1].Idx != 2 || runs[1].Cost != 0 || !runs[1].Started.IsZero() {
		t.Errorf("the untouched job = %+v", runs[1])
	}
}

// TestRecorder_RecordsAnInterruptedJobsImagesAndSteps.
//
// A request whose process died mid-batch keeps whatever images it produced --
// they were paid for, which is why StatusInterrupted leaves them listed. The
// figures behind them have to survive too, or the one request whose cost an
// operator most wants to understand is the one with no numbers.
func TestRecorder_RecordsAnInterruptedJobsImagesAndSteps(t *testing.T) {
	q, st := newQueue(t, nil)
	id := enqueue(t, st, "rtx2080ti", "only")

	snk := q.sinkFor(id)
	snk.Emit(report.Event{Kind: report.KindUnitStarted, Unit: report.UnitJob, Index: 1, Total: 1})
	snk.Emit(report.Event{Kind: report.KindJobProgress, Index: 1, Pass: 1, Step: 9, TotalSteps: 12})
	snk.Emit(report.Event{
		Kind: report.KindJobImages, Unit: report.UnitJob, Index: 1, Total: 1,
		Files: 1, Bytes: 2048, Wrote: []report.File{{Name: "001.png", Bytes: 2048}},
	})
	// And then nothing: no unit_cost, because the process went away.

	runs, err := st.JobRuns(t.Context(), id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("JobRuns = %+v, %v", runs, err)
	}
	if runs[0].Images != 1 || runs[0].Bytes != 2048 {
		t.Errorf("the images it produced were not recorded: %+v", runs[0])
	}
	if runs[0].Passes != 1 || runs[0].Steps != 12 {
		t.Errorf("passes/steps = %d/%d, want 1/12", runs[0].Passes, runs[0].Steps)
	}
	// No cost and no end, which is honest: the job started and never
	// finished, and inventing either would be a figure nothing measured.
	if runs[0].Cost != 0 || !runs[0].Ended.IsZero() {
		t.Errorf("an unfinished job reported a cost or an end: %+v", runs[0])
	}
	if runs[0].Sampling() != 0 {
		t.Errorf("Sampling = %s, want zero for a job with one end missing", runs[0].Sampling())
	}
}

// TestRecorder_AJobIndexNothingWroteIsLoggedNotFatal. The events carry a
// 1-based job number and the table is keyed on it, so an off-by-one writes
// nothing -- and it must not also take the generation down.
func TestRecorder_AJobIndexNothingWroteIsLoggedNotFatal(t *testing.T) {
	q, st := newQueue(t, nil)
	id := enqueue(t, st, "rtx2080ti", "only")

	snk := q.sinkFor(id)
	// Index 4 of a one-job request, and index 0, which is what an event with
	// no index set carries.
	snk.Emit(report.Event{Kind: report.KindUnitStarted, Unit: report.UnitJob, Index: 4})
	snk.Emit(report.Event{Kind: report.KindUnitCost, Unit: report.UnitJob, Index: 0, Cost: 5})

	runs, err := st.JobRuns(t.Context(), id)
	if err != nil || len(runs) != 1 {
		t.Fatalf("JobRuns = %+v, %v", runs, err)
	}
	if runs[0].Cost != 0 || !runs[0].Started.IsZero() {
		t.Errorf("an out-of-range index wrote to the real job: %+v", runs[0])
	}
}

// TestLifecycle_WritesTheRentalsOwnFactsThrough.
//
// The cost was already written through; the rate, the elapsed and billable
// spans and the model-load pair were not, and they are what an overhead or a
// resident-model figure is computed from.
func TestLifecycle_WritesTheRentalsOwnFactsThrough(t *testing.T) {
	q, st := newQueue(t, nil)
	id := enqueue(t, st, "rtx2080ti", "only")

	snk := q.lifecycle(id)
	snk.Emit(report.Event{
		Kind:     report.KindModelResident,
		Measured: 94 * time.Second, Reported: 551 * time.Millisecond, Total: 8,
	})
	snk.Emit(report.Event{
		Kind: report.KindRunCost, Node: "runpod/abc", Cost: 1_200_000,
		Rate: 2_490_000, Elapsed: 30 * time.Minute, Billable: 31 * time.Minute,
	})

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cost != 1_200_000 || got.Rate != 2_490_000 {
		t.Errorf("cost/rate = %v/%v", got.Cost, got.Rate)
	}
	if got.Elapsed != 30*time.Minute || got.Billable != 31*time.Minute {
		t.Errorf("elapsed/billable = %s/%s; a provider minimum makes these differ and both are kept",
			got.Elapsed, got.Billable)
	}
	// Both load figures, because the only measurement anybody has made had
	// them disagreeing by two orders of magnitude.
	if got.LoadMeasured != 94*time.Second || got.LoadReported != 551*time.Millisecond {
		t.Errorf("load measured/reported = %s/%s, want both as emitted",
			got.LoadMeasured, got.LoadReported)
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
// comparable.
//
// It used to assert on an injected Out as well. There is no Out: the field and
// its accessor were the ninth instance of AGENTS.md's written-covered-and-
// connected-to-nothing defect, and these two assertions were the only thing
// that ever read it.
func TestQueue_UsesItsInjectedCollaborators(t *testing.T) {
	at := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	q := &Queue{
		Now: func() time.Time { return at },
		Log: slog.New(slog.DiscardHandler),
	}
	if !q.at().Equal(at) {
		t.Errorf("at() = %v, want the injected %v", q.at(), at)
	}
	if q.log() == nil {
		t.Error("log() is nil")
	}
}

// TestOnce_RecordsThePreviewFrameAsAPreview is the wire that was missing for
// the life of the feature.
//
// The frame was written to the run directory, the images table had a preview
// column, the handler answered ?preview=1 and the page looked for it -- and
// nothing ever inserted a row with it set, so the UI showed a step counter
// advancing beside no picture. Everything agreed with itself and nothing had
// been observed, which is the closed loop AGENTS.md records one layer down.
func TestOnce_RecordsThePreviewFrameAsAPreview(t *testing.T) {
	served := &fakeServed{
		progress: func(sink report.Sink, _ int) {
			// Two distinct steps writing the same file, which is what a real
			// generation does: the frame is written over, so the row is
			// overwritten rather than accumulated.
			for _, step := range []int{2, 4} {
				sink.Emit(report.Event{
					Kind: report.KindJobProgress, Unit: report.UnitJob,
					Index: 1, Total: 1, Pass: 1, Step: step, TotalSteps: 20,
					Wrote: []report.File{{Name: "cat-preview-00.png", Bytes: 250}},
				})
			}
			sink.Emit(report.Event{
				Kind: report.KindJobImages, Unit: report.UnitJob,
				Index: 1, Total: 1, Files: 1, Bytes: 2048,
				Wrote: []report.File{{Name: "cat-00.png", Bytes: 2048}},
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
	var previews, results []genstore.Image
	for _, im := range ims {
		if im.Preview {
			previews = append(previews, im)
		} else {
			results = append(results, im)
		}
	}
	if len(previews) != 1 {
		t.Fatalf("recorded %d preview(s), want exactly 1 -- the frame is written "+
			"over on every step, so the row is overwritten and not accumulated: %+v", len(previews), ims)
	}
	if want := string(id) + "/cat-preview-00.png"; previews[0].Key != want {
		t.Errorf("preview key = %q, want %q", previews[0].Key, want)
	}
	if len(results) != 1 {
		t.Fatalf("recorded %d result(s), want 1", len(results))
	}
	// The two must not collide: a result and a preview for the same job are
	// different rows, or the gallery shows the latent where the output goes.
	if results[0].Key == previews[0].Key {
		t.Errorf("the result and the preview share key %q", results[0].Key)
	}
}

// TestOnce_AProgressEventWithNoFrameRecordsNothing is the other half: with
// previews off, every progress event carries no file and must not leave a row
// behind pointing at nothing.
func TestOnce_AProgressEventWithNoFrameRecordsNothing(t *testing.T) {
	served := &fakeServed{
		progress: func(sink report.Sink, _ int) {
			sink.Emit(report.Event{
				Kind: report.KindJobProgress, Unit: report.UnitJob,
				Index: 1, Total: 1, Pass: 1, Step: 2, TotalSteps: 20,
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
	if len(ims) != 0 {
		t.Fatalf("recorded %d image(s) for a run that wrote none: %+v", len(ims), ims)
	}
}

// TestOnce_RecordsWhatTheNodeSaidAboutItsMemory.
//
// Written through to the OPENER, like the rest of the rental's own facts: the
// memory limit is the node's, and every request that rode along ran under the
// same one. Writing it to each would be the same multiplication that
// attributing the whole rental's cost to every request would be.
//
// The figures are the two arms of the FLUX.2 measurement in miniature: a
// container bounded at 32 GiB on a host reporting 700 GiB, about to read a
// 56 GiB model. Which is to say -- a node that cannot hold the weights, with
// nothing in the host's view to say so.
func TestOnce_RecordsWhatTheNodeSaidAboutItsMemory(t *testing.T) {
	pl := &fakePlanner{served: &fakeServed{}}
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
	if got.MemCgroup != 2 {
		t.Errorf("mem cgroup = %d, want the hierarchy the probe read", got.MemCgroup)
	}
	if got.MemLimit != 32<<30 || got.MemCurrent != 31<<30 {
		t.Errorf("the cgroup's view was not recorded: %+v", got)
	}
	if got.MemTotal != 700<<30 || got.MemCached != 29<<30 {
		t.Errorf("the host's view was not recorded: %+v", got)
	}
	if got.AssetBytes != 56<<30 {
		t.Errorf("asset bytes = %d, want the weights the run was about to read", got.AssetBytes)
	}

	second, err := st.Get(t.Context(), rider)
	if err != nil {
		t.Fatal(err)
	}
	if second.MemLimit != 0 {
		t.Errorf("the rider carries the node's memory limit too (%d); it belongs to the rental",
			second.MemLimit)
	}
}

// TestOnce_RecordsWhichModelServedEveryRequestOnTheRental.
//
// EVERY REQUEST AND NOT ONLY THE OPENER, which is the opposite of the rule
// just above and for a stated reason: the memory figures and the rental's cost
// are quantities, so attributing one rental's to each of its requests
// multiplies them. A model is a label -- every request this rental served ran
// against the same weights -- and recording it on the opener alone would split
// one model's spend between its own name and the dash in
// `ember spend --by model`, which is the figure the column exists to produce.
//
// From the server's own /capabilities response, as PutCaps caches it, and
// never from `sd.serve`: an operator can edit the config between two rentals,
// and then a reading of it attributes last week's spend to this week's
// weights. That is the `allowedCudaVersions` mistake -- a value read from the
// wrong place and believed.
func TestOnce_RecordsWhichModelServedEveryRequestOnTheRental(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "flux2-dev-Q8_0.gguf"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	opener := enqueue(t, st, "rtx2080ti", "first")
	rider := enqueue(t, st, "rtx2080ti", "second")

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, id := range []genstore.ReqID{opener, rider} {
		got, err := st.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Model != "flux2-dev-Q8_0.gguf" {
			t.Errorf("request %s records model %q, want what the server reported", id, got.Model)
		}
	}
}

// A server that reports no model is recorded as reporting none.
//
// sd-server resolves capabilities.model from --model, then from
// --diffusion-model, and reports an empty name when a configuration gave
// neither. Writing something in its place -- the card, the image, the word
// "unknown" -- would be a value ember made up sitting in the column whose
// entire purpose is that it was read from a response.
func TestOnce_DoesNotInventAModelTheServerDidNotReport(t *testing.T) {
	pl := &fakePlanner{served: &fakeServed{caps: sdserver.Capabilities{}}}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "first")

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "" {
		t.Errorf("model = %q, want nothing: the server reported none", got.Model)
	}
}

// A request that never reached a node records no model.
//
// The write happens at claim time on a rental that exists, so a request the
// planner refused has nothing to record -- and a model name beside a request
// that never generated would claim a run happened.
func TestOnce_APlanThatFailsRecordsNoModel(t *testing.T) {
	pl := &fakePlanner{planErr: errors.New("no capacity")}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "first")

	if _, err := q.once(t.Context()); err == nil {
		t.Fatal("a plan that failed was not reported")
	}
	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Model != "" {
		t.Errorf("a request that never got a node records model %q", got.Model)
	}
}

// --- cancel ---

// TestCancel_StopsOneRequestAndTheRentalServesTheNext is the money assertion
// of the whole change.
//
// `ember serve` used to refuse a cancel on a generating request, and the
// reason it gave was that stopping it "would mean destroying that node, which
// would take every other request on the same rental with it". That is what
// this test exists to deny: the cancelled request ends as cancelled, the node
// is rented ONCE, and the request behind it is generated on the same warm
// card rather than waiting for a second rental to pay for provisioning, the
// image pull and a 56 GiB model load all over again.
func TestCancel_StopsOneRequestAndTheRentalServesTheNext(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "sd15", CancelGenerating: true}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	a := enqueue(t, st, "rtx2080ti", "stop this one")
	b := enqueue(t, st, "rtx2080ti", "and keep this one")

	served.onStop = func(stop <-chan struct{}, n int) error {
		if n != 0 {
			return nil
		}
		// From another goroutine, as the HTTP handler is: Cancel must not
		// block on the loop it is signalling.
		reached := make(chan bool, 1)
		go func() { reached <- q.Cancel(a) }()
		if !<-reached {
			return errors.New("Cancel did not reach the request that is generating")
		}
		// THE CHANNEL THE QUEUE HANDED OVER is the one that closes. A fake
		// reporting "I was cancelled" out of its own state would pass
		// whether or not the signal ever arrived -- see fakeServed.onStop.
		select {
		case <-stop:
			return app.ErrCancelled
		case <-time.After(5 * time.Second):
			return errors.New("the cancel never reached the batch")
		}
	}

	if _, err := q.once(t.Context()); err != nil {
		t.Fatalf("a cancelled request ended the rental with an error: %v", err)
	}

	if rentals, destroyed := pl.counts(); rentals != 1 || destroyed != 1 {
		t.Errorf("rented %d node(s) and destroyed %d, want 1 and 1: a cancelled request "+
			"stops a batch and not a rental", rentals, destroyed)
	}
	if n := served.count(); n != 2 {
		t.Errorf("generated %d batch(es), want 2: the request behind the cancelled one "+
			"should have been served by the same warm card", n)
	}

	got, err := st.Get(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusCancelled {
		t.Errorf("the cancelled request ended as %q, want cancelled", got.Status)
	}
	if got.Err == "" {
		t.Error("the cancelled request recorded no reason")
	}

	next, err := st.Get(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if next.Status != genstore.StatusDone {
		t.Errorf("the request behind it ended as %q, want done", next.Status)
	}
}

// A cancel names one request, and reaches only that one.
//
// The signal is per-request and not per-rental, which is the difference
// between "stop my batch" and "stop the node". Cancelling b while a generates
// must leave a alone -- otherwise one operator's cancel silently stops
// somebody else's work on the shared rental, which is the failure the old 409
// was right to be afraid of.
func TestCancel_ReachesOnlyTheRequestItNames(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "sd15"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	a := enqueue(t, st, "rtx2080ti", "generating")
	b := enqueue(t, st, "rtx2080ti", "still queued")

	var wrong error
	served.onStop = func(stop <-chan struct{}, n int) error {
		if n != 0 {
			return nil
		}
		if q.Cancel(b) {
			wrong = errors.New("Cancel claimed to have stopped a request that is not generating")
		}
		// Not merely "Cancel said no": the channel a is listening to must
		// still be open, because a signal delivered to the wrong batch is
		// exactly what this is about.
		select {
		case <-stop:
			wrong = errors.New("cancelling b closed a's stop channel")
		default:
		}
		return nil
	}

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if wrong != nil {
		t.Error(wrong)
	}
	for _, id := range []genstore.ReqID{a, b} {
		got, err := st.Get(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != genstore.StatusDone {
			t.Errorf("request %s ended as %q, want done: neither was cancelled", id, got.Status)
		}
	}
}

// Cancelling twice is not a panic, and the second one says it did nothing.
//
// A double-clicked button is the ordinary case here, not the exotic one, and
// closing a channel twice panics -- in the drain loop's own goroutine, which
// would take the process down with a node running.
func TestCancel_IsSafeToAskTwice(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "sd15"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	a := enqueue(t, st, "rtx2080ti", "stop this one")

	var second bool
	served.onStop = func(stop <-chan struct{}, _ int) error {
		if !q.Cancel(a) {
			return errors.New("the first cancel did not reach it")
		}
		second = q.Cancel(a)
		<-stop
		return app.ErrCancelled
	}

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if second {
		t.Error("the second cancel reported it stopped something, and there was nothing left to stop")
	}
}

// Nothing generating is nothing to cancel, and that is reported as false.
//
// The API turns false into a 409 rather than recording a cancellation, for
// the reason `ember ls --all` exits non-zero on a provider it could not ask:
// "I could not stop it" must not render as "it is stopped" over a card that
// may still be billing.
func TestCancel_ReportsFalseWhenNothingIsGenerating(t *testing.T) {
	q, st := newQueue(t, &fakePlanner{served: &fakeServed{}})
	id := enqueue(t, st, "rtx2080ti", "queued")

	if q.Cancel(id) {
		t.Error("Cancel claimed to have stopped a request before any rental existed")
	}
	if q.Cancel("") {
		t.Error("Cancel accepted an empty id, which matches nothing and would match current when it is empty")
	}
}

// A cancel arriving a moment too late does not reach the next request.
//
// The drain loop claims the next request onto the same rental, so the window
// between one batch returning and the next starting is real, and a stale
// signal landing there would stop a batch the operator said nothing about.
//
// THE ID CHECK IN Cancel IS WHAT PREVENTS IT, which the reinstatement pass
// settled rather than the reading: clearing q.stop in generate's defer looks
// like the guard and reinstating that alone leaves this test green, because
// the next batch overwrites the channel anyway. Removing the id check is what
// turns it red.
func TestCancel_DoesNotReachTheNextRequestOnTheSameRental(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "sd15"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	a := enqueue(t, st, "rtx2080ti", "first")
	b := enqueue(t, st, "rtx2080ti", "second")

	var leaked error
	served.onStop = func(stop <-chan struct{}, n int) error {
		if n == 0 {
			return nil
		}
		// The second batch is generating now. A cancel naming the FIRST
		// request -- an operator who clicked while it was still on screen --
		// must not touch this one.
		if q.Cancel(a) {
			leaked = errors.New("a cancel naming the finished request reached the one after it")
		}
		select {
		case <-stop:
			leaked = errors.New("the second batch's stop channel was closed by the first's cancel")
		default:
		}
		return nil
	}

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if leaked != nil {
		t.Error(leaked)
	}
	got, err := st.Get(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusDone {
		t.Errorf("the second request ended as %q, want done", got.Status)
	}
}

// observingPlanner looks at the queue from inside provisioning.
//
// The only moment a test can see the window `Holding` exists for: `Plan` is
// called after ClaimNext has written `running` to the row and before any
// rental exists, which is where a cancel from the web UI ordinarily lands.
type observingPlanner struct {
	*fakePlanner
	at func(genstore.Request)
}

func (p observingPlanner) Plan(ctx context.Context, r genstore.Request) (Rental, error) {
	p.at(r)
	return p.fakePlanner.Plan(ctx, r)
}

// Holding is true from the claim; Running is not. The gap is a 409's
// remediation.
//
// THE ROW READS `running` FROM ClaimNext and `generate` registers the stop
// channel after the plan, the create, the gpu probe, the asset verify and the
// model load -- so there is a window, often tens of seconds long, in which a
// rental IS in this process and `Cancel` has nothing to signal. webapi used to
// answer a cancel there by telling the operator to restart `ember serve`,
// which destroys the rental they are paying for to reconcile a request that
// was seconds from generating. These two answers are what let it say
// something else.
//
// BOTH DIRECTIONS, because a `Holding` that answered true always would
// satisfy the first assertion and would make the restart advice unreachable
// for the state it IS true of -- a row left `running` by an ember that is
// gone.
func TestHolding_IsTrueFromTheClaimAndNotOnlyWhileGenerating(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "sd15"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)
	id := enqueue(t, st, "rtx2080ti", "first")

	var heldDuringPlan, generatingDuringPlan bool
	q.Planner = observingPlanner{fakePlanner: pl, at: func(r genstore.Request) {
		heldDuringPlan = q.Holding(r.ID)
		_, generatingDuringPlan = q.Running()
	}}

	if q.Holding(id) {
		t.Error("a request nothing has claimed is held")
	}
	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !heldDuringPlan {
		t.Error("the request was not held while its rental was being provisioned, so the " +
			"API cannot tell that window from a row left running by a dead process")
	}
	if generatingDuringPlan {
		t.Error("Running() reported a generation during provisioning; if these two agreed " +
			"there would be nothing to tell the two refusals apart")
	}
	// AND IT IS GIVEN BACK. A finished request still held would make the
	// restart advice unreachable, which is the mirror defect.
	if q.Holding(id) {
		t.Error("the request is still held after its rental was destroyed")
	}
	if q.Holding("") {
		t.Error("an empty id is held; every request with no id would read as this one")
	}
}

// A rider is held as itself, and the request before it stops being held.
//
// `once` holds the request that OPENED the rental; a request claimed at the
// bottom of drain is a different id, and between its claim -- which writes
// `running` to its row -- and `generate` registering its stop channel, the
// same window exists. It is short, because the model is already resident, and
// a short window answered with "restart ember serve" is still answered with
// the one action that destroys the rental. Reinstating drain's own hold is
// green against every other test in this package, which is why this one is
// here.
//
// OBSERVED FROM INSIDE THE BATCH, which is where the two can be told apart:
// `generate` sets `current` and `stop` and never touches `held`, so by this
// point `held` is whatever the last claim recorded -- the rider with drain's
// hold in place, and the opener without it.
func TestHolding_FollowsTheClaimOntoARiderAndNotTheRequestThatOpenedIt(t *testing.T) {
	served := &fakeServed{caps: sdserver.Capabilities{Model: "sd15"}}
	pl := &fakePlanner{served: served}
	q, st := newQueue(t, pl)

	opener := enqueue(t, st, "rtx2080ti", "first")
	rider := enqueue(t, st, "rtx2080ti", "second")

	type seen struct{ opener, rider bool }
	got := map[int]seen{}
	served.onStop = func(_ <-chan struct{}, n int) error {
		got[n] = seen{opener: q.Holding(opener), rider: q.Holding(rider)}
		return nil
	}

	if _, err := q.once(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("batches = %d, want the rental to have served both", len(got))
	}
	if !got[0].opener || got[0].rider {
		t.Errorf("during the opener's batch: opener held = %v, rider held = %v",
			got[0].opener, got[0].rider)
	}
	if !got[1].rider {
		t.Error("the rider is not held while it is the request on the rental, so a cancel " +
			"in the gap before its stop channel exists would be told to restart ember serve")
	}
	if got[1].opener {
		t.Error("the opener is still held while the rider is the one on the rental; one " +
			"drain goroutine holds one request, and two would make the value a guess")
	}
}
