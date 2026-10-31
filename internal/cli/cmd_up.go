package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os/signal"
	"path/filepath"
	"sort"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/Legion112/ember/internal/assets"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/dockerengine/dockerclient"
	"github.com/Legion112/ember/internal/gpucheck"
	"github.com/Legion112/ember/internal/hostkey"
	"github.com/Legion112/ember/internal/keys"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provider/local"
	"github.com/Legion112/ember/internal/provider/runpod"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/session"
	"github.com/Legion112/ember/internal/sshx"
	"github.com/Legion112/ember/internal/sshx/sshexec"
	"github.com/Legion112/ember/internal/statestore"
	"github.com/Legion112/ember/internal/transfer"
	"github.com/Legion112/ember/internal/tunnel"
	"github.com/Legion112/ember/internal/tunnel/sshdial"
	"github.com/Legion112/ember/internal/workload"
)

// deps are the collaborators cmdUp needs, injected so the command is testable
// without a Docker daemon or a real home directory.
type deps struct {
	Engine   dockerengine.API
	ConfigIn string
	HomeDir  string
	Signals  []int

	// Creds overrides what is read from the environment and from disk, so a
	// test can register a cloud provider without a real token and without
	// touching the operator's credentials file.
	Creds *config.Credentials

	// DetectGPU overrides what the local provider finds in this machine.
	//
	// The same hazard Creds exists for. Left to resolve itself it reads the
	// developer's own graphics card, and a test asserting "nothing matches
	// --gpu h100" would pass on CI and fail on a workstation -- which is
	// exactly what happened when local GPU support was added.
	DetectGPU func(ctx context.Context) local.GPU

	// CloudBaseURL redirects every cloud provider's API.
	//
	// Without it a test that registers RunPod with a fake token talks to
	// api.runpod.io for real: slow, dependent on somebody's network, and in
	// CI a stream of authentication failures against a live service. The
	// first version of the test below did exactly that, and the comment
	// claiming otherwise was the only thing standing in for this field.
	CloudBaseURL string

	// S3BaseURL redirects the store upload endpoint.
	//
	// Separate from CloudBaseURL because they are separate services on
	// separate hosts, and for the same reason that one exists: a test that
	// pushed to a store must not be able to reach RunPod. Without it the
	// endpoint is derived from the store's datacentre and is a real
	// hostname.
	S3BaseURL string
}

// credentials resolves this run's provider tokens.
func (d deps) credentials(configDir string) (config.Credentials, error) {
	if d.Creds != nil {
		return *d.Creds, nil
	}
	return config.LoadCredentials(configDir)
}

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

	paths, err := resolvePaths(cfg, d)
	if err != nil {
		return err
	}

	creds, err := d.credentials(paths.config)
	if err != nil {
		return err
	}
	reg, err := buildRegistry(d, creds, cfg)
	if err != nil {
		return err
	}
	p, err := reg.Get(node.ProviderID(cfg.Provider))
	if err != nil {
		return usageError{err}
	}

	// The store is resolved before the spec is built, not after.
	//
	// Only the provider knows what a store's id is -- RunPod's is an opaque
	// token, the local provider's is the volume name -- so this needs the
	// registry. Doing it first is what lets NewSpec enforce that assets
	// always have somewhere to land: a spec built before resolution would
	// carry assets with no store and have to be patched afterwards, which is
	// an invariant that holds only if everybody remembers to patch.
	//
	// It also happens before anything is rented, because a run that
	// discovers its store is missing after creating a node has already
	// started paying for the discovery.
	store, err := attachStore(context.Background(), p, cfg)
	if err != nil {
		return err
	}

	filter, err := buildFilter(cfg, store)
	if err != nil {
		return usageError{err}
	}

	// Options before the offer, because PickOffer needs the budget.
	opts, err := buildOptions(paths, cfg, out)
	if err != nil {
		return err
	}

	// The offer is chosen here, for the same reasons the store is resolved
	// above: only the provider knows what it has, and the image depends on the
	// answer. An `images:` map is keyed by the architecture of the card, so
	// which card was picked has to be known before a Spec can be built -- and
	// a Spec built first would carry an image chosen for nothing in particular
	// and have to be patched afterwards, which is an invariant that holds only
	// if everybody remembers to patch.
	//
	// It is also the only place in the program where an offer is chosen. The
	// provisioner is handed this one rather than listing again, so the image
	// cannot have been selected for a different card than the one rented.
	offer, err := provision.PickOffer(context.Background(), p, filter, cfg.TTL, opts)
	if err != nil {
		return err
	}

	ref, from, err := resolveImage(cfg, offer)
	if err != nil {
		return usageError{err}
	}

	spec, err := buildSpec(cfg, resolved{
		Store: store, Filter: filter, Offer: offer, Image: ref, ImageFrom: from,
	})
	if err != nil {
		return usageError{err}
	}

	if *dryRun {
		return dryRunUp(fs.Output(), p, offer, spec, from)
	}

	// The first Ctrl-C unwinds through WithNode's deferred destroy, which
	// runs on a context that survives this cancellation. A second one is the
	// operator's choice, and the state record is already on disk by then.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return provision.WithNode(ctx, p, spec, offer, opts, func(ctx context.Context, s provision.Session) error {
		return serveSession(ctx, out, s, opts, spec, *holdFor, paths.outputs)
	})
}

// serveSession opens the tunnel and holds it until the operator stops or the
// node's deadline arrives. The decisions it used to make inline -- when to
// retry a dial, when to refuse one -- live in internal/session, where they
// can be tested against an in-process SSH server instead of a rented machine.
func serveSession(ctx context.Context, out io.Writer, s provision.Session, opts provision.Options, spec node.Spec, hold time.Duration, outDir string) error {
	client, so, err := dialNode(ctx, s, opts, hold)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// The card before the weights, and the weights before the workload.
	//
	// This order is the cheap-failure order. A dead GPU costs the probe
	// timeout; a dead GPU discovered after `verify: always` costs the probe
	// timeout plus re-hashing every byte on the store, which for a 56 GiB
	// model set is a minute of a card that cannot run it anyway.
	if err := gpucheck.Wait(ctx, sshexec.New(client.SSH()), spec.GPUProbe, spec.GPUProbeTimeout, out, so.Log); err != nil {
		return err
	}

	// Assets next, and a failure here means the workload never starts.
	//
	// The cheapest moment to discover that the weights are wrong is before
	// the GPU has been asked to do anything with them. It runs over the same
	// connection the workload uses, so its output streams live and its exit
	// status is known when it happens rather than polled for afterwards.
	if err := ensureAssets(ctx, out, sshexec.New(client.SSH()), spec, so); err != nil {
		return err
	}

	var serveErr error
	if len(spec.Cmd) > 0 {
		// A node rented to run something stops when it has run it. Holding
		// afterwards would bill for a machine with nothing left to do, which
		// is the whole failure mode ember exists to prevent.
		serveErr = runWorkload(ctx, out, client, sshexec.New(client.SSH()), spec, so)
	} else {
		serveErr = session.Serve(ctx, out, client, session.Forwards(spec.Ports), so)
	}

	// Outputs are fetched even when the session ended badly, and on a context
	// that survives the operator's Ctrl-C. The node is about to be destroyed
	// either way: whatever it produced is about to stop existing, and a run
	// that failed halfway is often exactly the one whose partial output and
	// logs are worth having.
	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), outputTimeout)
	defer cancel()

	if err := fetchOutputs(fetchCtx, out, sshexec.New(client.SSH()), s.RunID, spec.Outputs, outDir); err != nil {
		if serveErr != nil {
			return errors.Join(serveErr, err)
		}
		return err
	}
	return serveErr
}

// runWorkload holds the forwards open while the command runs.
//
// The forwards run alongside rather than instead: a workload may well want to
// reach a service on the node, and a ComfyUI run is exactly that. They are
// torn down when the command finishes, because the node is about to be.
// Both seams are parameters rather than one client, so the decision and the
// reporting here are testable against a recording dialer and runner instead
// of needing a node.
func runWorkload(ctx context.Context, out io.Writer, d tunnel.Dialer, r sshx.Runner, spec node.Spec, so session.Options) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if fwds := session.Forwards(spec.Ports); len(fwds) > 0 {
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

	outf(out, "running: %s\n", workload.Describe(spec.Cmd))
	res, err := workload.Run(ctx, r, spec.Cmd, out)
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
func fetchOutputs(ctx context.Context, out io.Writer, r sshx.Runner, runID node.RunID, outputs []node.OutputPath, base string) error {
	if len(outputs) == 0 {
		return nil
	}

	dir := transfer.RunDir(base, runID, time.Now())
	outf(out, "fetching %d output path(s) into %s\n", len(outputs), dir)

	res, err := transfer.Fetch(ctx, r, outputs, dir)

	// What arrived is reported even when something failed, because "3 of 4
	// paths, 1.2 GiB" is what tells the operator whether the run is worth
	// keeping.
	outf(out, "fetched %d file(s), %s\n", res.Files, humanBytes(res.Bytes))
	if err != nil {
		return fmt.Errorf("fetch outputs into %s: %w", dir, err)
	}
	return nil
}

// humanBytes is for one line of operator-facing output.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v, exp := float64(n), 0
	for v >= unit && exp < 4 {
		v /= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", v, "KMGT"[exp-1])
}

// dryRunUp prints what would happen and creates nothing.
//
// A pure print now: the offer was chosen by runUp, because the image depends
// on it. So the budget refusal that TestUp_DryRunStillEnforcesTheBudget
// exercises now happens in PickOffer at runUp's call site rather than inside
// this function -- same exit, earlier line.
func dryRunUp(out io.Writer, p provider.Provider, offer node.Offer, spec node.Spec, imageFrom string) error {
	outf(out, "dry-run: would rent %s from %s at %v for %v\n", offer.Token, p.Name(), offer.Price, spec.TTL)
	outf(out, "dry-run: would run %s (%s) and forward %d port(s)\n", spec.Image, imageFrom, len(spec.Ports))
	if len(spec.Cmd) > 0 {
		outf(out, "dry-run: would execute: %s\n", workload.Describe(spec.Cmd))
	}
	outf(out, "dry-run: nothing was created\n")
	return nil
}

type paths struct{ config, state, outputs string }

func resolvePaths(cfg config.Config, d deps) (paths, error) {
	if d.HomeDir != "" {
		return paths{
			config:  filepath.Join(d.HomeDir, "config"),
			state:   filepath.Join(d.HomeDir, "state"),
			outputs: filepath.Join(d.HomeDir, "outputs"),
		}, nil
	}
	c, err := cfg.ConfigDirOrDefault()
	if err != nil {
		return paths{}, err
	}
	s, err := cfg.StateDirOrDefault()
	if err != nil {
		return paths{}, err
	}
	o, err := cfg.OutputDirOrDefault()
	if err != nil {
		return paths{}, err
	}
	return paths{config: c, state: s, outputs: o}, nil
}

// buildRegistry registers every provider this run can reach.
//
// A provider with no credential is left out rather than registered and left
// to fail: `ember ls` reports an unreachable provider as a finding and exits
// 2, so registering RunPod on a machine that has never had a token would make
// every clean run look like a problem. The absence is reported by `ember
// offers` and by a named-provider lookup instead, where it is actionable.
func buildRegistry(d deps, creds config.Credentials, cfg config.Config) (*provider.Registry, error) {
	engine := d.Engine
	if engine == nil {
		c, err := dockerclient.NewClient()
		if err != nil {
			return nil, err
		}
		engine = c
	}
	reg := provider.NewRegistry()
	lp := local.New(engine)
	// Injected the same way credentials are, and for the same reason: left
	// to resolve itself, this reads the developer's own graphics card, and
	// the CLI tests would behave differently on a machine that has one.
	if d.DetectGPU != nil {
		lp.Detect = d.DetectGPU
	}
	if err := reg.Add(lp); err != nil {
		return nil, err
	}

	if !creds.RunPod.IsZero() {
		rp := runpod.New(creds.RunPod)
		if d.CloudBaseURL != "" {
			rp.HTTP.BaseURL = d.CloudBaseURL
		}
		rp.Cloud = cfg.RunPodCloud
		rp.DataCenters = cfg.DataCenters
		rp.DiskGiB = cfg.DiskGiB
		if err := reg.Add(rp); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func buildOptions(ps paths, cfg config.Config, out io.Writer) (provision.Options, error) {
	store, err := openStore(ps.state)
	if err != nil {
		return provision.Options{}, err
	}
	pins, err := hostkey.OpenStore(filepath.Join(ps.state, "known_hosts.json"))
	if err != nil {
		return provision.Options{}, err
	}
	id, err := keys.EnsureIdentity(ps.config)
	if err != nil {
		return provision.Options{}, err
	}
	hourly, run, err := cfg.Budgets()
	if err != nil {
		return provision.Options{}, usageError{err}
	}
	return provision.Options{
		Store:    store,
		Pins:     pins,
		Identity: id,
		Budget:   provision.Budget{MaxRate: hourly, MaxRun: run},
		Log:      slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}, nil
}

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

// buildFilter is the offer filter this config and store describe.
//
// Extracted from buildSpec because the filter is now needed before the spec
// exists: PickOffer runs first, so the image can be chosen for the card that
// was actually picked. Computed once and passed to both, because two filters
// built from the same config is two things that can disagree -- and the one
// that would disagree is the one that chose the offer.
func buildFilter(cfg config.Config, store Attached) (node.Filter, error) {
	filter, err := cfg.Filter()
	if err != nil {
		return node.Filter{}, err
	}
	// The store's own datacentre wins over whatever the config asked for.
	// They can only disagree by mistake, and the store is the half that
	// cannot move: the volume is where it is, and a node that mounts it has
	// to be created there.
	if store.Region != "" {
		filter.Region = store.Region
	}
	return filter, nil
}

// resolved is what runUp has to learn before a Spec can be validated.
//
// The store, because only the provider knows its id; the offer, because the
// image depends on which card it is for; and the image, because NewSpec will
// not accept a Spec without one. Written down as data so the ordering is a
// thing a reader can see rather than infer from the order of statements.
type resolved struct {
	Store  Attached
	Filter node.Filter
	Offer  node.Offer
	Image  string

	// ImageFrom says where Image came from, for `--dry-run`. An operator
	// staring at a digest needs to know which key produced it.
	ImageFrom string
}

// resolveImage picks the workload image for the card that will run it.
//
// The rules, and the one that matters is the third:
//
//   - A run that needs no card -- `ember store sync` on a CPU box -- uses
//     image: and never consults the map. There is no architecture to key on,
//     and a CPU machine is not a card ember failed to identify.
//   - No images: map at all means image:, which is the shape ember has always
//     had and always will: the CPU workload variant and the node image have no
//     architecture either.
//   - An images: map with no entry for this card REFUSES. It does not fall
//     back to image:. Falling back would run an image built for another
//     architecture, and that does not fail -- sd-cli finds no usable kernel,
//     drops to the CPU backend, and bills a GPU to do it. That is the failure
//     internal/gpucheck exists to catch, and catching it here costs nothing
//     because nothing has been rented yet.
func resolveImage(cfg config.Config, offer node.Offer) (string, string, error) {
	images, err := cfg.ParseImages()
	if err != nil {
		return "", "", err
	}

	scalar := func() (string, string, error) {
		if cfg.Image == "" {
			return "", "", fmt.Errorf("no image: pass -image or set it in %s", config.FileName)
		}
		return cfg.Image, "image:", nil
	}

	switch {
	case len(images) == 0:
		return scalar()

	case !offer.HasGPU():
		// A CPU offer, or the local provider's no-card offer. The map cannot
		// answer and is not consulted, so say which key is needed instead of
		// reporting a card ember could not identify.
		if cfg.Image == "" {
			return "", "", fmt.Errorf("this run needs no GPU, so there is no architecture to choose an "+
				"image by; images: cannot answer and image: is not set. Add image: to %s, or pass -image",
				config.FileName)
		}
		return cfg.Image, "image:", nil
	}

	cap, how := offer.ComputeCap()
	switch how {
	case node.CapNotCUDA:
		return "", "", fmt.Errorf("%s is not a CUDA card, so images: cannot choose for it. "+
			"Set image: to one built for it, or filter to a card ember has an image for: %v",
			offer.GPU, node.KnownCaps())
	case node.CapUnknown:
		return "", "", fmt.Errorf("ember does not know which CUDA architecture %q is, so it cannot "+
			"choose an image for it.\nA wrong-architecture image does not fail -- it runs on the CPU "+
			"backend at the GPU's price -- so this is a refusal rather than a guess.\n"+
			"Add the card to internal/node/compute.go, or filter to one of: %v",
			offer.GPU, node.KnownCards())
	}

	ref, ok := images[cap]
	if !ok {
		var have []string
		for c := range images {
			have = append(have, c.Key())
		}
		sort.Strings(have)
		return "", "", fmt.Errorf("no image for %s, which is what %s is; images: has %v.\n"+
			"ember will not fall back to image: here: an image built for another architecture does "+
			"not fail on this card, it runs on the CPU backend and bills the GPU for it.\n"+
			"Publish one with `make publish-sd-cuda CUDA_ARCH=%d` and add its digest under %s",
			cap, offer.GPU, have, int(cap), cap.Key())
	}
	return ref, fmt.Sprintf("images.%s, for %s", cap.Key(), offer.GPU), nil
}

func buildSpec(cfg config.Config, r resolved) (node.Spec, error) {
	ports, err := cfg.ParsePorts()
	if err != nil {
		return node.Spec{}, err
	}
	outputs, err := cfg.ParseOutputs()
	if err != nil {
		return node.Spec{}, err
	}

	probe := cfg.GPUProbe
	if r.Filter.CPUOnly {
		probe = nil
	}
	// Checked before anything is rented. A command ember cannot send is a
	// config error, and finding a config error after the create call is
	// finding it after the billing started.
	if err := workload.Validate(cfg.Cmd); err != nil {
		return node.Spec{}, err
	}
	assetList, err := cfg.ParseAssets()
	if err != nil {
		return node.Spec{}, err
	}
	// Parsed here rather than on the node, so a misspelled `verify: alwyas`
	// fails before anything is rented instead of after.
	if _, err := assets.ParsePolicy(cfg.Verify); err != nil {
		return node.Spec{}, err
	}

	return node.NewSpec(node.SpecOptions{
		Assets: assetList,
		Verify: cfg.Verify,

		// A run that asks for no card has nothing to probe, and NewSpec
		// refuses the combination. `ember store sync` configures CPUOnly and
		// may be pointed at a config whose gpu_probe exists for the workload
		// runs, so the probe is dropped here rather than making the operator
		// keep two configs.
		GPUProbe:        probe,
		GPUProbeTimeout: cfg.GPUProbeTimeout,
		MinCUDA:         cfg.MinCUDA,

		// Already resolved, and already the one reference this run will use.
		// NewSpec is still the only way to obtain a validated Spec, and it
		// still applies the pin rule -- now to the entry that was chosen
		// rather than to a scalar that might not have been the one that ran.
		Image:   r.Image,
		Cmd:     cfg.Cmd,
		Ports:   ports,
		Filter:  r.Filter,
		TTL:     cfg.TTL,
		Outputs: outputs,
		Store:   r.Store.Handle,
		Mount:   r.Store.Mount,
	})
}

// multiFlag collects a repeatable string flag.
type multiFlag []string

func (m *multiFlag) String() string     { return fmt.Sprint([]string(*m)) }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// openStore is a thin wrapper so cmd_up does not import statestore twice.
func openStore(dir string) (*statestore.Store, error) {
	return statestore.Open(filepath.Join(dir, statestore.DirName))
}

// ensureAssets puts every asset on the store before the workload runs.
//
// Sequential rather than concurrent. These are gigabytes over one link, so
// parallel fetches would share the same bandwidth and arrive no sooner, while
// making the progress output impossible to read and a failure impossible to
// attribute.
func ensureAssets(ctx context.Context, out io.Writer, r sshx.Runner, spec node.Spec, so session.Options) error {
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
	// buildSpec, before anything was rented; this is the conversion.
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

	so := session.Options{Log: opts.Log, Hold: hold}
	client, err := session.Dial(ctx, sshdial.Target{
		NodeID:   s.Node.Handle().ID,
		Endpoint: s.Node.Endpoint(),
	}, s.Trust, signers, so)
	if err != nil {
		return nil, so, err
	}
	return client, so, nil
}
