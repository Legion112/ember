package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/sshx/sshexec"

	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/control"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/workload"
)

// THE POLL CADENCE MOVED TO internal/worker, WITH ITS MEASUREMENT.
//
// It used to be here, because ember held the poll loop: it dialled the node's
// loopback over SSH and asked sd-server about each job itself. The loop is now
// on the node, which is the whole of this change -- so jobPollMin, jobPollMax
// and nextPoll live beside the code that uses them, and the figure that set
// them (a flat two seconds measured the served path *slower* than a fresh
// sd-cli per image, at every batch size tried) travelled with them rather
// than being left behind as a comment about a loop that is no longer here.
//
// What ember does instead is read frames. There is no interval on this side at
// all: a preview arrives when the node has one.

// Served is a rental whose generation server is up and whose weights are
// resident in VRAM.
//
// IT IS AN INTERFACE SO THE QUEUE CAN BE TESTED WITHOUT A CARD. What a drain
// loop needs from a rental is "generate this batch" and "what can you do", and
// both of those are answerable by a fake. The node's own lifetime is not on
// this interface on purpose: it belongs to WithServed's closure, for the same
// reason provision.WithNode owns a node's -- there is no path out of the
// closure that leaves a machine running.
type Served interface {
	// Generate runs one batch's jobs, reporting progress to sink and writing
	// images under dir.
	//
	// Every job is checked against the server's own capabilities before the
	// first one runs. Checking job n just before submitting it would mean
	// n-1 images already paid for when the typo in the last one surfaces.
	//
	// stop is the operator asking for THIS BATCH to end, and it is a channel
	// rather than a context for the one reason that matters: a cancelled
	// context would unwind through provision.WithNode and destroy the node,
	// taking every other request on the same rental with it -- which is
	// exactly the thing `ember serve` used to refuse a cancel to protect. It
	// would also kill the connection the cancel has to travel down. Closing
	// stop ends this batch and leaves the rental alone; a nil channel is
	// never closed, which is what a caller with nothing to cancel passes.
	Generate(ctx context.Context, sink report.Sink, jobs sdserver.Run, dir string, stop <-chan struct{}) error

	// Caps is what this server build reported it can do.
	Caps() sdserver.Capabilities
}

// served is the real Served: the control channel to this rental's worker.
//
// IT USED TO BE AN sdserver.Client, and that is the change. ember held a
// client pointed at the node's loopback through an SSH port-forward and ran
// the submit-poll-collect loop itself. Now it pushes typed jobs down one
// channel and reads what the node reports back, which is what collapses three
// mechanisms -- exec, forward, tar -- into a channel plus a fetch of files an
// event named.
type served struct {
	ch   *control.Conn
	caps sdserver.Capabilities
	rate node.Rate

	// runner is how the files a worker names are brought home.
	//
	// A rental's images land on the node's own disk and an event names them,
	// so ember fetches exactly those paths rather than globbing an `outputs:`
	// tree. It is the sshx.Runner seam and not the SSH client for the reason
	// every other fetch here takes one: the reporting and the error handling
	// are then testable against a recording runner instead of needing a node.
	runner sshx.Runner

	// inputs is where a job's image inputs are staged, empty when this
	// rental's jobs cannot name one. See resolveInputs.
	inputs string
}

func (s served) Caps() sdserver.Capabilities { return s.caps }

func (s served) Generate(ctx context.Context, sink report.Sink, jobs sdserver.Run, dir string, stop <-chan struct{}) error {
	// BEFORE Validate AND BEFORE THE FIRST SUBMISSION, which is as early as
	// this layer can manage: the drain loop builds the Run it passes here
	// from the stored request, so the resolution NewPlan did cannot travel
	// with it and this is the only place that sees the jobs that will
	// actually be submitted. The cheap refusal already happened one layer up,
	// at enqueue and again before the create call; reaching here with an
	// unstaged reference means the file went away in between, and the whole
	// batch is refused rather than its first job failing after the others
	// have been paid for.
	resolved, err := s.resolveInputs(jobs.Jobs)
	if err != nil {
		return err
	}
	jobs.Jobs = resolved
	for i, j := range jobs.Jobs {
		if err := sdserver.Validate(j, s.caps); err != nil {
			return Usage(fmt.Errorf("sd.jobs[%d]: %w", i, err))
		}
	}
	return pushBatch(ctx, sink, s, jobs, dir, stop)
}

// resolveInputs loads the bytes every job's image reference names.
//
// ONE READ PER REFERENCE AND NOT PER JOB. A batch of eight variations on one
// picture names the same digest eight times, and content addressing is what
// makes that a fact rather than a coincidence -- so the file is read once and
// the slice is shared. They are read-only from here: Body base64-encodes
// them and nothing writes through the slice.
//
// An empty inputs directory with a job that names an image is a refusal and
// not a nil map read as "nothing staged": `ember up` takes its jobs from a
// config that has no field for an image, so this is a state only a bug could
// produce, and the alternative is a generation that silently drops the input.
func (s served) resolveInputs(jobs []sdserver.Job) ([]sdserver.Job, error) {
	if !wantsInputs(jobs) {
		return jobs, nil
	}
	if s.inputs == "" {
		return nil, Usage(errors.New("a job names an image input and this rental has no " +
			"staging directory to read it from"))
	}
	in, err := blob.OpenInputs(s.inputs)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()

	seen := map[string][]byte{}
	out := make([]sdserver.Job, 0, len(jobs))
	for i, j := range jobs {
		if ref := j.Params.InitImage; ref != "" {
			b, ok := seen[ref]
			if !ok {
				if b, err = in.Load(ref); err != nil {
					return nil, Usage(fmt.Errorf("sd.jobs[%d]: %w", i, err))
				}
				seen[ref] = b
			}
			j.InitImageBytes = b
		}
		out = append(out, j)
	}
	return out, nil
}

// WithServed rents this plan's node, makes the weights resident, and calls fn.
//
// THE NODE IS DESTROYED WHEN fn RETURNS, whatever it returns. That is what
// lets a drain loop keep a rental and still not leak one: the loop decides
// when there is no more work, returns, and the deferred teardown in
// provision.WithNode takes the node away on a context that survives Ctrl-C.
//
// It is the seam the queue needs and `ember up` does not: up has exactly one
// batch, so it goes through Do. Both reach the same withServer below, so
// there is one spelling of "start the server and wait for the weights".
func (p Plan) WithServed(ctx context.Context, out io.Writer, sink report.Sink,
	fn func(context.Context, Served) error) error {

	if len(p.Spec.Serve) == 0 {
		return Usage(errors.New("this configuration has no sd.serve block, so there is no " +
			"generation server to start; a queued request has nowhere to run"))
	}
	return provision.WithNode(ctx, p.Provider, p.Spec, p.Offer, p.lifecycle(sink),
		func(ctx context.Context, s provision.Session) error {
			// No output directory decided here: fn is a drain loop serving
			// several requests on this one rental, and each of them writes
			// into its own, which is also its key in the blob store.
			client, so, err := dialNode(ctx, s, p.Options)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			if err := p.ready(ctx, out, sink, client, so); err != nil {
				return err
			}
			return withServer(ctx, out, sink, client, sshexec.New(client.SSH()),
				p.Spec, so, p.Offer.Price, len(p.Jobs.Jobs), p.InputsDir, fn)
		})
}

// runServed is the single-batch shape: `ember up` with an sd: block.
//
// Kept as its own function with its own signature because that is what the
// tests around the served path drive, and those tests are the proof that
// splitting Generate out of it changed nothing.
func runServed(
	ctx context.Context,
	out io.Writer,
	sink report.Sink,
	client tunnel.Dialer,
	r sshx.Runner,
	spec node.Spec,
	run sdserver.Run,
	so session.Options,
	runDir string,
	rate node.Rate,
) error {
	// NO STAGING DIRECTORY, AND THAT IS THE SEAL RATHER THAN AN OMISSION.
	// This is `ember up`: the jobs came from ember.yaml, and config.SDJob has
	// no field for an image input, so there is no reference for resolveInputs
	// to find. Passing the directory anyway would make the one path that
	// cannot name an image able to read the staging area.
	return withServer(ctx, out, sink, client, r, spec, so, rate, len(run.Jobs), "",
		func(ctx context.Context, s Served) error {
			// nil: `ember up` has one batch and no second caller, so the only
			// way to stop it is Ctrl-C, which cancels the whole run. There is
			// nothing for a per-request cancel to mean here.
			return s.Generate(ctx, sink, run, runDir, nil)
		})
}

// withServer starts sd-server, waits for the weights, and hands the result to
// fn.
//
// The order is the cheap-failure order the rest of the run already uses,
// extended: the card, then the weights on the store, then the server, then the
// weights in VRAM, then every job checked against what the server says it can
// do -- and only then is anything generated. A mistyped sampler refuses after
// about a minute of a rented card rather than at job seven.
//
// batch is how many jobs the first caller intends to run, for the one line
// this whole shape exists to justify. With a queue it is the first request's
// count and anything claimed afterwards is further saving; with `ember up` it
// is the whole batch.
func withServer(
	ctx context.Context,
	out io.Writer,
	sink report.Sink,
	client tunnel.Dialer,
	r sshx.Runner,
	spec node.Spec,
	so session.Options,
	rate node.Rate,
	batch int,
	inputsDir string,
	fn func(context.Context, Served) error,
) error {
	// The server dies with this context, and it is a child of the run's so
	// that returning for any reason stops it. There is no value in a server
	// outliving the connection: ember is about to destroy the node, and a
	// node ember cannot reach is a node ember cannot destroy.
	srvCtx, stopServer := context.WithCancel(ctx)
	defer stopServer()

	// Timed from here, not from the server's own report: see the resident
	// line below for why ember cannot believe that field.
	serveStart := time.Now()
	sink.Emit(report.Event{Kind: report.KindServerStarting, Cmd: workload.Describe(spec.Serve)})
	proc, err := startServer(srvCtx, out, r, spec.Serve)
	if err != nil {
		return err
	}
	// Stopped on the way out, and its status logged: a server that exited is
	// the explanation for every failure after this point.
	defer func() {
		stopServer()
		select {
		case werr := <-proc.Exited():
			so.Log.Debug("sd-server exited", "err", werr)
		case <-time.After(5 * time.Second):
			so.Log.Debug("sd-server did not exit promptly after being stopped")
		}
	}()

	// THE WORKER AND NOT sd-server IS WHAT EMBER DIALS NOW, and nothing is
	// dialled twice: the forward to sdserver.Port is gone, and the only
	// connection ember opens into the node is the control channel. The worker
	// reaches the generation server on its own loopback, where that call is a
	// syscall rather than a round trip to the operator's machine.
	//
	// WAIT FIRST AND OPEN SECOND, because the worker is started by embernode
	// at boot and ember's first dial can land in the window before it has
	// bound its port. A refused dial there would be reported as a node with no
	// worker, which is the one failure that actually needs a rebuilt image --
	// so the two are told apart by waiting, bounded, and then by asking.
	wctx, stopWait := context.WithTimeout(ctx, control.HandshakeTimeout)
	defer stopWait()
	remote := tunnel.RemoteLoopback(control.Port)
	if err := tunnel.WaitReady(wctx, client, remote, time.Second); err != nil {
		return fmt.Errorf("this node's worker is not listening on %v: %w; the node's own log says why", remote, err)
	}
	ch, err := control.Open(ctx, client.DialContext)
	if err != nil {
		return err
	}
	defer ch.Abort()

	// PREPARE IS RACED AGAINST THE SERVER'S OWN EXIT, which is the half of
	// orExited that matters most and the reason it is still here after the
	// poll loop left. The worker waits for sd-server's port and then for its
	// weights, and it cannot see that the process died -- that process is
	// ember's child over `ssh exec`. Without this race, a server that exited
	// one second in would be waited out for the whole ready_timeout: ten
	// minutes by default, which on an A100 at $1.79/hour is thirty cents of
	// billing for a program that was already gone.
	var load time.Duration
	var rawCaps json.RawMessage
	if err := orExited(ctx, proc, "the weights finished loading", func() error {
		var perr error
		load, rawCaps, perr = prepareWorker(ctx, out, ch, spec.ServeTimeout)
		return perr
	}); err != nil {
		return err
	}

	// The value the worker decoded, not the server's own document: the
	// generation server is on the node's loopback, so the one place that can
	// read its answer is the node. See sdserver.Capabilities on why that
	// mapper runs once rather than at both ends.
	var caps sdserver.Capabilities
	if err := json.Unmarshal(rawCaps, &caps); err != nil {
		return fmt.Errorf("read what the worker says the generation server can do: %w", err)
	}

	// The figure this whole shape exists to stop paying repeatedly, said out
	// loud: a process per image would spend it again for every job after the
	// first.
	//
	// Measured by ember rather than read from the server's own
	// load_elapsed_seconds, because that field is not this number. The first
	// FLUX.2 run reported 551ms for a model whose weights are 56 GiB, so
	// ember printed "551ms of loading is paid once rather than 8 times"
	// while the other arm of the same measurement spent 80-96s per image
	// re-reading those weights off the store -- understating the saving by
	// two orders of magnitude, in the one line an operator would size a
	// batch from. sd-server does not open its port until the load has
	// finished, so the span ember can time itself -- process start to
	// models/status reporting ready -- is the honest one, and it is also
	// exactly what a fresh sd-cli pays on every invocation.
	//
	// Both numbers go on the event, even though only ember's is printed: a UI
	// that showed the server's figure beside a log line carrying ember's
	// would reproduce the exact confusion this comment exists to prevent.
	// EMITTED WHATEVER THE BATCH SIZE, and the gate that used to be here moved
	// into report.Text. "paid once rather than 1 times" is not a saving and is
	// not printed, which is a decision about a line of prose -- and making it
	// here meant a single-job run emitted no event, so its load span and the
	// model that served it were recorded nowhere. Those are the columns #30
	// and #32 added to answer this exact question, and `ember up` with one job
	// is how most of README's measurements were taken. The output is
	// unchanged: the renderer is silent for Total <= 1.
	so.Log.Debug("sd-server load", "measured", time.Since(serveStart), "server_reported", load)
	sink.Emit(report.Event{
		Kind:     report.KindModelResident,
		Measured: time.Since(serveStart),
		Reported: load,
		Total:    batch,
		// What the server says it loaded, which is the only authority on it.
		Model: caps.Model,
	})

	err = fn(ctx, served{ch: ch, caps: caps, rate: rate, runner: r, inputs: inputsDir})

	// THE WORKER IS TOLD THERE IS NO MORE WORK, which is the frame that lets
	// it exit rather than being cut off.
	//
	// "The worker exits when its queue empties" needs both halves and only
	// ember holds the second: an empty queue between two requests of one
	// drain loop is not the end of a rental, and a worker that exited there
	// would throw away the resident weights the whole shape exists to pay for
	// once. Sent even when fn failed, because a failed request ends this
	// rental too.
	//
	// Best effort, and bounded. A channel that has already gone is the case
	// this is for -- the worker exits on its own when ember disappears -- so
	// a failure here is logged and nothing more: the node is about to be
	// destroyed either way, and holding up a destroy is holding up the end of
	// a bill.
	drainWorker(ctx, ch, so.Log)
	return err
}

// drainWorker tells the worker no more jobs are coming and waits briefly for
// it to say it is going.
//
// WHY WAIT AT ALL, given that the destroy is what ends the bill. The node's
// own log is fetched after this, and "the queue is empty and ember says there
// is no more work" is the line that distinguishes a worker that finished from
// one that was cut off mid-batch -- which is the difference between a tidy
// rental and a bug, read afterwards on a machine that no longer exists.
func drainWorker(ctx context.Context, ch *control.Conn, log *slog.Logger) {
	// On a context that survives the operator's Ctrl-C, for the reason every
	// other end-of-rental step here is: Ctrl-C is the most likely way to get
	// here, and the worker is the thing still holding a card.
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerDrainTimeout)
	defer cancel()

	if err := ch.Send(dctx, control.Frame{Kind: control.KindDrained}); err != nil {
		log.Debug("could not tell the worker the queue is drained", "err", err)
		return
	}
	for {
		f, err := ch.Recv(dctx)
		if err != nil {
			if !errors.Is(err, control.ErrClosed) {
				log.Debug("the worker did not say goodbye", "err", err)
			}
			return
		}
		if f.Kind == control.KindDone {
			log.Debug("the worker reported its queue empty and is exiting")
			return
		}
		// Anything else is a frame from a batch that has already ended -- a
		// preview in flight when fn returned. Dropped rather than reported:
		// the request it belongs to is closed, and a late event written to a
		// finished row is a row that changes after it ended.
		log.Debug("dropped a late frame from the worker", "kind", f.Kind)
	}
}

// workerDrainTimeout bounds the goodbye.
//
// THREE SECONDS, AND IT IS A TEARDOWN FIGURE RATHER THAN A PROTOCOL ONE. What
// is being waited for is one frame over a connection that is already open,
// from a worker that is idle by construction -- every batch has ended before
// this is called. The case that takes longer is the one where ember gave up on
// a batch the worker is still finishing, and there the right answer is to stop
// waiting: the node is being destroyed either way, a destroy that is held up
// is a bill that keeps running, and this runs on a context that survives
// Ctrl-C precisely so it cannot be skipped. Ten seconds would be ten seconds
// of billing bought for a log line.
const workerDrainTimeout = 3 * time.Second

// prepareWorker asks the worker to wait for the generation server and report
// what it can do.
//
// The log frames are written to out as they arrive, which is what keeps a
// four-minute model load visible. That line used to be printed by ember's own
// poll loop, on the operator's machine; the loop is on the node now, so the
// line travels back or an operator watches nothing happen.
func prepareWorker(ctx context.Context, out io.Writer, ch *control.Conn, timeout time.Duration) (time.Duration, json.RawMessage, error) {
	if err := ch.Send(ctx, control.Frame{Kind: control.KindPrepare, Timeout: timeout}); err != nil {
		return 0, nil, err
	}
	for {
		f, err := ch.Recv(ctx)
		if err != nil {
			return 0, nil, err
		}
		switch f.Kind {
		case control.KindLog:
			outf(out, "%s", f.Text)
		case control.KindPrepared:
			if len(f.Caps) == 0 {
				// A prepared frame with no capabilities document is a worker
				// that read the server and found nothing, and ember's whole
				// job refusal rests on that document: an empty one would make
				// Validate wave every job through, which is the "an empty
				// list from the server is not a licence to accept anything"
				// rule reached one layer out.
				return 0, nil, errors.New("the worker reported the generation server ready without its capabilities, so no job could be checked before being paid for")
			}
			return f.Elapsed, f.Caps, nil
		case control.KindFailed:
			return 0, nil, errors.New(f.Err)
		default:
			return 0, nil, fmt.Errorf("the worker answered a prepare with a %q frame", f.Kind)
		}
	}
}

// startServer starts a long-running process on the node and returns the
// function that reaps it.
//
// The first thing in ember to use sshx.Runner asynchronously. Run already
// calls Start rather than Run and hands back the stream plus a wait func, so
// nothing new is needed on the seam -- but every other caller copies and
// waits inline, so the pattern is worth naming: the stream is drained in a
// goroutine, the process ends when ctx is cancelled, and wait must be called
// or the SSH session leaks for as long as the connection lives.
func startServer(ctx context.Context, out io.Writer, r sshx.Runner, argv []string) (*serverProc, error) {
	if len(argv) == 0 {
		return nil, errors.New("no server command")
	}
	cmd := sshx.Cmd(argv[0], argv[1:]...)
	// Both streams go to the operator's terminal, live. A model load that is
	// going to fail says so here, forty seconds before the status endpoint
	// admits it, and that output is the only explanation of why.
	cmd.Stderr = out
	if err := cmd.Validate(); err != nil {
		return nil, fmt.Errorf("sd.serve: %w", err)
	}

	stdout, wait, err := r.Run(ctx, cmd)
	if err != nil {
		return nil, fmt.Errorf("start sd-server: %w", err)
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(out, stdout)
	}()

	p := &serverProc{exited: make(chan error, 1)}
	go func() {
		err := wait()
		_ = stdout.Close()
		<-drained
		// Buffered and sent once, so every reader of Exited sees the status
		// whether it is waiting at the time or asks afterwards.
		p.exited <- err
		close(p.exited)
	}()
	return p, nil
}

// serverProc is a server running on the node.
//
// It exists for one reason: a server that died has to be noticed at once.
// Without it, ember polled the port until ready_timeout expired -- ten
// minutes, by default -- for a process that had already exited one second in.
// Measured locally with a flag sd-server rejected: the process printed its
// complaint, exited, and ember waited out the whole budget before reporting
// "never accepted a connection". On an A100 at $1.79/hour that is thirty
// cents of billing for a program that was already gone, and the operator's
// only clue is in output that scrolled past ten minutes earlier.
//
// Same rule as gpucheck's second switch arm: a failure no amount of waiting
// can fix is reported now, not at the deadline.
type serverProc struct {
	exited chan error
}

// Exited reports the server's status once it has ended.
func (p *serverProc) Exited() <-chan error { return p.exited }

// orExited runs fn while watching the server, and reports whichever happens
// first.
//
// The server's own exit status is the useful half of the message: "sd-server
// exited with status 1 before it accepted a connection" plus the complaint it
// printed is actionable, where "never accepted a connection" after ten
// minutes is not.
func orExited(ctx context.Context, p *serverProc, what string, fn func() error) error {
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		return err
	case werr := <-p.Exited():
		// The server also exits when ember stops it, and ember stops it by
		// cancelling the context -- so on Ctrl-C or a deadline both arms are
		// ready at once and the select picks one at random. Attributing a
		// cancellation to a server that died on its own would send the
		// operator looking for a crash that did not happen, so the
		// operation's own answer wins whenever the context is already done.
		if ctx.Err() != nil {
			return <-done
		}
		if werr == nil {
			return fmt.Errorf("sd-server exited before %s; its output above is the only explanation", what)
		}
		return fmt.Errorf("sd-server exited before %s: %w", what, werr)
	}
}

// ErrCancelled is a batch the operator stopped, as opposed to one that failed.
//
// A SENTINEL BECAUSE THE DIFFERENCE DECIDES WHETHER A RENTAL DIES. A failed
// request stops the rental -- a batch whose premise an operator is about to
// re-examine should not keep billing, and the requests behind it get a fresh
// node. A cancelled one must not: the whole point of cancelling a job rather
// than a rental is that the node, the server process and the resident weights
// survive it, so the next queued request is served by the card that is
// already warm. context.Canceled cannot carry that distinction, because it is
// also what Ctrl-C and an expired deadline look like, and both of those DO
// end the rental.
var ErrCancelled = errors.New("cancelled while it was generating")

// pushBatch sends one request's jobs to the worker and turns what comes back
// into this request's events.
//
// THE WHOLE BATCH GOES AT ONCE AND THE WORKER HOLDS IT, which is the opposite
// of what this function used to do and does not contradict the reason it used
// to do it. sd-server's own async_jobs.cpp purges a finished result ten
// minutes after it completed, so queueing ten jobs *there* would leave eight
// finished images -- each already paid for -- one lapse away from being
// deleted. That argument is about the generation server's queue. The worker
// holds the batch in memory and submits to that server one job at a time, so
// the TTL stays unreachable, the 64-job pending cap stays irrelevant, and
// ember stops having to be present for each submission.
//
// WHAT EMBER STILL DECIDES, and the list is deliberate: which jobs exist and
// whether they are allowed (Validate, against the server's own capabilities,
// before anything is pushed); what a job is called in the history; what each
// one cost, from ember's own clock and ember's own rate, because money is
// ember's and `int64` microdollars never cross a wire as anything else; and
// which events a request's row records. The node reports what happened and
// never what it was worth.
//
// A NODE IS UNTRUSTED INPUT -- the rule internal/transfer enforces twice over
// a tar stream -- so every index that arrives is checked against the batch
// ember pushed. The alternative is `jobs.Jobs[f.Index-1]` on a number a rented
// machine chose, which is a panic in the process holding the operator's
// credentials.
func pushBatch(ctx context.Context, sink report.Sink, s served, run sdserver.Run, runDir string, stop <-chan struct{}) error {
	// Created here, because nothing else on this path does. The fetching
	// paths get it from transfer.Fetch's own MkdirAll; a preview frame never
	// calls Fetch, since it arrives on the channel rather than as a tar.
	// Without this, every served run failed at the first preview with
	// "no such file or directory" after the generation had been paid for.
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return fmt.Errorf("create the run's output directory: %w", err)
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return fmt.Errorf("open the run's output directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	total := len(run.Jobs)
	stems := make([]string, total)
	for i, j := range run.Jobs {
		stems[i] = j.Output
		if stems[i] == "" {
			// Numbered from the position in the list, so a config that names
			// nothing still produces files an operator can tell apart.
			stems[i] = fmt.Sprintf("%03d", i+1)
		}
	}

	var (
		spent   node.MicroUSD
		started = make(map[int]time.Time, total)
		ended   int
		asked   bool
		done    bool
		pushed  int
	)

	// EMBER GIVING UP ON A BATCH HAS TO REACH THE WORKER, because the worker
	// is holding the rest of it.
	//
	// This is the CI failure's rule applied to the cases the worker cannot
	// notice for itself. A job that failed, it polled: it drops the rest of
	// the batch on its own. But ember also abandons a batch for reasons only
	// ember can see -- a landed file whose fetch failed, a frame it refuses as
	// untrusted, a worker that sent something in the middle of a batch that
	// does not belong there -- and in every one of those the worker is still
	// working through a queue nobody is reading any more. A card paid for
	// pictures the request that asked for them has already ended.
	//
	// The frame is `cancel` and not a new kind, because "stop the batch" is
	// exactly what it means: the operator's button is one caller of that and
	// ember giving up is another, and both want the same thing done to the
	// same queue.
	//
	// REGISTERED BEFORE THE PUSH LOOP AND GUARDED ON `pushed`, which is the
	// one ordering here that was wrong. A Send that fails on job three of
	// five leaves jobs one and two on the worker's queue -- the channel is
	// not necessarily gone, since Send has a bound of its own and can expire
	// against a slow peer -- so the loop below has to be inside this defer's
	// reach. And `pushed` is what keeps the frame to the case it is for: a
	// batch that failed before anything was pushed has nothing to drop, and a
	// cancel for an empty queue is a cancellation event ember could later read
	// as the operator's.
	//
	// Not sent when the operator already asked -- that cancel is in flight or
	// landed -- and not sent when the batch finished, which is what `done`
	// distinguishes.
	defer func() {
		if done || asked || pushed == 0 {
			return
		}
		// On a context that survives Ctrl-C, for the reason the fetch and the
		// goodbye are: Ctrl-C is a likely way to reach this, and what is on
		// the other side is a card.
		//
		// The error is dropped rather than logged, and there is nowhere
		// honest to put it: this function reports through a sink that writes
		// a request's history, and a line there about a frame ember sent
		// while giving up on that request is noise in the one place an
		// operator reads to find out what happened. A send that fails means
		// the channel has gone, which ends the worker by itself.
		sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerDrainTimeout)
		defer cancel()
		_ = s.ch.Send(sctx, control.Frame{Kind: control.KindCancel})
	}()

	for i, j := range run.Jobs {
		body, err := j.Body(run.Preview, run.PreviewInterval)
		if err != nil {
			return fmt.Errorf("%s: %w", report.Label(report.UnitJob, i+1, total), err)
		}
		raw, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: encode the job: %w", report.Label(report.UnitJob, i+1, total), err)
		}
		// Index is one-based on the wire; see control.Frame on why a zero
		// index and an absent one would otherwise be the same bytes.
		if err := s.ch.Send(ctx, control.Frame{
			Kind: control.KindJob, Index: i + 1, Total: total,
			Stem: stems[i], Job: raw,
		}); err != nil {
			return fmt.Errorf("push %s to the worker: %w", report.Label(report.UnitJob, i+1, total), err)
		}
		pushed++
	}

	// THE CANCEL TRAVELS DOWN THE CHANNEL FROM A SECOND GOROUTINE, which is
	// the one place in this program where two goroutines write to a control
	// channel -- and the reason control.Conn serialises its sends. The
	// operator presses the button while a card is sampling, so the loop below
	// is blocked in a read and cannot be the thing that sends.
	//
	// A nil stop channel is never closed and a receive on one blocks forever,
	// so a caller with nothing to cancel needs no branch here.
	cancelCtx, stopCancel := context.WithCancel(ctx)
	defer stopCancel()
	go func() {
		select {
		case <-stop:
			if err := s.ch.Send(cancelCtx, control.Frame{Kind: control.KindCancel}); err != nil {
				// Not returned anywhere, because there is nowhere to return
				// it to: the batch is still running and the loop below
				// reports what actually happens to it. A cancel that could
				// not be sent shows up as a batch that completes, which is
				// the honest outcome.
				sink.Emit(report.Event{
					Kind: report.KindJobCancelled, Err: err.Error(),
				})
			}
		case <-cancelCtx.Done():
		}
	}()

	for ended < total {
		f, err := s.ch.Recv(ctx)
		if err != nil {
			return fmt.Errorf("read the worker's events: %w", err)
		}
		switch f.Kind {
		case control.KindStarted:
			i, err := jobIndex(f, total)
			if err != nil {
				return err
			}
			// Timed from when the NODE submitted it rather than from when
			// ember pushed it: what this figure is for is card time, and the
			// time a job spent queued in the worker is time the card spent on
			// the job before it.
			started[i] = time.Now()
			sink.Emit(report.Event{
				Kind: report.KindUnitStarted, Unit: report.UnitJob,
				Index: i, Total: total, Desc: run.Jobs[i-1].Describe(),
			})

		case control.KindProgress:
			i, err := jobIndex(f, total)
			if err != nil {
				return err
			}
			preview(sink, root, stems[i-1], i, total, f)

		case control.KindLanded:
			i, err := jobIndex(f, total)
			if err != nil {
				return err
			}
			if err := s.collect(ctx, sink, runDir, i, total, f); err != nil {
				return fmt.Errorf("%s: %w", report.Label(report.UnitJob, i, total), err)
			}

		case control.KindCancelled:
			// The operator's stop, reported once by the worker. Index and
			// Total are stamped HERE and not taken from the frame, because
			// which numbers they are differs: with a job in flight they name
			// that job, and in the gap between two jobs the index is how many
			// finished -- which is a fact about ember's own bookkeeping.
			asked = true
			e := report.Event{
				Kind:  report.KindJobCancelled,
				Index: ended, Total: total, Err: f.Err,
			}
			if f.InFlight {
				// No Unit when none was in flight: the cancel landed in the
				// gap between one job finishing and the next starting, and
				// Total-Index is then what was dropped.
				e.Unit, e.Index = report.UnitJob, ended+1
			}
			sink.Emit(e)
			if !f.InFlight {
				// The worker has dropped the rest of the batch, so no frame
				// about any of them is coming. Returning here is what stops
				// ember waiting for events that will never arrive -- and a
				// cancelled request means every job of it that has not
				// started, never starts.
				return ErrCancelled
			}

		case control.KindEnded:
			i, err := jobIndex(f, total)
			if err != nil {
				return err
			}
			ended++
			label := report.Label(report.UnitJob, i, total)
			switch {
			case f.Cancelled && f.Status == sdserver.StatusCancelled.String():
				// The expected end of a job ember asked to stop, AND IT HAS
				// TO BE TESTED BEFORE f.Err, which the lab found and no fake
				// had.
				//
				// A real cancelled job carries the server's own explanation
				// in `error`: {"code":"cancelled","message":"job cancelled by
				// client"}. With the refusal check first, every live cancel
				// came back as `sd-server refused it: cancelled: job
				// cancelled by client` -- a plain failure, not ErrCancelled
				// -- so the queue would have read the operator's own cancel
				// as a failed batch and DESTROYED THE RENTAL, taking every
				// other request on it. The exact outcome the whole feature
				// exists to avoid, reached by pressing the button it added.
				//
				// f.Cancelled keeps the other meaning available: a server
				// that cancelled a job nobody asked about has done something
				// ember cannot account for, and filing that under "the
				// operator pressed cancel" would record a request nobody
				// stopped as cancelled.
			case f.Err != "":
				return fmt.Errorf("%s: sd-server refused it: %s", label, f.Err)
			case f.Status != sdserver.StatusCompleted.String():
				return fmt.Errorf("%s: ended as %s", label, f.Status)
			}

			// The accumulating price, from ember's clock and ember's rate.
			// Card time, which is why it is measured across the collection
			// and not only the sampling -- and why a job whose started frame
			// never arrived is priced from this moment rather than from zero,
			// which would bill the rental's whole history to one job.
			step := s.rate.Cost(time.Since(startedAt(started, i)))
			spent += step
			sink.Emit(report.Event{
				Kind: report.KindUnitCost, Unit: report.UnitJob,
				Index: i, Total: total, Cost: step, Spent: spent,
			})

			// AFTER the cost, not before: the card time this job spent is
			// money the operator is billed for whether or not they stopped
			// it, and a return that skipped it would make a cancelled batch
			// look cheaper than it was -- the one direction a money figure
			// must never be wrong in.
			if f.Cancelled || asked {
				return ErrCancelled
			}

		case control.KindFailed:
			// Labelled when the worker said which job it was about, because
			// that is the sentence an operator reads off a batch of eight.
			// The label is ember's prose and the numbers are the worker's,
			// which is why the frame carries the second and not the first.
			if f.Index >= 1 && f.Index <= total {
				return fmt.Errorf("%s: %s", report.Label(report.UnitJob, f.Index, total), f.Err)
			}
			return fmt.Errorf("the worker stopped: %s", f.Err)

		case control.KindLog:
			sink.Emit(report.Event{Kind: report.KindLog, Text: f.Text})

		default:
			// Named rather than ignored. A frame nobody handles is a job
			// ember would wait for forever, and the version handshake is what
			// should have made this unreachable -- so arriving here means the
			// two ends disagree about what a version means.
			return fmt.Errorf("the worker sent a %q frame in the middle of a batch", f.Kind)
		}
	}
	// Every job ended, so there is nothing left for the worker to drop. The
	// deferred cancel above reads this.
	done = true
	return nil
}

// jobIndex is the untrusted-input check on every frame that names a job.
//
// A rented machine chooses this number, and `run.Jobs[f.Index-1]` on a number
// it chose is an out-of-range panic in the process that holds the operator's
// provider credentials. Checked once, here, so no call site can forget it.
func jobIndex(f control.Frame, total int) (int, error) {
	if f.Index < 1 || f.Index > total {
		return 0, fmt.Errorf("the worker sent a %s frame for job %d of a batch of %d: this node is reporting work ember never pushed",
			f.Kind, f.Index, total)
	}
	return f.Index, nil
}

// startedAt is when job i began, falling back to now.
//
// THE FALLBACK IS NOT COSMETIC. A zero time.Time is the year 1, and
// time.Since of it is two thousand years -- which Rate.Cost would turn into a
// price larger than every ceiling in the program, on a request's own row,
// because a started frame went missing. A job whose start ember never saw is
// priced at nothing instead, which is wrong in the direction that cannot
// trigger a false refusal and is visible beside the rental's own run_cost.
func startedAt(started map[int]time.Time, i int) time.Time {
	if t, ok := started[i]; ok {
		return t
	}
	return time.Now()
}

// preview writes one frame and reports where it went.
//
// The frame is written BEFORE the event that reports its step, and travels ON
// that event rather than on one of its own. A reader recording where the bytes
// went needs the step and the filename in the same value: with the name on a
// second event, a UI learns there is a step 4 before it learns there is a
// frame for it, and shows step 4 beside step 3's picture. The name is not
// re-derived either -- safeStem decides what a file is called, and a second
// spelling of that rule is a gallery pointing at names that are not on disk.
//
// Each distinct frame is written over the last one rather than accumulating,
// because the useful thing is a file an operator can leave open in a viewer
// and not three hundred PNGs of a half-finished cat. The worker is what drops
// a repeated step, which is also where dropping it saves the bytes.
func preview(sink report.Sink, root *os.Root, stem string, index, total int, f control.Frame) {
	var wrote []report.File
	var werr error
	if len(f.PNG) > 0 {
		var w sdserver.Written
		w, werr = sdserver.WriteImages(root, stem+"-preview", []sdserver.Image{{Data: f.PNG}})
		for _, each := range w.Each {
			wrote = append(wrote, report.File{Name: each.Name, Bytes: each.Bytes})
		}
	}
	sink.Emit(report.Event{
		Kind: report.KindJobProgress, Unit: report.UnitJob,
		Index: index, Total: total,
		Pass: f.Pass, Step: f.Step, TotalSteps: f.TotalSteps,
		Wrote: wrote,
	})
	if werr != nil {
		// Best effort: a preview that cannot be written is not a reason to
		// abandon a generation that is already paid for.
		sink.Emit(report.Event{
			Kind: report.KindPreviewFailed, Unit: report.UnitJob,
			Index: index, Total: total, Err: werr.Error(),
		})
	}
}

// collect brings home the files a worker named.
//
// THIS IS WHAT "an event names what landed" IS FOR. The images are on the
// node's own disk -- not on the store, which bills by the gigabyte-month and
// which nothing in ember deletes a file from -- and this fetch asks for
// exactly the paths the worker wrote rather than tarring a tree and hoping.
//
// ON A CONTEXT THAT SURVIVES Ctrl-C, for the reason every other fetch here is:
// the node is about to be destroyed either way, and these bytes have already
// been paid for. An operator pressing Ctrl-C is the most likely way to reach
// this function with images on a machine that is about to stop existing.
//
// WHAT IT COSTS, SAID PLAINLY: one `tar` over SSH per file, where the images
// used to arrive inside the poll response ember was already reading. For a
// 512-square image that is a few milliseconds of SSH session against the
// 80-96 s the card spent producing it, and it buys a node that writes its
// results down before anything fetches them -- so a dropped connection loses
// the fetch rather than the picture.
func (s served) collect(ctx context.Context, sink report.Sink, runDir string, index, total int, f control.Frame) error {
	if len(f.Files) == 0 {
		// The worker guards against this, and it is checked anyway because
		// what it would produce is a "0 file(s), 0 B" event that reads as a
		// job which finished and produced nothing.
		return errors.New("the worker named no files in a landed frame")
	}
	if f.Dir == "" {
		return errors.New("the worker named files without naming the directory they are in")
	}

	outs := make([]node.OutputPath, 0, len(f.Files))
	wrote := make([]report.File, 0, len(f.Files))
	for _, each := range f.Files {
		if each.Name == "" || strings.ContainsRune(each.Name, '/') {
			// A node is untrusted input. transfer.Extract's own containment
			// would hold -- every write goes through an os.Root -- and the
			// refusal is here as well because what this builds is a REMOTE
			// path for a tar command, where a name with a slash in it means
			// a directory nobody asked for rather than the file ember is
			// about to report by that name.
			return fmt.Errorf("the worker named a file as %q, which is not a single filename", each.Name)
		}
		outs = append(outs, node.OutputPath{Remote: path.Join(f.Dir, each.Name)})
		wrote = append(wrote, report.File{Name: each.Name, Bytes: each.Bytes})
	}

	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outputTimeout)
	defer cancel()

	res, err := transfer.Fetch(fetchCtx, s.runner, outs, runDir)
	if err != nil {
		return err
	}
	sink.Emit(report.Event{
		Kind: report.KindJobImages, Unit: report.UnitJob,
		Index: index, Total: total,
		// What ARRIVED, from the fetch's own count, and the names from the
		// frame. The two are the same files; the counts are the ones ember can
		// stand behind, because they are of bytes that are on this disk.
		Files: res.Files, Bytes: res.Bytes, Wrote: wrote,
	})
	return nil
}
