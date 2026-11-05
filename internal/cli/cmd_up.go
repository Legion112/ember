package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os/signal"
	"path/filepath"
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
	)
	logs := addLogFlags(fs)

	fs.Usage = func() {
		outf(out, "ember up rents a node, runs the jobs this config describes, and destroys it.\n\n")
		outf(out, "Nothing ember creates is reachable from the internet: the only port a node\npublishes is sshd, and ember reaches everything else over that one SSH connection.\n\n")
		outf(out, "WITH AN ember serve RUNNING, this submits the run to it over its unix socket\nand streams the result, so the run survives this terminal -- and Ctrl-C then\nDETACHES rather than destroying, which makes the ttl the only thing that stops\nthe bill. With no ember serve, it rents here and Ctrl-C destroys the node, as\nit always has. A paid provider needs that ember serve to have been started\nwith --allow-paid; --dry-run never asks one.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}

	// Before the config is read, because a mistyped level is a mistyped
	// command line and should not cost a file open -- the order
	// internal/s3's push already follows: check what is local and free
	// before what is remote and slow.
	//
	// IT IS STILL RESOLVED ON THE SUBMITTED PATH AND REACHES NOTHING THERE,
	// which is a gap said out loud rather than left to be noticed. A submitted
	// run's own log lines are written by the DAEMON, at the level `ember serve`
	// was started with, to the daemon's terminal -- so `ember up --log-level
	// debug` through a daemon still refuses a bad level here and changes
	// nothing about the run. Carrying the level on the wire was the
	// alternative and buys nothing: the extra lines would go to a terminal
	// this client cannot read. What the client does get either way is the
	// node's own bytes, which are events on this path.
	logOpts, err := logs.resolve()
	if err != nil {
		return err
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	ov := app.Overrides{
		Provider:     *providerName,
		Image:        *image,
		GPU:          *gpu,
		MaxHourlyUSD: *maxHourly,
		TTL:          *ttl,
	}

	// A RUNNING `ember serve` IS ASKED TO DO THIS INSTEAD, and a dry run never
	// is.
	//
	// The dry run rents nothing and prints what a rental would be, so routing
	// it through a daemon would make the cheapest thing in ember depend on a
	// process being up -- and it would have to answer with a plan rather than
	// a request id, which is a second shape on the route for no gain. See
	// upclient.go for the two paths and what differs between them.
	if !*dryRun {
		// The paths come from the file as written, before any override:
		// nothing on the command line names a state directory, and the socket
		// is under it. This is the ONLY thing the client reads the config for
		// -- the daemon reads the same file for everything else, which is the
		// reversal app.RunAsk documents.
		ps, perr := app.ResolvePaths(cfg, d)
		if perr != nil {
			return perr
		}
		if sock, ok := serveSocket(configPaths{State: ps.State}); ok {
			abs, aerr := filepath.Abs(*cfgPath)
			if aerr != nil {
				return aerr
			}
			// Its own signal context, and Ctrl-C here DETACHES: the run is in
			// the daemon, on the daemon's context, so there is nothing for
			// this cancellation to destroy. detached() says what that costs.
			sctx, sstop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer sstop()
			return upOverSocket(sctx, out, sock, app.RunAsk{
				Config: abs, Overrides: ov,
			})
		}
	}

	ov.Apply(&cfg)

	// Everything that can be refused for free happens in NewPlan: the store,
	// the offer, the image for the card that offer names, the jobs, the spec.
	// Nothing is rented until Do.
	plan, err := app.NewPlan(context.Background(), app.Request{
		Config: cfg, Deps: d, Log: logOpts,
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

	sink, finish, err := boundAndRecord(ctx, out, cfg, plan)
	if err != nil {
		return err
	}

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

// boundAndRecord is a shell rental's whole money-and-ledger step: it opens the
// generation history once, refuses a rental the configured period ceilings
// would carry a period past, and hands back the sink the run reports to.
//
// WHY THESE TWO THINGS ARE ONE FUNCTION. They need the same open store and
// they need it in one order. `max_day_usd` and `max_month_usd` reached neither
// `ember up` nor `ember store sync` before #165 -- five rental paths write
// `requests.cost` and two of them were gated -- so the key an operator wrote
// to bound a day bounded the queue alone. Closing that needs a SELECT over the
// ledger, which is the file `recordRental` was already opening a line later;
// two opens of one SQLite database in one process to answer two questions
// about the same rows is the kind of thing somebody later makes one anyway,
// and would make it in whichever order they happened to write.
//
// THE ORDER IS THE WHOLE CARE IN IT. The ceiling is checked BEFORE Adopt
// writes a row and before anything is created, which is the journal's own rule
// read from the other end: the row says a rental is coming, so a refused run
// that had already written one would have told the ledger about a machine
// nobody rented. It also leaves a second, independent witness that the gate
// fired where it claims to -- no row.
//
// RECORDING IS BEST-EFFORT AND BOUNDING IS NOT, which is the one asymmetry
// here and is the reason this is not simply "open the store, do both". A
// database that will not open -- one written by a newer ember, a state
// directory that is not writable -- is a diagnostic problem, and refusing to
// rent over it would be ember declining to do the thing it is for because it
// could not take notes. But a CONFIGURED ceiling with no ledger to read is a
// refusal: an operator who wrote `max_month_usd` has said unattended spending
// must stop somewhere, and "probably fine" is the one answer a spend ceiling
// must never give. So the same failure is a printed line when no ceiling is
// set and a refusal when one is, and nothing silently loses a ceiling.
//
// AND A FREE RENTAL IS A PRINTED LINE TOO, which the first form of this got
// wrong. `queue.Ceilings.Binds` is what the refusal asks, so the question is
// "could this rental be refused at all" rather than "is a ceiling set" --
// `ember up --provider local` projects zero, adds nothing to any period, and
// must run whatever the month has spent and whatever the history will do. Said
// here as well as at Binds because this is the only reachable caller of that
// arm, so this is where a reader would look for the rule and where the defect
// was.
//
// NOT ON A DRY RUN, and both callers return before reaching this. `--dry-run`
// rents nothing, so its projection is a rental that will not happen -- and a
// money ceiling that refused a command which spends nothing is the free-rental
// mistake with a different zero in it.
//
// THE CEILING IS THE NAMED CONFIG'S, because on this path that file is the
// only one there is: `ember up --config x.yaml` reads x.yaml for the ledger's
// own location as well, so the limit and the total it is compared against come
// from one place. The socket path is the one where two configs reach one
// ledger, and DirectRunner.Config has that argument.
func boundAndRecord(
	ctx context.Context,
	out io.Writer,
	cfg config.Config,
	plan app.Plan,
) (report.Sink, func(error), error) {
	cs, err := queue.CeilingsOf(cfg)
	if err != nil {
		return nil, nil, err
	}

	store, serr := genstore.Open(plan.Paths.State)
	if serr != nil {
		// Binds AND NOT IsZero, which is what the first form of this asked and
		// is the defect review found: a FREE rental -- `ember up --provider
		// local`, projection zero, adding nothing to any period -- was refused
		// outright by a ceiling ember could not check, against the paragraph
		// three above saying that is the mistake. The predicate and the
		// sentence are both queue's, because this arm and
		// queue.RefuseOverPeriod's `led == nil` arm are one money rule with
		// two readers and they used to word it differently.
		if cs.Binds(plan.Spec.TTL, plan.Offer.Price) {
			return nil, nil, cs.UnreadableLedger(serr)
		}
		sink, finish := declineToRecord(out, serr)
		return sink, finish, nil
	}

	// BEFORE Adopt AND WITH THE PLAN'S OWN FIGURES. plan.Offer is the card
	// that was chosen and plan.Spec.TTL is the deadline the node will get
	// after every override has had its say -- the two halves #157 measured as
	// gates that cannot fail when either is a literal zero.
	//
	// AND A COMMITTED TOTAL OF ZERO, which is honest rather than an omission:
	// this is a shell, so there is no loop of this process's whose rentals
	// could be in flight and unrecorded. Another `ember serve`'s in-flight
	// rental is invisible here, which is the residue commit.go names and is
	// unchanged by max_nodes -- a figure in one process's memory is not
	// something a second process can read.
	if err := queue.RefuseOverPeriod(ctx, cs, store, time.Now(), plan.Spec.TTL, plan.Offer.Price, 0); err != nil {
		_ = store.Close()
		return nil, nil, err
	}

	sink, finish := recordRental(out, store, plan)
	return sink, finish, nil
}

// declineToRecord says out loud that this run is not going in the history, and
// runs it anyway.
func declineToRecord(out io.Writer, err error) (report.Sink, func(error)) {
	outf(out, "not recording this run in the generation history: %v\n", err)
	return report.Text(out), func(error) {}
}

// recordRental puts this run in the generation history and hands back the sink
// the run reports to.
//
// IT IS NAMED FOR A RENTAL AND NOT FOR `up`, because `ember store sync` calls
// it too: a machine with no card rented for as long as fifty-six gigabytes
// take to arrive is a rental, and it was in no ledger either -- the same hole
// #148 found in the queue's own store fill, reached from the command line. A
// name that claimed one caller while two used it is the shape of defect this
// repository records about `AssertNoStray`.
//
// THE STORE IS A PARAMETER NOW, because boundAndRecord above needs the same
// open one for the period ceilings and the two had to happen in one order.
// Closing it stays here, with the finish that is the last thing to use it.
//
// WHY A RENTAL FROM A SHELL RECORDS, AND WHY WITHOUT BEING ASKED. `ember serve` was the
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
// THE ADOPT AND THE RECORDER ARE queue.AdoptRun's, SHARED WITH THE SUBMITTED
// PATH. This function is the shell's half of it: open the store, hand
// report.Text in as an extra sink so the operator's terminal gets the lines,
// and close the store when the run ends. A second copy of the row and the
// recorder would be two things to keep in step the first time one gained a
// column -- which is the reason the recorder moved out of Queue in the first
// place, met again one layer up.
func recordRental(out io.Writer, store genstore.Store, plan app.Plan) (report.Sink, func(error)) {
	text := report.Text(out)

	_, sink, finish, err := queue.AdoptRun(context.Background(), store, plan,
		plan.Options.Log, time.Now, text)
	if err != nil {
		_ = store.Close()
		return declineToRecord(out, err)
	}
	return sink, func(runErr error) {
		finish(runErr)
		_ = store.Close()
	}
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
	outf(out, "dry-run: would run %s (%s)\n", spec.Image, plan.ImageFrom)
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
// IT IS app.Overrides NOW, and the move is the point rather than tidiness. It
// REPLACES rather than narrows, deliberately: `ember up --max-hourly-usd 999`
// raises the ceiling, because the operator typing it is the operator who set
// the ceiling -- and queue.Narrow takes the smaller of the two because a
// queued request arrives from somewhere the operator is not. Two rules that
// opposite in one program is exactly the thing that gets copied to the wrong
// place, so there is one implementation of each and the privileged socket
// route CALLS this one rather than growing a second copy. See
// app.Overrides, which has the whole argument.
func applyFlags(cfg *config.Config, providerName, image, gpu, maxHourly string, ttl time.Duration) {
	app.Overrides{
		Provider:     providerName,
		Image:        image,
		GPU:          gpu,
		MaxHourlyUSD: maxHourly,
		TTL:          ttl,
	}.Apply(cfg)
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }
