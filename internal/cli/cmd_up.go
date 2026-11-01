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
		Config: cfg, Deps: d, Hold: *holdFor,
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

	return plan.Do(ctx, out, report.Text(out))
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
