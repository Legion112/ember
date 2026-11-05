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
// THERE ARE TWO KINDS OF WORK NOW AND STILL ONE PLACE IT RUNS. `ember up`
// submitted over the unix socket is not a queued request -- see direct.go for
// the four reasons it cannot be one -- so it does not arrive through
// ClaimNext. It arrives through Submit, which is a RENDEZVOUS on an
// unbuffered channel that only this loop receives from: one iteration of the
// loop does either a direct run or a claim-and-drain, never both, so the
// invariant above is unchanged rather than re-argued. A submission that
// arrives while a rental is in flight waits in the channel until the
// iteration ends, which is the honest expression of "the one goroutine is
// busy" -- and the client is told it is waiting rather than refused.
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
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/tts"
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

	// WithSpoken is the same for a speech rental, which has no server to
	// start and therefore nothing to wait for.
	//
	// TWO METHODS AND NOT ONE TAKING A KIND, because the two closures are
	// handed different things: a `Served` publishes what the server can do
	// and a `Spoken` publishes nothing. One method returning an `any` would
	// put a type assertion in the drain loop, which is the dispatch this
	// change exists to remove rather than relocate.
	WithSpoken(ctx context.Context, sink report.Sink, fn func(context.Context, app.Spoken) error) error
}

// Queue is the drain loop.
type Queue struct {
	Store   genstore.Store
	Planner Planner

	// Workloads is what the operator's file declares, resolved once at
	// startup by the one function that reads it.
	//
	// ON THE QUEUE AND READ NOWHERE ELSE ON THIS PATH, which is where the
	// seal lands: a stored row carries an entry **id** and no image and no
	// path, so this is the only thing that can turn one into a voice filename
	// -- and it comes from the config this process was started with, never
	// from a request. The IMAGE is resolved from the same file one layer down,
	// by `Narrow`, which is the planner's business and not the loop's.
	//
	// IT WAS A `tts.Catalogue` FOR THE WHOLE PROCESS, which stopped being
	// right when the image moved into the entry: two declared speech
	// workloads are two images with two voice sets on them, and one list
	// would offer each one's ids for the other's node.
	//
	// Empty means this process can be asked for nothing, which `webapi`
	// refuses at enqueue so that no row reaches the loop that could not be
	// resolved.
	Workloads []config.Workload

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

	// runs is the second way work reaches this loop, and the only other one.
	//
	// UNBUFFERED, which is the whole mechanism: a send succeeds exactly when
	// this loop is between iterations, so Submit returning means the loop owns
	// the run. A buffer would turn that into "accepted, and it will start some
	// time", which is the shape a counter has -- the handler would have to be
	// told how many were already waiting in order to say anything true. See
	// Submit and direct.go.
	runs     chan Direct
	runsOnce sync.Once

	mu      sync.Mutex
	current genstore.ReqID

	// held is the request this loop has CLAIMED, which is not the same as
	// the one it is generating.
	//
	// THE GAP BETWEEN THE TWO IS A 409'S REMEDIATION. `current` is set by
	// generate, so it is empty for the whole of provisioning -- the plan,
	// the create, the wait for sshd, the gpu probe, the asset verify and the
	// model load -- while the row has read `running` since ClaimNext. A
	// cancel arriving in that window reaches a queue with nothing to signal,
	// and webapi answered it by telling the operator to restart
	// `ember serve`: the one action that destroys the rental they are paying
	// for, in order to reconcile a request that was seconds from generating.
	// This field is what tells that window from the state the message is
	// true of -- a row left `running` by a process that is gone, which Run's
	// Interrupt sweep clears on the next start.
	//
	// ONE VALUE AND NOT A SET, for the reason `stop` is one channel: there
	// is one drain goroutine and a rental lives inside one iteration of its
	// loop, so "the request this process holds" is a single value and a
	// counter that could be wrong is not needed. Set at each claim and
	// cleared when `once` returns, so it covers the teardown as well: a
	// request whose destroy is still running has not been handed back to
	// anybody.
	held genstore.ReqID

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

// Holding reports whether this process has claimed that request -- anywhere
// between the claim and the end of its batch, generating or not.
//
// IT IS NOT Running. The two differ for exactly as long as a rental takes to
// come up, and that difference is the only thing that can tell a cancel
// arriving during provisioning -- where a rental IS in this process and is
// about to generate -- from a row left `running` by an ember that is gone.
// Telling those apart is the whole of webapi's two 409s, because the
// remediation for the second destroys the rental in the first.
func (q *Queue) Holding(id genstore.ReqID) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return id != "" && q.held == id
}

// hold records which request this loop is responsible for; "" is none.
func (q *Queue) hold(id genstore.ReqID) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.held = id
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

	// pending is a submitted run this loop has taken and not yet started.
	//
	// IT IS A VALUE AND NOT A QUEUE, for the reason `held` and `stop` are
	// single values: the channel is unbuffered, so the loop holds at most one
	// run at a time and every other submitter is still parked on its own
	// send. A slice here would be a backlog this loop owned, which is the
	// thing Submit's rendezvous exists not to create -- a submission the
	// client could not be told the position of.
	var pending *Direct

	for {
		if ctx.Err() != nil {
			// A SUBMISSION THIS LOOP TOOK AND NEVER STARTED LEAVES THROUGH
			// HERE, and it used to leave silently. Called unconditionally so
			// the ordinary shutdown -- `pending` nil -- goes through the same
			// line rather than through a condition that is almost always
			// false.
			q.dropPending(pending)
			return nil
		}

		// A SUBMITTED RUN IS TAKEN BEFORE A CLAIM, and the non-blocking shape
		// is what makes that true without starving the queue: a submission
		// already parked on the channel goes first, so it is not overtaken by
		// a request queued while it was waiting for the last rental to end,
		// and a loop with nothing parked falls straight through to ClaimNext.
		if pending == nil {
			select {
			case d := <-q.handoff():
				pending = &d
			default:
			}
		}

		var (
			worked bool
			err    error
		)
		if pending != nil {
			d := *pending
			pending = nil
			worked, err = true, q.direct(ctx, d)
		} else {
			worked, err = q.once(ctx)
		}
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
		case d := <-q.handoff():
			// RECEIVED HERE AND NOT ONLY AT THE TOP, because an idle loop is
			// parked in this select and a submission must not wait out
			// idlePoll to be noticed. Kicking instead -- Submit calling Kick
			// and then parking on the send -- races: the loop can reach the
			// top, find nothing parked yet, claim nothing, and come back here
			// having already consumed the kick, so the run would start up to
			// fifteen seconds later for no reason a reader could see.
			//
			// AND IT IS A SIBLING OF ctx.Done() ABOVE, which is the case
			// dropPending exists for: both are ready at a shutdown that lands
			// while a submission is parked, Go picks uniformly, and taking the
			// handoff means the run is held across one more top-of-loop
			// `ctx.Err()` that returns without starting it. Submit has already
			// answered nil by then. See Submit's own comment for the
			// measurement and for why that outcome is right and only its
			// account was wrong.
			pending = &d
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

	// HELD FROM THE CLAIM AND NOT FROM THE GENERATION. Everything between
	// here and drain's first iteration is provisioning, and the row has read
	// `running` since the statement above -- so this is the window a cancel
	// used to be answered in with "restart ember serve". Cleared when this
	// returns, which is after the deferred destroy: a request whose rental is
	// still being torn down is still this process's.
	q.hold(first.ID)
	defer q.hold("")

	rental, err := q.Planner.Plan(ctx, first)
	if err != nil {
		// Nothing was rented, so nothing is billing. The request carries the
		// reason, which is the whole point of resolving before renting.
		q.fail(first.ID, err)
		return true, fmt.Errorf("request %s: %w", first.ID, err)
	}

	// THE WORKLOAD IS IN Compat, so a speech request cannot ride an sd rental
	// and a request for one declared image workload cannot ride another's.
	// Without it the provider and the GPU would match, the claim would
	// succeed, and the drain loop would hand an utterance to a resident
	// sd-server that has no route for it -- or a FLUX batch to a server
	// holding SD1.5 -- after a card had been paid for. See genstore.Compat
	// for why the entry id and not the driver.
	compat := genstore.Compat{
		Workload: genstore.WorkloadOf(first.Workload, first.Kind),
		Kind:     genstore.KindOf(first.Kind),
		Provider: first.Provider,
		GPU:      first.GPU,
	}

	// Released after the rental returns and not inside it: the rental's cost
	// and a destroy that failed arrive from the deferred teardown, which runs
	// after the closure below has finished.
	var served []genstore.ReqID
	defer func() { q.release(served...) }()

	// ONE RENTAL EITHER WAY, AND STILL ONE DRAIN GOROUTINE. This is a branch
	// over which rental to open, not over where the work happens: both arms
	// call q.drain, which is called from nowhere else, and the rental lives
	// inside this one iteration of Run's loop. Nothing here is a second path
	// that could rent -- cmd/check-coverage's TestNothingRentsOutsideInternalApp
	// is what bounds that, and neither arm names provision.WithNode.
	var rentErr error
	switch genstore.KindOf(first.Kind) {
	case genstore.KindTTS:
		// THE ENTRY'S OWN CATALOGUE, resolved from the file this process was
		// started with. Narrow has already refused a row whose entry no
		// longer has this driver, so the lookup below cannot land on an sd
		// entry -- and a miss here is a row whose entry was deleted between
		// the claim and now, which is a refusal rather than a rental.
		cat, cerr := q.voicesFor(first)
		if cerr != nil {
			q.endIfUnfinished(first.ID, cerr)
			return true, cerr
		}
		rentErr = rental.WithSpoken(ctx, q.lifecycle(first.ID), func(ctx context.Context, sp app.Spoken) error {
			// NO PutCaps, and that is the absence rather than an omission:
			// piper publishes no capabilities, so there is nothing to cache
			// and nothing a request could be validated against that
			// internal/tts does not already check before anything is
			// rented. A `caps` row written here would be an sd server's
			// shape holding a speech rental's facts.
			return q.drain(ctx, ttsRunner{sp: sp, cat: cat}, lease, compat, first, &served)
		})
	default:
		rentErr = rental.WithServed(ctx, q.lifecycle(first.ID), func(ctx context.Context, s app.Served) error {
			// Learned once per process and cached, so a mistyped sampler is
			// refused at the moment it is typed rather than a minute into a
			// rented card. `sd.serve` is fixed by the config, so every rental
			// in this process reports the same capabilities.
			if err := q.Store.PutCaps(ctx, s.Caps()); err != nil {
				q.log().Warn("could not cache the server's capabilities; the next request will be "+
					"checked against a card instead of against a cache", "err", err)
			}
			return q.drain(ctx, sdRunner{s: s}, lease, compat, first, &served)
		})
	}
	if rentErr != nil {
		// The rental broke. If drain never reached this request, nothing has
		// written how it ended and the row still says `running`; if it did,
		// this leaves what drain wrote alone.
		q.endIfUnfinished(first.ID, rentErr)
	}
	return true, rentErr
}

// drain generates each request in turn on one rental.
//
// It returns when nothing compatible is left, which destroys the node: no idle
// timeout, because a node with nothing to do is a node billing for nothing.
func (q *Queue) drain(ctx context.Context, s runner, lease string, compat genstore.Compat, first genstore.Request, served *[]genstore.ReqID) error {
	req := first
	for {
		*served = append(*served, req.ID)
		// A rider is held too, from the moment it is claimed: the claim at
		// the bottom of this loop writes `running` to its row, and there is
		// a real gap before generate registers its stop channel -- the model
		// is resident by now, so it is short, and a short window answered
		// with a destructive remediation is still answered wrongly.
		q.hold(req.ID)
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
		q.set(req.ID, func(r *genstore.Request) { r.Model = s.Model() })
		err := q.generate(ctx, s, req)
		q.finish(req.ID, err)
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
func (q *Queue) generate(ctx context.Context, s runner, req genstore.Request) error {
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

	// THE CONVERSION FROM A STORED ROW TO A DRIVER'S BATCH MOVED TO THE
	// RUNNER, which is what made a second kind possible without a switch
	// here: this function is the stop channel's registration and nothing
	// else, and it reads the same for both kinds.
	dir := q.dirOf(req.ID)
	snk := q.sinkFor(req.ID)
	err := s.Generate(ctx, snk, req, dir, stop)
	snk.flush()
	return err
}

// fail records a request that never got a node.
//
// THE THIRD WRITER OF A TERMINAL STATUS, found by sweeping the class #159
// names two of. Its own call site says "nothing was rented, so nothing is
// billing -- the request carries the reason, which is the whole point of
// resolving before renting", and that was untrue in one window: Planner.Plan
// takes the drain loop's context, so a SIGINT landing during the offer
// listing, the store pre-flight or the period check returned a context error
// AND left this write unable to land. Same writeCtx for the same two reasons.
//
// Through classify rather than a literal StatusFailed, because detaching the
// context without that would have traded a row with no reason for a row with a
// confident wrong one: a plan a money ceiling refused is a failure, and a plan
// the operator interrupted is not. There is one account of what Ctrl-C means
// in this package and this is now the third caller of it.
func (q *Queue) fail(id genstore.ReqID, cause error) {
	status, msg := classify(cause)

	ctx, cancel := writeCtx()
	defer cancel()

	if uerr := q.Store.Update(ctx, id, func(r *genstore.Request) {
		r.Status = status
		r.Err = msg
		r.Ended = q.at()
	}); uerr != nil {
		q.log().Error("could not record a failed request", "request", id, "err", uerr)
	}
}

// classify maps the error a request ended with onto the row it leaves behind.
//
// ONE FUNCTION, AND FOR A WHILE THERE WERE TWO. The writers are fail, for a
// request that never got a node; drain, for one it generated; once, for one
// whose rental broke before drain could reach it; and Recorder.Finish, which
// is how `ember up` and a run submitted over the socket record the same fact.
// Two copies of this switch are two accounts of what Ctrl-C means -- and
// record.go held the second one, `statusOf`, byte-for-byte the same answers
// under a comment claiming to be the single spelling, while this one's comment
// claimed the same thing. See the note where it was: it predated this function
// and the commit that extracted this one in the name of having only one is the
// commit that gave the package two.
//
// The interesting answer is the one that is not obvious: a cancelled context
// is NOT a failure.
func classify(cause error) (genstore.Status, string) {
	switch {
	case cause == nil:
		return genstore.StatusDone, ""
	case errors.Is(cause, app.ErrCancelled):
		// The operator asked for this one. Recorded with its reason, which
		// names how far it got and whether the job in flight could be
		// stopped -- the event stream has the detail, and the row is what
		// `ember ls` and the queue table read.
		return genstore.StatusCancelled, cause.Error()
	case errors.Is(cause, context.Canceled), errors.Is(cause, context.DeadlineExceeded):
		// Not a failure. The operator pressed Ctrl-C, or the node reached its
		// deadline; calling that a failed generation would send them looking
		// for a bug that is not there.
		//
		// THERE IS A THIRD CAUSE AND IT IS CONFLATED WITH THE OTHER TWO,
		// which is #171.
		// `http.Client.Timeout` expiring satisfies
		// errors.Is(err, context.DeadlineExceeded), and internal/httpx sets a
		// 60s DefaultTimeout on every client -- so a provider that did not
		// answer inside app.NewPlan lands here, and the row then says the
		// operator stopped a run the provider never started. That is the
		// mirror of the confidently-wrong row the literal StatusFailed in
		// fail used to write, and it is recorded rather than fixed here for
		// two reasons: finish and endIfUnfinished have answered this way
		// since 001a0d3, so routing fail through this function makes the
		// third writer consistent rather than inventing the conflation, and
		// requests.err carries the provider's own sentence either way.
		//
		// The discriminator that would separate them is whether OUR context
		// ended, which is a different question from what the error wraps --
		// and it is exactly the input writeCtx's no-context-parameter design
		// removes from these writers on purpose. So telling the two apart is
		// a change to what a writer is told rather than to this switch, and
		// belongs to whoever takes #171 -- cited by number because a pointer
		// to an issue nobody can find is the shape this repository keeps
		// correcting.
		return genstore.StatusCancelled, cause.Error()
	default:
		return genstore.StatusFailed, cause.Error()
	}
}

// finish records how a request ended.
//
// IT TAKES NO CONTEXT, and that is the fix for #159 rather than an economy.
// This used to be handed the drain loop's own -- the context `ember serve`
// cancels on SIGINT, which is what produced most of the errors that reach
// here -- so the write answered `begin: context canceled` and the row kept the
// `running` ClaimNext wrote: a generation an operator stopped with Ctrl-C came
// back from the next start swept to `interrupted` with no reason anywhere. A
// parameter the caller cannot supply is a parameter nobody can get wrong,
// which is this repository's "prefer making a bug unrepresentable over testing
// for it" rule; writeCtx has both halves and the argument for each.
func (q *Queue) finish(id genstore.ReqID, cause error) {
	status, msg := classify(cause)

	ctx, cancel := writeCtx()
	defer cancel()

	if err := q.Store.Update(ctx, id, func(r *genstore.Request) {
		r.Status = status
		r.Err = msg
		r.Ended = q.at()
	}); err != nil {
		q.log().Error("could not record how a request ended", "request", id, "status", status, "err", err)
	}
}

// endIfUnfinished records how a rental ended for a request drain never
// reached, and leaves alone one it did.
//
// WHAT IT IS FOR. ClaimNext writes `running` and the terminal status is
// written by drain, inside WithServed's closure -- so every failure between
// those two points returned with the row still reading `running`: the create
// call, waiting for sshd, the GPU probe, the asset verify, the model load.
// Every one of them happens after money has been committed, and nothing
// corrected the row afterwards, because Interrupt only sweeps at startup. The
// operator was left with a request that ran for the life of the process
// against a rental that no longer existed, no reason recorded in
// requests.err, and a cancel button answering with the provisioning-window
// 409 -- "the rental is coming up" -- which by then is false.
//
// WHY IT IS CONDITIONAL AND NOT A SECOND WRITE. drain calls finish for every
// request it generates, so by the time an error reaches here the row may
// already say how it really ended -- done, or cancelled by the operator, or
// failed with the server's own sentence. An unconditional write would replace
// that with whatever the rental returned, which is this repository's
// two-writers rule exactly: the loser overwrites the one that knew. A
// teardown that failed after a successful batch is the ordinary case, and it
// must not turn a done request into a failed one.
//
// The check is inside the mutator rather than a read before it, because
// Store.Update runs SELECT, the mutator and UPDATE in one transaction: asking
// first and writing after would be the same decision with a window in it.
//
// IT TAKES NO CONTEXT, for finish's reason and more sharply: the error being
// recorded here is the one the RENTAL returned, and the ordinary way for a
// rental to return an error is a cancelled context. So this writer's most
// common input was precisely the state in which its write could not land --
// the half-working fix #159 is about, in the one case its own argument is
// written around.
func (q *Queue) endIfUnfinished(id genstore.ReqID, cause error) {
	status, msg := classify(cause)

	ctx, cancel := writeCtx()
	defer cancel()

	if err := q.Store.Update(ctx, id, func(r *genstore.Request) {
		if r.Status.Terminal() {
			return
		}
		r.Status = status
		r.Err = msg
		r.Ended = q.at()
	}); err != nil {
		q.log().Error("could not record a request whose rental failed", "request", id, "err", err)
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
	ctx, cancel := writeCtx()
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
//
// ON ITS OWN CONTEXT, for the reason finish takes none: generate's only caller
// hands it the drain loop's, and this flush runs AFTER s.Generate has returned
// -- so on Ctrl-C it was the tail of the event stream being written through
// the context that had just ended the generation it describes. The stream is
// the truth and the columns a derived index of it, which makes a lost tail the
// expensive half rather than the cheap one.
func (s *sink) flush() {
	ctx, cancel := writeCtx()
	defer cancel()
	s.buf.Flush(ctx)
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

// voicesFor is the catalogue the request's own declared workload publishes.
//
// RESOLVED FROM THE ENTRY AND NOT FROM A PROCESS-WIDE FIELD, which is what the
// image moving into the entry forces: two declared speech workloads are two
// images with two voice sets on them. The lookup is `WorkloadOf`'s, so a row
// written before the column and a row naming the implicit entry resolve the
// same way the claim did.
//
// A REFUSAL AND NOT A FALLBACK for an id that is not there. It can only happen
// when the operator deleted or renamed the entry between the claim and here,
// and the honest answer is that this row names a workload this process does not
// have -- a fallback to any other entry would rent a machine for a voice set
// nobody asked for.
func (q *Queue) voicesFor(r genstore.Request) (tts.Catalogue, error) {
	id := genstore.WorkloadOf(r.Workload, r.Kind)
	for _, w := range q.Workloads {
		if w.ID != id {
			continue
		}
		cat, err := w.TTSCatalogue()
		if err != nil {
			return nil, fmt.Errorf("the voices workload %q publishes: %w", id, err)
		}
		return cat, nil
	}
	return nil, fmt.Errorf("this request names workload %q and this ember serve declares no "+
		"such workload; nothing has been rented", id)
}
