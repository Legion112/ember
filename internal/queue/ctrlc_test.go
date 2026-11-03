package queue

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
)

// The Ctrl-C tests (#159).
//
// WHAT WAS WRONG. Three writers of a terminal status -- fail, finish and
// endIfUnfinished -- all wrote through the drain loop's own context, which is
// the context `ember serve` cancels on SIGINT and therefore the context that
// produced most of the errors they were recording. The write answered `begin:
// context canceled` before SQLite was reached, the row kept the `running`
// ClaimNext wrote, and the next start's Interrupt sweep -- one statement that
// sets a status and an end time and nothing else -- turned it into
// `interrupted` with requests.err EMPTY. So the whole of #103's argument, that
// the expensive failures all happen after money has been committed and an
// operator has to be able to see why, was unreachable on the one path an
// operator takes every time they stop the server.
//
// WHY NO TEST SAW IT. RecordingStore.Update ignored its context, so the fake
// wrote happily in exactly the state where the real store writes nothing --
// the seventh fake in this repository tidier than the thing it stands in for.
// Measured: teaching it to answer `begin: context canceled` left `go test
// ./...` at exit 0 across the whole tree, which is the proof that nothing
// anywhere had ever handed that fake a cancelled context.
//
// WHAT THE TESTS BELOW DRIVE. The cancellation arrives while the rental is up,
// through fakeRental's onRented hook, because ClaimNext takes the same context
// and the real store refuses a claim on a cancelled one: a test that cancelled
// before calling once would be asserting about a state the program cannot
// reach.

// TestQueue_ACtrlCdRentalRecordsWhyThroughEndIfUnfinished is the writer for a
// request drain never got to: the create call, the wait for sshd, the GPU
// probe, the asset verify, the model load.
func TestQueue_ACtrlCdRentalRecordsWhyThroughEndIfUnfinished(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	served := &fakeServed{}
	// The rental is up, SIGINT lands, and the rental returns what a cancelled
	// provisioning step returns.
	pl := &fakePlanner{served: served, onRented: cancel, rentErr: context.Canceled}
	q, st := newQueue(t, pl)

	id := enqueue(t, st, "rtx2080ti", "a paper boat")

	if _, err := q.once(ctx); err == nil {
		t.Fatal("a rental ended by Ctrl-C was reported as success")
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusCancelled {
		t.Errorf("the request ended as %q, want cancelled: a row that still says running "+
			"outlives the rental and the next start sweeps it to interrupted with no reason",
			got.Status)
	}
	// THE REASON IS THE WHOLE POINT and the status alone is not enough: the
	// Interrupt sweep writes a terminal status too, so a row can be terminal
	// and still say nothing about a $0.63 rental that spent 97% of itself
	// fetching weights.
	if got.Err == "" {
		t.Error("the request carries no reason, which is the defect: " +
			"requests.err is what the watcher and `ember history` print")
	}
	if got.Ended.IsZero() {
		t.Error("the request has no end time, so every reader treats it as still in flight")
	}
	if n := served.count(); n != 0 {
		t.Errorf("%d batch(es) generated on a rental that never came up", n)
	}
}

// TestQueue_ACtrlCdBatchRecordsWhyThroughFinish is the other writer: the
// request drain WAS generating when the signal arrived.
//
// THE ASSERTION IS THE SENTENCE AND NOT THE STATUS, because the two writers
// cover each other on this path and a weaker assertion would pass against the
// defect. With finish writing through the cancelled context its write is lost,
// the row is left non-terminal, and endIfUnfinished -- which is detached --
// then writes the status itself. What it cannot write is the same reason:
// drain wraps the batch's error as `request <id>: …` before returning it, so
// the sentence on the row says which writer landed.
func TestQueue_ACtrlCdBatchRecordsWhyThroughFinish(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// A batch that ends because the context was cancelled under it, which is
	// what s.Generate returns when SIGINT lands mid-generation. The progress
	// frame is buffered rather than urgent, so the only thing that can write
	// it is the flush generate makes once Generate has returned -- which was
	// the other write on the cancelled context.
	served := &fakeServed{
		failNth: map[int]error{0: context.Canceled},
		progress: func(sink report.Sink, _ int) {
			sink.Emit(report.Event{
				Kind: report.KindJobProgress, Unit: report.UnitStep,
				Index: 1, Pass: 1, Step: 3, TotalSteps: 4,
			})
		},
	}
	pl := &fakePlanner{served: served, onRented: cancel}
	q, st := newQueue(t, pl)

	id := enqueue(t, st, "rtx2080ti", "a paper boat")

	if _, err := q.once(ctx); err == nil {
		t.Fatal("a batch ended by Ctrl-C was reported as success")
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusCancelled {
		t.Errorf("a batch ended by Ctrl-C left the request %q, want cancelled: "+
			"recording it as failed sends the operator looking for a bug that is not there",
			got.Status)
	}
	if got.Err != context.Canceled.Error() {
		t.Errorf("the reason on the row is %q, want %q: anything else means finish's own "+
			"write never landed and endIfUnfinished wrote the row instead",
			got.Err, context.Canceled.Error())
	}
	if got.Ended.IsZero() {
		t.Error("the request has no end time, so every reader treats it as still in flight")
	}
	if n := served.count(); n != 1 {
		t.Errorf("generated %d batch(es), want 1", n)
	}

	// THE TAIL OF THE EVENT STREAM, which is the truth the columns are a
	// derived index of. generate flushes what the batch buffered after
	// Generate returns, and that flush took the same cancelled context -- so
	// the last progress an operator saw on screen was one the database never
	// got.
	es, err := st.Events(t.Context(), id, 0)
	if err != nil {
		t.Fatal(err)
	}
	var sawProgress bool
	for _, e := range es {
		if e.Kind == report.KindJobProgress {
			sawProgress = true
		}
	}
	if !sawProgress {
		t.Errorf("the batch's buffered progress never reached the store: %d event(s) stored", len(es))
	}
}

// TestQueue_ACtrlCdPlanRecordsWhyAndIsNotAFailure is the third writer, found
// by sweeping the class rather than by the issue.
//
// Planner.Plan takes the drain loop's context too, so a SIGINT landing during
// the offer listing, the store pre-flight or the period ceiling left fail
// unable to write -- and fail wrote a literal StatusFailed, so merely
// detaching it would have traded a row with no reason for a row with a
// confident wrong one. It goes through classify, which is the one account of
// what Ctrl-C means in this package.
func TestQueue_ACtrlCdPlanRecordsWhyAndIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	q, st := newQueue(t, &fakePlanner{served: &fakeServed{}})
	q.Planner = cancellingPlanner{cancel: cancel}

	id := enqueue(t, st, "rtx2080ti", "a paper boat")

	if _, err := q.once(ctx); err == nil {
		t.Fatal("a plan interrupted by Ctrl-C was reported as success")
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusCancelled {
		t.Errorf("a plan interrupted by Ctrl-C left the request %q, want cancelled", got.Status)
	}
	if got.Err == "" {
		t.Error("the request carries no reason for a plan that was interrupted")
	}
	if got.Ended.IsZero() {
		t.Error("the request has no end time, so every reader treats it as still in flight")
	}
}

// TestQueue_ACtrlCdTeardownStillLeavesADoneBatchDone is the negative control,
// and it is the assertion this change could most easily have broken.
//
// drain calls finish for every request it generates, so an unconditional write
// on the way out would replace a real outcome with whatever the rental
// returned -- the two-writers rule, where the loser overwrites the one that
// knew. A teardown that fails after a successful batch is the ordinary way to
// get here, and a done request must not become a failed one. The existing
// control for this runs with a live context, where neither write was in
// danger; this one is the same shape with the signal in it, which is the state
// the fix creates.
func TestQueue_ACtrlCdTeardownStillLeavesADoneBatchDone(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	served := &fakeServed{}
	pl := &fakePlanner{
		served:      served,
		onRented:    cancel,
		teardownErr: errors.New("destroy failed: node may still be billing"),
	}
	q, st := newQueue(t, pl)

	id := enqueue(t, st, "rtx2080ti", "a paper boat")

	if _, err := q.once(ctx); err == nil {
		t.Fatal("a teardown failure was swallowed; the operator must hear about a node that may still bill")
	}

	got, err := st.Get(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != genstore.StatusDone {
		t.Errorf("a request that generated its batch ended as %q, want done: the rental's "+
			"teardown error was written over what drain knew", got.Status)
	}
	if got.Err != "" {
		t.Errorf("a successful request carries a failure reason: %q", got.Err)
	}
	if n := served.count(); n != 1 {
		t.Errorf("generated %d batch(es), want 1", n)
	}
}

// TestQueue_ARecordingWriteCannotOutliveItsBound is the second half of the
// fix, and it is not optional.
//
// An unbounded write on a detached context is the first defect's clothes on a
// worse one: `ember serve` waits for queue.Run before it exits, so a write
// that cannot complete is a server that will not stop -- which is
// `runServed`'s bare wait() exactly, twenty-three minutes after SIGTERM with
// the error it was in the middle of returning never printed.
//
// IT CARRIES ITS OWN DEADLINE, because A WEDGE PRINTS NO `--- FAIL`: CI reads
// `go test`'s status and greps for `--- SKIP`, and a hang is invisible to both
// until the binary's own -timeout dumps goroutines naming nothing a reviewer
// can act on. So the thing being waited for is raced against the thing that
// means it will never happen, which is the browser labs' rule one layer down.
//
// The store blocks until the context it was handed is done, which is what a
// store behind a connection nobody is answering does. With writeCtx's bound
// the call returns at the bound; without it, it never returns at all.
func TestQueue_ARecordingWriteCannotOutliveItsBound(t *testing.T) {
	// Each subtest spends the bound, so they run together: the same trade
	// ws.go's ten-second write bound makes, where the parent context must
	// have no deadline of its own or a bounded parent would pass against the
	// defect as happily as against the fix.
	writers := map[string]func(q *Queue, id genstore.ReqID){
		"finish": func(q *Queue, id genstore.ReqID) {
			q.finish(id, errors.New("the batch failed"))
		},
		"endIfUnfinished": func(q *Queue, id genstore.ReqID) {
			q.endIfUnfinished(id, errors.New("the rental failed"))
		},
	}

	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			st := &blockingUpdateStore{Store: genstore.NewRecordingStore()}
			q := &Queue{Store: st, Outputs: t.TempDir()}

			done := make(chan struct{})
			go func() {
				defer close(done)
				write(q, genstore.ReqID("r1"))
			}()

			select {
			case <-done:
			case <-time.After(writeTimeout + 10*time.Second):
				t.Fatalf("%s did not return: a write that records how a run ended is "+
					"abandoned at its bound, or `ember serve` never exits", name)
			}
		})
	}
}

// cancellingPlanner is a plan interrupted by the signal, which is the one
// shape fail is reached in with a context that cannot be written through.
type cancellingPlanner struct{ cancel func() }

func (p cancellingPlanner) Plan(ctx context.Context, _ genstore.Request) (Rental, error) {
	p.cancel()
	return nil, ctx.Err()
}

// blockingUpdateStore is a store whose Update never answers until its context
// is done -- a connection pool with nobody on the other end.
//
// The rest of genstore.Store is embedded rather than reimplemented, so this
// cannot drift from the interface and nothing here has an opinion about the
// methods it is not about.
type blockingUpdateStore struct {
	genstore.Store
}

func (s *blockingUpdateStore) Update(ctx context.Context, _ genstore.ReqID, _ func(*genstore.Request)) error {
	<-ctx.Done()
	return ctx.Err()
}
