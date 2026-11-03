package queue

import (
	"context"
	"log/slog"
	"path"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
)

// The write-through, and why it is one type rather than two.
//
// #47 asked whether `ember up` should enqueue through this package or write a
// terminal record of its own, and said neither was obviously right. Going
// through the queue would reuse everything below and is wrong for four
// separate reasons, each of which costs money or changes behaviour:
//
//   - Run's loop claims whatever is QUEUED, not a particular request. An
//     `ember up` that pushed a row and drained would serve the web UI's
//     backlog as well -- renting, generating and billing for other requests
//     from a command somebody ran to measure one card.
//   - Run begins by marking everything it finds running as interrupted, which
//     is right for a restarting server and wrong for a second process.
//   - AppPlanner NARROWS a request against the config, where `ember up`'s
//     applyFlags REPLACES: `--max-hourly-usd 999` raises the ceiling, because
//     the person typing it is the person who set it. Routing up through the
//     planner would silently stop that working.
//   - Enqueue refuses a request with no jobs, and two of up's three shapes
//     have none.
//
// Writing a second recorder instead is the other half of the dilemma: two
// sources of truth for one set of columns, drifting the first time a column is
// added. So the recorder moved OUT of Queue -- this file -- and both callers
// use it. The queue keeps one per request it serves; `ember up` builds one for
// its single run. There is exactly one place that knows how an event becomes a
// row, which is what the question was really about.
//
// ONE RENTAL AT A TIME IS UNTOUCHED BY ANY OF THIS. It is structural because a
// rental lives inside one iteration of one drain goroutine, and `ember up`
// does not enter that loop at all: it rents through provision.WithNode exactly
// as it always has, and this type only watches.

// RunStore is the generation history as a layer that RENTS needs it, and
// nothing more.
//
// FIVE METHODS RATHER THAN genstore.Store, for Spender's reason one direction
// over: what a layer can do to the database should be visible in its type.
// Spender is the ceiling's READ and says "this decides whether to spend
// money"; this is the WRITE that renting forces, and says "this records the
// rental it started". What it deliberately cannot reach is Enqueue and
// ClaimNext -- a queued row is exactly what a drain loop takes, so a layer
// that could write one could rent a second card from a row describing the run
// already in flight, which is the two-GPU defect this repository keeps
// recording. AppPlanner holds one of these because filling a store is a
// rental it makes on its own initiative, and a rental nobody recorded is the
// hole in the ledger #148 is about.
//
// genstore.Store satisfies it, so every existing caller passes its store
// unchanged and there is no second implementation to keep in step.
type RunStore interface {
	Adopt(ctx context.Context, r genstore.Request) (genstore.ReqID, error)
	Update(ctx context.Context, id genstore.ReqID, fn func(*genstore.Request)) error
	UpdateJob(ctx context.Context, id genstore.ReqID, idx int, fn func(*genstore.JobRun)) error
	AppendEvents(ctx context.Context, id genstore.ReqID, es []report.Event) error
	AddImage(ctx context.Context, im genstore.Image) error
}

// Recorder writes one run's event stream through to the generation history.
//
// THE NUMBERING IS NOT ITS JOB, and that separation was a real bug. Emitters
// do not set Seq -- report.Stream does -- and nothing was wrapping them, so
// every event arrived as Seq 0 and genstore's INSERT OR REPLACE on
// (request, seq) meant each one overwrote the last: a generation that produced
// a node_ready, a job_started, four progress frames, an images event and a
// cost left exactly one row. A caller hands this to report.NewStream; it is
// not a Stream itself, because a Stream inside a Stream stamps twice.
type Recorder struct {
	// Store is where the rows go, and Req which request they belong to.
	Store RunStore
	Req   genstore.ReqID

	// Key is where a file this run produced can be found, in blob.Store
	// terms, and NIL MEANS IT CANNOT BE.
	//
	// `ember serve` writes a request's images under the request's own id,
	// which is both the directory on disk and the key in the blob store --
	// one name for a path, so the row and the file cannot disagree. `ember
	// up` writes into a timestamped run directory the operator is told about
	// and no gallery serves, so a row naming one would be a second name for
	// a path that nothing reads, and `ember history rm` would offer to remove
	// files it did not put there. The image COUNTS and bytes are recorded
	// either way: they are columns on the job, which is what the ledger and
	// the aggregate read.
	Key func(name string) string

	Now func() time.Time
	Log *slog.Logger

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

func (r *Recorder) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

func (r *Recorder) at() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// sawPass notes a pass and what it says its length is.
func (r *Recorder) sawPass(job, pass, total int) {
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
func (r *Recorder) sampled(job int) (passes int, steps int64) {
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

func (r *Recorder) Emit(e report.Event) {
	r.mu.Lock()
	r.buf = append(r.buf, e)
	due := len(r.buf) >= flushEvery || r.at().Sub(r.last) >= flushAfter
	if e.Kind == report.KindJobProgress && e.TotalSteps > 0 {
		r.sawPass(e.Index, e.Pass, e.TotalSteps)
	}
	r.mu.Unlock()

	if r.Key != nil && e.Kind == report.KindJobImages {
		r.recordImages(e, false)
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
	if r.Key != nil && e.Kind == report.KindJobProgress && len(e.Wrote) > 0 {
		r.recordImages(e, true)
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
	//
	// A `cmds` STEP REACHES NONE OF THESE, and that is decided rather than
	// missed: its events carry Unit == report.UnitStep, so every case below
	// skips it. See the package comment on why a step is not a job row.
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

	// AND THE RENTAL'S OWN FACTS, written through to the request row so that
	// `ember ls`'s node, `ember spend`'s money and this request can be joined
	// without reading the event stream.
	//
	// This used to live in Queue.lifecycle, wrapped around the sink, and it is
	// here now because `ember up` needs the same columns and a second copy of
	// them is two sources of truth for one set. Nothing about WHICH request
	// gets them changed: these events reach the sink of the request that
	// OPENED the rental and no other, which is where the queue emits them --
	// see Queue.lifecycle.
	switch e.Kind {
	case report.KindNodeReady:
		r.set(func(q *genstore.Request) { q.Node = e.Node })
	case report.KindRunCost:
		r.set(func(q *genstore.Request) {
			q.Cost = e.Cost
			// The rest of the rental's own facts, promoted out of the event
			// body for the reason Cost was: billable is what the invoice
			// charges and elapsed is what the clock said, and an overhead
			// figure computed against the wrong one credits ember with a
			// saving that is not there.
			q.Rate, q.Elapsed, q.Billable = e.Rate, e.Elapsed, e.Billable
		})
	case report.KindModelResident:
		r.set(func(q *genstore.Request) {
			q.LoadMeasured, q.LoadReported = e.Measured, e.Reported
			// WHICH MODEL SERVED THIS RUN, off the /capabilities response the
			// event carries rather than off `sd.serve`: an operator can edit
			// that file between two rentals, so a reading of the config
			// attributes last week's spend to this week's weights. Empty is
			// left alone rather than written, because a server that reported
			// no model (neither --model nor --diffusion-model named) and a
			// column nobody wrote are the same dash and the queue may already
			// have filled this in from s.Caps().
			if e.Model != "" {
				q.Model = e.Model
			}
		})
	case report.KindNodeMemory:
		// To the opener, like the rest of the rental's own facts: the memory
		// limit is the NODE's, and every request that rode along ran under
		// the same one. Writing it to each would be the same multiplication
		// that attributing the whole rental's cost to every request would be.
		r.set(func(q *genstore.Request) {
			q.MemCgroup = e.MemCgroup
			q.MemLimit, q.MemCurrent = e.MemLimit, e.MemCurrent
			q.MemTotal, q.MemCached = e.MemTotal, e.MemCached
			q.AssetBytes = e.Bytes
		})
	}

	if due || urgent(e.Kind) {
		// writeCtx and not a bare Background: this runs on the emitter's
		// goroutine, which is the drain loop, so an unbounded write here is a
		// rental that cannot finish -- `runServed`'s bare wait() reached
		// through an event. Detached for the other half of the same reason:
		// the last urgent event of a run is emitted by a teardown whose
		// context is already cancelled.
		ctx, cancel := writeCtx()
		defer cancel()
		r.Flush(ctx)
	}
}

// urgent is an event that must not wait for the buffer.
//
// THE LAST TWO ARE THE ONES THAT WOULD BE LOST RATHER THAN LATE. run_cost and
// destroy_failed arrive from provision.WithNode's DEFERRED teardown, after the
// last ordinary flush and after the caller has stopped emitting -- so a
// buffered one is a buffer nobody will drain, and what goes missing is what
// the rental cost and the one line an operator must not miss.
//
// run_failed WAS IN THIS LIST AND WAS EMITTED BY NOTHING, so the list
// reserved for events that cannot afford to wait named one that never
// arrived. It is gone: a request's failure is `requests.status` and
// `requests.err`, which are columns the socket reads as current state rather
// than events a buffer could lose -- see report.go, where the two kinds were
// declared. The rest are
// here because they arrive once per rental, so flushing them costs nothing,
// and because an image is the thing the operator is waiting for: a gallery
// that lags behind the file on disk looks broken.
func urgent(k report.Kind) bool {
	switch k {
	case report.KindJobImages,
		report.KindServerStarting, report.KindModelResident,
		report.KindNodeReady, report.KindNodeMemory,
		report.KindRunCost, report.KindDestroyFailed:
		return true
	}
	return false
}

// sampledLocked reads the pass tally under the lock.
func (r *Recorder) sampledLocked(job int) (int, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sampled(job)
}

// recordJob writes one job's execution facts through to its row.
//
// Off the event stream's own hot path in the sense that matters: a failure is
// logged and not returned, for the reason Flush does it -- losing a figure is
// not a reason to abandon a generation that is already paid for. The log line
// names the index, because an off-by-one between the 1-based job number the
// events carry and the key the table uses would write every figure to nowhere
// and leave the columns at zero with nothing saying so.
func (r *Recorder) recordJob(idx int, fn func(*genstore.JobRun)) {
	if idx <= 0 {
		return
	}
	ctx, cancel := writeCtx()
	defer cancel()
	if err := r.Store.UpdateJob(ctx, r.Req, idx, fn); err != nil {
		r.log().Warn("could not record what a job did",
			"request", r.Req, "job", idx, "err", err)
	}
}

// set applies fn to the request row.
//
// WithoutCancel: this runs from a teardown whose context is already cancelled
// on Ctrl-C, and the row is the only record of what the rental cost.
func (r *Recorder) set(fn func(*genstore.Request)) {
	ctx, cancel := writeCtx()
	defer cancel()
	if err := r.Store.Update(ctx, r.Req, fn); err != nil {
		r.log().Error("could not record a rental's lifecycle", "request", r.Req, "err", err)
	}
}

// writeTimeout bounds a write made off the event path, where the caller's own
// context may already be cancelled.
const writeTimeout = 10 * time.Second

// writeCtx is the context every write that records how something ENDED travels
// on: detached from the caller's, and bounded.
//
// BOTH HALVES, and neither is optional.
//
// Detached, for provision's destroy reason -- "Ctrl-C cancels the context the
// run was using, and a destroy call on that context fails instantly" -- with a
// store write in place of the destroy call. Ctrl-C is likewise the most likely
// way to reach these writes: stopping `ember serve` is what produces the error
// being recorded, so the context the rental used is already done by the time
// there is an outcome to write. #159 is what that cost -- Queue.finish and
// Queue.endIfUnfinished both wrote through the drain loop's own context, the
// write answered `begin: context canceled`, the row kept the `running`
// ClaimNext wrote, and the next start's Interrupt sweep turned it into
// `interrupted` with requests.err empty. The whole of #103's argument --
// that the expensive failures all happen after money has been committed, so an
// operator has to be able to see why -- was unreachable on the one path an
// operator takes every time they stop the server.
//
// Bounded, because an unbounded write on a detached context is the second
// defect wearing the first one's clothes: `runServed`'s bare `wait()` wedged
// for twenty-three minutes after SIGTERM and never printed the error it was in
// the middle of returning, so the run looked like a hang with no diagnosis.
// `ember serve` waits for queue.Run before it exits, so a write that cannot
// complete is a server that will not stop -- in the code whose job is to
// record why the last one did not. A write that cannot land inside the bound is
// abandoned and the failure logged, which is the honest trade: a figure lost
// against a shutdown that never finishes.
//
// One function rather than a copy per call site, which is this repository's
// "one name for a path, not one per call site" rule met in a context: ten
// writes take it, where there were five inline spellings of it and one site
// that had the detachment and no bound at all. A copy per call site is a place
// for one of them to lose a half, and one of them had.
func writeCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(context.Background()), writeTimeout)
}

// Flush writes what is buffered.
func (r *Recorder) Flush(ctx context.Context) {
	r.mu.Lock()
	es := r.buf
	r.buf = nil
	r.last = r.at()
	r.mu.Unlock()

	if len(es) == 0 {
		return
	}
	if err := r.Store.AppendEvents(ctx, r.Req, es); err != nil {
		// Logged and not returned: losing a progress line is not a reason to
		// abandon a generation that is already paid for.
		r.log().Warn("could not record progress", "request", r.Req, "events", len(es), "err", err)
	}
}

// Finish records how a run ended, and flushes whatever is left.
//
// IN THIS ORDER, because the event stream is the truth and the columns are a
// derived index of it: a status written over an unflushed buffer would be a
// row saying "done" above events that never arrived.
func (r *Recorder) Finish(ctx context.Context, cause error) {
	r.Flush(ctx)
	status, msg := classify(cause)
	r.set(func(q *genstore.Request) {
		q.Status = status
		q.Err = msg
		q.Ended = r.at()
	})
}

// statusOf is gone: it was a second copy of classify, in the same package.
//
// Deleted rather than kept, because the two were the same switch with the same
// answers and each carried a comment claiming to be the only one -- this one
// said "one spelling for the queue and for `ember up`", and queue.go's said
// "two copies of this switch would be two accounts of what Ctrl-C means, which
// is why there is one". By `git log -S`, this one came FIRST (cbd3f14, the
// recorder `ember up` writes through) and classify was extracted four files
// over by 001a0d3, the commit whose whole argument is that there must be one:
// the change that wrote the rule created the second copy of it. Nothing
// reported it, because a copy with a caller is a live function to every
// detector here. The surviving name is classify, which is the one AGENTS.md
// and both issues cite, and its doc carries this one's claim.

// recordImages joins the files a job wrote to the request that asked for them.
//
// The names come off the event, which got them from sdserver.WriteImages --
// the one place that decides what an image is called. Deriving them here would
// be a second spelling of the same rule, and the two would drift the first time
// a stem is sanitised differently, leaving the gallery pointing at files that
// are not on disk.
// A preview row is overwritten rather than accumulated: AddImage keys on
// (request, job, index, preview), and the frame on disk is written over on
// every distinct step, so one row per job tracks the file for the whole
// generation.
func (r *Recorder) recordImages(e report.Event, preview bool) {
	ctx, cancel := writeCtx()
	defer cancel()

	for i, f := range e.Wrote {
		im := genstore.Image{
			Req: r.Req, Job: e.Index, Index: i,
			Key:     r.Key(f.Name),
			Bytes:   f.Bytes,
			Preview: preview,
			At:      r.at(),
		}
		if err := r.Store.AddImage(ctx, im); err != nil {
			r.log().Warn("could not record an image", "request", r.Req, "key", im.Key, "err", err)
		}
	}
}

// BlobKey is where a request's files live in the blob store, named once.
//
// The directory on disk and the key in the store are derived from the same
// field so they cannot point at different places -- which is why Queue.dirOf
// and this function both exist and neither spells the join twice.
func BlobKey(id genstore.ReqID, name string) string { return path.Join(string(id), name) }
