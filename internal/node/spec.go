package node

import (
	"fmt"
	"path"
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
	Image   string
	Cmd     []string
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
}

// SpecOptions is the input to NewSpec.
type SpecOptions struct {
	Image   string
	Cmd     []string
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
		Cmd:     append([]string(nil), o.Cmd...),
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
	}, nil
}

// validateImage insists the image reference names one specific image.
//
// ":latest", and a bare name with no tag at all, both resolve to whatever the
// registry is serving at pull time. That is fine for a web server and useless
// here: the entire point of ember is to measure what a model costs and how
// long it takes, and two runs a week apart that say "latest" may not have run
// the same software. A result nobody can reproduce is not a measurement.
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
