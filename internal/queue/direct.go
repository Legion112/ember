package queue

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/logx"
	"github.com/Legion112/ember/internal/report"
)

// WHY `ember up` IS NOT AN ENQUEUE, which is the whole reason this file is
// not three lines in webapi.
//
// #146 reads as a transport change -- the CLI becomes a second client of the
// API the browser already uses -- and for `ls`, `history` and `spend` it is.
// For `up` it is not, and AGENTS.md already recorded the four reasons. All
// four are still true of the code:
//
//  1. ClaimNext takes whatever is QUEUED and matches, not a named request. An
//     `up` that enqueued would be served by whichever drain loop got there
//     first, and would serve the web UI's backlog on the way -- billing a
//     measurement run for somebody else's batch.
//  2. Enqueue refuses a request with no jobs, and TWO of `up`'s three shapes
//     have none: a plain forwarded session, and a `cmds` run. `up` cannot be
//     expressed as an enqueue at all for those.
//  3. Run marks everything it finds running as interrupted, which is right
//     for a restarting server and wrong for a second process.
//  4. `up`'s overrides REPLACE where Narrow narrows, so --max-hourly-usd 999
//     raises the ceiling. That is operator-visible and it is a money ceiling.
//
// So this is not POST /api/requests with a different body. It runs `up`'s own
// pipeline -- app.NewPlan, then app.Plan.Do -- and 2 and 4 stop being
// problems the moment it is a different route: there is no Enqueue to refuse
// a jobless run, and Overrides.Apply is the function `up` itself uses. 1 and 3
// do not arise because nothing is claimed.
//
// AND ONE RENTAL STAYS STRUCTURAL, which is the part that had to be designed
// rather than reused. A run started beside the drain loop would be a SECOND
// rental: two GPUs, from a process whose whole shape is built on there being
// one. A boolean or a counter guarding it is the answer this repository
// refuses by name -- "a counter that can be wrong, and being wrong costs two
// GPUs" -- so the invariant comes from there being one place work can run.
// Submit hands the run to the drain goroutine over an unbuffered channel and
// returns when that goroutine has taken it; the loop then runs it INSIDE one
// iteration, exactly where a claimed request's rental lives. Nothing else
// receives from that channel and nothing else starts a run.
//
// WHAT A TEST CAN SEE OF THAT, because "structural" is a claim and this
// repository has been wrong about those. Three assertions in direct_test.go:
// Submit does not return while a queued rental is in flight; the fake counts
// rentals and the maximum is one; and the run's own goroutine stack contains
// (*Queue).Run, so a mechanism that moved the run beside the loop -- a
// goroutine with a mutex, which is the shape somebody would reach for -- goes
// red even if its locking were correct. The honest limit is written where
// that last one is: it proves the run is on the loop's stack and not that no
// other goroutine in the program could ever rent, which is what
// cmd/check-coverage's TestNothingRentsOutsideInternalApp bounds instead.

// Direct is a run somebody else has already planned, waiting for the one
// goroutine that can rent.
//
// A STRUCT WITH A FUNC AND NOT AN INTERFACE, which is the opposite of
// Planner beside it. An interface here would have exactly one production
// implementation and one method, and the method's body is a closure over a
// plan, a writer and a recorder that the submitter built and this package has
// no business naming -- DirectRunner.Start is where all three are decided.
// What this type is for is the HANDOFF, and a handoff needs the request id
// (so the loop can say which run it is holding) and something to call.
type Direct struct {
	// Req is the row this run is recorded against, written by Adopt before
	// the create call. The loop holds it, so a cancel arriving mid-run is
	// answered by a queue that knows the request exists.
	Req genstore.ReqID

	// Run rents, does what the run was planned to do, destroys the node and
	// records how it ended.
	//
	// IT TAKES THE LOOP'S CONTEXT AND NOT THE SUBMITTER'S, which is what
	// makes detaching a client structural rather than a promise. The HTTP
	// request that submitted this has already been answered by the time this
	// is called, so there is no request context left to cancel: a Ctrl-C on
	// the client cannot reach the rental, because the rental never held
	// anything the client owned. The daemon's own shutdown still destroys it,
	// through provision.WithNode's deferred teardown, exactly as it does a
	// queued rental's.
	Run func(context.Context) error
}

// handoff is the one channel a submitted run crosses.
//
// Created lazily for kicks()' reason -- a Queue is a struct literal in
// cmd_serve and in every test, and a constructor that had to be called would
// be one that somebody does not -- and unbuffered for the reason on the
// field.
func (q *Queue) handoff() chan Direct {
	q.runsOnce.Do(func() {
		if q.runs == nil {
			q.runs = make(chan Direct)
		}
	})
	return q.runs
}

// Submit hands a planned run to the drain goroutine and returns when that
// goroutine has taken it.
//
// IT WAITS RATHER THAN REFUSING, and that is the decision. The three answers
// available to a daemon asked to rent now while it is already renting are:
// refuse, queue it, or rent a second card. The third is unrepresentable by
// design and is the one that costs money. Queueing it is not what `up` means
// -- `up` rents now -- and it would mean telling the client a request id for
// a run that might start in an hour. Waiting is the honest third: the caller
// blocks for exactly as long as the one goroutine is busy, the client prints
// that it is waiting, and a Ctrl-C in that window cancels a SUBMISSION rather
// than a rental, because nothing has been rented.
//
// SO ctx HERE IS THE CLIENT'S AND THE RUN'S IS THE LOOP'S. That split is the
// whole detach story: this context bounds the wait, and Direct.Run's bounds
// the rental.
//
// A SHUTDOWN RACES THIS SEND, AND THERE ARE TWO OUTCOMES RATHER THAN ONE.
// This paragraph used to read "the drain loop returns when `ember serve` is
// stopping and nothing will ever receive", and that sentence is true only of
// a submitter still parked here. `Run`'s idle select has `<-ctx.Done()` and
// `<-q.handoff()` as two cases of ONE select, so a shutdown landing while a
// submission is parked makes both ready and Go picks uniformly -- and if the
// handoff wins, the loop TAKES the run, reaches `if ctx.Err() != nil` at the
// top of the next iteration, and returns having started nothing. Submit has
// already answered nil by then. So something did receive it, and the old
// account said that could not happen.
//
// MEASURED RATHER THAN REASONED, because the obvious shape reports it as
// unreachable. With the send and the cancel released from one closed channel,
// so both cases are genuinely ready together: 3000 attempts, 21 accepted
// submissions, 2 of which ran and **19 taken and dropped**. The control is
// what anybody would try first -- park the send, then cancel, where
// `ctx.Done()` is not yet ready at the moment the handoff is: 400 attempts,
// 400 accepted, **400 ran, 0 dropped**. A test that raced for this would be a
// flake, so what is committed is TestDropPending_... over the line the loop
// now prints, and these numbers live here.
//
// THE OUTCOME IS RIGHT EITHER WAY; ONLY THE ACCOUNT WAS WRONG. `Direct.Run`
// is what rents and on both paths it never ran, so there is no money and no
// leak. The row `Adopt` wrote is swept to `interrupted` by the next start's
// Interrupt, which is exactly what a process killed outright already leaves
// behind, and the client reads a dropped connection as the process exits
// whichever case won. What the drop is no longer is SILENT: `Run` calls
// dropPending before that return, so "swept by the next start" is something a
// reader can confirm in the log rather than take on trust -- which is this
// repository's own rule about a comment that is wrong concerning why a seal
// holds, applied to the seal's own paragraph.
//
// A `done` channel on the Queue is still not the answer, and the reason is
// stronger than it was. It would turn the taken-and-dropped case into a
// refusal, which is the same outcome with more state -- the run does not
// happen either way -- and its only reader would be this select. Dropping it
// at shutdown is the correct behaviour; being able to see that it happened is
// what was missing.
func (q *Queue) Submit(ctx context.Context, d Direct) error {
	if d.Req == "" || d.Run == nil {
		// Refused rather than left to panic in the drain goroutine, where the
		// panic would unwind past provision.WithNode's deferred destroy with
		// a node running -- AppPlanner.out()'s reason for answering a nil.
		return errors.New("refusing to submit a run with no request id or nothing to run: " +
			"queue.DirectRunner.Start builds both, and a zero Direct is one nobody filled in")
	}
	select {
	case q.handoff() <- d:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("gave up waiting for a rental to be free: %w", ctx.Err())
	}
}

// dropPending says out loud that a submitted run was taken and never started.
//
// WHAT IT IS FOR. `Run`'s top-of-loop `ctx.Err()` return is the one place a
// submission the loop has ALREADY ACCEPTED can be discarded -- see Submit for
// why that is reachable and measured -- and it used to discard it with no line
// anywhere. So a run an operator had been told was starting vanished with
// nothing in the daemon's log about it, and "the next start sweeps the row to
// interrupted" was a thing a reader had to take on trust rather than confirm.
//
// THE MONEY FACT COMES FIRST, because that is the one an operator reads this
// line to settle: `Direct.Run` is what rents and it never ran, so there is
// nothing billing. Then what the row will say, then what to do -- the client
// was told the run had started, so resubmitting it is the operator's move and
// not something ember will do for them.
//
// A nil d is the ordinary shutdown and is silent. It is a no-op here rather
// than a condition at the call site, so both answers go through one line.
func (q *Queue) dropPending(d *Direct) {
	if d == nil {
		return
	}
	q.log().Warn("a submitted run was accepted and this process stopped before it could rent; "+
		"nothing was rented and nothing is billing, the next ember serve marks the request "+
		"interrupted, and the client was told it had started -- so submit it again",
		"request", d.Req)
}

// direct runs a submitted run, in this iteration of the drain loop.
//
// It records `held` for the same reason once does: the row has read `running`
// since Adopt wrote it, and a cancel arriving now must be answered by a queue
// that knows this process holds the request. Cleared when the run returns,
// which is after the deferred destroy.
func (q *Queue) direct(ctx context.Context, d Direct) error {
	q.hold(d.Req)
	defer q.hold("")
	q.log().Info("running a submitted run on the rental loop", "request", d.Req)
	if err := d.Run(ctx); err != nil {
		return fmt.Errorf("run %s: %w", d.Req, err)
	}
	return nil
}

// DirectRunner starts a run the way `ember up` does, inside this process.
//
// IT IS IN internal/queue AND NOT IN internal/webapi, because the thing that
// makes it safe is the drain loop and this is the package that owns it. webapi
// names an interface with one method (webapi.Runs) and never reaches a plan, a
// provider or a rental -- which is what keeps "a handler cannot start a
// rental" a fact about the import graph rather than a rule somebody remembers.
//
// IT IS NOT IN internal/cli EITHER, although `ember up` lives there: cli is
// the shell, nothing imports it, and the runner has to be constructible by
// whoever wires the server. What IS shared with `ember up` is AdoptRun below,
// so the two paths record through one function rather than two that can drift.
type DirectRunner struct {
	// Deps is internal/app's injection seam, so a test does not resolve the
	// developer's own credentials and cannot reach a cloud provider.
	Deps app.Deps

	// Store is the open generation history. The same one the queue writes, so
	// a submitted run and a queued one land in one ledger.
	Store genstore.Store

	// Queue is the one goroutine that can rent. Nil is refused rather than
	// dereferenced: a runner with no loop is a runner that would have to
	// start its own, which is the second rental.
	Queue *Queue

	// AllowPaid gates every provider that is not free, HERE TOO.
	//
	// THIS IS THE ONE PLACE THE SOCKET IS NOT SIMPLY "THE OPERATOR'S OWN
	// SHELL", and it is deliberate. Everything else about this channel rests
	// on connect(2) having proved the caller is the operator, so the config
	// path travels and the ceilings replace. The flag is different because it
	// is a property of the LONG-RUNNING PROCESS rather than of the request: an
	// `ember serve` left running without it is a process the operator has said
	// must not spend money, and a channel that spent anyway would make the
	// flag mean "unless you came in the other door" -- which is the thing
	// refusePaid is checked twice to prevent. The cost is that `ember up
	// --provider runpod` is refused through a daemon started without it and
	// would have spent in a shell; the refusal names the flag, and README says
	// so where it describes the two paths.
	AllowPaid bool

	// Out is where this run's own structured log goes: the daemon's terminal,
	// as AppPlanner.Out is for a queued rental.
	//
	// NOT WHERE THE NODE'S BYTES GO. Those become KindLog events on the
	// request's stream, because the operator who submitted this run is at the
	// client and not at the daemon's terminal -- see Start.
	Out io.Writer

	// Log is the level and the format the operator chose on `ember serve`,
	// carried for the reason AppPlanner.Log is: a rental's own lines come from
	// inside app.Plan.Do and should obey the same flag as the server's.
	Log logx.Options

	// Now is injected so a test can choose what a row's timestamps say.
	Now func() time.Time
}

func (r DirectRunner) out() io.Writer {
	if r.Out == nil {
		return io.Discard
	}
	return r.Out
}

func (r DirectRunner) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r DirectRunner) log() *slog.Logger {
	if r.Queue != nil {
		return r.Queue.log()
	}
	return slog.New(slog.DiscardHandler)
}

// Start plans the run, records it, and hands it to the one goroutine that can
// rent. It returns once that goroutine has taken it.
//
// THE ORDER IS THE CHEAP-REFUSAL ORDER, which is app.NewPlan's own discipline
// reached one layer up: everything a bad ask can be refused for happens before
// anything is rented AND before a row is written, so a mistyped provider, an
// unpinned image, a card with no image, a sampler the server does not list and
// a store that is missing the weights are all answered synchronously, with the
// operator's own error text, as a 400 the client prints. That is why this
// blocks on a plan rather than answering 202 and failing into the event
// stream: `ember up` refuses a bad config before it rents, and a client that
// had to read a database to find out would be a worse command than the one it
// replaces.
//
// WHAT IT DOES NOT DO is enqueue. There is no Enqueue call on this path and
// there must not be: Enqueue writes StatusQueued whatever the caller said, and
// a queued row is exactly what ClaimNext takes -- so an `ember serve` in
// another terminal would claim it, rent a SECOND card and generate the batch
// again from the record of the run still happening. genstore.Adopt writes
// StatusRunning, which no claim will take, which is the method `ember up`
// already uses for the same reason.
func (r DirectRunner) Start(ctx context.Context, ask app.RunAsk) (genstore.ReqID, error) {
	if r.Queue == nil {
		return "", errors.New("this server has no rental loop, so there is nothing to run a " +
			"submitted run on; ember serve wires one")
	}
	if r.Store == nil {
		return "", errors.New("this server has no generation history, so a run could not be " +
			"recorded before it rented; ember serve wires one")
	}
	if err := ask.Validate(); err != nil {
		return "", err
	}

	// READ HERE AND NOT AT STARTUP, which is the reversal RunAsk documents.
	// The ceilings this run is bounded by are the ones in the file the
	// operator named, because the operator is who asked -- and the daemon's
	// own config, which bounds the queue, is untouched by it.
	cfg, err := config.Load(ask.Config)
	if err != nil {
		return "", err
	}
	ask.Overrides.Apply(&cfg)

	plan, err := app.NewPlan(ctx, app.Request{
		Config: cfg, Deps: r.Deps, Hold: ask.Hold, Log: r.Log,
	}, r.out())
	if err != nil {
		return "", err
	}

	// After the offer is chosen, so the refusal names the price it refused --
	// refusePaid's own reason, and the same function the drain loop uses
	// rather than a second copy of the gate.
	if err := refusePaid(r.AllowPaid, plan.Provider, plan.Offer); err != nil {
		return "", err
	}

	id, sink, finish, err := AdoptRun(ctx, r.Store, plan, r.log(), r.now)
	if err != nil {
		return "", err
	}

	// THE NODE'S BYTES BECOME EVENTS, which is the one thing this path does
	// that neither existing caller does.
	//
	// app.Plan.Do takes a writer for byte streams and a sink for progress
	// because for `ember up` they both end at one terminal and for the queue
	// the first is the daemon's terminal and the second is a database. Here
	// the operator is at neither: they are at a client on the far side of a
	// socket, reading the event stream. report.Writer is the adapter that
	// already existed for exactly this -- "so that the packages which only
	// print need no change to reach a UI" -- and it had no production caller
	// until now, which cmd/check-coverage's dead-code baseline recorded with
	// the reason "node byte streams reach the operator another way". On this
	// path they do not. What it costs is written down rather than discovered:
	// a chatty workload's output is rows in `events`, one per line, which
	// `ember history rm` is what removes. What it buys is that the client's
	// output is byte-for-byte what `ember up` prints, because report.Text
	// renders a KindLog event's text verbatim and the terminator travels with
	// it.
	w := report.Writer(sink)

	d := Direct{Req: id, Run: func(ctx context.Context) error {
		runErr := plan.Do(ctx, w, sink)
		// CLOSE BEFORE FINISH, AND NEITHER DEFERRED, because the sequence is
		// load-bearing: Close flushes the trailing partial line -- which is
		// usually the error -- into the sink, and Finish is what flushes the
		// sink to the database. Deferring Close would run it after Finish and
		// lose exactly that line. The statement order is `ember up`'s own
		// reason for calling finish after Do returns rather than deferring it.
		_ = w.Close()
		finish(runErr)
		return runErr
	}}

	if err := r.Queue.Submit(ctx, d); err != nil {
		// Nothing was rented and nothing is billing, and the row says
		// `running` because Adopt wrote it before this point -- which is the
		// journal's rule, not an accident. So it is ended here with the reason:
		// a row left reading `running` by a client that went away is the
		// phantom this repository has already paid for once, and the only
		// thing that used to clear one was a restart.
		finish(err)
		return "", err
	}
	return id, nil
}

// AdoptRun writes the row for a run nobody queued and hands back the stream it
// reports to.
//
// ONE ADOPT AND ONE RECORDER FOR BOTH CALLERS. `ember up` in a shell and a run
// submitted over the socket record the same columns from the same events, and
// two copies of this would drift the first time one of them gained a field --
// which is the reason the recorder moved out of Queue in the first place. The
// `also` sinks are the difference between the two: the shell path passes
// report.Text so the operator's terminal gets the lines, and the socket path
// passes none, because its operator reads the stream through the client.
//
// WRAPPED IN A Stream, which is not decoration: emitters do not number events,
// and an unnumbered one arrives as Seq 0 -- where genstore's INSERT OR REPLACE
// on (request, seq) makes every event overwrite the last, leaving one row for a
// whole generation. That was a real bug in the queue, found by reading the
// database rather than by a test.
func AdoptRun(
	ctx context.Context,
	store genstore.Store,
	plan app.Plan,
	log *slog.Logger,
	now func() time.Time,
	also ...report.Sink,
) (genstore.ReqID, report.Sink, func(error), error) {
	// WHAT WAS ASKED FOR, and nothing about what it produced. Adopt's INSERT
	// names no more columns than Enqueue's, so the cost, the node, the memory
	// figures and the model cannot be set here even by mistake: they are
	// written through from the events the run emits.
	//
	// The ceilings come from the resolved Options rather than from the config
	// strings, because a flag beats the file on this path -- Overrides.Apply
	// REPLACES -- and the number that bounded this run is the one worth
	// recording.
	id, err := store.Adopt(ctx, genstore.Request{
		Provider: string(plan.Provider.Name()),

		// THE CARD THAT WAS RENTED, NOT THE ONE THE FILTER NAMED, and the
		// difference is the whole of what `ember spend --by gpu` is for. A
		// queued request has only a selection to record, because it is stored
		// before an offer exists; this path has already chosen one -- and most
		// measurement runs name no `gpu:` at all, so recording the filter
		// would group every one of them under a dash in the column that exists
		// to tell cards apart. Empty here means a machine with no card, which
		// is what `ember store sync` rents.
		GPU:       RunGPU(plan),
		Offer:     string(plan.Offer.Token),
		TTL:       plan.Spec.TTL,
		MaxHourly: plan.Options.Budget.MaxRate,
		MaxRun:    plan.Options.Budget.MaxRun,

		Preview:         plan.Jobs.Preview,
		PreviewInterval: plan.Jobs.PreviewInterval,
		Jobs:            RunJobs(plan),
	})
	if err != nil {
		return "", nil, nil, err
	}

	rec := &Recorder{
		Store: store, Req: id, Now: now, Log: log,
		// Key stays nil: a run recorded this way writes into a timestamped run
		// directory it tells the operator about, which no gallery serves and
		// which `ember history rm` must not offer to remove. The image counts
		// and bytes are recorded regardless -- they are columns on the job,
		// which is what the ledger and the aggregate read. See Recorder.Key.
	}

	sinks := append(append([]report.Sink{}, also...), rec)
	return id, report.NewStream(now, sinks...), func(runErr error) {
		// WithoutCancel, because the ordinary way to end a run is a cancelled
		// context -- Ctrl-C on the shell path, a shutting-down daemon on the
		// other -- so the context the run used is already done by the time
		// there is an outcome to record, and the row is the only lasting
		// record of what the rental cost.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), writeTimeout)
		defer cancel()
		rec.Finish(ctx, runErr)
	}, nil
}

// RunGPU is the card that was rented, or empty for a machine with none.
//
// THROUGH HasGPU AND NOT STRAIGHT OFF THE OFFER, because the local provider
// spells a machine with no card `node.GPUNone` -- the literal "none" -- while
// a queued request with no selection stores the empty string. Both mean "no
// card", and two spellings of it in one column is what keyOr exists to avoid:
// the reader would print a dash for one and the word "none" for the other, in
// a report whose whole point is telling cards apart. HasGPU is the one
// function that decides what counts as a card, so asking it keeps the answer
// in one place. Found by reading the output of a local run.
func RunGPU(plan app.Plan) string {
	if !plan.Offer.HasGPU() {
		return ""
	}
	return string(plan.Offer.GPU)
}

// RunJobs is the batch as the history records it.
//
// Only the served shape has any. A `cmds` run's steps and a plain forwarded
// session are recorded as the request row and its events and as no job rows at
// all -- see record.go, which argues it: a step is not a job, and the figures
// the jobs table exists for (images, passes, steps per second) are not things
// an `ffmpeg` invocation has.
func RunJobs(plan app.Plan) []genstore.Job {
	jobs := make([]genstore.Job, 0, len(plan.Jobs.Jobs))
	for _, j := range plan.Jobs.Jobs {
		jobs = append(jobs, genstore.FromSD(j))
	}
	return jobs
}
