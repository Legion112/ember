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
	"github.com/Legion112/ember/internal/node"
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
	return true, rental.WithServed(ctx, q.lifecycle(first.ID), func(ctx context.Context, s app.Served) error {
		// Learned once per process and cached, so a mistyped sampler is
		// refused at the moment it is typed rather than a minute into a
		// rented card. `sd.serve` is fixed by the config, so every rental in
		// this process reports the same capabilities.
		if err := q.Store.PutCaps(ctx, s.Caps()); err != nil {
			q.log().Warn("could not cache the server's capabilities; the next request will be "+
				"checked against a card instead of against a cache", "err", err)
		}
		return q.drain(ctx, s, lease, compat, first)
	})
}

// drain generates each request in turn on one rental.
//
// It returns when nothing compatible is left, which destroys the node: no idle
// timeout, because a node with nothing to do is a node billing for nothing.
func (q *Queue) drain(ctx context.Context, s app.Served, lease string, compat genstore.Compat, first genstore.Request) error {
	req := first
	for {
		err := q.generate(ctx, s, req)
		q.finish(ctx, req.ID, err)
		if err != nil {
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
	q.mu.Lock()
	q.current = req.ID
	q.mu.Unlock()
	defer func() {
		q.mu.Lock()
		q.current = ""
		q.mu.Unlock()
	}()

	run := sdserver.Run{Preview: req.Preview, PreviewInterval: req.PreviewInterval}
	for _, j := range req.Jobs {
		run.Jobs = append(run.Jobs, j.SD())
	}

	dir := q.dirOf(req.ID)
	sink := q.batchSink(req.ID)
	err := s.Generate(ctx, sink, run, dir)
	sink.flush(ctx)
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
	s := q.batchSink(opener)
	return report.SinkFunc(func(e report.Event) {
		s.Emit(e)
		// Written through to the row as well, so `ember ls`'s node and this
		// request can be joined without reading the event stream.
		switch e.Kind {
		case report.KindNodeReady:
			q.set(opener, func(r *genstore.Request) { r.Node = e.Node })
		case report.KindRunCost:
			q.set(opener, func(r *genstore.Request) { r.Cost = e.Cost })
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

// batchSink persists a request's events and notices what its jobs cost.
func (q *Queue) batchSink(id genstore.ReqID) *recorder {
	return &recorder{q: q, id: id}
}

// recorder buffers events and writes them to the store.
//
// BUFFERED BECAUSE A PREVIEW IS A POLL. awaitJob emits a progress event per
// distinct sampling step, and the preview lab measured seventeen frames for a
// six-step generation -- so a write per event is a transaction per frame on
// the path that is meant to be reporting progress rather than causing it.
type recorder struct {
	q  *Queue
	id genstore.ReqID

	mu  sync.Mutex
	buf []report.Event
}

// flushEvery bounds the buffer. Reached long before a batch finishes on
// anything slower than a laptop, so a watching client is never more than this
// many events behind.
const flushEvery = 8

func (r *recorder) Emit(e report.Event) {
	r.mu.Lock()
	r.buf = append(r.buf, e)
	full := len(r.buf) >= flushEvery
	// An image is worth writing through at once: it is the thing the operator
	// is waiting for, and a gallery that lags eight events behind the file on
	// disk looks broken.
	urgent := e.Kind == report.KindJobImages || e.Kind == report.KindRunFailed
	r.mu.Unlock()

	if e.Kind == report.KindJobImages {
		r.q.recordImages(r.id, e)
	}
	if full || urgent {
		r.flush(context.Background())
	}
}

// flush writes what is buffered.
func (r *recorder) flush(ctx context.Context) {
	r.mu.Lock()
	es := r.buf
	r.buf = nil
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
func (q *Queue) recordImages(id genstore.ReqID, e report.Event) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 10*time.Second)
	defer cancel()

	for i, f := range e.Wrote {
		im := genstore.Image{
			Req: id, Job: e.Index, Index: i,
			Key:   q.keyOf(id, f.Name),
			Bytes: f.Bytes,
			At:    q.at(),
		}
		if err := q.Store.AddImage(ctx, im); err != nil {
			q.log().Warn("could not record an image", "request", id, "key", im.Key, "err", err)
		}
	}
}

// Cost is a request's own card time, summed from its jobs.
//
// Separate from the rental's total for the reason lifecycle explains: this is
// what generating these images cost, and the rental also paid for
// provisioning, the image pull and the asset verify once for everything it
// served.
func Cost(es []report.Event) node.MicroUSD {
	var last node.MicroUSD
	for _, e := range es {
		if e.Kind == report.KindUnitCost {
			last = e.Spent
		}
	}
	return last
}
