package node

import (
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"
)

// MaxTTL is the longest life ember will give a node.
//
// A day is far longer than any experiment needs and far shorter than a
// forgotten node costs. The ceiling exists so that a typo in a config — "24h"
// written as "240h" — fails at parse time rather than four hundred dollars
// later.
const MaxTTL = 24 * time.Hour

// PortMap is one workload port forwarded home.
//
// Remote is always reached on the node's loopback; see tunnel.RemoteLoopback
// for why the remote host is never configurable.
type PortMap struct {
	Name   string // "comfyui" — appears in logs and in the ready line
	Remote uint16
	Local  uint16
}

// OutputPath is a file or directory to retrieve from the node before it dies.
type OutputPath struct {
	Remote string // absolute path on the node
	Local  string // relative path under the run's output directory
}

// Spec is everything ember needs to run one workload.
//
// There is no exported way to build a Spec with a zero TTL or an unpinned
// image: NewSpec is the only constructor, and the fields it validates are the
// ones whose wrong values cost money or make a measurement meaningless.
type Spec struct {
	Image string

	// Cmds are the workloads, run in sequence on one node.
	//
	// A list rather than one command because every per-rental cost -- waiting
	// for the machine, pulling the image, reading and verifying the weights,
	// loading them -- is paid once per `ember up` and not once per result.
	// Measured on a rented A100 that overhead is about three minutes against
	// roughly 100 seconds of actual generation, so ten images rented ten
	// times spend more than half the bill re-doing setup.
	//
	// Empty holds the node open with its ports forwarded, which is the
	// interactive shape.
	Cmds    [][]string
	Env     map[string]string
	Ports   []PortMap
	Outputs []OutputPath
	Filter  Filter
	TTL     time.Duration
	Volume  MiB

	// Store is the persistent store to mount, and Mount is where it lands
	// inside the node. Both zero means no store.
	//
	// They live on the spec rather than on a provider, because what gets
	// mounted is a property of the run and not of the account: two runs
	// against the same provider can want different stores, and a provider
	// carrying the answer would have to be rebuilt between them. Every
	// provider receives them through CreateRequest with everything else the
	// run needs.
	Store StoreHandle
	Mount Mount

	// Assets must be on the store, verified, before Cmd runs.
	Assets []Asset

	// Verify is the policy for an asset already present. Empty is the strict
	// one; the string is parsed by internal/assets, which owns the meaning.
	Verify string

	// GPUProbe must exit zero before anything else runs on the node, and
	// GPUProbeTimeout is how long it may take to start doing so.
	//
	// A command rather than something ember knows how to check itself. What
	// proves a card usable depends on the image -- a CUDA build answers a
	// device listing, a ROCm one does not, and a CPU-only run has nothing to
	// prove -- so the contract is an exit status and the knowledge lives in
	// the config next to the image it describes.
	//
	// Empty runs no probe, which is what every config written before this
	// existed asks for.
	GPUProbe        []string
	GPUProbeTimeout time.Duration

	// Serve is a long-running process started on the node before the jobs
	// are, and ServeTimeout is how long it has to become usable.
	//
	// The resident-server shape, and the reason it exists is the one cost
	// Cmds cannot remove. Each entry in Cmds is a fresh process that reads
	// the weights into VRAM again; a server started once holds them. On a
	// rented RTX PRO 6000 running FLUX.2 at 2048x2048 that is 4.5s per image
	// on a node that caches the weights and 86s on one that re-reads them.
	//
	// Still argv, because starting a process is the part ember has always
	// done this way -- what is new is that ember then speaks the protocol
	// rather than handing the next command to a shell. The jobs themselves
	// are not here: they never reach the node as argv, and this package
	// imports nothing else from the module, so the type that describes them
	// cannot live here either. They travel beside the Spec.
	//
	// Empty means no server, which is what every config written before this
	// existed asks for.
	Serve        []string
	ServeTimeout time.Duration
}

// SpecOptions is the input to NewSpec.
type SpecOptions struct {
	Image   string
	Cmd     []string
	Cmds    [][]string
	Env     map[string]string
	Ports   []PortMap
	Outputs []OutputPath
	Filter  Filter
	TTL     time.Duration
	Volume  MiB
	Store   StoreHandle
	Mount   Mount
	Assets  []Asset
	Verify  string

	GPUProbe        []string
	GPUProbeTimeout time.Duration
	Serve           []string
	ServeTimeout    time.Duration
}

// NewSpec validates o and returns the Spec it describes.
func NewSpec(o SpecOptions) (Spec, error) {
	if err := validateImage(o.Image); err != nil {
		return Spec{}, err
	}
	if o.TTL <= 0 {
		return Spec{}, fmt.Errorf("ttl must be positive: a node with no deadline is a node nothing will stop")
	}
	if o.TTL > MaxTTL {
		return Spec{}, fmt.Errorf("ttl %v is above the %v ceiling", o.TTL, MaxTTL)
	}
	if o.Volume < 0 {
		return Spec{}, fmt.Errorf("negative volume size")
	}
	// Half a mount is not a smaller mount, it is a silent one. A store with
	// nowhere to land would be attached and unreachable; a mount with no
	// store would be a path the workload reads and finds empty. Both would
	// look like a successful run that produced nothing.
	if len(o.Assets) > 0 && o.Store.IsZero() {
		return Spec{}, fmt.Errorf("%d asset(s) are listed but no store is mounted; they would be written to "+
			"the node's own disk and thrown away with it", len(o.Assets))
	}
	if _, err := ValidateAssets(o.Assets); err != nil {
		return Spec{}, err
	}
	if o.Store.IsZero() != o.Mount.IsZero() {
		return Spec{}, fmt.Errorf("a store needs a mount path and a mount path needs a store; got store %q and mount %q",
			o.Store, o.Mount)
	}
	if o.GPUProbeTimeout < 0 {
		return Spec{}, fmt.Errorf("negative gpu probe timeout")
	}
	if o.ServeTimeout < 0 {
		return Spec{}, fmt.Errorf("negative serve timeout")
	}
	// A CPU-only run has no card, and the only reason to hold weights
	// resident is to generate with them. Same shape as the gpu probe check
	// below, and the same reason: it would fail after the machine was rented.
	if len(o.Serve) > 0 && o.Filter.CPUOnly {
		return Spec{}, fmt.Errorf("a server is configured but this run asks for no GPU")
	}
	for i, a := range o.Serve {
		if strings.TrimSpace(a) == "" {
			return Spec{}, fmt.Errorf("serve[%d] is empty", i)
		}
	}
	// One spelling, never two. A config carrying any two of `cmd`, `cmds` and
	// `sd.serve` has two answers to "what does this node run", and picking
	// one silently is how an operator watches the wrong thing execute.
	//
	// `serve` joins the other two rather than being allowed beside them
	// because it is not a step: it is the thing the steps are sent to, and a
	// run that had both would start a server, hold the weights in VRAM, and
	// then execute a separate sd-cli that loads them a second time -- paying
	// for the resident copy and not using it.
	if named := namedWorkloads(o); len(named) > 1 {
		return Spec{}, fmt.Errorf("%s are set; use one", humanList(named))
	}
	cmds := make([][]string, 0, len(o.Cmds)+1)
	if len(o.Cmd) > 0 {
		cmds = append(cmds, append([]string(nil), o.Cmd...))
	}
	for i, c := range o.Cmds {
		if len(c) == 0 {
			return Spec{}, fmt.Errorf("cmds[%d] is empty", i)
		}
		cmds = append(cmds, append([]string(nil), c...))
	}
	// A CPU-only run has no card, so a probe against one could only ever
	// fail -- and it would fail after the machine was rented, which is the
	// shape of mistake this whole package exists to catch early.
	if len(o.GPUProbe) > 0 && o.Filter.CPUOnly {
		return Spec{}, fmt.Errorf("a gpu probe is configured but this run asks for no GPU")
	}
	if err := o.Filter.Validate(); err != nil {
		return Spec{}, err
	}
	if err := validatePorts(o.Ports); err != nil {
		return Spec{}, err
	}
	if err := validateOutputs(o.Outputs); err != nil {
		return Spec{}, err
	}
	return Spec{
		Image:   o.Image,
		Cmds:    cmds,
		Env:     cloneEnv(o.Env),
		Ports:   append([]PortMap(nil), o.Ports...),
		Outputs: append([]OutputPath(nil), o.Outputs...),
		Filter:  o.Filter,
		TTL:     o.TTL,
		Volume:  o.Volume,
		Store:   o.Store,
		Mount:   o.Mount,
		Assets:  append([]Asset(nil), o.Assets...),
		Verify:  o.Verify,

		GPUProbe:        append([]string(nil), o.GPUProbe...),
		GPUProbeTimeout: o.GPUProbeTimeout,
		Serve:           append([]string(nil), o.Serve...),
		ServeTimeout:    o.ServeTimeout,
	}, nil
}

// validateImage insists the image reference names one specific image.
//
// ":latest", and a bare name with no tag at all, both resolve to whatever the
// registry is serving at pull time. That is fine for a web server and useless
// here: the entire point of ember is to measure what a model costs and how
// long it takes, and two runs a week apart that say "latest" may not have run
// the same software. A result nobody can reproduce is not a measurement.
// Exported because internal/config has to apply the same rule to every entry
// of an `images:` map, and the alternative is a second copy of it. It refuses;
// it does not construct, so NewSpec remains the only way to obtain a validated
// Spec and this is not a way around that seal.
func ValidateImage(s string) error { return validateImage(s) }

func validateImage(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("image is empty")
	}
	if strings.Contains(s, "@sha256:") {
		return nil
	}
	// Only the part after the last slash can carry the tag; a registry host
	// may contain a colon for its port, as in "localhost:5000/comfyui".
	last := s[strings.LastIndex(s, "/")+1:]
	name, tag, ok := strings.Cut(last, ":")
	if !ok || tag == "" {
		return fmt.Errorf("image %q has no tag: pin it to a tag or a digest, or two runs will not have measured the same software", s)
	}
	if name == "" {
		return fmt.Errorf("image %q is malformed", s)
	}
	if tag == "latest" {
		return fmt.Errorf("image %q is pinned to :latest, which is not a pin", s)
	}
	return nil
}

func validatePorts(ps []PortMap) error {
	seenRemote := map[uint16]bool{}
	seenLocal := map[uint16]bool{}
	seenName := map[string]bool{}
	for _, p := range ps {
		if p.Remote == 0 {
			return fmt.Errorf("port %q has no remote port", p.Name)
		}
		if p.Local == 0 {
			return fmt.Errorf("port %q has no local port: 0 means \"any\" to a listener and \"refused\" to a browser", p.Name)
		}
		if strings.TrimSpace(p.Name) == "" {
			return fmt.Errorf("port %d has no name", p.Remote)
		}
		if seenRemote[p.Remote] {
			return fmt.Errorf("remote port %d mapped twice", p.Remote)
		}
		if seenLocal[p.Local] {
			return fmt.Errorf("local port %d mapped twice", p.Local)
		}
		if seenName[p.Name] {
			return fmt.Errorf("port name %q used twice", p.Name)
		}
		seenRemote[p.Remote], seenLocal[p.Local], seenName[p.Name] = true, true, true
	}
	return nil
}

// validateOutputs insists remote paths are absolute and already clean.
//
// Requiring path.Clean to be a no-op rather than cleaning silently: "/out/.."
// and "/out" are different requests, and a fetch that quietly rewrites one
// into the other retrieves a directory nobody asked for. The extraction side
// enforces its own containment separately — this check is about the request
// meaning what it says.
func validateOutputs(os []OutputPath) error {
	for _, o := range os {
		if !path.IsAbs(o.Remote) {
			return fmt.Errorf("output path %q is not absolute", o.Remote)
		}
		if path.Clean(o.Remote) != o.Remote {
			return fmt.Errorf("output path %q is not clean: write it as %q", o.Remote, path.Clean(o.Remote))
		}
		if o.Local == "" {
			return fmt.Errorf("output %q has no local name", o.Remote)
		}
		if path.IsAbs(o.Local) || strings.Contains(o.Local, "..") {
			return fmt.Errorf("local output name %q must be a relative path inside the run directory", o.Local)
		}
	}
	return nil
}

func cloneEnv(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// ParseCUDAVersion reads a "major.minor" CUDA version.
//
// Strings and not a number, because "12.10" is not 12.1 and a float would say
// it was. Compared as a pair of integers for the same reason: a provider's
// list of acceptable versions is sorted by what the driver supports, and
// sorting it as text puts 12.9 above 12.10.
//
// THIS FUNCTION IS LIVE AND CUDAAtLeast BELOW IS NOT, which is a distinction
// an earlier draft of this comment got wrong in the direction that costs a
// reader their afternoon. It claimed both were callerless and that deleting
// them was "three lines"; deleting this one does not compile, because
// ParseComputeCap calls it twenty lines into compute.go and
// local.Provider.Detect calls THAT -- the `compute_cap` path, which is the
// mechanism #91 settled on as the one that works. The tree's own detector
// classifies it live and always did; the comment was the only thing that
// disagreed, two hundred lines from the call.
//
// CUDAAtLeast is the callerless one, and that is #91's answer rather than an
// oversight. It existed for `min_cuda`, a config key that was removed because
// no provider here can honour it: RunPod's create endpoint has no CUDA field
// and answers 422 to the allowedCudaVersions ember used to send, and the
// per-GPU cudaVersions its catalogue does report tracks capacity rather than
// capability -- pinned to the committed capture by
// runpod.TestCatalogue_ReportsCudaVersionsAndTheyAreNotAFloorForMinCUDA, which
// is also what reopens the question if a later capture breaks the correlation.
// `gpu_probe` is the live mechanism and runs on the node.
//
// So CUDAAtLeast should go the way node.RateFromFloat went -- its premise was
// read from a schema, and a second mechanism for the half that matters already
// exists, which are the two halves of that precedent. It is NOT deleted here
// only because cmd/check-coverage's deadExcluded names it and fails over a
// waiver it stops reporting, so the function and its waiver have to be removed
// in one commit and that file was held by another change (#161). TWO lines of
// deletion, not three, and this function stays. The two judgements worth
// keeping are the ones above, and they belong to the parser rather than to the
// comparison: strings rather than a float, and compared as a pair of integers.
func ParseCUDAVersion(s string) (major, minor int, err error) {
	maj, min, ok := strings.Cut(strings.TrimSpace(s), ".")
	if !ok {
		return 0, 0, fmt.Errorf("cuda version %q is not major.minor", s)
	}
	major, err = strconv.Atoi(maj)
	if err != nil || major < 0 {
		return 0, 0, fmt.Errorf("cuda version %q has an unreadable major", s)
	}
	minor, err = strconv.Atoi(min)
	if err != nil || minor < 0 {
		return 0, 0, fmt.Errorf("cuda version %q has an unreadable minor", s)
	}
	return major, minor, nil
}

// CUDAAtLeast reports whether have is version want or newer.
func CUDAAtLeast(have, want string) (bool, error) {
	hm, hn, err := ParseCUDAVersion(have)
	if err != nil {
		return false, err
	}
	wm, wn, err := ParseCUDAVersion(want)
	if err != nil {
		return false, err
	}
	if hm != wm {
		return hm > wm, nil
	}
	return hn >= wn, nil
}

// humanList renders a list the way the refusal reads it out: "cmd and cmds",
// or "cmd, cmds and sd.serve". Not strings.Join, because "cmd and cmds and
// sd.serve" is the kind of message that reads as a bug in the message.
func humanList(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " and " + xs[len(xs)-1]
}

// namedWorkloads lists which of the mutually exclusive workload keys a config
// set, so the refusal can name them rather than saying "two of these".
func namedWorkloads(o SpecOptions) []string {
	var out []string
	if len(o.Cmd) > 0 {
		out = append(out, "cmd")
	}
	if len(o.Cmds) > 0 {
		out = append(out, "cmds")
	}
	if len(o.Serve) > 0 {
		out = append(out, "sd.serve")
	}
	return out
}
