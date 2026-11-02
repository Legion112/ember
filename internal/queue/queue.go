// Package queue drains generation requests onto one rental.
//
// THE SHAPE, which is what was asked for: a request is a batch of jobs. While
// a node is up it keeps taking newly queued requests whose selection matches,
// and the moment there is none left it returns -- which destroys the node at
// once, with no idle timeout, because a node with nothing to do is a node
// billing for nothing.
//
// ONE RENTAL IS STRUCTURAL AND NOT CHECKED. There is one drain goroutine, and
// a rental lives inside one iteration of its loop, so two concurrent rentals
// cannot be expressed. The alternative -- a handler that starts a rental per
// request, with a counter saying no to the second -- is a counter that can be
// wrong, and being wrong costs two GPUs. A double-clicked button cannot rent
// twice here because there is nothing for the second click to start.
//
// THAT IS ALSO THE SHUTDOWN STORY. Because the rental is inside the loop,
// cancelling the loop's context unwinds through provision.WithNode's deferred
// destroy -- which runs on a context that survives the cancellation -- and Run
// does not return until that has happened. A caller shutting down waits for
// one goroutine rather than tracking a set of them, and a server that exits
// while a node is alive is the most expensive bug available here.
//
// PARALLELISM IS DELIBERATELY ABSENT. sd-server's generation context lock
// serializes work on the device anyway, so a second concurrent job buys
// nothing -- and a second concurrent *rental* doubles the bill. If it is ever
// wanted, it is a second drain goroutine and a Compat that can say "not this
// node", not a change here.
package queue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
)

// idlePoll is how often the loop looks for work it was not told about.
//
// A fallback and not the mechanism: Kick is what the API calls after an
// enqueue, so the ordinary latency is zero. This exists because a kick
// delivered while the loop was busy is a kick the loop did not hear, and the
// alternative to a slow poll is a request that sits queued until the next
// one arrives.
const idlePoll = 15 * time.Second

// Planner turns a request into something that can be rented.
//
// An interface so the drain loop is testable with no Docker, no provider and
// no card -- which matters because what is being tested here is money
// behaviour, and the expensive bugs are in the loop rather than in the
// provisioning it calls.
type Planner interface {
	// Plan resolves what a request would rent and rents nothing.
	Plan(ctx context.Context, r genstore.Request) (Rental, error)
}

// Rental is a plan that has not been rented yet.
type Rental interface {
	// WithServed rents the node, makes the weights resident and calls fn. The
	// node is destroyed when fn returns, whatever it returns.
	//
	// sink receives the rental's own lifecycle: the node becoming ready, what
	// it cost, and a destroy that failed.
	WithServed(ctx context.Context, sink report.Sink, fn func(context.Context, app.Served) error) error
}

// Queue is the drain loop.
type Queue struct {
	Store   genstore.Store
	Planner Planner

	// Outputs is the directory requests write into.
	//
	// A request's own files go in a subdirectory named after the request,
	// which is ALSO its blob key prefix -- one name for a path, so a row in
	// the database and a file on disk cannot disagree about where something
	// is. Two requests on one rental therefore cannot write over each
	// other's images, which a shared per-rental directory would allow the
	// moment two of them used the same output stem.
	Outputs string

	// Out is deliberately absent. The node's byte streams reach the operator
	// through AppPlanner.Out, which is the writer app.NewPlan is handed --
	// and a second copy on the Queue was set by `ember serve`, read by an
	// accessor nothing but two tests called, and therefore carried nothing
	// for the life of the field. It is the ninth instance of the defect
	// cmd/check-coverage's TestNoProductionDeadCode now reports, and the
	// first one a detector found rather than a person.

	Log *slog.Logger
	Now func() time.Time

	kick     chan struct{}
	kickOnce sync.Once

	mu      sync.Mutex
	current genstore.ReqID

	// stop is closed to cancel `current`, and nil once it has been.
	//
	// ONE CHANNEL AND NOT A MAP, for the reason there is one drain
	// goroutine: a rental lives inside one iteration of the loop and one
	// request generates at a time, so "the cancel signal for the request
	// that is generating" is a single value. A map keyed by request id would
	// be a set of signals only one of which can ever be listened to, and the
	// other entries would be a leak with a plausible-looking lookup over it.
	//
	// Set to nil by Cancel rather than closed twice: a double close panics,
	// and a double-clicked cancel button is the ordinary case rather than
	// the exotic one.
	stop chan struct{}

	// sinks is one event stream per request, memoised.
	//
	// ONE PER REQUEST AND NOT ONE PER CALL, because the sequence number is
	// the whole contract with a watching client: it asks for everything
	// after what it has seen, and two numberings for one request means
	// events that overwrite each other. A rental's lifecycle events and its
	// batch's progress go to the same request, so they have to come from the
	// same counter.
	sinks map[genstore.ReqID]*sink
}

func (q *Queue) log() *slog.Logger {
	if q.Log != nil {
		return q.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (q *Queue) at() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

// dirOf is where a request's files are written: a real path, because
// sdserver.WriteImages opens an os.Root over it.
func (q *Queue) dirOf(id genstore.ReqID) string {
	return filepath.Join(q.Outputs, string(id))
}

func (q *Queue) kicks() chan struct{} {
	q.kickOnce.Do(func() {
		if q.kick == nil {
			// Buffered by one. A kick while the loop is busy must not block
			// the API handler that sent it, and a second kick before the loop
			// wakes is the same information as the first.
			q.kick = make(chan struct{}, 1)
		}
	})
	return q.kick
}

// Kick tells the loop there may be work. Never blocks.
func (q *Queue) Kick() {
	select {
	case q.kicks() <- struct{}{}:
	default:
	}
}

// Running reports which request is generating right now, if any.
func (q *Queue) Running() (genstore.ReqID, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.current, q.current != ""
}

// Cancel stops the generation of one request and reports whether it reached
// it.
//
// THE RENTAL SURVIVES IT, which is the whole difference between this and what
// `ember serve` used to offer. Nothing here touches a context: cancelling the
// drain loop's would unwind through provision.WithNode's deferred destroy and
// take the node away, and with it every other request queued onto the same
// rental -- which is what the API's old 409 was protecting, correctly, against
// a mechanism that did not exist yet. Closing a channel ends one batch; the
// node, the server process and the resident weights are untouched, and the
// next claim is served by a card that is already warm.
//
// Kick's shape and for Kick's reason: the API has to reach the drain loop, and
// a handler must never block on it. False means this request is not the one
// generating -- it was already cancelled, or it has finished, or a different
// one is running -- and the caller must not read that as "stopped".
func (q *Queue) Cancel(id genstore.ReqID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if id == "" || q.current != id || q.stop == nil {
		return false
	}
	close(q.stop)
	// FORGOTTEN RATHER THAN CLOSED AGAIN, and this line is the one that
	// matters: a double close panics, in the drain goroutine, with a node
	// running -- and a double-clicked cancel button is the ordinary case
	// here, not the exotic one. Reinstating the bug makes
	// TestCancel_IsSafeToAskTwice report `close of closed channel`.
	q.stop = nil
	return true
}

// Run drains until ctx ends.
//
// It returns only once any rental in flight has been destroyed, which is what
// makes "wait for this goroutine" a complete shutdown.
func (q *Queue) Run(ctx context.Context) error {
	// Anything found running belonged to a process that is gone. Its rental
	// cannot be resumed, so it is marked rather than re-queued -- see
	// genstore.StatusInterrupted.
	if n, err := q.Store.Interrupt(ctx); err != nil {
		q.log().Error("could not mark interrupted requests; the history is wrong but the queue still works", "err", err)
	} else if n > 0 {
		q.log().Warn("requests were in flight when ember last stopped; their rentals are gone", "count", n)
	}

	for {
		if ctx.Err() != nil {
			return nil
		}

		worked, err := q.once(ctx)
		if err != nil {
			// Reported and not returned. A request that could not be planned
			// is that request's problem; ending the loop would mean one bad
			// config stops every later request from running.
			q.log().Error("a rental ended badly", "err", err)
		}
		if worked {
			// Straight round again: a request may have arrived while that
			// rental was up, and it should not wait for a poll.
			continue
		}

		select {
		case <-ctx.Done():
			return nil
		case <-q.kicks():
		case <-time.After(idlePoll):
		}
	}
}

// once claims a request, rents for it, and drains everything compatible onto
// that one rental. It reports whether it did anything.
func (q *Queue) once(ctx context.Context) (bool, error) {
	lease := string(genstore.NewReqID())

	first, ok, err := q.Store.ClaimNext(ctx, lease, genstore.Compat{})
	if err != nil {
		return false, fmt.Errorf("claim a request: %w", err)
	}
	if !ok {
		return false, nil
	}

	rental, err := q.Planner.Plan(ctx, first)
	if err != nil {
		// Nothing was rented, so nothing is billing. The request carries the
		// reason, which is the whole point of resolving before renting.
		q.fail(ctx, first.ID, err)
		return true, fmt.Errorf("request %s: %w", first.ID, err)
	}

	compat := genstore.Compat{Provider: first.Provider, GPU: first.GPU}

	// Released after WithServed returns and not inside it: the rental's cost
	// and a destroy that failed arrive from the deferred teardown, which runs
	// after the closure below has finished.
	var served []genstore.ReqID
	defer func() { q.release(served...) }()

	return true, rental.WithServed(ctx, q.lifecycle(first.ID), func(ctx context.Context, s app.Served) error {
		// Learned once per process and cached, so a mistyped sampler is
		// refused at the moment it is typed rather than a minute into a
		// rented card. `sd.serve` is fixed by the config, so every rental in
		// this process reports the same capabilities.
		if err := q.Store.PutCaps(ctx, s.Caps()); err != nil {
			q.log().Warn("could not cache the server's capabilities; the next request will be "+
				"checked against a card instead of against a cache", "err", err)
		}
		return q.drain(ctx, s, lease, compat, first, &served)
	})
}

// drain generates each request in turn on one rental.
//
// It returns when nothing compatible is left, which destroys the node: no idle
// timeout, because a node with nothing to do is a node billing for nothing.
func (q *Queue) drain(ctx context.Context, s app.Served, lease string, compat genstore.Compat, first genstore.Request, served *[]genstore.ReqID) error {
	req := first
	for {
		*served = append(*served, req.ID)
		// WHICH MODEL SERVED THIS REQUEST, recorded before it generates and
		// to EVERY request on the rental rather than only to the opener.
		//
		// That is the opposite of how the rental's cost, its memory figures
		// and its load spans are written, and the difference is that those
		// are money: attributing one rental's cost to each of its requests
		// multiplies the bill, so they go to whichever request paid for the
		// node to exist. A model is a label, not a quantity -- every request
		// this rental serves ran against the same weights -- and writing it
		// to the opener alone would split one model's spend between its own
		// name and the dash, which is the figure `ember spend --by model`
		// exists to produce.
		//
		// From s.Caps() and not from the config: `sd.serve` is editable
		// between rentals, and the capabilities are a response. There is no
		// event carrying the model, so unlike the cost columns this one is
		// not a derived index of the event stream -- the response is the
		// authority, read once per rental, cached by PutCaps a few lines up.
		// AN EMPTY NAME MEANS NOBODY READ A MODEL, and never "a server
		// answered and named nothing". This write happens only after
		// WaitModel reported ready, and a server with no weights reports
		// `empty`, which that loop retries and ends as a NotReadyError -- so
		// the rental fails before a claim gets this far, and s.Caps() is only
		// ever read from a server that loaded something. What leaves the
		// column empty is an `ember up` with `cmds` and no server at all, or
		// a request from before the column existed. That is what groupExpr
		// and keyOr already act on, and what `ember history`'s footnote says
		// in words.
		//
		// The earlier version of this comment defended the same write with
		// the unreachable case -- and the shape does exist, because
		// capabilities_no_model.json really reports
		// `{"name":"","path":"","stem":""}`; it is still the right fixture
		// for the feature-gating path, which is a different question and does
		// not pass through here. A reader who believed it had two ways to
		// act, both wrong: make the column nullable to tell the two states
		// apart, which is a branch nothing can reach, or read a stored `''`
		// as evidence that a server answered with no name, when it is
		// evidence that no server was asked. A comment that is wrong about
		// why a seal holds is how the seal gets removed by somebody being
		// helpful -- the AssertNoStray comment that claimed the cleanup ran
		// "before AssertNoStray records the failure, never instead of it"
		// while the code did the opposite. Nothing is invented either way:
		// inventing a value here is the mistake the column exists to end.
		//
		// The model_resident event carries the same name, so the OPENER has
		// it written twice with the same value. That is not two sources of
		// truth -- both read one /capabilities response -- but two carriers
		// of it, and each is the only one available on its own path: the
		// queue holds the app.Served and `ember up` holds nothing but a
		// sink. A rider is why this write cannot be dropped in favour of the
		// event: the rental's own events reach the opener's sink alone.
		q.set(req.ID, func(r *genstore.Request) { r.Model = s.Caps().Model })
		err := q.generate(ctx, s, req)
		q.finish(ctx, req.ID, err)
		switch {
		case errors.Is(err, app.ErrCancelled):
			// A CANCELLED REQUEST DOES NOT STOP THE RENTAL, and that is the
			// entire point of being able to cancel one. The operator asked
			// for this batch to stop, not for the node to go away -- and the
			// requests behind it on this rental are other people's work, or
			// their own, which they said nothing about. Treating this like a
			// failure would reinstate exactly the behaviour webapi's old 409
			// refused the cancel in order to avoid, this time without the
			// warning.
			q.log().Info("a request was cancelled while generating; the rental carries on",
				"request", req.ID)
		case err != nil:
			// A failed batch stops the rental. A rental that carried on after
			// one request failed would bill for work whose premise an
			// operator is now going to re-examine, and the remaining
			// requests are still queued -- they get a fresh node.
			return fmt.Errorf("request %s: %w", req.ID, err)
		}
		if ctx.Err() != nil {
			return nil
		}

		// THE RACE THAT MATTERS, and it is why ClaimNext is one statement: a
		// request pushed a moment ago must be served by the node that is
		// still up, and one pushed after this returns must start a new
		// rental. There is no third answer, and no window in which a request
		// is neither.
		next, ok, err := q.Store.ClaimNext(ctx, lease, compat)
		if err != nil {
			return fmt.Errorf("claim another request for this rental: %w", err)
		}
		if !ok {
			return nil
		}
		req = next
	}
}

// generate runs one request's jobs and records what came back.
func (q *Queue) generate(ctx context.Context, s app.Served, req genstore.Request) error {
	stop := make(chan struct{})
	q.mu.Lock()
	q.current, q.stop = req.ID, stop
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		// Not closed here: Cancel may already have, and the only reader is
		// the Generate call this defer runs after.
		//
		// Clearing `stop` is hygiene and NOT the guard, which is worth
		// saying because it reads like one. What stops a cancel arriving a
		// moment too late from reaching the next request on the same rental
		// is the id check in Cancel -- reinstating that is what makes
		// TestCancel_DoesNotReachTheNextRequestOnTheSameRental fail, and
		// reinstating this does not. It stays so that `q.stop != nil` means
		// "a batch is in flight" and Cancel's own guard reads as the
		// invariant it is.
		q.current, q.stop = "", nil
		q.mu.Unlock()
	}()

	run := sdserver.Run{Preview: req.Preview, PreviewInterval: req.PreviewInterval}
	for _, j := range req.Jobs {
		run.Jobs = append(run.Jobs, j.SD())
	}

	dir := q.dirOf(req.ID)
	snk := q.sinkFor(req.ID)
	err := s.Generate(ctx, snk, run, dir, stop)
	snk.flush(ctx)
	return err
}

// fail records a request that never got a node.
func (q *Queue) fail(ctx context.Context, id genstore.ReqID, cause error) {
	if uerr := q.Store.Update(ctx, id, func(r *genstore.Request) {
		r.Status = genstore.StatusFailed
		r.Err = cause.Error()
		r.Ended = q.at()
	}); uerr != nil {
		q.log().Error("could not record a failed request", "request", id, "err", uerr)
	}
}

// finish records how a request ended.
func (q *Queue) finish(ctx context.Context, id genstore.ReqID, cause error) {
	status, msg := genstore.StatusDone, ""
	switch {
	case errors.Is(cause, app.ErrCancelled):
		// The operator asked for this one. Recorded with its reason, which
		// names how far it got and whether the job in flight could be
		// stopped -- the event stream has the detail, and the row is what
		// `ember ls` and the queue table read.
		status, msg = genstore.StatusCancelled, cause.Error()
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		// Not a failure. The operator pressed Ctrl-C, or the node reached its
		// deadline; calling that a failed generation would send them looking
		// for a bug that is not there.
		status, msg = genstore.StatusCancelled, cause.Error()
	case cause != nil:
		status, msg = genstore.StatusFailed, cause.Error()
	}

	if err := q.Store.Update(ctx, id, func(r *genstore.Request) {
		r.Status = status
		r.Err = msg
		r.Ended = q.at()
	}); err != nil {
		q.log().Error("could not record how a request ended", "request", id, "status", status, "err", err)
	}
}

// release drops a request's event stream once nothing more will be emitted
// for it.
//
// Not in finish: a rental's cost and a destroy that failed arrive from a
// deferred teardown, AFTER the last batch has been recorded as done, and they
// belong to the request that opened the rental. Forgetting its stream at
// finish would renumber them from 1 and overwrite the progress already
// stored.
func (q *Queue) release(ids ...genstore.ReqID) {
	for _, id := range ids {
		q.forget(id)
	}
}

// lifecycle routes a rental's own events to the request that opened it.
//
// WHICH REQUEST GETS THE RENTAL'S COST is a real question with no tidy answer,
// so the answer is written down: node_ready and run_cost go to the request
// that opened the rental, because it is the one that paid for it to exist.
// Every request that rode along gets its own per-job card time, which its own
// sink records -- and those two figures differ by exactly the per-rental
// overhead this whole shape exists to pay once. Summing the per-request
// figures and calling it the bill would understate it; attributing the whole
// rental to every request would multiply it.
//
// It is the opener's ordinary sink and nothing more, which is the point: the
// write-through of those events to the row lives in Recorder, where `ember up`
// reaches the same code rather than a second copy of it. What makes "only the
// opener is charged" true is that these events are emitted HERE, to this one
// request's sink, and a rider's sink never sees them.
func (q *Queue) lifecycle(opener genstore.ReqID) report.Sink { return q.sinkFor(opener) }

// set applies fn to a request row, off the event path.
//
// Only drain's model write uses it now: everything else a rental learns is
// written through by Recorder, from the events it already emits. WithoutCancel
// for Recorder.set's reason -- a teardown's context is already cancelled on
// Ctrl-C, and the row is the only record of what the rental cost.
func (q *Queue) set(id genstore.ReqID, fn func(*genstore.Request)) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), writeTimeout)
	defer cancel()
	if err := q.Store.Update(ctx, id, fn); err != nil {
		q.log().Error("could not record a rental's lifecycle", "request", id, "err", err)
	}
}

// sinkFor is this request's event stream, created once.
func (q *Queue) sinkFor(id genstore.ReqID) *sink {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.sinks == nil {
		q.sinks = map[genstore.ReqID]*sink{}
	}
	if s, ok := q.sinks[id]; ok {
		return s
	}
	rec := &Recorder{
		Store: q.Store, Req: id, Now: q.at, Log: q.log(),
		// The blob key, because this request's images are served by the web
		// UI out of a store rooted at Outputs -- see Recorder.Key, and
		// dirOf, which names the same path on disk.
		Key: func(name string) string { return BlobKey(id, name) },
	}
	s := &sink{Stream: report.NewStream(q.at, rec), buf: rec}
	q.sinks[id] = s
	return s
}

// forget releases a finished request's stream, so a long-running server does
// not hold one per request it has ever served.
func (q *Queue) forget(id genstore.ReqID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	delete(q.sinks, id)
}

// sink is one request's event stream: numbered, then buffered, then stored.
//
// THE NUMBERING IS WHY THIS EXISTS, and its absence was a real bug that only
// running the thing found. Emitters do not set Seq -- report.Stream does, and
// nothing was wrapping them -- so every event arrived as Seq 0, and
// genstore's INSERT OR REPLACE on (request, seq) meant each one overwrote the
// last. A generation that produced a node_ready, a job_started, four progress
// frames, an images event and a cost left exactly one row.
//
// The unit test did not catch it because its fake set Seq itself, which is
// kinder than anything real: a fake that is tidier than the thing it stands
// in for tests a program that does not exist.
type sink struct {
	*report.Stream
	buf *Recorder
}

// flush writes whatever is buffered.
func (s *sink) flush(ctx context.Context) { s.buf.Flush(ctx) }

// Cost is gone, and what replaced it is the point of the change that removed
// it.
//
// It summed a request's card time by scanning that request's entire event
// stream in Go, and it was the only aggregate anywhere in ember -- against a
// table whose facts live inside one opaque JSON blob per event, growing per
// step per job per rental. It was also called by nothing but its own test for
// the whole of its life, so the coverage floor was the only thing keeping it.
//
// genstore.Spend answers the same question with a SUM over columns, and the
// questions it can additionally answer -- per card, per day, cost per image,
// the sampling share of the bill -- are the ones an event loop could never
// have reached. Two cost-summing paths would have been the "two sources of
// truth for one number" mistake Offer.VCPU exists to avoid, so there is one.
