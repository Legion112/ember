package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/assets"
	"github.com/Legion112/ember/internal/gpucheck"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/logx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/nodemem"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/report"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/sshx/sshexec"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
	"github.com/Legion112/ember/internal/workload"
)

// Do rents the node this plan chose, does what the spec asks, and destroys it.
//
// The node's lifetime is the closure's: provision.WithNode's deferred destroy
// runs on a context that survives the caller's cancellation, so there is no
// path out of here that leaves a machine running.
//
// out carries byte streams -- a workload's stdout, an sd-server's complaint --
// and sink carries ember's own progress. They are separate parameters because
// they are separate things: for `ember up` both end at the same terminal, and
// for a server the first becomes log events and the second becomes rows in a
// database.
func (p Plan) Do(ctx context.Context, out io.Writer, sink report.Sink) error {
	return provision.WithNode(ctx, p.Provider, p.Spec, p.Offer, p.lifecycle(sink),
		func(ctx context.Context, s provision.Session) error {
			return p.serve(ctx, out, sink, s)
		})
}

// lifecycle returns this plan's Options with the two moments a caller cannot
// otherwise see routed to sink.
//
// A COPY, so the hooks belong to this run and not to the process. Options is
// a value and `ember serve` builds one at startup and shares it -- see
// BuildOptions on why the journal and the pin store must be per process --
// so assigning the hooks in place would send one run's progress to whichever
// run started last.
//
// The destroy half is the one that is otherwise unreachable: WithNode's
// teardown is deferred, so it runs after the closure above has already
// returned. A reader of events that stopped when the pipeline returned would
// never see what the rental cost, and would miss "the node may still be
// running and billing" entirely -- which is the one line an operator must not
// miss.
func (p Plan) lifecycle(sink report.Sink) provision.Options {
	o := p.Options
	o.OnReady = func(h node.Handle, e node.Endpoint, deadline time.Time, price node.Rate) {
		sink.Emit(report.Event{
			Kind: report.KindNodeReady,
			Node: h.String(), Endpoint: e.String(),
			Deadline: deadline, Rate: price,
		})
	}
	o.OnDestroyed = func(h node.Handle, bill node.Billing, err error) {
		if err != nil {
			sink.Emit(report.Event{
				Kind: report.KindDestroyFailed,
				Node: h.String(), Err: err.Error(),
			})
			return
		}
		sink.Emit(report.Event{
			Kind: report.KindRunCost,
			Node: h.String(), Cost: bill.Cost, Rate: bill.Rate,
			Elapsed: bill.Elapsed, Billable: bill.Billable,
		})
	}
	return o
}

// SyncAssets rents a machine with no card and has it fill the store.
//
// The same rental discipline as Do and a different thing done on it: no
// workload follows, and nothing is brought home, because the bytes land on
// the store which is the whole point.
func (p Plan) SyncAssets(ctx context.Context, out io.Writer) error {
	return provision.WithNode(ctx, p.Provider, p.Spec, p.Offer, p.Options,
		func(ctx context.Context, s provision.Session) error {
			client, so, err := dialNode(ctx, s, p.Options, 0)
			if err != nil {
				return err
			}
			defer func() { _ = client.Close() }()
			return EnsureAssets(ctx, out, sshexec.New(client.SSH()), p.Spec, so)
		})
}

// prepare dials the node and gets it ready to be used.
//
// THE ORDER IS THE CHEAP-FAILURE ORDER, and it is the reason this is one
// function rather than three lines repeated by every shape. A dead GPU costs
// the probe timeout; a dead GPU discovered after `verify: always` costs the
// probe timeout plus re-hashing every byte on the store, which for a 56 GiB
// model set is a minute of a card that cannot run it anyway. So: the card,
// then the weights, then whatever the node was rented for.
//
// The returned client is the caller's to close.
func (p Plan) prepare(ctx context.Context, out io.Writer, sink report.Sink, s provision.Session) (*sshdial.Client, session.Options, error) {
	client, so, err := dialNode(ctx, s, p.Options, p.Hold)
	if err != nil {
		return nil, so, err
	}

	if err := gpucheck.Wait(ctx, sshexec.New(client.SSH()), p.Spec.GPUProbe, p.Spec.GPUProbeTimeout, out, so.Log); err != nil {
		_ = client.Close()
		return nil, so, err
	}

	// Assets next, and a failure here means the workload never starts.
	//
	// The cheapest moment to discover that the weights are wrong is before
	// the GPU has been asked to do anything with them. It runs over the same
	// connection the workload uses, so its output streams live and its exit
	// status is known when it happens rather than polled for afterwards.
	if err := EnsureAssets(ctx, out, sshexec.New(client.SSH()), p.Spec, so); err != nil {
		_ = client.Close()
		return nil, so, err
	}

	// And the memory facts last, because the asset figure is a stat of the
	// weights and the weights are only there once the step above has run.
	// Still before the workload, which is what matters: this is the question
	// "can this node hold the model" asked before the card is paid to find
	// out the hard way.
	reportMemory(ctx, sink, sshexec.New(client.SSH()), p.Spec, so)
	return client, so, nil
}

// reportMemory records what the node says about its memory, and swallows
// every failure.
//
// IT CANNOT FAIL A RUN, which is the whole shape of it: these are reads of
// four files and a stat of the weights, and a node that will not answer them
// is a node whose generation should still happen. The alternative -- a probe
// that can refuse -- would be a diagnostic with the authority of a safety
// check, over a value nothing yet acts on.
//
// Logged at warn rather than silently, because a probe that stopped working
// would otherwise leave an empty column that reads as "this node had no
// limit" -- which is the answer that makes a slow node look fine.
func reportMemory(ctx context.Context, sink report.Sink, r sshx.Runner, spec node.Spec, so session.Options) {
	if sink == nil {
		return
	}
	paths, err := nodemem.AssetPaths(spec.Mount, spec.Assets)
	if err != nil {
		so.Log.Warn("could not name this run's assets for the memory probe", "err", err)
		return
	}
	rep, err := nodemem.Probe(ctx, r, paths)
	if err != nil {
		so.Log.Warn("could not read the node's memory facts; the run is unaffected", "err", err)
		return
	}
	if rep.IsZero() {
		// Nothing at all came back, which is not the same as a node with no
		// limit: recording zeros would put a row in the history claiming
		// facts nobody read.
		so.Log.Warn("the node reported no memory facts at all; nothing recorded")
		return
	}
	sink.Emit(report.Event{
		Kind:       report.KindNodeMemory,
		MemCgroup:  rep.CgroupVersion,
		MemLimit:   rep.CgroupMax,
		MemCurrent: rep.CgroupCurrent,
		MemTotal:   rep.HostTotal,
		MemCached:  rep.HostCached,
		Bytes:      rep.Assets,
	})
}

// serve opens the tunnel and does what the spec asks.
//
// The decisions it used to make inline -- when to retry a dial, when to
// refuse one -- live in internal/session, where they can be tested against an
// in-process SSH server instead of a rented machine.
func (p Plan) serve(ctx context.Context, out io.Writer, sink report.Sink, s provision.Session) error {
	client, so, err := p.prepare(ctx, out, sink, s)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// Decided once, here, rather than inside fetchOutputs: a batch fetches
	// several times and every step of one run has to land in one directory.
	runDir := transfer.RunDir(p.Paths.Outputs, s.RunID, time.Now())

	// THE NODE'S OWN LOG, COLLECTED ONCE FOR EVERY SHAPE BELOW.
	//
	// A defer rather than a line in each of the three branches -- steps,
	// served, interactive -- because three call sites for one decision are
	// three that can disagree, and the branch that would be missed is
	// whichever one is added next. It runs before `client.Close()` above:
	// defers unwind last-registered-first, and that one was registered
	// first.
	//
	// It covers every rental that brings something home. `store sync` does
	// not, because nothing on that path has an output directory at all --
	// the bytes it rents a machine for land on the store, which is the whole
	// point of it.
	defer collectNodeLog(ctx, sshexec.New(client.SSH()), runDir, so.Log)

	// A node rented to run something stops when it has run it. Holding
	// afterwards would bill for a machine with nothing left to do, which is
	// the whole failure mode ember exists to prevent. So this returns rather
	// than falling through to session.Serve, and runSteps does its own
	// fetching -- one fetch per step, not one at the end.
	if len(p.Spec.Cmds) > 0 {
		return runSteps(ctx, out, sink, client, sshexec.New(client.SSH()), p.Spec, so, runDir, p.Offer.Price)
	}

	// The served shape. Same rule as above -- a node rented to generate
	// something stops when it has -- reached a different way: the weights
	// stay in VRAM between jobs instead of being read again for each one.
	if len(p.Spec.Serve) > 0 {
		return runServed(ctx, out, sink, client, sshexec.New(client.SSH()), p.Spec, p.Jobs, so, runDir, p.Offer.Price)
	}

	serveErr := session.Serve(ctx, out, client, session.Forwards(p.Spec.Ports), so)

	// Outputs are fetched even when the session ended badly, and on a context
	// that survives the operator's Ctrl-C. The node is about to be destroyed
	// either way: whatever it produced is about to stop existing, and a run
	// that failed halfway is often exactly the one whose partial output and
	// logs are worth having.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outputTimeout)
	defer cancel()

	if err := fetchOutputs(fetchCtx, out, sshexec.New(client.SSH()), p.Spec.Outputs, runDir, ""); err != nil {
		if serveErr != nil {
			return errors.Join(serveErr, err)
		}
		return err
	}
	return serveErr
}

// collectNodeLog brings embernode's own log home, and cannot fail a run.
//
// WHY THE NODE'S LOG NEEDS BRINGING HOME AT ALL. embernode installs the host
// key, installs the authorized key, starts sshd and arms the self-destruct
// timer, and it wrote to a stream nothing in ember read -- so the component
// whose failures are hardest and most expensive to diagnose was the one whose
// evidence ember destroyed by design, on a context that survives Ctrl-C.
//
// ON A CONTEXT THAT SURVIVES Ctrl-C, for the reason every other fetch here
// is: the node is about to be destroyed either way, and the run that failed
// halfway is exactly the one whose log is worth having. The operator pressing
// Ctrl-C is the most likely way to reach this function.
//
// EVERY FAILURE IS SWALLOWED, AT DEBUG. A log fetch that could fail a run
// would make logging load-bearing, which is the one thing it must never be --
// and the ways this fails are ordinary: an older node image whose embernode
// predates the file, a node whose sshd is already gone, a /tmp that was
// read-only. GNU tar over a missing path exits non-zero and writes a valid
// empty archive, so "there is no log" arrives here as an error rather than as
// an empty result, and treating it as fatal would turn a successful
// generation into a failed command.
//
// NOTHING IS PRINTED ON SUCCESS. `ember up`'s default output is asserted by
// test/integration/lifecycle_test.go, and a diagnostic that is always there
// is not worth a line an operator has to read past on every run. README says
// where the file lands; --log-level debug says that it did.
func collectNodeLog(ctx context.Context, r sshx.Runner, dir string, log *slog.Logger) {
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), nodeLogTimeout)
	defer cancel()

	res, err := transfer.Fetch(fetchCtx, r,
		[]node.OutputPath{{Remote: logx.NodeLogPath}}, dir, "")
	if err != nil {
		log.Debug("could not bring the node's own log home", "path", logx.NodeLogPath, "err", err)
		return
	}
	log.Debug("the node's log is home", "dir", dir, "bytes", res.Bytes)
}

// nodeLogTimeout bounds it, and is short because the file is small.
//
// outputTimeout is thirty minutes because an output can be minutes of video.
// This is a few kilobytes of text, and it is fetched on the path where the
// node is already being torn down -- so a stalled connection here would hold
// up a destroy, and a destroy that is held up is a bill that keeps running.
const nodeLogTimeout = time.Minute

// fetchMarker is the file on the node whose mtime bounds the next fetch.
//
// /tmp and not the output directory, so it never lands in an archive and is
// never mistaken for a result.
const fetchMarker = "/tmp/.ember-fetch-mark"

// runSteps runs each command in turn and brings that step's output home
// before the next one starts.
//
// Fetching between steps rather than once at the end is the point. A ten-step
// batch is over an hour on a rented card; fetching only at the end means an
// expired deadline, a dropped connection or a closed laptop loses all of it
// rather than everything so far, and an operator cannot look at the third
// result and decide to stop paying for the other seven.
//
// The first step fetches everything, which is exactly what a single-command
// run did before this existed. Each later step fetches only what is newer
// than the previous fetch, so a file already home is not sent again -- which
// matters little for a 2 MB image and a great deal for the minutes of video
// the store was built for.
func runSteps(
	ctx context.Context,
	out io.Writer,
	sink report.Sink,
	client tunnel.Dialer,
	r sshx.Runner,
	spec node.Spec,
	so session.Options,
	runDir string,
	rate node.Rate,
) error {
	var errs []error
	since := ""
	var spent node.MicroUSD

	for i, cmd := range spec.Cmds {
		if len(spec.Cmds) > 1 {
			sink.Emit(report.Event{
				Kind: report.KindUnitStarted, Unit: report.UnitStep,
				Index: i + 1, Total: len(spec.Cmds), Desc: workload.Describe(cmd),
			})
		}
		// Timed from here and not from workload.Run's own duration, because
		// the fetch below is billed too: the card stays rented while the
		// output comes over the wire, which for the minutes of video the
		// store was built for is not a rounding error. "Card time" has to
		// mean all of the card's time or the figure is just wrong.
		stepStart := time.Now()

		stepErr := runWorkload(ctx, out, client, r, cmd, spec.Ports, so)
		if stepErr != nil {
			errs = append(errs, fmt.Errorf("%s: %w", report.Label(report.UnitStep, i+1, len(spec.Cmds)), stepErr))
		}

		// On a context that survives Ctrl-C, for the same reason the
		// single-command path does it: the node is about to be destroyed and
		// a step that failed is often the one whose output is worth having.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outputTimeout)
		ferr := fetchOutputs(fetchCtx, out, r, spec.Outputs, runDir, since)
		cancel()
		if ferr != nil {
			errs = append(errs, ferr)
		}

		// The accumulating price, so an operator watching a ten-step batch can
		// decide to stop paying for the rest -- which is half the reason the
		// outputs come home as they are produced rather than at the end.
		//
		// Card time, and the wording says so: the rental also billed for
		// provisioning, the image pull and the asset verify, and a batch pays
		// those once for all of it rather than once per step. Folding them
		// into a per-step figure would overstate what batching saves, which is
		// the number this whole feature exists to improve.
		//
		// Printed only for a real batch. A single command's output is
		// byte-for-byte what it was before `cmds` existed, because somebody is
		// parsing it.
		if len(spec.Cmds) > 1 {
			step := rate.Cost(time.Since(stepStart))
			spent += step
			sink.Emit(report.Event{
				Kind: report.KindUnitCost, Unit: report.UnitStep,
				Index: i + 1, Total: len(spec.Cmds), Cost: step, Spent: spent,
			})
		}

		// Stop at the first failure. A batch where step 3 failed and 4
		// through 10 ran anyway is a batch nobody can trust, and the
		// remaining steps would bill for work the operator no longer wants.
		if stepErr != nil {
			break
		}
		if i < len(spec.Cmds)-1 {
			if err := markFetched(ctx, r); err != nil {
				// Not fatal: a missing marker means the next fetch sends
				// everything again, which is wasteful rather than wrong.
				so.Log.Warn("could not mark the fetch point; the next step will re-send what is already home", "err", err)
				since = ""
			} else {
				since = fetchMarker
			}
		}
	}
	return errors.Join(errs...)
}

// markFetched touches the marker on the node, after a fetch, so the next one
// sends only what the next step writes.
func markFetched(ctx context.Context, r sshx.Runner) error {
	// workload.Run reports a non-zero exit as an error as well as a code, so
	// the error is the whole answer; a separate ExitCode check here would be
	// unreachable.
	_, err := workload.Run(ctx, r, []string{"touch", fetchMarker}, io.Discard)
	return err
}

// runWorkload holds the forwards open while the command runs.
//
// The forwards run alongside rather than instead: a workload may well want to
// reach a service on the node, and a ComfyUI run is exactly that. They are
// torn down when the command finishes, because the node is about to be.
// Both seams are parameters rather than one client, so the decision and the
// reporting here are testable against a recording dialer and runner instead
// of needing a node.
func runWorkload(ctx context.Context, out io.Writer, d tunnel.Dialer, r sshx.Runner, cmd []string, ports []node.PortMap, so session.Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if fwds := session.Forwards(ports); len(fwds) > 0 {
		for _, f := range fwds {
			outf(out, "  %-12s http://%s\n", f.Name, f.Local[0])
		}
		go func() {
			// A forward that fails while a workload is running is logged and
			// not fatal: the command may not be using it, and killing a
			// four-minute generation because a port ember opened on the
			// operator's behalf went away would be the wrong trade.
			f := &tunnel.Forwarder{Dialer: d, Log: so.Log}
			if err := f.Run(ctx, fwds); err != nil && ctx.Err() == nil {
				so.Log.Warn("a forward stopped while the workload was running", "err", err)
			}
		}()
	}

	outf(out, "running: %s\n", workload.Describe(cmd))
	res, err := workload.Run(ctx, r, cmd, out)
	outf(out, "workload finished in %s\n", res.Duration.Round(time.Millisecond))
	return err
}

// outputTimeout bounds the output fetch.
//
// Generous, because a run of generated video is gigabytes over a home upload
// link, and ungenerous enough that a wedged transfer cannot hold the node --
// and the bill -- open indefinitely.
const outputTimeout = 30 * time.Minute

// fetchOutputs brings the run's outputs home.
//
// It takes the sshx.Runner seam rather than the SSH client, so the reporting
// and the error handling here are testable against a recording runner instead
// of needing a node.
// dir is the run's output directory, decided once by the caller. It used to
// be computed here from time.Now(), which was invisible while a run fetched
// once and wrong the moment it fetched per step: three steps produced three
// directories a minute apart, for one run.
func fetchOutputs(ctx context.Context, out io.Writer, r sshx.Runner, outputs []node.OutputPath, dir, since string) error {
	if len(outputs) == 0 {
		return nil
	}

	outf(out, "fetching %d output path(s) into %s\n", len(outputs), dir)

	res, err := transfer.Fetch(ctx, r, outputs, dir, since)

	// What arrived is reported even when something failed, because "3 of 4
	// paths, 1.2 GiB" is what tells the operator whether the run is worth
	// keeping.
	outf(out, "fetched %d file(s), %s\n", res.Files, report.HumanBytes(res.Bytes))
	if err != nil {
		return fmt.Errorf("fetch outputs into %s: %w", dir, err)
	}
	return nil
}

// EnsureAssets puts every asset on the store before the workload runs.
//
// Sequential rather than concurrent. These are gigabytes over one link, so
// parallel fetches would share the same bandwidth and arrive no sooner, while
// making the progress output impossible to read and a failure impossible to
// attribute.
func EnsureAssets(ctx context.Context, out io.Writer, r sshx.Runner, spec node.Spec, so session.Options) error {
	if len(spec.Assets) == 0 {
		return nil
	}
	if spec.Mount.IsZero() {
		// Unreachable through NewSpec, which refuses assets without a store,
		// and checked anyway because what it guards is a fetch writing
		// gigabytes to the node's own disk and throwing them away with it.
		return fmt.Errorf("%d asset(s) with nowhere to put them: no store is mounted", len(spec.Assets))
	}

	// Parsed once rather than per asset. It was already validated in
	// BuildSpec, before anything was rented; this is the conversion.
	policy, err := assets.ParsePolicy(spec.Verify)
	if err != nil {
		return err
	}

	for _, a := range spec.Assets {
		cmd, err := assets.Command(spec.Mount, a, policy)
		if err != nil {
			return err
		}
		res, err := workload.Run(ctx, r, append([]string{cmd.Path}, cmd.Args...), out)
		if err != nil {
			return fmt.Errorf("asset %s: %w", a.As, err)
		}
		if res.ExitCode != 0 {
			return fmt.Errorf("asset %s: the node refused it (exit %d); nothing was run against it", a.As, res.ExitCode)
		}
		so.Log.Debug("asset ready", "as", a.As, "took", res.Duration)
	}
	return nil
}

// dialNode opens the SSH connection to a ready node.
//
// Shared by `up` and `store sync` rather than written twice: the host key is
// pinned the same way for both, and a second copy is a second place for the
// one security decision in this program to drift.
func dialNode(ctx context.Context, s provision.Session, opts provision.Options, hold time.Duration) (*sshdial.Client, session.Options, error) {
	signers := []ssh.Signer{opts.Identity.Signer}
	if agentSigners, err := keys.AgentSigners(); err == nil {
		signers = append(signers, agentSigners...)
	}

	// s.Log and not opts.Log: the first carries this rental's run id, which
	// provision.WithNode attached once. Reading it off Options here would
	// hand every package below -- session, tunnel, gpucheck, sdserver -- a
	// logger whose lines cannot be joined to the node they are about.
	so := session.Options{Log: s.Log, Hold: hold}
	client, err := session.Dial(ctx, sshdial.Target{
		NodeID:   s.Node.Handle().ID,
		Endpoint: s.Node.Endpoint(),
	}, s.Trust, signers, so)
	if err != nil {
		return nil, so, err
	}
	return client, so, nil
}
