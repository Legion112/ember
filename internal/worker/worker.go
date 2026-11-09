// Package worker is what runs on a rented node and does the work.
//
// It is the node's second supervised child, beside sshd, and everything it
// does is driven by frames ember pushes down the control channel: it holds no
// credential, opens no outbound connection, and knows nothing about providers,
// prices or queues. That is deliberate and it is what keeps the node
// credential-free -- results are a plain filesystem write on the node's own
// disk, and ember fetches what an event names.
//
// WHY IT IS A PACKAGE AND NOT cmd/embernode. The job loop here is the one that
// used to live in internal/app, with a poll cadence measured against a real
// card and a cancel path a lab found a bug in. cmd/embernode is excluded from
// the coverage gate because every statement in it is process wiring; this is
// decisions, so it lives where `go test` can drive it with no node, no Docker
// and no sshd, and the gate can hold it.
//
// WHAT IT DOES NOT DO: it never dials ember. There is no egress from an ember
// node and nothing here changes that -- the worker answers on the node's own
// loopback and ember reaches it through the SSH connection it already holds.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/sdserver"
)

// The poll cadence, which starts fast and backs off.
//
// A FLAT TWO SECONDS WAS WRONG, AND MEASURED SO. Three sd15 images at
// 256x256 and four steps take about 0.3s each to sample, and a flat 2s poll
// noticed each one up to two seconds late: the marginal cost per job came to
// 2.08s served against 1.81s for a fresh sd-cli per image. The resident model
// saved about 0.8s of tensor loading per job and the poll interval then spent
// more than that waiting, so the served path measured *slower* at every batch
// size tried -- 1, 3 and 8 jobs -- and the gap widened. The whole point of the
// feature, hidden by its own client.
//
// Starting at jobPollMin and doubling to jobPollMax fits both ends: a short
// job is noticed almost at once, and a long one settles to a cadence that
// does not pull a preview frame every tenth of a second.
//
// AND THE POLL IS NOW A LOOPBACK CALL ON THE NODE ITSELF rather than one over
// an SSH channel, which is the one number this move changes in ember's favour:
// the fast end costs a syscall instead of a round trip to the operator's
// machine, so noticing a finished job early is close to free.
var (
	jobPollMin = 50 * time.Millisecond
	jobPollMax = 2 * time.Second
)

// nextPoll is the backoff: double, capped.
func nextPoll(d time.Duration) time.Duration {
	if d <= 0 {
		return jobPollMin
	}
	if d *= 2; d > jobPollMax {
		return jobPollMax
	}
	return d
}

// Options is what a session needs.
type Options struct {
	// Gen is the generation server on this node's own loopback.
	//
	// THE PRODUCTION CLIENT AND NOT AN INTERFACE, for the reason
	// internal/sdserver's own tests give: the external system is an HTTP API,
	// so what the tests drive should be the real client with its real decoding
	// and its real error typing, pointed at an httptest.Server. An interface
	// here would be a seam shaped by a test's convenience over a struct that
	// already has one.
	Gen sdserver.Client

	// Results is the directory results are written under, control.ResultsDir
	// in production. A parameter so a test needs no /tmp.
	Results string

	Log *slog.Logger
}

// Session runs one control channel to its end.
//
// IT RETURNS WHEN THE WORK IS DONE, which is how "the worker exits when its
// in-memory queue empties" reaches the process: control.Serve returns what
// this returns, cmd/embernode's worker subcommand exits on it, and embernode's
// supervisor reports the child's exit in the node's own log.
//
// "Done" is the queue being empty AND ember having said no more jobs are
// coming. Those are two different facts and only ember holds the second one:
// a worker that exited the moment its queue emptied would end between two
// requests of one drain loop and throw away the resident weights, which cost
// 80-96 s per image to put back. See control.KindDrained.
//
// A CHANNEL THAT DROPS ALSO ENDS IT, and that is the recorded rule rather than
// a new one: "a node ember cannot reach is a node ember cannot destroy, so
// there is no value in a workload outliving the connection". A job finished
// after the laptop died is a card paid for a picture nobody will fetch.
func Session(ctx context.Context, c *control.Conn, o Options) error {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Results == "" {
		return errors.New("the worker has no results directory, so nothing it generated could be written down")
	}

	q := newQueue()

	// The reader and the runner are separate goroutines because both
	// directions have to work at once: a cancel that could only be read
	// between jobs is a cancel that arrives after the job it was for has
	// finished, which is the shape of a button that looks like it works.
	//
	// Only the runner sends. See control.Conn on why that matters.
	//
	// NOT WAITED FOR ON THE WAY OUT, deliberately: it is blocked in a read on
	// a connection the caller drops the moment this returns, and a wait here
	// would be a worker that cannot exit until ember hangs up -- the deadlock
	// version of the thing this whole file is about.
	go func() {
		for {
			f, err := c.Recv(ctx)
			if err != nil {
				q.hangUp(err)
				return
			}
			q.push(f, o.Log)
		}
	}()

	return run(ctx, c, q, o)
}

// run is the runner goroutine: one item at a time, in the order ember pushed
// them.
func run(ctx context.Context, c *control.Conn, q *queue, o Options) error {
	var caps sdserver.Capabilities
	seq := 0

	for {
		item, ok := q.next()
		if !ok {
			hungUp, err := q.endedBy()
			if err != nil {
				// The channel went away without a goodbye. Reported as an
				// error so the node's log says what happened, and NOT sent
				// anywhere: there is nothing left to send it down.
				o.Log.Info("the control channel went away; the worker is stopping rather than finishing work nobody can fetch", "err", err)
				return err
			}
			if hungUp {
				// SAID DIFFERENTLY FROM THE LINE BELOW, because the node's
				// log is the only record of which happened and they mean
				// different things: ember disconnecting mid-rental is a
				// laptop that went away, and a worker reporting "ember says
				// there is no more work" over it would be the log asserting
				// something nobody said.
				o.Log.Info("ember hung up; the worker is stopping rather than finishing work nobody can fetch")
				return nil
			}
			o.Log.Info("the queue is empty and ember says there is no more work; the worker is exiting")
			// Best effort: ember may already have stopped reading, and this
			// frame is a courtesy rather than the thing that ends the rental.
			if err := c.Send(ctx, control.Frame{Kind: control.KindDone}); err != nil {
				o.Log.Debug("could not send the last frame", "err", err)
			}
			return nil
		}

		var err error
		switch item.Kind {
		case control.KindPrepare:
			caps, err = prepare(ctx, c, item, o)
		case control.KindJob:
			seq++
			err = runOne(ctx, c, q, item, caps, seq, o)
		case control.KindCancelled:
			// The synthetic item queue.next makes when a cancel landed
			// between two jobs: the rest of the batch is already gone and
			// this is what tells ember so. Forwarded rather than rebuilt,
			// because the queue is what knows there was no job in flight.
			err = c.Send(ctx, item)
		default:
			// A kind this build does not know, named rather than ignored. The
			// version handshake is what should have caught it, so reaching
			// here means the two ends disagree about what a version means --
			// and a frame silently dropped is a job ember is waiting for
			// forever.
			err = fmt.Errorf("the worker does not know what to do with a %q frame", item.Kind)
		}
		if err != nil {
			o.Log.Error("the worker is stopping", "kind", item.Kind, "err", err)
			// Sent so ember learns the reason from the channel rather than
			// from a connection that simply closed. A send that fails here
			// changes nothing: the error is returned either way.
			//
			// THE JOB'S NUMBERS TRAVEL WITH IT, so ember can put its own
			// label on the sentence. A failure that could not name which job
			// it was about is the message an operator reads off a batch of
			// eight -- and the label is ember's prose, not the node's, which
			// is why the frame carries the numbers rather than the words.
			if serr := c.Send(ctx, control.Frame{
				Kind: control.KindFailed, Index: item.Index, Total: item.Total, Err: err.Error(),
			}); serr != nil {
				o.Log.Debug("could not report the failure", "err", serr)
			}
			return err
		}
	}
}

// prepare waits for the generation server on this node and reports what it can
// do.
//
// EMBER STARTED THAT SERVER, over `ssh exec`, with argv ember built. This end
// only waits for it, which is the division that keeps "nothing an operator
// writes in a config reaches a node as a command" true after the arrival of a
// daemon on the node: the worker has no argv, so there is nothing for a config
// to reach it through.
func prepare(ctx context.Context, c *control.Conn, f control.Frame, o Options) (sdserver.Capabilities, error) {
	// The wait streams a line when it took more than one poll, and that line
	// used to go straight to the operator's terminal from ember's own loop.
	// The loop moved here, so the line has to travel back or an operator
	// watches nothing happen for the four minutes a 56 GiB model takes.
	load, err := sdserver.WaitModel(ctx, o.Gen, f.Timeout, logWriter{ctx: ctx, c: c}, o.Log)
	if err != nil {
		return sdserver.Capabilities{}, err
	}

	// DECODED HERE AND NOWHERE ELSE, which is the one place it can be: the
	// generation server is on this node's own loopback. The worker needs
	// exactly one field out of the answer -- whether this build will stop a
	// generation in flight -- and ember needs the rest to refuse a bad job
	// before anything is paid for, so the decoded value is what travels.
	// Relaying the server's own bytes instead would run sdserver's mapper at
	// both ends, and it is that mapper which learned `capabilities.model` is
	// an object and not the string api.md's field list implies.
	caps, err := o.Gen.Capabilities(ctx)
	if err != nil {
		return sdserver.Capabilities{}, err
	}
	o.Log.Info("the generation server is ready", "model", caps.Model, "load_elapsed", load)

	// Marshalled rather than relayed, and the asymmetry with a job's body is
	// the point: sdserver.Capabilities is strings, numbers, bools and *bool,
	// with no map[string]any anywhere, so it survives a JSON round trip
	// exactly -- including the nil-versus-false distinction in Supported,
	// which is load-bearing in both directions. A job's body does not, which
	// is why that one crosses as bytes nobody parsed.
	raw, err := json.Marshal(caps)
	if err != nil {
		return sdserver.Capabilities{}, fmt.Errorf("encode what the generation server can do: %w", err)
	}
	return caps, c.Send(ctx, control.Frame{Kind: control.KindPrepared, Elapsed: load, Caps: raw})
}

// runOne submits one job, polls it to a conclusion, writes what it produced
// and names it.
//
// ONE JOB IN FLIGHT ON THE SERVER, AND THE WHOLE BATCH IN THIS PROCESS, which
// is the arrangement the two constraints together require. sd-server's
// async_jobs.cpp purges a finished result ten minutes after it completed, so
// queueing ten jobs there would leave eight finished images -- each already
// paid for -- one lapse away from being deleted. Holding the batch HERE
// instead costs nothing, makes that TTL unreachable, and is what lets ember
// push a batch and stop driving: the queue is in this process and the server
// sees one job at a time.
func runOne(ctx context.Context, c *control.Conn, q *queue, f control.Frame, caps sdserver.Capabilities, seq int, o Options) error {
	// A FRESH DIRECTORY PER JOB, because a rental serves several requests and
	// two of them can name the same output. Without it, ember's fetch of
	// `boat-00.png` could bring home the previous request's picture -- a
	// result that is plausible, paid for, and wrong, which is the worst of the
	// three. The name is the worker's and travels on the landed frame rather
	// than being derived twice.
	dir := filepath.Join(o.Results, strconv.Itoa(seq))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create this job's results directory: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fmt.Errorf("open this job's results directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	id, err := o.Gen.Submit(ctx, f.Job)
	if err != nil {
		return err
	}

	// SENT AFTER THE SUBMIT AND NOT BEFORE IT, which is what makes it a
	// measurement rather than an announcement: ember prices the job from this
	// frame, so a frame sent before the card had the job would bill the
	// submission round trip to the generation.
	if err := c.Send(ctx, control.Frame{
		Kind: control.KindStarted, Index: f.Index, Total: f.Total,
	}); err != nil {
		return err
	}

	st, asked, err := await(ctx, c, q, o, id, f, caps)
	if err != nil {
		return err
	}

	// WRITTEN AND NAMED EVEN WHEN THE CANCEL LANDED, because a card that
	// produced an image was paid for it whatever happened next. A job the
	// server stopped mid-pass carries none, which is why the frame is sent
	// only when there are files: "0 file(s)" against a cancelled job reads as
	// a job that finished and produced nothing.
	if len(st.Images) > 0 {
		stem := f.Stem
		if stem == "" {
			// Numbered from the job's position, so a request that names
			// nothing still produces files an operator can tell apart. The
			// index is one-based on the wire; see control.Frame.
			stem = fmt.Sprintf("%03d", f.Index)
		}
		w, werr := sdserver.WriteImages(root, stem, st.Images)
		if werr != nil {
			return werr
		}
		files := make([]control.File, 0, len(w.Each))
		for _, each := range w.Each {
			files = append(files, control.File{Name: each.Name, Bytes: each.Bytes})
		}
		if err := c.Send(ctx, control.Frame{
			Kind: control.KindLanded, Index: f.Index, Total: f.Total,
			Dir: dir, Files: files,
		}); err != nil {
			return err
		}
	}

	var apiErr string
	if st.Err != nil {
		// The sentence and not the type: the far end of this channel is a
		// different process, so an *APIError crosses as what it says. ember
		// builds the refusal message around it, which is where the job's
		// label lives.
		apiErr = st.Err.Error()
	}
	return c.Send(ctx, control.Frame{
		Kind: control.KindEnded, Index: f.Index, Total: f.Total,
		Status: st.Status.String(), Cancelled: asked, Err: apiErr,
	})
}

// await polls one job to a conclusion, pushing each distinct preview frame up
// the channel as it arrives.
//
// EACH DISTINCT STEP ONCE, deduplicated here rather than by ember. sd-server
// returns the same frame between intervals, and a step already reported is a
// preview ember would write over the identical file and a UI would re-render
// for nothing. Dropping it on the node is also the one place where it saves
// bytes on the channel instead of after them.
//
// It reports whether ember asked for this job to stop, which is the one fact
// ember cannot work out for itself: a job that came back `cancelled` because
// ember asked is a different outcome from one that came back `cancelled` on
// its own, and the difference decides whether the rental dies.
func await(ctx context.Context, c *control.Conn, q *queue, o Options, id string, f control.Frame, caps sdserver.Capabilities) (sdserver.State, bool, error) {
	lastStep := -1
	wait := jobPollMin
	asked := false

	for {
		st, err := o.Gen.Poll(ctx, id)
		if err != nil {
			return sdserver.State{}, asked, err
		}
		if p := st.Preview; p != nil && p.Step != lastStep {
			lastStep = p.Step
			if err := c.Send(ctx, control.Frame{
				Kind: control.KindProgress, Index: f.Index, Total: f.Total,
				Pass: p.Pass, Step: p.Step, TotalSteps: p.TotalSteps, PNG: p.PNG,
			}); err != nil {
				// A preview that cannot be sent means the channel is gone,
				// and the channel being gone is the end of the rental. Not
				// swallowed: ember's own best-effort handling of a preview it
				// cannot write is about a full disk, which is a different
				// thing from a connection that closed.
				return sdserver.State{}, asked, err
			}
		}
		if st.Status.Done() {
			return st, asked, nil
		}

		// THE ONE CALL THIS PATH EXISTS FOR, made once per job and not once
		// per poll: asking repeatedly while the server works through a
		// 141-second FLUX.2 step would be a request a second against the
		// program the worker is waiting for.
		//
		// Polling CONTINUES after the ask. The server's own answer decides the
		// outcome -- it may come back `cancelled` with nothing, or `completed`
		// because the last step finished while the request was in flight --
		// and what happened is recorded rather than assumed.
		if !asked && q.takeCancel() {
			asked = true
			ev := control.Frame{Kind: control.KindCancelled, InFlight: true}
			switch {
			case !caps.CancelGenerating:
				// NOT A REFUSAL. ember does not ask a server that says it
				// cannot, because the 404 or 400 that came back would be
				// reported as a transport problem rather than as the
				// capability it is -- but the jobs after this one are ember's
				// to drop, and in a batch they are most of the bill. What is
				// left is to be explicit about the part that keeps billing.
				ev.Err = "this sd-server build does not report features.cancel_generating"
			default:
				if cerr := o.Gen.Cancel(ctx, id); cerr != nil {
					// Reported and not returned. The generation is still
					// running on a card that is still billing, so abandoning
					// the poll here would lose the image it is about to
					// produce AND the record of what it cost -- turning a
					// failed cancel into a worse outcome than no cancel.
					ev.Err = cerr.Error()
				}
			}
			if err := c.Send(ctx, ev); err != nil {
				return sdserver.State{}, asked, err
			}
		}

		select {
		case <-ctx.Done():
			return sdserver.State{}, asked, ctx.Err()
		case <-q.woken():
			// Woken to ask at once rather than at the next poll. The backoff
			// settles at two seconds, and a cancel that takes effect two
			// seconds late is a cancel the operator presses again -- which is
			// how a button that works comes to look like one that does not.
		case <-time.After(wait):
		}
		wait = nextPoll(wait)
	}
}

// logWriter turns what sdserver.WaitModel prints into frames.
//
// The wait writes one line, when it took more than one poll, and that line is
// the only evidence an operator has that a four-minute model load is
// progressing rather than stuck. It used to be written straight to their
// terminal because the loop was on their machine.
type logWriter struct {
	ctx context.Context
	c   *control.Conn
}

func (w logWriter) Write(p []byte) (int, error) {
	if err := w.c.Send(w.ctx, control.Frame{Kind: control.KindLog, Text: string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

// queue is the worker's in-memory job list.
//
// IN MEMORY AND NOT PERSISTED, which is a decision rather than a shortcut: a
// node that restarted has lost the card's warm weights, the generation server
// and the SSH connection ember was holding, so a queue that survived the
// restart would be a list of jobs nobody is waiting for on a machine ember is
// about to destroy.
//
// PER SESSION AND THEREFORE PER RENTAL, which is the property `max_nodes`
// needs on this side of the wire: there is one of these per control channel,
// control.Serve answers one channel per worker, and there is one worker per
// node. Nothing here is per process, because a process holds one node.
type queue struct {
	mu   sync.Mutex
	cond *sync.Cond

	items   []control.Frame
	drained bool
	hungUp  bool
	err     error

	// pending is a cancel that has not been reported yet.
	//
	// ONE FLAG, CONSUMED EXACTLY ONCE, and that is not an optimisation. ember
	// emits one cancellation event per cancel, and which one it is differs:
	// with a job in flight it names that job, and in the gap between two jobs
	// it names none. Both ends of that fork read this flag, so a flag either
	// could read twice would be two events for one button press -- and one
	// neither consumed would be ember waiting forever for frames about jobs
	// this queue has already dropped.
	pending bool

	// wake is closed and replaced whenever a cancel or a hang-up arrives, so
	// a poll loop can wait on it and be woken at once instead of at its next
	// tick. A channel rather than the cond, because the poll loop also waits
	// on a timer and a context, and a sync.Cond cannot be selected on.
	wake chan struct{}
}

func newQueue() *queue {
	q := &queue{wake: make(chan struct{})}
	q.cond = sync.NewCond(&q.mu)
	return q
}

// push takes one frame off the reader.
func (q *queue) push(f control.Frame, log *slog.Logger) {
	q.mu.Lock()
	defer q.mu.Unlock()

	switch f.Kind {
	case control.KindCancel:
		// THE QUEUED JOBS GO AT ONCE, and that is where the money in a batch
		// is. Stopping only the job in flight would leave the next one
		// starting a second later on the same card: the button would appear
		// to work and the bill would not change.
		dropped := len(q.items)
		q.items = nil
		q.pending = true
		log.Info("ember cancelled the batch", "dropped", dropped)
		q.rouse()
	case control.KindDrained:
		q.drained = true
	default:
		q.items = append(q.items, f)
	}
	q.cond.Broadcast()
}

// rouse wakes a poll loop waiting on wake. Called with the lock held.
func (q *queue) rouse() {
	close(q.wake)
	q.wake = make(chan struct{})
}

// hangUp records that the channel went away.
func (q *queue) hangUp(err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.items = nil
	q.drained = true
	q.hungUp = true
	// Dropped, because there is nothing left to report it down. A pending
	// cancel surviving a hang-up would make the runner's next step a send on
	// a dead channel, which turns the ordinary end of a rental into a failure
	// with a transport error in it.
	q.pending = false
	if q.err == nil && !errors.Is(err, control.ErrClosed) {
		// ErrClosed is ember saying goodbye, which is the ordinary end of a
		// rental and not a reason. Anything else is why the worker stopped,
		// and it is the one line the node's log can offer about a laptop that
		// went to sleep mid-batch.
		q.err = err
	}
	q.rouse()
	q.cond.Broadcast()
}

// next blocks until there is work, and reports false when there will be no
// more.
//
// A CANCEL NOBODY HAS REPORTED IS WORK, and that is what stops ember waiting
// forever. When the cancel landed in the gap between two jobs, this queue
// dropped the rest of the batch -- so the frames ember is expecting about them
// are never coming, and something has to say so. The runner sends what this
// returns.
func (q *queue) next() (control.Frame, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.drained && !q.pending {
		q.cond.Wait()
	}
	if q.err != nil {
		return control.Frame{}, false
	}
	if q.pending {
		q.pending = false
		return control.Frame{Kind: control.KindCancelled}, true
	}
	if len(q.items) == 0 {
		return control.Frame{}, false
	}
	f := q.items[0]
	q.items = q.items[1:]
	return f, true
}

// takeCancel reports whether ember has asked for the job in flight to stop,
// and consumes the ask. See queue.pending.
func (q *queue) takeCancel() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.pending {
		return false
	}
	q.pending = false
	return true
}

// woken is closed when a cancel or a hang-up arrives.
func (q *queue) woken() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.wake
}

// endedBy reports how the work ended: whether ember hung up rather than
// draining, and why if it was a failure.
func (q *queue) endedBy() (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.hungUp, q.err
}
