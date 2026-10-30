// Package local rents a container on the operator's own Docker daemon.
//
// It costs nothing and it is the substrate the whole stack is tested on: the
// SSH handshake, the host-key pin, the port forward, the output fetch and the
// destroy ordering all run their real production code paths against a real
// sshd, for zero cents and with no cloud account.
//
// The container ember starts here is a node, not the workload. The workload
// runs inside it exactly as it would on a rented machine, reached over SSH, so
// nothing above this package can tell the difference.
package local

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/dockerengine"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// Name is this provider's id.
const Name node.ProviderID = "local"

// Labels ember sets on everything it creates here.
const (
	LabelKind    = "ember.kind"     // "node" | "network" | "store"
	LabelLabel   = "ember.label"    // provider.Label(...)
	LabelHostKey = "ember.hostkey"  // "ssh-ed25519 AAAA..." — public, safe in a label
	LabelUser    = "ember.ssh-user" // who to log in as
)

// DefaultImage is the node image. It carries sshd and a docker client, and is
// built by `make docker-build`.
const DefaultImage = "ember:lab"

// sshPort is the port sshd listens on inside the node container.
const sshPort uint16 = 22

// Provider is the local Docker-backed provider.
type Provider struct {
	Engine dockerengine.API
	Image  string

	// User is the account sshd accepts. The lab image runs sshd as root
	// because a container is already a sandbox and a second unprivileged user
	// buys nothing here.
	User string

	// Store is the named volume to mount into every node this provider
	// creates, and Mount is where it lands. Both zero means no store, which
	// is what every run looked like before stores existed.
	Store node.StoreHandle
	Mount node.Mount
}

// New returns a provider using engine.
func New(engine dockerengine.API) *Provider {
	return &Provider{Engine: engine, Image: DefaultImage, User: "root"}
}

func (p *Provider) Name() node.ProviderID { return Name }

func (p *Provider) Caps() provider.Caps {
	return provider.Caps{
		// ember minted the host key and injected it, and the public half is
		// on the container label, so the first connection is verified rather
		// than trusted.
		HostKeyFromAPI: true,
		Free:           true,
		Tags:           true,

		// Docker named volumes. They cost nothing, which is the point: the
		// whole store lifecycle -- create, mount, survive a destroy, list,
		// remove -- is exercised for free here against the same code paths a
		// rented one uses.
		Stores: true,
	}
}

// imageFor picks the image a node runs.
//
// The spec's image wins, so `--image` means the same thing on every provider.
// It did not before: this provider ignored the spec entirely and always ran
// its own, so an operator who passed `--image` to the local provider had it
// silently dropped while the identical flag on RunPod chose the node image.
// A flag that works on one provider and is quietly discarded on another is
// worse than one that fails.
//
// ember runs one container that is both the node and the workload, so that
// image has to carry sshd and embernode. DefaultImage is the fallback for a
// spec with no image at all, which is the shape the provider's own tests use.
func (p *Provider) imageFor(spec node.Spec) string {
	if spec.Image != "" {
		return spec.Image
	}
	return p.image()
}

func (p *Provider) image() string {
	if p.Image != "" {
		return p.Image
	}
	return DefaultImage
}

func (p *Provider) user() string {
	if p.User != "" {
		return p.User
	}
	return "root"
}

// Offers returns the single synthetic offer this provider has.
//
// Price is zero and Caps().Free says so, which lets provision skip the budget
// check honestly rather than by comparing a real price against zero.
func (p *Provider) Offers(ctx context.Context, f node.Filter) ([]node.Offer, error) {
	if err := p.Engine.Ping(ctx); err != nil {
		return nil, fmt.Errorf("docker daemon is not reachable: %w", err)
	}
	o := node.Offer{
		Provider: Name,
		Token:    "local",
		GPU:      "none",
		GPUCount: 1,
		VRAM:     0,
		Price:    0,
		Region:   "localhost",
		Label:    "a container on this machine's docker daemon, free",
	}
	// A filter asking for a GPU must not match: the local provider has none,
	// and silently handing back a CPU container for `--gpu rtx4090` would
	// make a benchmark meaningless.
	if f.GPU != "" && f.GPU != "none" {
		return nil, nil
	}
	if f.MinVRAM > 0 {
		return nil, nil
	}
	return provider.FilterOffers(f, []node.Offer{o}), nil
}

// Create starts a node container on its own bridge network.
func (p *Provider) Create(ctx context.Context, req node.CreateRequest) (node.Handle, error) {
	if req.Offer.Provider != Name {
		return node.Handle{}, fmt.Errorf("offer belongs to %q, not %q", req.Offer.Provider, Name)
	}
	img := p.imageFor(req.Spec)
	ok, err := p.Engine.ImageExists(ctx, img)
	if err != nil {
		return node.Handle{}, fmt.Errorf("check for image %s: %w", img, err)
	}
	if !ok {
		// A hard failure naming the fix, not a pull: the node image is built
		// from this repository and a silent pull would fetch somebody else's.
		return node.Handle{}, fmt.Errorf("image %s is not on this docker daemon; build it with `make docker-build` or pull it first", img)
	}

	label := provider.Label(req.RunID, req.Deadline)
	netName := "ember-" + string(req.RunID)
	common := map[string]string{
		dockerengine.LabelRunID: string(req.RunID),
		LabelLabel:              label,
	}

	netID, err := p.Engine.NetworkCreate(ctx, netName, withKind(common, "network"))
	if err != nil {
		return node.Handle{}, fmt.Errorf("create network %s: %w", netName, err)
	}

	labels := withKind(common, "node")
	labels[LabelHostKey] = req.HostKeyPublic.Algo + " " + req.HostKeyPublic.Base64
	labels[LabelUser] = p.user()

	spec := dockerengine.ContainerSpec{
		Name:    "ember-node-" + string(req.RunID),
		Image:   img,
		Labels:  labels,
		Network: netName,
		Env: map[string]string{
			// Key material travels in the Engine API's JSON body, never in
			// argv: /proc/<pid>/cmdline is world-readable, and on this
			// provider that box is the operator's own machine.
			"EMBER_AUTHORIZED_KEY": req.AuthorizedKey,
			"EMBER_HOST_KEY":       string(req.HostKey.Bytes()),
			// The node ends itself even if ember never gets to. On a rented
			// machine this is what stops the bill when the laptop dies; here
			// it keeps the lab honest by exercising the same path.
			"EMBER_TTL_SECONDS": fmt.Sprintf("%d", int(time.Until(req.Deadline).Seconds())),
			"EMBER_RUN_ID":      string(req.RunID),
		},
		PublishTCP: []uint16{sshPort},
		Binds:      p.binds(),
	}

	id, err := p.Engine.ContainerCreate(ctx, spec)
	if err != nil {
		_ = p.Engine.NetworkRemove(ctx, netID)
		return node.Handle{}, fmt.Errorf("create node container: %w", err)
	}
	if err := p.Engine.ContainerStart(ctx, id); err != nil {
		_ = p.Engine.ContainerRemove(ctx, id)
		_ = p.Engine.NetworkRemove(ctx, netID)
		return node.Handle{}, fmt.Errorf("start node container: %w", err)
	}
	return node.Handle{Provider: Name, ID: id}, nil
}

// Inspect reports what the daemon says about the node right now.
func (p *Provider) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	c, err := p.Engine.ContainerInspect(ctx, h.ID)
	if err != nil {
		if errors.Is(err, dockerengine.ErrNotFound) {
			return node.Node{}, fmt.Errorf("node %s: %w", h, node.ErrNotFound)
		}
		return node.Node{}, fmt.Errorf("inspect node %s: %w", h, err)
	}
	return mapContainer(c, Name)
}

// Destroy removes the node container and its network, in that order.
//
// Idempotent: a missing object is success, because the goal of destroy is that
// the node is gone and it is.
func (p *Provider) Destroy(ctx context.Context, h node.Handle) error {
	var runID string
	if c, err := p.Engine.ContainerInspect(ctx, h.ID); err == nil {
		runID = c.Labels[dockerengine.LabelRunID]
	} else if !errors.Is(err, dockerengine.ErrNotFound) {
		return fmt.Errorf("inspect node %s before destroying it: %w", h, err)
	}

	if err := p.Engine.ContainerRemove(ctx, h.ID); err != nil && !errors.Is(err, dockerengine.ErrNotFound) {
		return fmt.Errorf("remove node %s: %w", h, err)
	}

	if runID == "" {
		return nil
	}
	// The network goes second. Removing it first fails while the container is
	// still attached, and the error would be about the network rather than
	// about the node the operator asked to destroy.
	nets, err := p.Engine.NetworkList(ctx, dockerengine.LabelRunID+"="+runID)
	if err != nil {
		return fmt.Errorf("list networks for run %s: %w", runID, err)
	}
	for _, n := range nets {
		if err := p.Engine.NetworkRemove(ctx, n.ID); err != nil && !errors.Is(err, dockerengine.ErrNotFound) {
			return fmt.Errorf("remove network %s: %w", n.Name, err)
		}
	}
	return nil
}

// List reports every node container ember created here, found by label.
func (p *Provider) List(ctx context.Context) ([]node.Node, error) {
	cs, err := p.Engine.ContainerList(ctx, LabelKind+"=node")
	if err != nil {
		return nil, fmt.Errorf("list ember containers: %w", err)
	}
	out := make([]node.Node, 0, len(cs))
	for _, c := range cs {
		n, err := mapContainer(c, Name)
		if err != nil {
			// A container wearing ember's label that does not map is a
			// finding, not something to drop: it is still running and still
			// ember's.
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func withKind(common map[string]string, kind string) map[string]string {
	out := make(map[string]string, len(common)+1)
	for k, v := range common {
		out[k] = v
	}
	out[LabelKind] = kind
	return out
}

// mapContainer turns a container into a node.
//
// The "ready implies an endpoint" invariant is enforced here, in one pure
// function: a container that is running but whose port 22 has no published
// host port yet is StateProvisioning, not StateReady. Docker reports the
// container as running the instant it starts, which is before the port map is
// readable.
func mapContainer(c dockerengine.Container, prov node.ProviderID) (node.Node, error) {
	n := node.Node{
		Handle: node.Handle{Provider: prov, ID: c.ID},
		Offer:  node.Offer{Provider: prov, Token: "local", GPU: "none", GPUCount: 1},
	}
	// Reported unconditionally, including for a container that publishes
	// nothing. An unreported node fails every exposure assertion, which is
	// the right default, but it must not be where a correct node lands.
	n = n.WithExposed(publicPorts(c))

	if lbl, ok := c.Labels[LabelLabel]; ok {
		if runID, deadline, err := provider.ParseLabel(lbl); err == nil {
			n.RunID = runID
			n.Deadline = deadline
		}
	}
	if line, ok := c.Labels[LabelHostKey]; ok && line != "" {
		algo, b64, found := strings.Cut(line, " ")
		if found {
			n.HostKey = node.HostKey{Source: node.HostKeyMinted, Algo: algo, Base64: b64}
		}
	}

	state, err := parseState(c.State)
	if err != nil {
		return node.Node{}, fmt.Errorf("node %s: %w", c.ID, err)
	}
	n.State = state
	if state == node.StateFailed {
		n.Message = fmt.Sprintf("container exited with code %d", c.ExitCode)
	}

	if state != node.StateReady {
		return n, nil
	}

	b, ok := c.Ports[sshPort]
	if !ok || b.Port == 0 {
		// Running, but the daemon has not published 22 yet.
		n.State = node.StateProvisioning
		return n, nil
	}
	user := c.Labels[LabelUser]
	if user == "" {
		user = "root"
	}
	return n.WithEndpoint(node.Endpoint{Host: "127.0.0.1", Port: b.Port, User: user}), nil
}

// publicPorts reports which of the container's published ports are reachable
// from beyond this machine.
//
// It is derived from the host IP the daemon reports back, not from the IP the
// create call asked for. Those are the same today -- client.go asks for
// 127.0.0.1 -- and reading back the daemon's answer is the only version that
// stays true if that ever changes, or if the daemon is configured to ignore
// the request. A loopback binding is not exposure: nothing outside this
// machine can reach it, which is why the local provider is held to
// exposure.AssertNoPublishedPorts.
func publicPorts(c dockerengine.Container) []node.Exposed {
	es := []node.Exposed{}
	for port, b := range c.Ports {
		if b.IsLoopback() {
			continue
		}
		es = append(es, node.Exposed{Port: port, Proto: node.ProtoTCP, URL: b.String()})
	}
	sort.Slice(es, func(i, j int) bool { return es[i].Port < es[j].Port })
	return es
}

// parseState maps Docker's vocabulary, and errors on anything it does not
// know. An unrecognised state means ember cannot tell whether the node is
// running, and on a paid provider that is the difference between a bill and
// no bill — so the same strictness applies here, where the lab is the thing
// that would otherwise hide the bug.
func parseState(s string) (node.State, error) {
	switch s {
	case "created":
		return node.StateProvisioning, nil
	case "running":
		return node.StateReady, nil
	case "restarting":
		return node.StateProvisioning, nil
	case "paused":
		return node.StateStopped, nil
	case "exited", "dead":
		return node.StateFailed, nil
	case "removing":
		return node.StateDestroyed, nil
	default:
		return node.StateInvalid, fmt.Errorf("unrecognised container state %q", s)
	}
}

// binds is the store mount, or nothing.
//
// Docker's bind syntax takes a volume name in the host position, which is how
// a named volume is mounted rather than a host directory. Nothing is produced
// when there is no store, so a node without one is created exactly as it was
// before stores existed.
func (p *Provider) binds() []string {
	if p.Store.ID == "" || p.Mount.IsZero() {
		return nil
	}
	return []string{string(p.Store.ID) + ":" + p.Mount.Path()}
}

// volumeName is the Docker name for a store.
//
// The same label `ember store rm` parses, so a volume created here is one
// StoreList recognises and one somebody else's volume cannot be mistaken for.
func volumeName(name string) string { return provider.StoreLabel(name) }

// StoreCreate makes a Docker named volume.
//
// Idempotent, because the daemon is: creating a name that exists returns it.
// That matches what the cloud providers do closely enough for the labs to
// exercise the same command twice without special-casing.
func (p *Provider) StoreCreate(ctx context.Context, name string, sizeGiB int, region string) (node.Store, error) {
	if err := provider.ValidateStoreName(name); err != nil {
		return node.Store{}, err
	}
	// Size and region are accepted and ignored rather than refused. A Docker
	// volume grows into whatever the host has and lives wherever the daemon
	// does, so there is nothing to enforce -- but refusing them would mean a
	// config that works against RunPod fails here, and the labs exist to run
	// the same config for free.
	v, err := p.Engine.VolumeCreate(ctx, volumeName(name), map[string]string{
		LabelKind:  "store",
		LabelLabel: provider.StoreLabel(name),
	})
	if err != nil {
		return node.Store{}, fmt.Errorf("create store %s: %w", name, err)
	}
	return node.Store{
		Handle:  node.StoreHandle{Provider: Name, ID: node.StoreID(v.Name)},
		Name:    name,
		SizeGiB: sizeGiB,
		Region:  region,
		// Free, and said so rather than left at a zero that happens to mean
		// the same thing: Caps.Free is how the budget check is skipped
		// honestly, and this is the storage equivalent.
		Price: 0,
	}, nil
}

// StoreList returns every volume ember created here.
func (p *Provider) StoreList(ctx context.Context) ([]node.Store, error) {
	vols, err := p.Engine.VolumeList(ctx, LabelKind)
	if err != nil {
		return nil, fmt.Errorf("list stores: %w", err)
	}
	var out []node.Store
	for _, v := range vols {
		if v.Labels[LabelKind] != "store" {
			continue
		}
		name, err := provider.ParseStoreLabel(v.Name)
		if err != nil {
			// Carries ember's kind label but not a name ember wrote. Skipped
			// rather than reported, same as a foreign volume at RunPod.
			continue
		}
		out = append(out, node.Store{
			Handle: node.StoreHandle{Provider: Name, ID: node.StoreID(v.Name)},
			Name:   name,
		})
	}
	return out, nil
}

// StoreDestroy removes a volume. Not found counts as success.
func (p *Provider) StoreDestroy(ctx context.Context, h node.StoreHandle) error {
	if h.ID == "" {
		return fmt.Errorf("store handle names no volume")
	}
	if err := p.Engine.VolumeRemove(ctx, string(h.ID)); err != nil {
		if errors.Is(err, dockerengine.ErrNotFound) {
			return nil
		}
		return fmt.Errorf("destroy store %s: %w", h, err)
	}
	return nil
}

var _ provider.StoreAPI = (*Provider)(nil)
