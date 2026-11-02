package runpod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
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

// The two catalogue reads, spelled once each.
//
// `include=AVAILABILITY` is what adds the levels at all, and `product=POD`
// is not optional with it: the endpoint refuses the pair without it, because
// stock differs between pods and serverless. Written out rather than built
// from url.Values at three call sites -- two offer paths and the availability
// snapshot -- because the query is the difference between a catalogue with
// capacity in it and one without, and three copies is three places to drop a
// parameter. The spelling and the refusal are both in the live v2 OpenAPI
// document.
const (
	gpuCataloguePath = "/v2/catalog/gpus?include=AVAILABILITY&product=POD"
	cpuCataloguePath = "/v2/catalog/cpus?include=AVAILABILITY&product=POD"
)

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
		//
		// Through reclaimOf, so this claim and the Reclaim stamped on every
		// offer are one derivation of one fact. Two readings of p.Cloud
		// would be two sources of truth for the same thing, and the one that
		// disagreed would be the one that let a reclaimable pod be rented --
		// the Offer.VCPU rule, applied to the field that used to be read by
		// nothing.
		InterruptibleTier: reclaimOf(p.cloud()).Interruptible(),

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

		// The catalogue is worth sampling here and nowhere else so far:
		// RunPod reports availability as a level with no timestamp and keeps
		// no history of its own, and a store pins every future run to one
		// datacentre. See provider.CatalogueAPI for why that is a capability
		// rather than a method on Provider.
		Catalogue: true,

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

func (p *Provider) disk(o node.Offer) int {
	want := p.DiskGiB
	if want <= 0 {
		want = DefaultDiskGiB
	}
	// CPU machines take at most 20 GiB of container disk, and the API
	// refuses the pod outright above it: "Container Disk must be less than
	// or equal to 20". GPU pods have no such ceiling, so this is not a
	// default that could be lowered for both -- it is a property of the
	// product being rented.
	//
	// Clamping rather than refusing, because the disk size is incidental to
	// what a CPU machine is rented for here: the download lands on the
	// mounted store, not on the container's own disk, which only has to hold
	// the image.
	if o.IsCPU() && want > CPUMaxDiskGiB {
		return CPUMaxDiskGiB
	}
	return want
}

// CPUMaxDiskGiB is the largest container disk a CPU pod will accept.
//
// Measured, not documented: the schema gives the field a minimum of 1 and no
// maximum, and the limit is enforced at the handler with a 400.
const CPUMaxDiskGiB = 20

// Offers lists what can be rented.
//
// The catalogue is asked for availability as well as price, so an offer ember
// returns is one RunPod says it has rather than one it merely has a price
// for. provision re-filters regardless of what this asked.
func (p *Provider) Offers(ctx context.Context, f node.Filter) ([]node.Offer, error) {
	// A different catalogue entirely, on a different endpoint. Asked for
	// only when a CPU machine is what is wanted, so the common case still
	// makes one request.
	if f.CPUOnly {
		return p.cpuOffers(ctx, f)
	}

	var resp listGpuTypesResponse
	if err := p.HTTP.Get(ctx, gpuCataloguePath, &resp); err != nil {
		return nil, fmt.Errorf("list runpod gpu types: %w", err)
	}

	// The region comes from the filter, which is where a placement
	// constraint belongs. A store pins its node to one datacentre, and the
	// caller expresses that by narrowing the filter rather than by
	// configuring the provider -- so two runs wanting different stores need
	// no second provider.
	offers := regionOffers(resp.Gpus, p.cloud(), f.Region)

	// A store that pins placement and a datacentre with nothing available is
	// a refusal, not an empty list. "no offers matched" reads as "that GPU is
	// sold out everywhere" and sends the operator to try a different card,
	// when the real answer is that their weights are in the wrong site -- and
	// the fix is to pick a datacentre, not a GPU.
	if f.Region != "" && len(offers) == 0 && len(resp.Gpus) > 0 {
		return nil, fmt.Errorf("%w: nothing is available in %s, where the store lives. "+
			"A node must be created in the store's datacentre, so this is a choice between "+
			"waiting for capacity there and creating a store somewhere else",
			node.ErrNoCapacity, f.Region)
	}
	return p.filter(f, offers)
}

// filter narrows the catalogue and explains an empty answer the one way it
// cannot be read off the result.
//
// With runpod_cloud: COMMUNITY every offer is reclaimable, so an operator who
// has not set allow_interruptible gets nothing back at all -- and "no offers
// matched" reads as "that card is sold out everywhere" and sends them off to
// try another one. It is the same wrong story the datacentre refusal above
// exists to avoid, and it has to be told here rather than in
// provision.PickOffer: this is the last place that still holds the unfiltered
// list and knows the tier, because the offers the reclaim guard drops are
// dropped by the FilterOffers call on the line below.
func (p *Provider) filter(f node.Filter, offers []node.Offer) ([]node.Offer, error) {
	kept := provider.FilterOffers(f, offers)
	if len(kept) > 0 {
		return kept, nil
	}
	if n := provider.RefusedForReclaim(f, offers); n > 0 {
		return nil, fmt.Errorf("%w: all %d of RunPod's matching %s offers can be reclaimed mid-run, "+
			"and allow_interruptible is false. A reclaimed pod goes away without ember destroying "+
			"it, mid-generation, and whatever it had produced that was not fetched yet is gone.\n"+
			"Set allow_interruptible: true to bid on them anyway, or runpod_cloud: SECURE for "+
			"hardware RunPod owns and does not take back",
			node.ErrNoCapacity, n, p.cloud())
	}
	return kept, nil
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

	// min_cuda cannot be expressed against this API, and saying so is better
	// than dropping it.
	//
	// ember used to send it as allowedCudaVersions, which came from RunPod's
	// published schema. The wrong one: rest.runpod.io/v1/pods accepts that
	// field, and api.runpod.io/v2/pods -- the endpoint below -- declares 18
	// properties with unevaluatedProperties: false and no CUDA field at all.
	// Every create with min_cuda set therefore died at:
	//
	//   status 422: additional properties 'allowedCudaVersions' not allowed
	//
	// Filtering offers instead is not available either, and the reason is NOT
	// the one this comment used to give. It said "the v2 schema puts
	// cudaVersions on GpuType, and the live catalogue does not return it",
	// which was false when it was written: testdata/gpus.json is a live
	// capture, committed two days earlier, and three of its four entries
	// carry cudaVersions. A schema was consulted where the response was
	// already in the tree -- allowedCudaVersions a third time, with the roles
	// reversed.
	//
	// What the field cannot do is be a floor, and that is measured rather
	// than reasoned: `available` tracks the card's top-level availability
	// summary on every captured row, so a floor built on it refuses the RTX
	// 4090 that reads NONE overall, LOW in US-IL-1 and reports all eight of
	// its CUDA versions unavailable -- for every value of min_cuda, including
	// ones the card satisfies. That is the bug hasCapacity and regionOffers
	// exist to prevent, through a different field. Read without the flag it
	// describes the catalogue and not the host the pod lands on, and the
	// versions are described upstream as "suitable for
	// gpu.allowedCudaVersions on pod create" -- the property below answers
	// 422 for. It is also on GpuType rather than on dataCenters[], so it
	// cannot be re-read through a store's site. See
	// TestCatalogue_ReportsCudaVersionsAndTheyAreNotAFloorForMinCUDA, which
	// pins all of that to the capture so the next person re-decides it on
	// evidence.
	//
	// So this is a refusal rather than a silent drop. A constraint the
	// operator believes is in force and is not would be worse than none --
	// the same reason the field was added with a comment admitting nobody had
	// confirmed it filtered. What does enforce a driver floor is gpu_probe,
	// which runs on the node and costs the probe rather than the run.
	//
	// The second time this provider's two APIs have disagreed about a field.
	// The first was network volumes, dataCenter against dataCenterId.
	if strings.TrimSpace(req.Spec.MinCUDA) != "" {
		return node.Handle{}, fmt.Errorf(
			"min_cuda is set to %q and RunPod's pod API does not accept it: "+
				"api.runpod.io/v2/pods has no CUDA field, and the per-GPU cudaVersions its "+
				"catalogue does report tracks whether the card has capacity rather than what a "+
				"host can run -- a card that is rentable in one datacentre reports every CUDA "+
				"version unavailable -- so ember cannot narrow placement by CUDA version.\n"+
				"Remove min_cuda. To refuse a host that cannot run the image, use gpu_probe: it "+
				"runs on the node and fails before any workload does, costing the probe rather "+
				"than the run",
			req.Spec.MinCUDA)
	}

	body := createPodRequest{
		// The pod name carries ember's label, because RunPod has no tag API.
		Name:          provider.Label(req.RunID, req.Deadline),
		Image:         req.Spec.Image,
		Gpu:           gpuFor(req.Offer),
		Cpu:           cpuFor(req.Offer),
		Env:           p.env(req),
		Ports:         ports,
		Disk:          p.disk(req.Offer),
		Cloud:         p.cloud(),
		DataCenterIds: p.dataCenters(req),
		Mounts:        mountsFor(req),

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
//
// The run id is ignored, and Caps.Leftovers is false to say so in a form a
// caller can branch on: a pod is the only object a run creates here. A store
// is not one of these -- it outlives every node deliberately, and removing it
// as a side effect of a destroy is the mistake StoreAPI is a separate
// interface to prevent.
func (p *Provider) Destroy(ctx context.Context, h node.Handle, _ node.RunID) error {
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
func (p *Provider) dataCenters(req node.CreateRequest) []string {
	if !req.Spec.Store.IsZero() && req.Offer.Region != "" {
		return []string{req.Offer.Region}
	}
	return p.DataCenters
}

// mounts is the storage to attach, or nothing.
//
// Nothing when there is no store, so a pod created without one carries no
// mounts field at all rather than an empty object -- the create body stays
// exactly what it was before stores existed.
func mountsFor(req node.CreateRequest) *mounts {
	if req.Spec.Store.IsZero() || req.Spec.Mount.IsZero() {
		return nil
	}
	return &mounts{Network: []networkMount{{
		VolumeID: string(req.Spec.Store.ID),
		Path:     req.Spec.Mount.Path(),
	}}}
}

// gpuFor and cpuFor fill exactly one of the create body's two machine
// fields, because the API refuses a pod that asks for both.
//
// The offer decides, not a flag: a CPU offer carries its vCPU count and no
// card, so there is one answer and nothing to keep in step.
func gpuFor(o node.Offer) *gpuConfig {
	if o.IsCPU() {
		return nil
	}
	return &gpuConfig{ID: string(o.Token), Count: max(o.GPUCount, 1)}
}

func cpuFor(o node.Offer) *cpuConfig {
	if !o.IsCPU() {
		return nil
	}
	return &cpuConfig{ID: string(o.Token), VcpuCount: o.VCPU}
}

// cpuOffers lists CPU-only machines.
//
// A separate call from the GPU catalogue because it is a separate endpoint
// with a different shape, and a separate request because asking for both on
// every Offers call would double the latency of the common case to serve the
// rare one.
func (p *Provider) cpuOffers(ctx context.Context, f node.Filter) ([]node.Offer, error) {
	var resp listCpuTypesResponse
	if err := p.HTTP.Get(ctx, cpuCataloguePath, &resp); err != nil {
		return nil, fmt.Errorf("list runpod cpu types: %w", err)
	}

	offers := mapCPUOffers(resp.Cpus, p.cloud(), f.Region, f.MinCount)
	if f.Region != "" && len(offers) == 0 && len(resp.Cpus) > 0 {
		return nil, fmt.Errorf("%w: no CPU machine is available in %s, where the store lives",
			node.ErrNoCapacity, f.Region)
	}
	return p.filter(f, offers)
}

// CatalogueSnapshot reads both catalogues once, for the availability history.
//
// Two GETs, no pod, no GPU, nothing billed -- which is the argument for
// sampling hourly rather than when somebody remembers. What it records is the
// one thing in this program that cannot be collected later: RunPod's
// availability is a bare level with no timestamp and no window, so every read
// is a point sample and nothing on its side remembers the previous one.
//
// A CPU failure returns the GPU rows ALONGSIDE the error, the way List returns
// what it understood beside a status it did not. The caller records what came
// back and then reports the failure: an hour of GPU observations is the thing
// that cannot be recovered, and discarding it because the second request
// failed would turn a partial answer into no answer. A GPU failure is fatal,
// because that is the series the store decision is made from.
func (p *Provider) CatalogueSnapshot(ctx context.Context, now time.Time) ([]catalogue.Sample, error) {
	if now.IsZero() {
		// Refused rather than defaulted to time.Now(). The caller passes the
		// instant precisely so that what gets written is the instant it
		// recorded, and a zero time silently replaced by this function's own
		// clock is a row dated by whichever layer looked at a clock last.
		return nil, fmt.Errorf("a catalogue snapshot was asked for with no observation time; " +
			"observed_at is the only field RunPod does not supply and ember will not invent it")
	}

	var gpus listGpuTypesResponse
	if err := p.HTTP.Get(ctx, gpuCataloguePath, &gpus); err != nil {
		return nil, fmt.Errorf("list runpod gpu types: %w", err)
	}
	out := gpuSamples(gpus.Gpus, now)

	var cpus listCpuTypesResponse
	if err := p.HTTP.Get(ctx, cpuCataloguePath, &cpus); err != nil {
		return out, fmt.Errorf("list runpod cpu types: %w", err)
	}
	return append(out, cpuSamples(cpus.Cpus, now)...), nil
}
