package runpod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/httpx"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// WebTerminalPort is the public HTTP mapping RunPod adds to every pod.
//
// Found the hard way: ember's first real provision was refused and destroyed
// because RunPod published 19123/http when ember had asked for 22/tcp and
// nothing else. There is no way to disable it -- the create API's only
// toggles are startJupyter and startSsh -- so a provider that is usable at
// all has to name it.
//
// It is not harmless in general. RunPod's own documentation says of an
// exposed HTTP port that "the service becomes publicly accessible" and that
// "while the Pod ID provides some obscurity, implement proper authentication
// in your application". The pod id is obscurity, not a credential.
//
// What makes it tolerable here is a property of ember's own node image rather
// than a promise from the provider: nothing in it listens on this port, so
// the proxy has nothing to reach. That is asserted in a lab, and
// RefuseWorkloadPort keeps a workload from binding it later.
const WebTerminalPort uint16 = 19123

var platformPorts = []exposure.PlatformPort{{
	Port:  WebTerminalPort,
	Proto: node.ProtoHTTP,
	Why:   "RunPod's web terminal, mapped to every pod with no way to disable it; ember's node image listens on nothing there",
}}

// DefaultDiskGiB is the container disk ember asks for.
//
// Model weights are the reason this is not smaller: a single video model
// checkpoint is tens of gigabytes, and a pod that runs out of disk mid-pull
// fails after it has already started billing.
const DefaultDiskGiB = 50

// Provider rents pods from RunPod.
type Provider struct {
	// HTTP is the API client. BaseURL defaults to RunPod's v2 API.
	HTTP httpx.Client

	// Cloud is SECURE or COMMUNITY.
	//
	// SECURE by default, matching RunPod's own default. COMMUNITY is cheaper
	// and runs on hardware somebody else owns and can see; that is a choice
	// to make deliberately, not one to inherit from a zero value.
	Cloud string

	// DataCenters restricts placement. Empty lets RunPod choose.
	DataCenters []string

	// DiskGiB overrides DefaultDiskGiB.
	DiskGiB int

	// Store is the network volume to attach to every node this provider
	// creates, and Mount is where it lands inside the container.
	//
	// Both zero means no store, which is the shape every run had before this
	// existed. Set, they also pin placement: a volume lives in one datacentre
	// and a pod that mounts it must be created there, so StoreRegion takes
	// over from DataCenters rather than being merged with it.
	Store node.StoreHandle
	Mount node.Mount

	// StoreRegion is the datacentre the store lives in.
	//
	// Carried separately from Store because the offer filter needs it before
	// anything has been created, and asking the API where a volume lives on
	// every Offers call would be a request per listing for a fact that cannot
	// change: a volume's datacentre is fixed at creation.
	StoreRegion string
}

// New returns a provider using the given API token.
func New(token node.Secret) *Provider {
	return &Provider{HTTP: httpx.Client{BaseURL: BaseURL, Token: token}}
}

func (p *Provider) Name() node.ProviderID { return Name }

// Caps describes what ember may assume about RunPod.
func (p *Provider) Caps() provider.Caps {
	return provider.Caps{
		// ember injects its own host key through env and runs its own sshd,
		// so the first connection is verified rather than trusted on sight.
		HostKeyFromAPI: true,

		// Secure cloud pods are not reclaimed under the operator. Community
		// hosts are somebody else's machines and can go away.
		Interruptible: strings.EqualFold(p.Cloud, cloudCommunity),

		// No tag API: ember's label lives in the pod name, which is why
		// ParseLabel is strict rather than a substring match.
		Tags: false,

		Free: false,

		// Per second, with no minimum. Measured rather than assumed: across
		// four pods of 37s, 165s, 49s and 67s, RunPod charged $0.08 against
		// a per-second total of $0.0787 and a one-minute-minimum total of
		// $0.0870. The first is within the dashboard's rounding; the second
		// is 0.7 cents out, which is exactly what the assumed minimum added
		// to the two sub-minute pods.
		//
		// It was a minute before, carried over from general knowledge about
		// GPU providers and never checked. That inflated every short run by
		// up to a minute's charge -- always upward, so nothing was ever
		// under-budgeted, but an estimate that is wrong in a safe direction
		// is still wrong and teaches the operator the wrong shape.
		MinBilling: 0,

		// Reached over a public 22/tcp mapping, so exactly one public port is
		// expected and anything else is a violation. See
		// exposure.AssertSSHOnly.
		PublicSSH: true,

		PlatformPorts: platformPorts,

		// Network volumes, which ember calls stores. Secure cloud only --
		// RunPod does not offer them on community hosts -- but the capability
		// is reported unconditionally because it describes the provider
		// rather than this run's cloud tier, and a create against the wrong
		// tier fails with RunPod's own message rather than ember guessing.
		Stores: true,
	}
}

func (p *Provider) cloud() string {
	if p.Cloud != "" {
		return strings.ToUpper(p.Cloud)
	}
	return cloudSecure
}

func (p *Provider) disk() int {
	if p.DiskGiB > 0 {
		return p.DiskGiB
	}
	return DefaultDiskGiB
}

// Offers lists what can be rented.
//
// The catalogue is asked for availability as well as price, so an offer ember
// returns is one RunPod says it has rather than one it merely has a price
// for. provision re-filters regardless of what this asked.
func (p *Provider) Offers(ctx context.Context, f node.Filter) ([]node.Offer, error) {
	q := url.Values{}
	q.Set("include", "AVAILABILITY")
	q.Set("product", "POD")

	var resp listGpuTypesResponse
	if err := p.HTTP.Get(ctx, "/v2/catalog/gpus?"+q.Encode(), &resp); err != nil {
		return nil, fmt.Errorf("list runpod gpu types: %w", err)
	}

	offers := regionOffers(resp.Gpus, p.cloud(), p.StoreRegion)

	// A store that pins placement and a datacentre with nothing available is
	// a refusal, not an empty list. "no offers matched" reads as "that GPU is
	// sold out everywhere" and sends the operator to try a different card,
	// when the real answer is that their weights are in the wrong site -- and
	// the fix is to pick a datacentre, not a GPU.
	if p.StoreRegion != "" && len(offers) == 0 && len(resp.Gpus) > 0 {
		return nil, fmt.Errorf("%w: nothing is available in %s, where the store lives. "+
			"A node must be created in the store's datacentre, so this is a choice between "+
			"waiting for capacity there and creating a store somewhere else",
			node.ErrNoCapacity, p.StoreRegion)
	}
	return provider.FilterOffers(f, offers), nil
}

// Create starts a pod and returns its handle.
//
// It returns a Handle and not a Node because a pod created a millisecond ago
// is never ready: RunPod leaves ssh.direct null until the public 22/tcp
// mapping is assigned, which happens well after the status reads RUNNING.
func (p *Provider) Create(ctx context.Context, req node.CreateRequest) (node.Handle, error) {
	ports, err := p.ports(req.Spec.Ports)
	if err != nil {
		return node.Handle{}, err
	}

	body := createPodRequest{
		// The pod name carries ember's label, because RunPod has no tag API.
		Name:  provider.Label(req.RunID, req.Deadline),
		Image: req.Spec.Image,
		Gpu: gpuConfig{
			ID:    string(req.Offer.Token),
			Count: max(req.Offer.GPUCount, 1),
		},
		Env:           p.env(req),
		Ports:         ports,
		Disk:          p.disk(),
		Cloud:         p.cloud(),
		DataCenterIds: p.dataCenters(),
		Mounts:        p.mounts(),
		// ember runs its own sshd with the host key it minted. RunPod's own
		// sshd would present a key ember cannot predict, forcing
		// trust-on-first-use against a brand-new IP.
		StartSsh: false,
	}

	var created pod
	if err := p.HTTP.Post(ctx, "/v2/pods", body, &created); err != nil {
		return node.Handle{}, fmt.Errorf("create runpod pod: %w", err)
	}
	if created.ID == "" {
		// A create that returns 2xx with no id is the worst case: a pod may
		// exist and ember cannot name it. Saying so loudly is the only
		// honest response, and the journal's label is how it gets found.
		return node.Handle{}, fmt.Errorf("runpod accepted the create but returned no pod id; a pod named %q may exist and be billing -- check with: ember ls", body.Name)
	}
	return node.Handle{Provider: Name, ID: created.ID}, nil
}

// env is everything the node needs to configure itself.
//
// All of it travels in the JSON body. None of it may ever reach the pod's
// command line: /proc/<pid>/cmdline is world-readable, and on rented hardware
// the other tenants of that host are strangers.
func (p *Provider) env(req node.CreateRequest) map[string]string {
	return map[string]string{
		"EMBER_AUTHORIZED_KEY": req.AuthorizedKey,
		"EMBER_HOST_KEY":       string(req.HostKey.Bytes()),
		"EMBER_RUN_ID":         string(req.RunID),
		// The node ends itself even if ember never gets to. On rented
		// hardware this is what stops the bill when the operator's laptop
		// dies mid-run.
		"EMBER_TTL_SECONDS": fmt.Sprintf("%d", int(time.Until(req.Deadline).Seconds())),
	}
}

func (p *Provider) ports(workload []node.PortMap) ([]string, error) {
	// A workload port that collides with the platform's own mapping would be
	// served through RunPod's unauthenticated proxy. ember tolerates that
	// port only because nothing listens on it; a workload binding it is
	// exactly the case that stops being true.
	for _, w := range workload {
		if w.Remote == WebTerminalPort {
			return nil, fmt.Errorf(
				"workload port %q asks for %d, which RunPod maps to its public proxy on every pod: "+
					"a service there is reachable at {pod-id}-%d.proxy.runpod.net with no authentication. Move it to another port",
				w.Name, WebTerminalPort, WebTerminalPort)
		}
	}
	reqs, err := exposure.Requests(workload)
	if err != nil {
		return nil, err
	}
	return mapPorts(reqs)
}

// Inspect reports what RunPod says about one pod right now.
//
// A single fetch by id, never a filtered list: RunPod documents ssh.direct and
// runtime as null on every entry of a list response, so a list-based Inspect
// would report every ready pod as still provisioning forever.
func (p *Provider) Inspect(ctx context.Context, h node.Handle) (node.Node, error) {
	var got pod
	err := p.HTTP.Get(ctx, "/v2/pods/"+url.PathEscape(h.ID), &got)

	var se *httpx.StatusError
	if errors.As(err, &se) && se.Code == http.StatusNotFound {
		return node.Node{}, fmt.Errorf("pod %s: %w", h.ID, node.ErrNotFound)
	}
	if err != nil {
		return node.Node{}, fmt.Errorf("inspect runpod pod %s: %w", h.ID, err)
	}
	return mapPod(got)
}

// List reports every pod wearing ember's label.
//
// The entries carry a handle and a state and no endpoint, which is what
// RunPod returns for a list and all reconcile needs. Anything whose name does
// not parse as an ember label is skipped: somebody else's pod in the same
// account is not ember's to report, let alone to reap.
func (p *Provider) List(ctx context.Context) ([]node.Node, error) {
	var resp listPodsResponse
	if err := p.HTTP.Get(ctx, "/v2/pods", &resp); err != nil {
		return nil, fmt.Errorf("list runpod pods: %w", err)
	}

	out := make([]node.Node, 0, len(resp.Pods))
	for _, got := range resp.Pods {
		if _, _, err := provider.ParseLabel(got.Name); err != nil {
			continue
		}
		n, err := mapPod(got)
		if err != nil {
			// One unrecognised status must not hide the rest of the account.
			// It is still an error, because not knowing a pod's state means
			// not knowing whether it is billing -- so it is returned
			// alongside what was understood.
			return out, fmt.Errorf("list runpod pods: %w", err)
		}
		out = append(out, n)
	}
	return out, nil
}

// Destroy removes the pod, treating "no such pod" as success.
func (p *Provider) Destroy(ctx context.Context, h node.Handle) error {
	if err := p.HTTP.Delete(ctx, "/v2/pods/"+url.PathEscape(h.ID)); err != nil {
		return fmt.Errorf("destroy runpod pod %s: %w", h.ID, err)
	}
	return nil
}

// dataCenters is where RunPod may place this pod.
//
// A store overrides the configured list rather than intersecting with it. The
// volume is in exactly one site and a pod that mounts it must be created
// there, so an intersection could be empty while the store's own site has
// capacity -- and a placement list that excluded the store's site would be a
// request RunPod can only refuse.
func (p *Provider) dataCenters() []string {
	if p.StoreRegion != "" {
		return []string{p.StoreRegion}
	}
	return p.DataCenters
}

// mounts is the storage to attach, or nothing.
//
// Nothing when there is no store, so a pod created without one carries no
// mounts field at all rather than an empty object -- the create body stays
// exactly what it was before stores existed.
func (p *Provider) mounts() *mounts {
	if p.Store.ID == "" || p.Mount.IsZero() {
		return nil
	}
	return &mounts{Network: []networkMount{{
		VolumeID: string(p.Store.ID),
		Path:     p.Mount.Path(),
	}}}
}
