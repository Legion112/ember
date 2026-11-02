package runpod

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/catalogue"
	"github.com/Legion112/ember/internal/exposure"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
)

// Name is this provider's id.
const Name node.ProviderID = "runpod"

// statusTable maps RunPod's lifecycle status onto ember's.
//
// A table with an error on anything unlisted, not a switch with a default. An
// unrecognised status means ember does not know whether money is burning, and
// the safe reading of that is "fail and say so", not "assume it is fine".
var statusTable = map[string]node.State{
	"PROVISIONING": node.StateProvisioning,
	"STARTING":     node.StateProvisioning,
	"RUNNING":      node.StateReady,
	"EXITED":       node.StateFailed,
	"ERROR":        node.StateFailed,
	"TERMINATED":   node.StateDestroyed,
}

// mapStatus is total over the strings RunPod documents and an error on
// anything else.
func mapStatus(s string) (node.State, error) {
	if st, ok := statusTable[s]; ok {
		return st, nil
	}
	return node.StateInvalid, fmt.Errorf("runpod reported pod status %q, which ember does not recognise; it cannot tell whether that pod is costing money, so it refuses to guess", s)
}

// mapProto maps RunPod's port protocol.
//
// "http" is kept distinct from "tcp" rather than folded into it, because an
// http port on RunPod is served at {id}-{port}.proxy.runpod.net with no
// authentication at all. Collapsing the two would make the exposure assertion
// unable to tell a forwarded SSH port from a public web shell.
func mapProto(s string) (node.Proto, error) { return node.ParseProto(s) }

// mapExposure turns a pod's runtime ports into what ember checks.
//
// Only a port RunPod has assigned a public mapping to counts. The second
// return says whether RunPod reported its runtime at all: it is null while the
// pod is provisioning and on every entry of a list response, and "RunPod did
// not say" must not be read as "RunPod published nothing". See
// node.Node.Exposure.
func mapExposure(p pod) ([]node.Exposed, bool, error) {
	if p.Runtime == nil {
		return nil, false, nil
	}

	out := []node.Exposed{}
	for _, rp := range p.Runtime.Ports {
		if rp.Public == nil {
			// Reachable only from inside the pod's own network. That is what
			// ember asks for for every workload port.
			continue
		}
		proto, err := mapProto(rp.Type)
		if err != nil {
			return nil, true, fmt.Errorf("pod %s port %d: %w", p.ID, rp.Private, err)
		}
		port, err := port16(rp.Private)
		if err != nil {
			return nil, true, fmt.Errorf("pod %s: public port: %w", p.ID, err)
		}
		e := node.Exposed{Port: port, Proto: proto}
		if rp.IP != nil && *rp.IP != "" {
			e.URL = fmt.Sprintf("%s:%d", *rp.IP, *rp.Public)
		}
		out = append(out, e)
	}
	return out, true, nil
}

// mapPod turns a RunPod pod into an ember node.
//
// Pure and total: every failure is a returned error, and nothing here calls
// out to the network or the clock. This is the function a table test over
// committed fixtures can exercise exhaustively, which matters because it is
// the code deciding whether a machine is ready to be trusted and billed.
func mapPod(p pod) (node.Node, error) {
	state, err := mapStatus(p.Status)
	if err != nil {
		return node.Node{}, err
	}

	n := node.Node{
		Handle: node.Handle{Provider: Name, ID: p.ID},
		State:  state,
	}

	// The run id and deadline come from the pod's name, which is where
	// ember's label lives on this provider. Parsed strictly: a name that
	// does not match is somebody else's pod, and ember must not claim it.
	if runID, deadline, err := provider.ParseLabel(p.Name); err == nil {
		n.RunID = runID
		n.Deadline = deadline
	}

	region := ""
	if p.DataCenterID != nil {
		region = *p.DataCenterID
	}
	gpuID, gpuCount := "", 1
	if p.Gpu != nil {
		gpuID = p.Gpu.ID
		if p.Gpu.Count > 0 {
			gpuCount = p.Gpu.Count
		}
	}
	n.Offer = node.Offer{
		Provider: Name,
		Token:    node.OfferToken(gpuID),
		GPU:      node.NormalizeGPU(gpuID),
		GPUCount: gpuCount,
		Region:   region,
		// The rate RunPod says it is billing, not the catalogue list price.
		Price: node.RateFromUSD(p.Cost),
	}

	exposed, reported, err := mapExposure(p)
	if err != nil {
		return node.Node{}, err
	}
	if reported {
		n = n.WithExposed(exposed)
	}

	if state != node.StateReady {
		return n, nil
	}

	// Ready, so an endpoint is expected -- but RunPod leaves ssh.direct null
	// until the public 22/tcp mapping is assigned, which lags RUNNING. That
	// is reported as still provisioning rather than as a failure: it is a
	// state the pod passes through, and WaitReady is built to poll.
	if p.Ssh == nil || p.Ssh.Direct == nil || p.Ssh.Direct.Host == "" || p.Ssh.Direct.Port == 0 {
		n.State = node.StateProvisioning
		n.Message = "running, but RunPod has not yet assigned the public 22/tcp mapping that ssh.direct needs"
		return n, nil
	}

	user := p.Ssh.Direct.Username
	if user == "" {
		user = "root"
	}
	sshPort, err := port16(p.Ssh.Direct.Port)
	if err != nil {
		return node.Node{}, fmt.Errorf("pod %s: ssh.direct port: %w", p.ID, err)
	}
	return n.WithEndpoint(node.Endpoint{
		Host: p.Ssh.Direct.Host,
		Port: sshPort,
		User: user,
	}), nil
}

// port16 narrows a provider's int port, refusing anything out of range.
//
// Checked rather than converted, because a silent wrap is worse than either a
// refusal or a crash here. uint16(70000) is 4464: on the endpoint path ember
// would dial a port the node is not listening on and report a timeout that
// blames the node, and on the exposure path a public port outside the range
// would be *reported as a different port* -- so an assertion about what is
// reachable would be made about the wrong number.
func port16(n int) (uint16, error) {
	if n < 1 || n > 65535 {
		return 0, fmt.Errorf("runpod reported port %d, which is outside 1..65535; ember will not narrow it to something else", n)
	}
	return uint16(n), nil
}

// mapOffers turns the GPU catalogue into ember offers.
//
// cloud selects which of the two quoted prices applies. Both are list prices;
// what a pod actually costs is read back from the pod itself.
func mapOffers(gpus []gpuType, cloud string) []node.Offer {
	community := strings.EqualFold(cloud, cloudCommunity)
	reclaim := reclaimOf(cloud)

	out := make([]node.Offer, 0, len(gpus))
	for _, g := range gpus {
		price, maxCount, available := g.Price.Secure, g.MaxCount.Secure, g.Secure
		if community {
			price, maxCount, available = g.Price.Community, g.MaxCount.Community, g.Community
		}
		// A GPU this cloud tier does not offer is not an offer. Listing it
		// would mean every `ember offers` shows hardware that cannot be
		// rented, and `ember up` would pick it and fail at create.
		if !available {
			continue
		}
		// A zero price is not free here: it is RunPod declining to quote, and
		// a budget check against zero would wave through whatever it really
		// costs.
		if price <= 0 {
			continue
		}
		// Dropped only when nothing anywhere has capacity. The top-level
		// figure is a summary: the live catalogue reported an RTX 4090 as
		// NONE overall while its US-IL-1 entry read LOW, and filtering on
		// the summary alone would have had ember answer "no capacity" for a
		// card that was rentable. Offering one that turns out to be gone
		// costs a failed create and no money; hiding one costs the operator
		// the thing they came for.
		if !hasCapacity(g) {
			continue
		}
		if maxCount < 1 {
			maxCount = 1
		}
		out = append(out, node.Offer{
			Provider: Name,
			Token:    node.OfferToken(g.ID),
			GPU:      node.NormalizeGPU(g.ID),
			GPUCount: 1,
			// RunPod quotes GB; ember's Offer carries MiB.
			VRAM:    node.GiB(g.Memory),
			Price:   node.RateFromUSD(price),
			Region:  bestRegion(g),
			Reclaim: reclaim,
			Label:   fmt.Sprintf("%s, %d GiB, up to %d per pod, availability %s", g.Name, g.Memory, maxCount, strings.ToLower(availabilityOf(g))),
		})
	}
	return out
}

const (
	cloudSecure      = "SECURE"
	cloudCommunity   = "COMMUNITY"
	availabilityNone = "NONE"
)

// reclaimOf is the one place RunPod's cloud tier becomes a reclaim answer.
//
// One function read by Caps and by both offer mappers, because the tier is
// the whole of the fact: a community pod runs on hardware somebody else owns
// and can take back, a secure one does not. Deriving it twice -- once for the
// capability and once in the mapper -- is two sources of truth for one fact,
// and the copy that disagreed would be the one that let a reclaimable pod
// through the filter.
//
// Never ReclaimUnknown. Unknown is the answer for a provider that has not
// been taught to say, and this one has: every pod is created with
// Cloud: p.cloud(), so the tier is known before anything is rented.
func reclaimOf(cloud string) node.Reclaim {
	if strings.EqualFold(cloud, cloudCommunity) {
		return node.ReclaimAnyTime
	}
	return node.ReclaimNever
}

// hasCapacity reports whether anything, anywhere, says this GPU can be had.
func hasCapacity(g gpuType) bool {
	if g.Availability != availabilityNone && g.Availability != "" {
		return true
	}
	for _, dc := range g.DataCenters {
		if dc.Availability != availabilityNone && dc.Availability != "" {
			return true
		}
	}
	return false
}

// availabilityOf is the level worth printing: the best any site reports,
// falling back to the overall figure.
func availabilityOf(g gpuType) string {
	best := g.Availability
	for _, dc := range g.DataCenters {
		if rank(dc.Availability) > rank(best) {
			best = dc.Availability
		}
	}
	if best == "" {
		return "unknown"
	}
	return best
}

// rank orders RunPod's availability levels. An unrecognised level sorts above
// NONE and below LOW: ember should not hide a GPU because the API grew a word
// it does not know, nor claim it is plentiful.
//
// The ordering itself lives in catalogue.Level, because the availability
// history reads these levels back years later and needs the same answer. Two
// copies of one ordering is two places for "what does a word nobody has seen
// mean" to be decided differently -- and the two would disagree about exactly
// the level neither was written for.
func rank(a string) int { return catalogue.Level(a).Rank() }

// bestRegion names a site that actually has capacity, so `ember offers` does
// not print a data centre with none as though it were where the node would
// land.
func bestRegion(g gpuType) string {
	best, bestRank := "", -1
	for _, dc := range g.DataCenters {
		if r := rank(dc.Availability); r > bestRank {
			best, bestRank = dc.ID, r
		}
	}
	return best
}

// mapPorts turns ember's port requests into RunPod's "port/protocol" strings.
//
// It refuses any public request that is not SSH. exposure.PortRequest already
// makes that unrepresentable -- only SSHPort can produce a public one -- so
// this is the second lock on the same door, at the exact point where a wrong
// value would become an unauthenticated public URL.
//
// That refusal is therefore unreachable, and deliberately left uncovered. The
// first version of this file reached it by exporting a helper from the
// exposure package that minted a public non-SSH request, which put a hole in
// the wall the seal exists to be: an escape hatch for tests is an escape
// hatch. The branch stays untested, because a narrowing that is only safe by
// construction is one nobody re-checks when the construction changes -- and
// if exposure ever grows a second public constructor, this is what catches it.
func mapPorts(reqs []exposure.PortRequest) ([]string, error) {
	out := make([]string, 0, len(reqs))
	for _, r := range reqs {
		if r.IsZero() {
			return nil, fmt.Errorf("a zero port request reached the runpod mapper; it names port 0, which is not a port")
		}
		if !r.IsPublic() {
			// Internal ports are simply not declared. RunPod only publishes
			// what is listed, so the safest way to keep a workload port
			// private is to say nothing about it: it is reached by
			// forwarding over the SSH connection.
			continue
		}
		if r.Port() != 22 || r.Proto() != node.ProtoTCP {
			return nil, fmt.Errorf("refusing to declare %s publicly on runpod: only 22/tcp may be public, and a port declared any other way is served at {id}-{port}.proxy.runpod.net with no authentication", r)
		}
		out = append(out, "22/tcp")
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no public SSH port was requested; RunPod leaves ssh.direct null without 22/tcp in ports, and ember would have no way to reach the pod")
	}
	return out, nil
}

// --- network volumes -------------------------------------------------------

// Storage rates, per GiB per month.
//
// From RunPod's published pricing rather than from an invoice, which is the
// weaker of the two and said so here: the hourly GPU rate in this file comes
// from the live catalogue, and MinBilling was corrected once already because a
// plausible assumption about a provider turned out to be wrong. There is a
// GET /v2/billing/network-volumes endpoint that reports what was actually
// charged, and when a store has existed for a month these become a comparison
// rather than a belief.
const (
	standardRate        = node.StorePrice(70_000)  // $0.07/GiB/month below 1 TiB
	standardBulkRate    = node.StorePrice(50_000)  // $0.05/GiB/month at and above 1 TiB
	highPerformanceRate = node.StorePrice(140_000) // $0.14/GiB/month, any size

	// bulkFromGiB is where the cheaper standard tier starts. RunPod writes
	// the boundary as "1 TB"; this is the GiB figure the size field uses.
	bulkFromGiB = 1024
)

// volumePrice is what this volume costs per GiB per month.
//
// Derived from the volume's own reported type and size rather than from what
// ember asked for, so a volume created elsewhere -- or one whose tier RunPod
// changed under it -- is priced as it actually is.
func volumePrice(v networkVolume) node.StorePrice {
	if strings.EqualFold(v.Type, "HIGH_PERFORMANCE") {
		return highPerformanceRate
	}
	if v.Size >= bulkFromGiB {
		return standardBulkRate
	}
	return standardRate
}

// mapVolume turns one API volume into a store, or says why it cannot.
//
// A volume ember did not create is not an error and not a store: it is
// somebody else's, and `ember store rm` acts on what this returns.
func mapVolume(v networkVolume) (node.Store, bool, error) {
	name, err := provider.ParseStoreLabel(v.Name)
	if err != nil {
		return node.Store{}, false, nil //nolint:nilerr // not ours is not a failure
	}
	if v.ID == "" {
		return node.Store{}, false, fmt.Errorf("volume %q has no id", v.Name)
	}
	if v.Size < 0 {
		return node.Store{}, false, fmt.Errorf("volume %s reports a negative size (%d)", v.Name, v.Size)
	}
	return node.Store{
		Handle:  node.StoreHandle{Provider: Name, ID: node.StoreID(v.ID)},
		Name:    name,
		SizeGiB: v.Size,
		Region:  v.DataCenter,
		Price:   volumePrice(v),
	}, true, nil
}

// regionOffers narrows the catalogue to one datacentre.
//
// This is what a store costs you in flexibility: a volume lives in one site
// and a pod that mounts it must be placed there, so the whole catalogue is
// re-read through that site's own availability.
//
// It is not the same as filtering mapOffers' output by Region. mapOffers
// reports bestRegion(g) -- the single site with the most capacity -- so an
// offer available in EU-RO-1 but best in US-IL-1 would be labelled US-IL-1
// and then rejected by a filter looking for EU-RO-1. The card was rentable
// where the store is and ember would have said "no capacity", which is the
// same shape of bug as the one hasCapacity exists to prevent.
func regionOffers(gpus []gpuType, cloud, region string) []node.Offer {
	if region == "" {
		return mapOffers(gpus, cloud)
	}
	var kept []gpuType
	for _, g := range gpus {
		for _, dc := range g.DataCenters {
			if dc.ID != region {
				continue
			}
			if rank(dc.Availability) <= 0 {
				break
			}
			// Rewritten so the rest of the mapping reads this site's
			// availability rather than the global summary.
			g.Availability = dc.Availability
			g.DataCenters = []dataCentre{dc}
			kept = append(kept, g)
			break
		}
	}
	return mapOffers(kept, cloud)
}

// defaultVCPU is how many cores a CPU offer asks for when nothing says.
//
// Two, which is every family's minimum. The job these are rented for is
// downloading a file onto a volume: it is bound by the network and the disk,
// and paying for cores that wait on them is paying for nothing.
const defaultVCPU = 2

// mapCPUOffers turns the CPU catalogue into offers.
//
// Priced per machine rather than per core, because that is what a budget
// ceiling compares against and what every other offer in this program means
// by Price.
func mapCPUOffers(cpus []cpuType, cloud, region string, vcpu int) []node.Offer {
	if vcpu <= 0 {
		vcpu = defaultVCPU
	}
	// From the tier and not from the price, which is the one place these two
	// disagree: the CPU catalogue quotes securePerVcpu and nothing else, so
	// a community-configured run is quoted a secure rate -- but Create still
	// sends Cloud: p.cloud(), so the machine it gets is a community one and
	// can be reclaimed. Reading the reclaim answer off the price would
	// therefore promise ReclaimNever about a pod that is not.
	reclaim := reclaimOf(cloud)
	var out []node.Offer
	for _, c := range cpus {
		avail := c.Availability
		if region != "" {
			avail = ""
			for _, dc := range c.DataCenters {
				if dc.ID == region {
					avail = dc.Availability
					break
				}
			}
		}
		if rank(avail) <= 0 {
			continue
		}
		per := c.Price.SecurePerVcpu
		if per <= 0 {
			continue
		}
		cores := vcpu
		if c.Vcpu.Min > 0 && cores < c.Vcpu.Min {
			cores = c.Vcpu.Min
		}
		if c.Vcpu.Max > 0 && cores > c.Vcpu.Max {
			continue
		}
		out = append(out, node.Offer{
			Provider: Name,
			Token:    node.OfferToken(c.ID),
			// No GPU and no VRAM: IsCPU is what tells the kinds apart, and
			// filling either in would make a CPU box match a filter that
			// wanted a card.
			GPU:      node.GPUModel(c.ID),
			GPUCount: 0,
			VCPU:     cores,
			Price:    node.RateFromUSD(per * float64(cores)),
			Region:   regionOrBest(c, region),
			Reclaim:  reclaim,
			Label:    fmt.Sprintf("%s, %d vCPU, %.0f GB RAM per core (%s)", c.Name, cores, c.RAMPerVCPU, availabilityOrUnknown(avail)),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Price < out[j].Price })
	return out
}

func regionOrBest(c cpuType, want string) string {
	if want != "" {
		return want
	}
	best, bestRank := "", -1
	for _, dc := range c.DataCenters {
		if r := rank(dc.Availability); r > bestRank {
			best, bestRank = dc.ID, r
		}
	}
	return best
}

func availabilityOrUnknown(a string) string {
	if a == "" {
		return "unknown"
	}
	return a
}

// --- availability history --------------------------------------------------

// gpuSamples turns one catalogue read into rows for the series.
//
// ONE ROW PER SITE, NEVER ONE PER CARD. mapOffers above collapses a card's
// datacentres into a single offer at bestRegion(g), which is the right answer
// for renting and the wrong one to keep: the per-site rows it throws away are
// the entire measurement. The live catalogue reported an RTX 4090 as NONE
// overall while its US-IL-1 entry read LOW, so a file holding only the summary
// would record "unavailable" for a card that was rentable all along -- data
// that looks like an answer, which is worse than none.
//
// Nothing is filtered. mapOffers drops a tier that does not sell the card, a
// zero price and a card with no capacity anywhere, because an offer ember
// cannot rent is noise. Here each of those is the observation: "this card was
// unavailable everywhere at this hour" is what answers whether a card worth
// buying is one that is persistently unrentable.
//
// Pure and total, like everything else in this file: the clock arrives as an
// argument so the row that lands in a day file is decided by the caller that
// recorded it.
func gpuSamples(gpus []gpuType, now time.Time) []catalogue.Sample {
	out := make([]catalogue.Sample, 0, len(gpus))
	for _, g := range gpus {
		base := catalogue.Sample{
			ObservedAt: now,
			Provider:   Name,
			Kind:       catalogue.KindGPU,
			ID:         g.ID,
			Name:       g.Name,
			Overall:    catalogue.Level(g.Availability),
			Secure: catalogue.Tier{
				Offered:  g.Secure,
				Price:    node.RateFromUSD(g.Price.Secure),
				MaxCount: g.MaxCount.Secure,
			},
			Community: catalogue.Tier{
				Offered:  g.Community,
				Price:    node.RateFromUSD(g.Price.Community),
				MaxCount: g.MaxCount.Community,
			},
			MemoryGiB: g.Memory,
		}
		if len(g.DataCenters) == 0 {
			// A type RunPod named no site for. The captured catalogue has
			// three of these and every one reads NONE overall, which is a
			// correlation and not a promise -- so the row is kept rather
			// than dropped, because a card that vanishes from the series is
			// indistinguishable from a card that was never offered, and the
			// first is the answer to "is this card persistently
			// unrentable".
			//
			// Availability is left unreported rather than filled in from
			// Overall: a summary written into the site column is exactly the
			// row this function refuses to produce, and catalogue.Sample.Level
			// is what reads the summary back for a row that has no site.
			out = append(out, base)
			continue
		}
		for _, dc := range g.DataCenters {
			s := base
			s.Datacentre = dc.ID
			s.Availability = catalogue.Level(dc.Availability)
			out = append(out, s)
		}
	}
	return out
}

// cpuSamples is the same for the CPU catalogue.
//
// Collected because the question it answers is about the store too: `ember
// store sync` rents the cheapest CPU machine it can find to download weights
// onto a volume, and that volume is in one datacentre -- so "is a $0.06/hour
// box reliably available in the site holding the weights" has the same shape
// as the GPU question and the same absence of any history to answer it from.
//
// The price is per vCPU per hour, which is what the endpoint quotes.
// mapCPUOffers multiplies it by a core count to get a per-machine rate
// because that is what a budget compares against; the core count is a
// property of a run and not of the catalogue, so it is not applied to a row
// that will be read back in a year. catalogue.Kind is what says which of the
// two a price is.
func cpuSamples(cpus []cpuType, now time.Time) []catalogue.Sample {
	out := make([]catalogue.Sample, 0, len(cpus))
	for _, c := range cpus {
		base := catalogue.Sample{
			ObservedAt: now,
			Provider:   Name,
			Kind:       catalogue.KindCPU,
			ID:         c.ID,
			Name:       c.Name,
			Overall:    catalogue.Level(c.Availability),
			// There is no community price for a CPU family: the endpoint
			// quotes securePerVcpu and serverlessPerVcpu, and serverless is
			// not a pod. So the community tier is not offered rather than
			// offered at zero, which is the distinction mapOffers makes
			// between "declined to quote" and "free".
			Secure: catalogue.Tier{Offered: true, Price: node.RateFromUSD(c.Price.SecurePerVcpu)},
		}
		if len(c.DataCenters) == 0 {
			out = append(out, base)
			continue
		}
		for _, dc := range c.DataCenters {
			s := base
			s.Datacentre = dc.ID
			s.Availability = catalogue.Level(dc.Availability)
			out = append(out, s)
		}
	}
	return out
}
