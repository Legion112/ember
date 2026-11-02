package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"syscall"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/queue"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/workload"
)

// deps is internal/app's injection seam, under the name every test here
// already uses.
//
// An alias and not a wrapper: the fields are the same fields, and a wrapper
// would be a second struct to keep in step with the first.
type deps = app.Deps

func cmdUp(args []string, out io.Writer) error { return runUp(args, out, deps{}) }

func runUp(args []string, out io.Writer, d deps) error {
	fs := flag.NewFlagSet("up", flag.ContinueOnError)
	fs.SetOutput(out)

	var (
		providerName = fs.String("provider", "", "which provider to rent from")
		image        = fs.String("image", "", "workload image, pinned to a tag or digest")
		ttl          = fs.Duration("ttl", 0, "how long the node may live before it destroys itself")
		gpu          = fs.String("gpu", "", "GPU model to require, for example rtx4090")
		maxHourly    = fs.String("max-hourly-usd", "", "refuse any offer dearer than this per hour")
		cfgPath      = fs.String("config", config.FileName, "path to ember.yaml")
		dryRun       = fs.Bool("dry-run", false, "say what would be rented and create nothing")
		holdFor      = fs.Duration("hold", 0, "stop after this long instead of waiting for Ctrl-C")
	)
	logs := addLogFlags(fs)
	var ports multiFlag
	fs.Var(&ports, "port", "forward a port, as name:remote or name:remote:local (repeatable)")
	var cmdArgs multiFlag
	fs.Var(&cmdArgs, "cmd", "a word of the command to run on the node (repeatable, in order)")

	fs.Usage = func() {
		outf(out, "ember up rents a node, forwards its ports to loopback, and destroys it.\n\n")
		outf(out, "Every forwarded port is bound to 127.0.0.1 and reached over SSH.\nNothing ember creates is reachable from the internet.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	// Before the config is read, because a mistyped level is a mistyped
	// command line and should not cost a file open -- the order
	// internal/s3's push already follows: check what is local and free
	// before what is remote and slow.
	logOpts, err := logs.resolve()
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	applyFlags(&cfg, *providerName, *image, *gpu, *maxHourly, *ttl, ports)
	if len(cmdArgs) > 0 {
		cfg.Cmd = cmdArgs
	}

	// Everything that can be refused for free happens in NewPlan: the store,
	// the offer, the image for the card that offer names, the jobs, the spec.
	// Nothing is rented until Do.
	plan, err := app.NewPlan(context.Background(), app.Request{
		Config: cfg, Deps: d, Hold: *holdFor, Log: logOpts,
	}, out)
	if err != nil {
		return err
	}

	if *dryRun {
		return dryRunUp(fs.Output(), plan)
	}

	// The first Ctrl-C unwinds through WithNode's deferred destroy, which
	// runs on a context that survives this cancellation. A second one is the
	// operator's choice, and the state record is already on disk by then.
	//
	// Installed here and not in internal/app, which takes a context and
	// installs nothing. A handler per run would mean a server that provisions
	// on request installing one SIGINT handler per rental -- swallowing the
	// signal while a run is in flight and killing the process when none is.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sink, finish := recordUp(out, plan)

	runErr := plan.Do(ctx, out, sink)

	// AFTER Do RETURNS AND NOT DEFERRED, because the ordering is the whole
	// reason the rental's cost is recorded at all: provision.WithNode's
	// destroy is deferred inside Do, so by the time it returns the run_cost
	// event has already been emitted and written through. A defer here would
	// run at the same point and read the same way; this is a statement
	// because the sequence is load-bearing and a reader should not have to
	// work out which defer wins.
	finish(runErr)
	return runErr
}

// recordUp puts this run in the generation history and hands back the sink the
// run reports to.
//
// WHY `ember up` RECORDS, AND WHY WITHOUT BEING ASKED. `ember serve` was the
// only writer to the history, so the command every measurement in README was
// taken with printed its cost to a terminal and kept nothing: `ember history`,
// `ember spend --by gpu|day|provider|model` and every per-job cost and timing
// column were blind to it. A flag would have left that true for anyone who did
// not remember to pass it, which is the failure, not the fix -- and recording
// adds no write the command was not already making, because provision refuses
// to rent at all without the journal in the same state directory.
//
// IT CANNOT FAIL THE RUN. A database that will not open -- one written by a
// newer ember, a state directory that is not writable -- is a diagnostic
// problem, and refusing to rent over it would be ember declining to do the
// thing it is for because it could not take notes. Said out loud rather than
// swallowed, on internal/app's reportMemory discipline: the one line printed
// here is the only output `ember up` has gained, it appears only when
// recording is impossible, and a silent "recorded nothing" is how an operator
// comes to trust a ledger with a hole in it.
//
// THE REQUEST ID IS FRESH AND RANDOM, which is #47's fifth question. The run
// id would have been the tidier answer -- one identifier, and the join to
// `ember ls` for free -- and it does not exist yet: provision.WithNode mints
// it after this point, and the row has to be written BEFORE the create call,
// because the window between asking for a machine and reading the answer is
// where a rental nothing recorded is born. Keying on the run id would have
// meant writing the row from the first event instead, which silently drops the
// two runs most worth having: one whose create call failed, and one whose node
// billed without ever becoming ready. The node's own handle is written through
// from node_ready, so the join to `ember ls` survives; what is left empty is
// run_id, which neither reader prints.
func recordUp(out io.Writer, plan app.Plan) (report.Sink, func(error)) {
	text := report.Text(out)
	decline := func(err error) (report.Sink, func(error)) {
		outf(out, "not recording this run in the generation history: %v\n", err)
		return text, func(error) {}
	}

	store, err := genstore.Open(plan.Paths.State)
	if err != nil {
		return decline(err)
	}

	// WHAT WAS ASKED FOR, and nothing about what it produced. Adopt's INSERT
	// names no more columns than Enqueue's, so the cost, the node, the memory
	// figures and the model cannot be set here even by mistake: they are
	// written through from the events the run emits.
	//
	// The ceilings come from the resolved Options rather than from the config
	// strings, because a flag beats the file for `ember up` -- applyFlags
	// REPLACES -- and the number that bounded this run is the one worth
	// recording.
	id, err := store.Adopt(context.Background(), genstore.Request{
		Provider: string(plan.Provider.Name()),

		// THE CARD THAT WAS RENTED, NOT THE ONE THE FILTER NAMED, and the
		// difference is the whole of what `ember spend --by gpu` is for. A
		// queued request has only a selection to record, because it is stored
		// before an offer exists; `ember up` has already chosen one by the
		// time this runs -- and most measurement runs name no `gpu:` at all,
		// so recording the filter would group every one of them under a dash
		// in the column that exists to tell cards apart. Empty here means a
		// machine with no card, which is what `ember store sync` rents.
		//
		// It is the provider's own normalised name, the same spelling
		// node.Filter.Matches compares against, so `ember history --gpu
		// rtx4090` finds these rows.
		GPU:       upGPU(plan),
		Offer:     string(plan.Offer.Token),
		TTL:       plan.Spec.TTL,
		MaxHourly: plan.Options.Budget.MaxRate,
		MaxRun:    plan.Options.Budget.MaxRun,

		Preview:         plan.Jobs.Preview,
		PreviewInterval: plan.Jobs.PreviewInterval,
		Jobs:            upJobs(plan),
	})
	if err != nil {
		_ = store.Close()
		return decline(err)
	}

	rec := &queue.Recorder{
		Store: store, Req: id, Now: time.Now, Log: plan.Options.Log,
		// Key stays nil: `ember up` writes into a timestamped run directory
		// it tells the operator about, which no gallery serves and which
		// `ember history rm` must not offer to remove. The image counts and
		// bytes are recorded regardless -- they are columns on the job, which
		// is what the ledger and the aggregate read. See Recorder.Key.
	}

	// WRAPPED IN A Stream, which is not decoration: emitters do not number
	// events, and an unnumbered one arrives as Seq 0 -- where genstore's
	// INSERT OR REPLACE on (request, seq) makes every event overwrite the
	// last, leaving one row for a whole generation. That was a real bug in
	// the queue, found by reading the database rather than by a test. Text
	// ignores Seq, so `ember up`'s output is byte-for-byte what it was.
	return report.NewStream(time.Now, text, rec), func(runErr error) {
		// WithoutCancel, because the ordinary way to end `ember up` is
		// Ctrl-C: the context this run used is already cancelled by the time
		// there is an outcome to record, and the row is the only lasting
		// record of what the rental cost.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), recordTimeout)
		defer cancel()
		rec.Finish(ctx, runErr)
		_ = store.Close()
	}
}

// recordTimeout bounds the last write, so a wedged database cannot hold a
// finished command open.
const recordTimeout = 10 * time.Second

// upGPU is the card that was rented, or empty for a machine with none.
//
// THROUGH HasGPU AND NOT STRAIGHT OFF THE OFFER, because the local provider
// spells a machine with no card `node.GPUNone` -- the literal "none" -- while
// a queued request with no selection stores the empty string. Both mean "no
// card", and two spellings of it in one column is what keyOr exists to avoid:
// the reader would print a dash for one and the word "none" for the other, in
// a report whose whole point is telling cards apart. HasGPU is the one
// function that decides what counts as a card, so asking it keeps the answer
// in one place. Found by reading the output of a local run.
func upGPU(plan app.Plan) string {
	if !plan.Offer.HasGPU() {
		return ""
	}
	return string(plan.Offer.GPU)
}

// upJobs is the batch as the history records it.
//
// Only the served shape has any. A `cmds` run's steps and a plain forwarded
// session are recorded as the request row and its events and as no job rows at
// all -- see internal/queue/record.go, which argues it: a step is not a job,
// and the figures the jobs table exists for (images, passes, steps per second)
// are not things an `ffmpeg` invocation has.
func upJobs(plan app.Plan) []genstore.Job {
	jobs := make([]genstore.Job, 0, len(plan.Jobs.Jobs))
	for _, j := range plan.Jobs.Jobs {
		jobs = append(jobs, genstore.FromSD(j))
	}
	return jobs
}

// dryRunUp prints what would happen and creates nothing.
//
// A pure print: the offer was chosen by NewPlan, because the image depends on
// it. So the budget refusal that TestUp_DryRunStillEnforcesTheBudget exercises
// happens in PickOffer rather than inside this function -- same exit, earlier
// line.
func dryRunUp(out io.Writer, plan app.Plan) error {
	spec, run := plan.Spec, plan.Jobs
	outf(out, "dry-run: would rent %s from %s at %v for %v\n",
		plan.Offer.Token, plan.Provider.Name(), plan.Offer.Price, spec.TTL)
	outf(out, "dry-run: would run %s (%s) and forward %d port(s)\n",
		spec.Image, plan.ImageFrom, len(spec.Ports))
	for i, c := range spec.Cmds {
		if len(spec.Cmds) > 1 {
			outf(out, "dry-run: step %d/%d: %s\n", i+1, len(spec.Cmds), workload.Describe(c))
			continue
		}
		outf(out, "dry-run: would execute: %s\n", workload.Describe(c))
	}
	if len(spec.Serve) > 0 {
		outf(out, "dry-run: would serve: %s\n", workload.Describe(spec.Serve))
		// The listen address is printed because ember appended it rather than
		// the config choosing it, and an operator reading a dry run should see
		// what ember added as clearly as what they wrote.
		for i, j := range run.Jobs {
			outf(out, "dry-run: job %d/%d: %s\n", i+1, len(run.Jobs), j.Describe())
		}
		outf(out, "dry-run: previews %s every %d step(s)\n", run.Preview, run.PreviewInterval)
	}
	outf(out, "dry-run: nothing was created\n")
	return nil
}

// applyFlags lets a flag beat the config file.
//
// It REPLACES rather than narrows, deliberately: `ember up --max-hourly-usd
// 999` raises the ceiling, because the operator typing it is the operator who
// set the ceiling. Anything that accepts a request from somewhere else -- a
// web handler -- must not reuse this, and must narrow against the config's
// own values instead.
func applyFlags(cfg *config.Config, providerName, image, gpu, maxHourly string, ttl time.Duration, ports multiFlag) {
	if providerName != "" {
		cfg.Provider = providerName
	}
	if image != "" {
		cfg.Image = image
		// And the map goes with it. An operator pinning one image for one run
		// is the first thing anybody reaches for when a run misbehaves, and
		// leaving the map in place would mean -image silently did nothing on
		// every config that has one. One field holds the answer, so there is
		// no "which wins" rule at the decision site.
		cfg.Images = nil
	}
	if gpu != "" {
		cfg.GPU = gpu
	}
	if maxHourly != "" {
		cfg.MaxHourlyUSD = maxHourly
	}
	if ttl > 0 {
		cfg.TTL = ttl
	}
	if len(ports) > 0 {
		cfg.Ports = ports
	}
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
