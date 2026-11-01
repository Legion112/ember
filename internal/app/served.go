package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/sshx/sshexec"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/workload"
)

// The poll cadence, which starts fast and backs off.
//
// A FLAT TWO SECONDS WAS WRONG, AND MEASURED SO. Three sd15 images at
// 256x256 and four steps take about 0.3s each to sample, and a flat 2s poll
// noticed each one up to two seconds late: the marginal cost per job came to
// 2.08s served against 1.81s for a fresh sd-cli per image. The resident model
// saved about 0.8s of tensor loading per job and ember's own poll interval
// then spent more than that waiting, so the served path measured *slower* at
// every batch size tried -- 1, 3 and 8 jobs -- and the gap widened. The whole
// point of the feature, hidden by its own client.
//
// Starting at jobPollMin and doubling to jobPollMax fits both ends: a short
// job is noticed almost at once, and a long one settles to a cadence that
// does not pull a preview frame every tenth of a second. The poll is a
// loopback HTTP call over an SSH channel that is already open, and with
// previews off it carries nothing, so the fast end is nearly free.
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
	Generate(ctx context.Context, sink report.Sink, jobs sdserver.Run, dir string) error

	// Caps is what this server build reported it can do.
	Caps() sdserver.Capabilities
}

// served is the real Served: a client pointed at the node's own loopback.
type served struct {
	client sdserver.Client
	caps   sdserver.Capabilities
	rate   node.Rate
}

func (s served) Caps() sdserver.Capabilities { return s.caps }

func (s served) Generate(ctx context.Context, sink report.Sink, jobs sdserver.Run, dir string) error {
	for i, j := range jobs.Jobs {
		if err := sdserver.Validate(j, s.caps); err != nil {
			return Usage(fmt.Errorf("sd.jobs[%d]: %w", i, err))
		}
	}
	return runJobs(ctx, sink, s.client, jobs, dir, s.rate)
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
			client, so, err := p.prepare(ctx, out, s)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			return withServer(ctx, out, sink, client, sshexec.New(client.SSH()),
				p.Spec, so, p.Offer.Price, len(p.Jobs.Jobs), fn)
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
	return withServer(ctx, out, sink, client, r, spec, so, rate, len(run.Jobs),
		func(ctx context.Context, s Served) error {
			return s.Generate(ctx, sink, run, runDir)
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

	remote := tunnel.RemoteLoopback(sdserver.Port)
	if err := orExited(ctx, proc, "it accepted a connection", func() error {
		return tunnel.WaitReady(ctx, client, remote, time.Second)
	}); err != nil {
		return fmt.Errorf("sd-server is not listening on %v: %w", remote, err)
	}

	c := sdserver.New(client.DialContext)
	var load time.Duration
	if err := orExited(ctx, proc, "the weights finished loading", func() error {
		var werr error
		load, werr = sdserver.WaitModel(ctx, c, spec.ServeTimeout, out, so.Log)
		return werr
	}); err != nil {
		return err
	}

	caps, err := c.Capabilities(ctx)
	if err != nil {
		return err
	}

	// The figure this whole shape exists to stop paying repeatedly, said out
	// loud: with cmds it would be spent again for every job after the first.
	//
	// Measured by ember rather than read from the server's own
	// load_elapsed_seconds, because that field is not this number. The first
	// FLUX.2 run reported 551ms for a model whose weights are 56 GiB, so
	// ember printed "551ms of loading is paid once rather than 8 times"
	// while the cmds arm of the same measurement spent 80-96s per image
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
	so.Log.Debug("sd-server load", "measured", time.Since(serveStart), "server_reported", load)
	if batch > 1 {
		sink.Emit(report.Event{
			Kind:     report.KindModelResident,
			Measured: time.Since(serveStart),
			Reported: load,
			Total:    batch,
		})
	}

	return fn(ctx, served{client: c, caps: caps, rate: rate})
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

// runJobs submits one job at a time and collects its images before the next.
//
// ONE JOB IN FLIGHT, and the reason is in sd-server's own source rather than
// its documentation. async_jobs.h sets completed_ttl_seconds = 600 and
// async_jobs.cpp measures that from completed_at and purges from the worker --
// when it wakes before popping the next job, and after each job finishes. So a
// finished result is erased ten minutes after it completed, and api.md lists
// 410 Gone for a job without ever saying why.
//
// Queueing ten jobs up front would therefore leave eight finished images, each
// already paid for, one dropped connection away from being deleted. Submitting
// one at a time costs nothing in throughput -- the generation context lock
// serializes work on the device anyway -- and makes the TTL unreachable, the
// 64-job pending cap irrelevant and queue_position moot.
func runJobs(ctx context.Context, sink report.Sink, c sdserver.Client, run sdserver.Run, runDir string, rate node.Rate) error {
	// Created here, because nothing else on this path does. The fetching
	// paths get it from transfer.Fetch's own MkdirAll; a served run never
	// calls Fetch, since the images arrive in the HTTP response rather than
	// as a tar. Without this, every served run failed at the first image with
	// "no such file or directory" after the generation had been paid for.
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return fmt.Errorf("create the run's output directory: %w", err)
	}
	root, err := os.OpenRoot(runDir)
	if err != nil {
		return fmt.Errorf("open the run's output directory: %w", err)
	}
	defer func() { _ = root.Close() }()

	var spent node.MicroUSD
	for i, j := range run.Jobs {
		label := report.Label(report.UnitJob, i+1, len(run.Jobs))
		stem := j.Output
		if stem == "" {
			// Numbered from the position in the list, so a config that names
			// nothing still produces files an operator can tell apart.
			stem = fmt.Sprintf("%03d", i+1)
		}

		start := time.Now()
		sink.Emit(report.Event{
			Kind: report.KindUnitStarted, Unit: report.UnitJob,
			Index: i + 1, Total: len(run.Jobs), Desc: j.Describe(),
		})

		id, err := c.Submit(ctx, j, run.Preview, run.PreviewInterval)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}

		st, err := awaitJob(ctx, sink, c, id, i+1, len(run.Jobs), stem, root)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if st.Err != nil {
			return fmt.Errorf("%s: sd-server refused it: %w", label, st.Err)
		}
		if st.Status != sdserver.StatusCompleted {
			return fmt.Errorf("%s: ended as %s", label, st.Status)
		}

		w, err := sdserver.WriteImages(root, stem, st.Images)
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		wrote := make([]report.File, 0, len(w.Each))
		for _, f := range w.Each {
			wrote = append(wrote, report.File{Name: f.Name, Bytes: f.Bytes})
		}
		sink.Emit(report.Event{
			Kind: report.KindJobImages, Unit: report.UnitJob,
			Index: i + 1, Total: len(run.Jobs),
			Files: w.Files, Bytes: w.Bytes, Wrote: wrote,
		})

		// The accumulating price, the same reporting runSteps does and for the
		// same reason: an operator watching a ten-job batch can stop paying
		// for the rest. Card time, which is why it is timed across the
		// collection and not only the sampling.
		step := rate.Cost(time.Since(start))
		spent += step
		sink.Emit(report.Event{
			Kind: report.KindUnitCost, Unit: report.UnitJob,
			Index: i + 1, Total: len(run.Jobs), Cost: step, Spent: spent,
		})
	}
	return nil
}

// awaitJob polls one job to a conclusion, reporting progress as it goes.
//
// Progress is reported as pass, step and total, never as a single percentage:
// sd-server's step and total_steps describe the current sampling pass and not
// the whole job, and passes restart for each batch item, each high- and
// low-noise stage and each highres pass. A bar built from them runs to the end
// and then starts again, which reads as a hang.
//
// Each distinct frame is written over the last one rather than accumulating,
// because the useful thing is a file an operator can leave open in a viewer
// and not three hundred PNGs of a half-finished cat.
func awaitJob(ctx context.Context, sink report.Sink, c sdserver.Client, id string, index, total int, stem string, root *os.Root) (sdserver.State, error) {
	lastStep := -1
	wait := jobPollMin
	for {
		st, err := c.Poll(ctx, id)
		if err != nil {
			return sdserver.State{}, err
		}
		if p := st.Preview; p != nil && p.Step != lastStep {
			lastStep = p.Step
			sink.Emit(report.Event{
				Kind: report.KindJobProgress, Unit: report.UnitJob,
				Index: index, Total: total,
				Pass: p.Pass, Step: p.Step, TotalSteps: p.TotalSteps,
			})
			if len(p.PNG) > 0 {
				// Best effort: a preview that cannot be written is not a
				// reason to abandon a generation that is already paid for.
				if _, werr := sdserver.WriteImages(root, stem+"-preview", []sdserver.Image{{Data: p.PNG}}); werr != nil {
					sink.Emit(report.Event{
						Kind: report.KindPreviewFailed, Unit: report.UnitJob,
						Index: index, Total: total, Err: werr.Error(),
					})
				}
			}
		}
		if st.Status.Done() {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return sdserver.State{}, ctx.Err()
		case <-time.After(wait):
		}
		wait = nextPoll(wait)
	}
}
