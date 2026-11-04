package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/Legion112/ember/internal/assets"
	"github.com/Legion112/ember/internal/blob"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/logx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/provision"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/tts"
	"github.com/Legion112/ember/internal/workload"
)

// Request is what a caller decides about one run. Everything else comes from
// the Config.
//
// A flag parser fills this in for `ember up`; a handler fills it in for a
// queued generation. Nothing here can widen a ceiling the Config set -- see
// BuildOptions, which reads the budget from the Config and nowhere else.
type Request struct {
	Config config.Config
	Deps   Deps

	// Hold ends the session after this long instead of waiting for the
	// context. Zero waits.
	Hold time.Duration

	// CPUOnly narrows this run to a machine with no card, which is what
	// `ember store sync` rents: a GPU idling while a download runs is a GPU
	// being paid for to do nothing.
	CPUOnly bool

	// VCPU is how many cores a CPU-only run wants; zero takes the minimum
	// for the family.
	VCPU int

	// Jobs replaces the config's sd.jobs, for a batch that came from
	// somewhere other than the file -- a queued request.
	//
	// WHY NOT GO THROUGH THE CONFIG. Writing them into cfg.SD.Jobs would mean
	// converting through config.SDJob, which is the YAML shape and carries
	// Extra -- the field that reaches every untyped part of the generation
	// API, including the ones naming file paths on the node. A request may
	// not set it, so routing a request through the type that can is handing
	// it a field it was refused. These are sdserver.Job directly, and
	// sdserver.ValidateStatic is the one function that refuses a bad one
	// whichever direction it arrived from.
	//
	// sdserver.Job does have Extra, so the seal is not here -- it is at
	// genstore.Job, which has no such field, and whose SD() leaves it nil.
	// This package is reached from internal/cli, where a config may
	// legitimately use it, and from internal/queue, where nothing can.
	Jobs            []sdserver.Job
	Preview         string
	PreviewInterval int

	// Budget overrides what the Config's own budget keys say, for a caller
	// that has already narrowed a ceiling.
	//
	// WHY NOT WRITE IT BACK INTO THE CONFIG. Those keys are decimal strings,
	// so a caller holding integer microdollars would have to render one --
	// and node.MicroUSD's own String is "$%.6g", which is scientific notation
	// for small values: one microdollar prints as $1e-06, which node.ParseUSD
	// refuses outright. The round trip is lossy exactly where money must not
	// be, so there is no round trip. Found by round-tripping every value the
	// path has to carry rather than a plausible-looking one.
	//
	// It can only narrow, because whoever sets it is expected to have taken
	// the smaller of the two already; nothing here re-checks, because nothing
	// here knows which config the caller meant.
	Budget *provision.Budget

	// Log is the level and the format this run's logger is built with.
	//
	// IT TRAVELS ON THE REQUEST AND NOT ON THE CONFIG, for the reason the
	// `serve` config path is read from a flag: these are two values the
	// *operator* chose on the command line, and ember.yaml is the file a
	// queued request must never be able to name. The zero value is
	// info-level text, so the one caller that has nothing to say about it --
	// internal/queue's planner, standing in for a browser -- gets what ember
	// printed before the flags existed.
	Log logx.Options
}

// Plan is everything decided before the create call.
//
// THE SPLIT IS AT THE CREATE CALL, and that is the point of the type. Every
// refusal in NewPlan costs nothing -- no node exists, nothing is billing --
// so a mistyped sampler, an unpinned image, a card with no image or a store
// that was never created all fail here. `--dry-run` is this half and no more,
// and a web request asking "what would this cost" needs exactly the same
// answer without renting anything.
type Plan struct {
	Provider provider.Provider
	Offer    node.Offer
	Spec     node.Spec

	// Jobs is the served batch, empty for every other shape. It travels
	// beside the Spec rather than on it because internal/node imports
	// nothing else in this module and TestNodeImportsNothing enforces that.
	Jobs sdserver.Run

	// Spoken is the speech batch, empty for every other shape, and it travels
	// here for the identical reason Jobs does.
	//
	// TWO TYPED FIELDS AND NOT ONE `any`, which is the shape a reader would
	// reasonably expect to find collapsed. They are separate because they are
	// READ separately by things outside this package that are about one kind:
	// internal/queue/direct.go projects Jobs into genstore rows and reads its
	// preview settings, and `ember up --dry-run` prints each job's own
	// description. An interface value there would make every one of those a
	// type assertion, which is the dispatch this change exists to remove
	// rather than move. What IS one value is the dispatch itself -- batchOf,
	// which is the only thing that has to know a run has exactly one kind.
	Spoken tts.Run

	Options provision.Options
	Paths   Paths

	// ImageFrom says where Spec.Image came from, for `--dry-run`. An
	// operator staring at a digest needs to know which key produced it.
	ImageFrom string

	// InputsDir is where a job's image inputs are staged.
	//
	// A PATH AND NOT AN OPEN STORE, because a Plan has no Close and an
	// os.Root held from NewPlan until a rental ends would be a descriptor
	// nothing accounts for -- on the queue's path that is the life of the
	// process. The two places that need the bytes open it for as long as
	// they are reading, which is one open(2) each.
	//
	// It travels on the Plan rather than being re-derived where it is used,
	// so that `ember up` -- whose jobs come from a config file, which has no
	// field for an image input -- carries an empty one and the resolution
	// path is unreachable from it by construction rather than by a check.
	InputsDir string

	Hold time.Duration
}

// NewPlan resolves everything a run needs and rents nothing.
//
// The order is load-bearing and each step explains itself below. What the
// whole sequence is for: by the time this returns, the only things left that
// can fail are the provider, the network and the node.
func NewPlan(ctx context.Context, req Request, out io.Writer) (Plan, error) {
	cfg := req.Config

	ps, err := ResolvePaths(cfg, req.Deps)
	if err != nil {
		return Plan{}, err
	}
	creds, err := req.Deps.Credentials(ps.Config)
	if err != nil {
		return Plan{}, err
	}
	reg, err := BuildRegistry(req.Deps, creds, cfg)
	if err != nil {
		return Plan{}, err
	}
	p, err := reg.Get(node.ProviderID(cfg.Provider))
	if err != nil {
		return Plan{}, Usage(err)
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
	store, err := AttachStore(ctx, p, cfg)
	if err != nil {
		return Plan{}, err
	}

	filter, err := Filter(cfg, store)
	if err != nil {
		return Plan{}, Usage(err)
	}

	if req.CPUOnly {
		// No workload, and no GPU to idle while the download runs. The filter
		// is exclusive in both directions, so this cannot quietly land on a
		// card.
		//
		// Built before the spec rather than patched into it afterwards.
		// Patching meant NewSpec validated a filter the command never used --
		// and the one check that cared, "a gpu probe is configured but this
		// run asks for no GPU", could therefore never fire, although
		// ember.flux2.yaml configures exactly that. A seal nothing could trip
		// is a seal that was not there.
		filter.CPUOnly = true
		filter.GPU = ""
		filter.MinVRAM = 0
		filter.MinCount = req.VCPU
	}

	// Options before the offer, because PickOffer needs the budget.
	opts, err := BuildOptions(ps, cfg, out, req.Log)
	if err != nil {
		return Plan{}, err
	}
	if req.Budget != nil {
		opts.Budget = *req.Budget
	}

	// Said before anything is rented, because that is the only moment it is
	// useful, and said rather than refused.
	//
	// workload.NonLoopbackBinds has the whole argument. The short version:
	// nothing is published for a workload port -- exposure.PortRequest's
	// `public` field is unexported and only SSHPort() sets it -- so an
	// operator who writes `--host 0.0.0.0` has concluded something false
	// about what ember exposed, and on the local provider that belief is
	// partly true, since a Docker bridge makes the bind reachable from this
	// machine. A refusal would be ember claiming to know a flag grammar it
	// deliberately does not learn.
	//
	// sd.serve is not read here: sdserver.ServeCommand owns that argv's
	// listen address and refuses a command that names it, which is a seal
	// rather than advice.
	for _, b := range cmdBinds(cfg) {
		opts.Log.Warn("this workload asks to listen somewhere other than the node's own loopback",
			"flag", b.Flag, "address", b.Addr, "argv", b.Index,
			"published", "nothing: a workload port is never published, and the post-create check destroys a node whose provider published one",
			"reachable", "the forward only, except on the local provider, where a Docker bridge makes the bind reachable from this machine")
	}

	// THE WEIGHTS, BEFORE THE CARD.
	//
	// Here rather than three statements lower because PickOffer is the first
	// call that commits to hardware, and the point of this question is that
	// it is answered while nothing has been chosen: one HEAD per asset over
	// the store's own endpoint, for nothing. Assets are still verified by
	// digest on the node, which is where a digest belongs; what this catches
	// is the absence, which on the node costs the download at the card's
	// rate -- measured at 97.9% of one RTX PRO 6000 rental.
	//
	// A CPU-only run is not asked, and that is the natural seal rather than
	// a flag: `ember store sync` is the run whose job is to FILL the store,
	// so refusing it for an empty store would refuse the remedy. It is also
	// what stops `ember serve` recursing, since the sync it starts on a
	// missing asset is itself a plan built this way.
	//
	// The finding is returned rather than acted on, because the two callers
	// want opposite things from it. See AssetsMissingError.
	if !req.CPUOnly {
		assetList, err := cfg.ParseAssets()
		if err != nil {
			return Plan{}, err
		}
		check, err := CheckStoreAssets(ctx, cfg, creds, store, assetList, req.Deps)
		if err != nil {
			return Plan{}, err
		}
		if !check.Asked && len(assetList) > 0 {
			// Said out loud. A check that silently does nothing leaves the
			// operator believing the expensive case cannot happen.
			opts.Log.Info("not checking whether this run's weights are on the store before renting",
				"why", check.Why,
				"consequence", "a missing asset is found on the node, after the card is paid for")
		}
		if err := MissingAssets(cfg.Store.Name, check); err != nil {
			return Plan{}, err
		}
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
	offer, err := provision.PickOffer(ctx, p, filter, cfg.TTL, opts)
	if err != nil {
		return Plan{}, err
	}

	ref, from, err := ResolveImage(cfg, offer)
	if err != nil {
		return Plan{}, Usage(err)
	}

	// Parsed before the spec, because BuildSpec refuses a served run whose
	// jobs are unusable and this is where that is found out. Every refusal
	// here happens before the create call.
	//
	// Skipped for a CPU-only run, which has no card to hold weights resident
	// for: BuildSpec drops the server for that run the way it drops the GPU
	// probe, so parsing the jobs would validate a batch this rental will
	// never send and print it in the dry run.
	var run sdserver.Run
	var spoken tts.Run
	switch {
	case req.CPUOnly:
		// Nothing: this rental runs `embernode fetch`.
	case len(req.Jobs) > 0:
		run, err = jobsFromRequest(req, ps.Inputs)
		if err != nil {
			return Plan{}, Usage(err)
		}
	default:
		run, err = cfg.ParseSD()
		if err != nil {
			return Plan{}, Usage(err)
		}
		spoken, err = cfg.ParseTTS()
		if err != nil {
			return Plan{}, Usage(err)
		}
	}

	spec, err := BuildSpec(cfg, Resolved{
		Store: store, Filter: filter, Offer: offer, Image: ref, ImageFrom: from,
	})
	if err != nil {
		return Plan{}, Usage(err)
	}

	// ONE KIND, NEVER TWO, AND HERE RATHER THAN IN node.NewSpec.
	//
	// That function already refuses any two of `cmd`, `cmds` and `sd.serve`,
	// through namedWorkloads, with the right argument: a config with two has
	// two answers to "what does this node run", and picking one silently is
	// how an operator watches the wrong thing execute. It cannot see this one,
	// because `node.Spec` HAS NO SPEECH FIELD -- and that is the seal rather
	// than a gap. A spec field is a thing a request could one day be consulted
	// about; the typed batch travels beside the spec for the same reason
	// Plan.Jobs does, and internal/node importing nothing else in this module
	// is what TestNodeImportsNothing holds.
	//
	// So the refusal is a layer up, which is where the two kinds are both in
	// scope. Without it `Plan.serve` would reach `len(p.Spec.Cmds) > 0` first
	// and run the commands, leaving an operator who wrote both with a rental
	// that said nothing and no line anywhere saying why.
	if len(spoken.Jobs) > 0 {
		if other := spec.Workload(); other != "" {
			return Plan{}, Usage(fmt.Errorf("tts.jobs and %s are both set; use one", other))
		}
	}

	if req.CPUOnly {
		// This rental runs `embernode fetch` and nothing else, and brings
		// nothing home: the bytes land on the store, which is the point.
		spec.Cmds = nil
		spec.Outputs = nil
	}

	return Plan{
		Provider:  p,
		Offer:     offer,
		Spec:      spec,
		Jobs:      run,
		Spoken:    spoken,
		Options:   opts,
		Paths:     ps,
		ImageFrom: from,
		Hold:      req.Hold,
		// Empty for every shape but a queued request, which is the only one
		// that can name an image input: `ember up` reads its jobs from
		// ember.yaml, and config.SDJob has no field for one.
		InputsDir: inputsDir(req, ps),
	}, nil
}

// cmdBinds collects the non-loopback binds across every step of a run.
//
// Every entry of `cmds` and not only the first, for the reason BuildSpec
// validates every entry's argv: a batch whose seventh step publishes is a
// batch where the finding arrives after six steps have been paid for -- and
// here it would never arrive at all, because the warning is printed once
// before the rental rather than per step.
func cmdBinds(cfg config.Config) []workload.NonLoopbackBind {
	out := workload.NonLoopbackBinds(cfg.Cmd)
	for _, c := range cfg.Cmds {
		out = append(out, workload.NonLoopbackBinds(c)...)
	}
	return out
}

// Filter is the offer filter this config and store describe.
//
// Extracted from BuildSpec because the filter is now needed before the spec
// exists: PickOffer runs first, so the image can be chosen for the card that
// was actually picked. Computed once and passed to both, because two filters
// built from the same config is two things that can disagree -- and the one
// that would disagree is the one that chose the offer.
func Filter(cfg config.Config, store Attached) (node.Filter, error) {
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

// Resolved is what NewPlan has to learn before a Spec can be validated.
//
// The store, because only the provider knows its id; the offer, because the
// image depends on which card it is for; and the image, because NewSpec will
// not accept a Spec without one. Written down as data so the ordering is a
// thing a reader can see rather than infer from the order of statements.
type Resolved struct {
	Store  Attached
	Filter node.Filter
	Offer  node.Offer
	Image  string

	// ImageFrom says where Image came from, for `--dry-run`. An operator
	// staring at a digest needs to know which key produced it.
	ImageFrom string
}

// ResolveImage picks the workload image for the card that will run it.
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
//     back to image:. A wrong-architecture image aborts -- measured: ggml
//     raises "no kernel image is available for execution on the device" and
//     sd-cli dies with SIGABRT about a second into the first generation --
//     but it aborts after the node exists, the image is pulled and the
//     weights are fetched, all of which is billed. Refusing here costs
//     nothing, because nothing has been rented yet.
func ResolveImage(cfg config.Config, offer node.Offer) (string, string, error) {
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
			"choose an image for it.\nGuessing would rent the card and abort on it -- \"no kernel "+
			"image is available for execution on the device\", after the pull and the weights -- so "+
			"this is a refusal.\nAdd the card to internal/node/compute.go, or filter to one of: %v",
			offer.GPU, node.KnownCards())
	}

	ref, ok := images[cap]
	if !ok {
		// Sorted numerically and then rendered, not sorted as strings: the
		// keys look like text and are numbers, so sort.Strings would list
		// sm_100 before sm_75 and the operator would read it as unordered.
		caps := make([]node.ComputeCap, 0, len(images))
		for c := range images {
			caps = append(caps, c)
		}
		sort.Slice(caps, func(i, j int) bool { return caps[i] < caps[j] })
		have := make([]string, 0, len(caps))
		for _, c := range caps {
			have = append(have, c.Key())
		}
		return "", "", fmt.Errorf("no image for %s, which is what %s is; images: has %v.\n"+
			"ember will not fall back to image: here. A wrong-architecture image aborts on this "+
			"card -- \"no kernel image is available for execution on the device\" -- but only "+
			"after the node exists, the image is pulled and the weights are fetched, all billed.\n"+
			"Publish one with `make publish-sd-cuda CUDA_ARCH=%d` and add its digest under %s",
			cap, offer.GPU, have, int(cap), cap.Key())
	}
	return ref, fmt.Sprintf("images.%s, for %s", cap.Key(), offer.GPU), nil
}

// BuildSpec turns a config and what was resolved from it into a validated Spec.
func BuildSpec(cfg config.Config, r Resolved) (node.Spec, error) {
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
	// Every step, for the same reason: an argv the remote shell cannot be
	// handed safely is a config error, and finding it at step seven means
	// six steps of a rented card have already been paid for.
	for i, c := range cfg.Cmds {
		if err := workload.Validate(c); err != nil {
			return node.Spec{}, fmt.Errorf("cmds[%d]: %w", i, err)
		}
	}
	// And the server, for the same reason -- plus the listen address, which
	// ember appends and refuses to let the config choose. A serve command
	// passing --listen-ip 0.0.0.0 would put an unauthenticated generator,
	// with arbitrary file paths in its request body, on a rented machine's
	// public interface.
	//
	// Dropped on a CPU-only run for the reason the probe is: `ember store
	// sync` shares a config with the runs that generate, and NewSpec refuses
	// a server on a run that asks for no card.
	var serve []string
	if !r.Filter.CPUOnly {
		if err := workload.Validate(cfg.SD.Serve); err != nil {
			return node.Spec{}, fmt.Errorf("sd.serve: %w", err)
		}
		serve, err = sdserver.ServeCommand(cfg.SD.Serve)
		if err != nil {
			return node.Spec{}, err
		}
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
		Serve:           serve,
		ServeTimeout:    cfg.SD.ReadyTimeout,

		// Already resolved, and already the one reference this run will use.
		// NewSpec is still the only way to obtain a validated Spec, and it
		// still applies the pin rule -- now to the entry that was chosen
		// rather than to a scalar that might not have been the one that ran.
		Image:   r.Image,
		Cmd:     cfg.Cmd,
		Cmds:    cfg.Cmds,
		Ports:   ports,
		Filter:  r.Filter,
		TTL:     cfg.TTL,
		Outputs: outputs,
		Store:   r.Store.Handle,
		Mount:   r.Store.Mount,
	})
}

// inputsDir is the staging directory for the shapes that can name an input.
//
// A function rather than `ps.Inputs` at the one call site, because the
// emptiness is the seal: a reader should be able to see that `ember up`
// cannot reach the resolution path at all, rather than inferring it from the
// fact that a YAML type happens to have no field today.
func inputsDir(req Request, ps Paths) string {
	if len(req.Jobs) == 0 {
		return ""
	}
	return ps.Inputs
}

// jobsFromRequest validates a batch that did not come from the config file.
//
// Every refusal here is config.ParseSD's, reached through the same function,
// and every one of them happens before the create call -- which is the whole
// discipline NewPlan exists to keep. A batch checked after the node was up
// would be a batch refused about a minute into a rented card.
//
// THE IMAGE INPUTS ARE VERIFIED HERE AND LOADED SOMEWHERE ELSE, which looks
// like duplication and is the cheap-refusal order. Verifying costs one read
// of a file on this machine and happens before PickOffer's choice becomes a
// create call; the bytes are not needed until a generation server is up and
// the weights are resident, which on a rented card is about a minute of
// billing later. A reference that was never staged must not be discovered
// then. blob.Inputs.Load is the one spelling of what verifying means, so the
// two moments cannot come to different answers.
func jobsFromRequest(req Request, inputsDir string) (sdserver.Run, error) {
	preview, err := sdserver.ParsePreview(req.Preview)
	if err != nil {
		return sdserver.Run{}, fmt.Errorf("preview: %w", err)
	}
	interval, err := sdserver.ParsePreviewInterval(req.PreviewInterval)
	if err != nil {
		return sdserver.Run{}, fmt.Errorf("preview_interval: %w", err)
	}

	// Opened once for the batch and closed before anything is rented. Nil
	// when no job names an image, so a configuration that cannot have one
	// does not create a staging directory as a side effect of planning.
	var in *blob.Inputs
	if wantsInputs(req.Jobs) {
		if inputsDir == "" {
			return sdserver.Run{}, errors.New("a job names an image input and this run has no " +
				"staging directory; image inputs reach ember through `ember serve`, which is " +
				"the only thing that accepts one")
		}
		var oerr error
		if in, oerr = blob.OpenInputs(inputsDir); oerr != nil {
			return sdserver.Run{}, oerr
		}
		defer func() { _ = in.Close() }()
	}

	run := sdserver.Run{Preview: preview, PreviewInterval: interval}
	for i, j := range req.Jobs {
		// ValidateStatic builds the body, which refuses a reference with no
		// bytes -- so the bytes have to be here even though they are thrown
		// away a line later. Resolving for the check and again for the
		// generation is two reads of a file on this machine; the alternative
		// is a check that does not exercise the thing it is checking.
		if ref := j.Params.InitImage; ref != "" {
			b, lerr := in.Load(ref)
			if lerr != nil {
				return sdserver.Run{}, fmt.Errorf("jobs[%d]: %w", i, lerr)
			}
			j.InitImageBytes = b
		}
		if err := sdserver.ValidateStatic(j, preview, interval); err != nil {
			return sdserver.Run{}, fmt.Errorf("jobs[%d]: %w", i, err)
		}
		// Carried WITHOUT the bytes. This Run reaches the dry run and the
		// batch-count line and nothing that submits -- the drain loop builds
		// its own from the stored request for every batch it claims,
		// including the first -- so megabytes held here would be megabytes
		// held for the life of a rental by nothing that reads them.
		j.InitImageBytes = nil
		run.Jobs = append(run.Jobs, j)
	}
	return run, nil
}

// wantsInputs reports whether any job names an image.
func wantsInputs(jobs []sdserver.Job) bool {
	for _, j := range jobs {
		if j.Params.InitImage != "" {
			return true
		}
	}
	return false
}
