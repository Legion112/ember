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
	"io"
	"log/slog"
	"path"
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

	// Out carries byte streams from the node -- the server's own stdout, a
	// failing model load's complaint. ember's structured progress does not go
	// here; it goes to the store and to whoever is watching.
	Out io.Writer

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

// keyOf is the same file as the blob store names it: relative to Outputs,
// which is what that store is rooted at. The two are derived from one field
// so they cannot point at different places.
func (q *Queue) keyOf(id genstore.ReqID, name string) string {
	return path.Join(string(id), name)
}

func (q *Queue) out() io.Writer {
	if q.Out != nil {
		return q.Out
	}
	return io.Discard
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
		// An empty name is written as empty: a server that reported no model
		// (neither --model nor --diffusion-model named) is a fact, and
		// inventing a value here is the mistake the column exists to end.
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
// Every request that rode along gets its own per-job card time, which
// q.batchSink records -- and those two figures differ by exactly the
// per-rental overhead this whole shape exists to pay once. Summing the
// per-request figures and calling it the bill would understate it; attributing
// the whole rental to every request would multiply it.
func (q *Queue) lifecycle(opener genstore.ReqID) report.Sink {
	s := q.sinkFor(opener)
	return report.SinkFunc(func(e report.Event) {
		s.Emit(e)
		// Written through to the row as well, so `ember ls`'s node and this
		// request can be joined without reading the event stream.
		switch e.Kind {
		case report.KindNodeReady:
			q.set(opener, func(r *genstore.Request) { r.Node = e.Node })
		case report.KindRunCost:
			q.set(opener, func(r *genstore.Request) {
				r.Cost = e.Cost
				// The rest of the rental's own facts, promoted out of the
				// event body for the reason Cost was: billable is what the
				// invoice charges and elapsed is what the clock said, and an
				// overhead figure computed against the wrong one credits
				// ember with a saving that is not there.
				r.Rate, r.Elapsed, r.Billable = e.Rate, e.Elapsed, e.Billable
			})
		case report.KindModelResident:
			q.set(opener, func(r *genstore.Request) {
				r.LoadMeasured, r.LoadReported = e.Measured, e.Reported
			})
		case report.KindNodeMemory:
			// To the opener, like the rest of the rental's own facts: the
			// memory limit is the NODE's, and every request that rode along
			// ran under the same one. Writing it to each would be the same
			// multiplication that attributing the whole rental's cost to
			// every request would be.
			q.set(opener, func(r *genstore.Request) {
				r.MemCgroup = e.MemCgroup
				r.MemLimit, r.MemCurrent = e.MemLimit, e.MemCurrent
				r.MemTotal, r.MemCached = e.MemTotal, e.MemCached
				r.AssetBytes = e.Bytes
			})
		}
		// Flushed at once: these arrive from a deferred teardown, and a
		// buffered destroy_failed is a buffer nobody will drain.
		s.flush(context.WithoutCancel(context.Background()))
	})
}

func (q *Queue) set(id genstore.ReqID, fn func(*genstore.Request)) {
	// WithoutCancel: this runs from a teardown whose context is already
	// cancelled on Ctrl-C, and the row is the only record of what the rental
	// cost.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
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
	buf := &recorder{q: q, id: id}
	s := &sink{Stream: report.NewStream(q.at, buf), buf: buf}
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
	buf *recorder
}

// flush writes whatever is buffered.
func (s *sink) flush(ctx context.Context) { s.buf.flush(ctx) }

// recorder buffers events and writes them to the store.
//
// BUFFERED BECAUSE A PREVIEW IS A POLL. awaitJob emits a progress event per
// distinct sampling step, and the preview lab measured seventeen frames for a
// six-step generation -- so a write per event is a transaction per frame on
// the path that is meant to be reporting progress rather than causing it.
type recorder struct {
	q  *Queue
	id genstore.ReqID

	mu   sync.Mutex
	buf  []report.Event
	last time.Time

	// passes is each job's sampling passes and the step total each one
	// declared: job index -> pass -> that pass's total_steps.
	//
	// ACCUMULATED HERE RATHER THAN COUNTED FROM THE FRAMES, because ember
	// deduplicates progress on the step -- so at a poll slower than the card
	// samples, it sees fewer frames than there were steps and a count of
	// frames would measure the poll interval instead of the work. Each pass
	// declares its own total, which is a fact about the generation, so the
	// steps of a pass are read off one frame of it rather than tallied.
	//
	// In memory and written through at the points a job is already causing a
	// write: progress arrives once per step, and FLUX.2 at 2048x2048 measured
	// 140 seconds per step, so a write per frame would be a transaction on
	// the path that exists to report progress rather than to cause it.
	passes map[int]map[int]int
}

// sawPass notes a pass and what it says its length is.
func (r *recorder) sawPass(job, pass, total int) {
	if r.passes == nil {
		r.passes = map[int]map[int]int{}
	}
	if r.passes[job] == nil {
		r.passes[job] = map[int]int{}
	}
	// The largest total seen for a pass wins. A pass reports the same total on
	// every frame of it, so this is normally an assignment; taking the max
	// means a frame that arrived with a smaller figure -- a highres pass whose
	// total grows as it is set up -- cannot shorten a pass that already
	// reported more.
	if total > r.passes[job][pass] {
		r.passes[job][pass] = total
	}
}

// sampled is how many passes a job ran and how many steps across all of them.
func (r *recorder) sampled(job int) (passes int, steps int64) {
	for _, total := range r.passes[job] {
		passes++
		steps += int64(total)
	}
	return passes, steps
}

// flushEvery and flushAfter bound how far behind a watching client can be.
//
// BOTH, AND THE TIME ONE IS THE IMPORTANT ONE. A count alone looked
// sufficient and is not: progress arrives once per sampling step, and FLUX.2
// at 2048x2048 measured 140 seconds per step -- so "flush every eight events"
// is twenty minutes of silence on exactly the generation somebody is watching.
// A time bound makes the latency a property of the clock instead of a property
// of how slow the card is.
//
// The count still earns its place at the other end: sd15 at 256x256 samples in
// 0.3s and the preview lab measured seventeen frames for six steps, where a
// write per event would be a transaction per frame on the path that is meant
// to be reporting progress rather than causing it.
const (
	flushEvery = 8
	flushAfter = 250 * time.Millisecond
)

func (r *recorder) Emit(e report.Event) {
	r.mu.Lock()
	r.buf = append(r.buf, e)
	due := len(r.buf) >= flushEvery || r.q.at().Sub(r.last) >= flushAfter
	if e.Kind == report.KindJobProgress && e.TotalSteps > 0 {
		r.sawPass(e.Index, e.Pass, e.TotalSteps)
	}
	// An image is written through whatever the clock says: it is the thing
	// the operator is waiting for, and a gallery that lags behind the file on
	// disk looks broken.
	urgent := e.Kind == report.KindJobImages || e.Kind == report.KindRunFailed
	r.mu.Unlock()

	if e.Kind == report.KindJobImages {
		r.q.recordImages(r.id, e, false)
	}
	// A PREVIEW FRAME IS AN IMAGE TOO, and nothing recorded it.
	//
	// The frame was written to the run directory, the images table has had a
	// `preview` column since it was created, the handler has answered
	// `?preview=1` with `Cache-Control: no-store` since it was written, and
	// the page has looked for `i.preview` since it was written -- and no row
	// was ever inserted with it set, so every one of those was dead code and
	// the UI showed a step counter advancing beside no picture at all. The
	// whole feature was built end to end and never connected, which is the
	// closed loop AGENTS.md records for previews one layer down: everything
	// agreed with itself and nothing had been observed.
	if e.Kind == report.KindJobProgress && len(e.Wrote) > 0 {
		r.q.recordImages(r.id, e, true)
	}

	// AND THE JOB'S ROW, which is the half #32 is about. Every figure here
	// was already in the event body and queryable from nowhere: the jobs
	// table had no execution columns at all, so "what did an image cost on
	// this card" meant JSON-parsing every row of the table that grows per
	// step per job per rental.
	//
	// Written at the three moments a job already causes a write rather than
	// on every event, and the middle one matters for a run that is
	// interrupted: the images are recorded as they are produced, because they
	// were paid for, and the steps behind them are recorded with them.
	switch {
	case e.Kind == report.KindUnitStarted && e.Unit == report.UnitJob:
		r.recordJob(e.Index, func(j *genstore.JobRun) { j.Started = e.At })

	case e.Kind == report.KindJobImages:
		passes, steps := r.sampledLocked(e.Index)
		r.recordJob(e.Index, func(j *genstore.JobRun) {
			j.Images, j.Bytes = e.Files, e.Bytes
			j.Passes, j.Steps = passes, steps
		})

	case e.Kind == report.KindUnitCost && e.Unit == report.UnitJob:
		passes, steps := r.sampledLocked(e.Index)
		r.recordJob(e.Index, func(j *genstore.JobRun) {
			j.Ended, j.Cost = e.At, e.Cost
			j.Passes, j.Steps = passes, steps
		})
	}

	if due || urgent {
		r.flush(context.Background())
	}
}

// sampledLocked reads the pass tally under the lock.
func (r *recorder) sampledLocked(job int) (int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sampled(job)
}

// recordJob writes one job's execution facts through to its row.
//
// Off the event stream's own hot path in the sense that matters: a failure is
// logged and not returned, for the reason flush does it -- losing a figure is
// not a reason to abandon a generation that is already paid for. The log line
// names the index, because an off-by-one between the 1-based job number the
// events carry and the key the table uses would write every figure to nowhere
// and leave the columns at zero with nothing saying so.
func (r *recorder) recordJob(idx int, fn func(*genstore.JobRun)) {
	if idx <= 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()
	if err := r.q.Store.UpdateJob(ctx, r.id, idx, fn); err != nil {
		r.q.log().Warn("could not record what a job did",
			"request", r.id, "job", idx, "err", err)
	}
}

// flush writes what is buffered.
func (r *recorder) flush(ctx context.Context) {
	r.mu.Lock()
	es := r.buf
	r.buf = nil
	r.last = r.q.at()
	r.mu.Unlock()

	if len(es) == 0 {
		return
	}
	if err := r.q.Store.AppendEvents(ctx, r.id, es); err != nil {
		// Logged and not returned: losing a progress line is not a reason to
		// abandon a generation that is already paid for.
		r.q.log().Warn("could not record progress", "request", r.id, "events", len(es), "err", err)
	}
}

// recordImages joins the files a job wrote to the request that asked for them.
//
// The names come off the event, which got them from sdserver.WriteImages --
// the one place that decides what an image is called. Deriving them here would
// be a second spelling of the same rule, and the two would drift the first
// time a stem is sanitised differently, leaving the gallery pointing at files
// that are not on disk.
// A preview row is overwritten rather than accumulated: AddImage keys on
// (request, job, index, preview), and the frame on disk is written over on
// every distinct step, so one row per job tracks the file for the whole
// generation.
func (q *Queue) recordImages(id genstore.ReqID, e report.Event, preview bool) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()

	for i, f := range e.Wrote {
		im := genstore.Image{
			Req: id, Job: e.Index, Index: i,
			Key:     q.keyOf(id, f.Name),
			Bytes:   f.Bytes,
			Preview: preview,
			At:      q.at(),
		}
		if err := q.Store.AddImage(ctx, im); err != nil {
			q.log().Warn("could not record an image", "request", id, "key", im.Key, "err", err)
		}
	}
}

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
