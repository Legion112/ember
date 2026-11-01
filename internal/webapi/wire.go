package webapi

import (
	"fmt"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/reconcile"
	"github.com/Legion112/ember/internal/sdserver"
)

// THIS FILE IS THE BROWSER'S WHOLE VIEW OF EMBER, and nothing in it is a
// domain type.
//
// WHY NOT JUST MARSHAL THE DOMAIN. json.Marshal of node.Node or
// statestore.Record would make the browser's view a decision taken by whoever
// adds the next field to those structs -- and the next field could be a
// token. A hand-written projection means adding a credential somewhere cannot
// publish it by default. node.Secret redacting itself is a second line of
// defence, not the first.
//
// It is also the shape internal/sdserver/map.go and
// internal/provider/runpod/map.go already use for the other direction, so a
// reader who has followed one mapping edge here reads this one.

// Money crosses the wire TWICE, and both halves are load-bearing.
//
// The integer is what a UI sorts, compares and thresholds on; the string is
// what it displays. node.MicroUSD.String is "$%.6g", which nobody reproduces
// by accident -- and the one place in this repository that formats money is
// the money type. Letting the frontend format it would create a second
// renderer that disagrees, in a language where the first one's tests cannot
// reach it.
type money struct {
	Micro   int64  `json:"micro_usd"`
	Display string `json:"display"`
}

func usd(m node.MicroUSD) money  { return money{Micro: int64(m), Display: m.String()} }
func rate(r node.Rate) money     { return money{Micro: int64(r), Display: r.String()} }
func dur(d time.Duration) string { return d.String() }

// configView is what a UI needs to know before it can offer anything.
type configView struct {
	// Providers are the ones this process can reach, which is not every
	// provider ember knows: one with no credential is deliberately absent, so
	// a UI showing it would offer a button that cannot work.
	Providers []providerView `json:"providers"`

	// Ceilings are the operator's, from their config file. A request may
	// narrow them and never widen them, so a UI showing a slider has to know
	// where it stops.
	Ceilings ceilings `json:"ceilings"`

	// AllowPaid says whether this process was started with --allow-paid. A UI
	// that did not know would offer a card it cannot rent and surface the
	// refusal as a failure.
	AllowPaid bool `json:"allow_paid"`

	// Servable says whether the config has an sd.serve block. Without one
	// there is no generation server to start, so a queued request has nowhere
	// to run -- and saying so at load is better than saying it after a card
	// has been rented.
	Servable bool   `json:"servable"`
	Store    string `json:"store,omitempty"`
	Mount    string `json:"mount,omitempty"`

	// Preview and PreviewInterval are the config's defaults, which a UI uses
	// as the starting position of its own controls.
	Preview         string `json:"preview"`
	PreviewInterval int    `json:"preview_interval"`

	// Caps is what the generation server reported, once something has asked
	// one. Absent before this process's first rental, which is why it is a
	// pointer: "nothing has asked yet" and "the server has no samplers" are
	// different answers, and a UI treating the first as the second would grey
	// out every control.
	Caps *capsView `json:"caps,omitempty"`
}

type providerView struct {
	Name string `json:"name"`
	Free bool   `json:"free"`

	// Stores says whether this provider has persistent storage, so a UI does
	// not offer a store where there is none.
	Stores bool `json:"stores"`
}

type ceilings struct {
	TTL       string `json:"ttl"`
	MaxHourly money  `json:"max_hourly"`
	MaxRun    money  `json:"max_run"`
}

type capsView struct {
	Model        string    `json:"model"`
	Samplers     []string  `json:"samplers"`
	Schedulers   []string  `json:"schedulers"`
	PreviewModes []string  `json:"preview_modes"`
	Limits       limitView `json:"limits"`
}

type limitView struct {
	MinWidth      int `json:"min_width"`
	MaxWidth      int `json:"max_width"`
	MinHeight     int `json:"min_height"`
	MaxHeight     int `json:"max_height"`
	MaxBatchCount int `json:"max_batch_count"`
}

func viewConfig(cfg config.Config, reg *provider.Registry, allowPaid bool, caps *sdserver.Capabilities) configView {
	hourly, run, _ := cfg.Budgets()

	v := configView{
		// An empty list and never null, for the reason ws.go gives: a client
		// whose field is a list gets a type error from `null` and drops the
		// whole response.
		Providers:       []providerView{},
		Ceilings:        ceilings{TTL: dur(cfg.TTL), MaxHourly: rate(hourly), MaxRun: usd(run)},
		AllowPaid:       allowPaid,
		Servable:        len(cfg.SD.Serve) > 0,
		Store:           cfg.Store.Name,
		Preview:         cfg.SD.Preview,
		PreviewInterval: cfg.SD.PreviewInterval,
	}
	if m, err := cfg.StoreMount(); err == nil && cfg.HasStore() {
		v.Mount = m.String()
	}
	for _, p := range reg.All() {
		c := p.Caps()
		v.Providers = append(v.Providers, providerView{
			Name: string(p.Name()), Free: c.Free, Stores: c.Stores,
		})
	}
	if caps != nil {
		v.Caps = &capsView{
			Model: caps.Model, Samplers: caps.Samplers, Schedulers: caps.Schedulers,
			PreviewModes: caps.PreviewModes,
			Limits: limitView{
				MinWidth: caps.Limits.MinWidth, MaxWidth: caps.Limits.MaxWidth,
				MinHeight: caps.Limits.MinHeight, MaxHeight: caps.Limits.MaxHeight,
				MaxBatchCount: caps.Limits.MaxBatchCount,
			},
		}
	}
	return v
}

// offerView is one rentable thing.
type offerView struct {
	Provider string `json:"provider"`
	Token    string `json:"token"`
	GPU      string `json:"gpu"`
	Count    int    `json:"gpu_count"`
	VRAMGiB  int    `json:"vram_gib"`
	Price    money  `json:"price"`
	Region   string `json:"region,omitempty"`

	// Interruptible means the provider can take it back mid-run, which is a
	// property an operator must see before choosing it rather than discover.
	Interruptible bool `json:"interruptible"`

	// VCPU is non-zero only for a machine with no card.
	VCPU  int    `json:"vcpu,omitempty"`
	Label string `json:"label,omitempty"`
}

// offersView keeps each provider's failure beside its answer.
//
// "I could not ask RunPod" must never render as "RunPod has nothing", which
// is the same rule `ember ls` exits 2 for. A flat list of offers cannot say
// it, so this is a list of listings.
type offersView struct {
	Listings []listingView `json:"listings"`
}

type listingView struct {
	Provider string      `json:"provider"`
	Offers   []offerView `json:"offers"`
	Error    string      `json:"error,omitempty"`
}

func viewOffers(ls []app.OfferListing) offersView {
	out := offersView{Listings: make([]listingView, 0, len(ls))}
	for _, l := range ls {
		lv := listingView{Provider: string(l.Provider), Offers: []offerView{}}
		if l.Err != nil {
			lv.Error = l.Err.Error()
		}
		for _, o := range l.Offers {
			lv.Offers = append(lv.Offers, offerView{
				Provider: string(o.Provider), Token: string(o.Token), GPU: string(o.GPU),
				Count: o.GPUCount, VRAMGiB: int(o.VRAM / node.MiBPerGiB),
				Price: rate(o.Price), Region: o.Region,
				Interruptible: o.Interruptible, VCPU: o.VCPU, Label: o.Label,
			})
		}
		out.Listings = append(out.Listings, lv)
	}
	return out
}

// newRequest is what a browser POSTs.
//
// EVERY FIELD HERE IS SOMETHING A REQUEST MAY CHOOSE, and the absences are
// the design. There is no image, no images map, no store, no mount, no ports,
// no serve argv, no gpu probe, no assets, no verify policy -- those stay in
// ember.yaml, which is what keeps every existing seal upstream of anything a
// request can influence: node.MountAt's refusal of /root, ValidateImage's
// refusal of :latest, sdserver.ServeCommand's refusal of a listen flag.
//
// There is also no `extra`. It reaches every field of the generation API ember
// does not type, including the ones naming file paths on the node, so a
// request that could set it could point a running sd-server at weights the
// config never mentioned. The seal is structural: the field does not exist
// here, and genstore.Job has none either.
type newRequest struct {
	Provider string `json:"provider"`
	GPU      string `json:"gpu,omitempty"`

	// TTL, MaxHourlyUSD and MaxRunUSD may only narrow the config's. A value
	// above the ceiling gets the ceiling rather than a refusal, because a UI
	// offering a slider should not have to know every limit before it can
	// offer anything.
	TTL string `json:"ttl,omitempty"`

	// Decimal strings and never JSON numbers. A JSON number is a float on
	// both sides of this wire, and ParseFloat("0.29")*1e6 is 289999 -- which
	// is the whole reason node.ParseUSD is hand-written.
	MaxHourlyUSD string `json:"max_hourly_usd,omitempty"`
	MaxRunUSD    string `json:"max_run_usd,omitempty"`

	Preview         string `json:"preview,omitempty"`
	PreviewInterval int    `json:"preview_interval,omitempty"`

	Jobs []newJob `json:"jobs"`
}

type newJob struct {
	Prompt         string  `json:"prompt"`
	NegativePrompt string  `json:"negative_prompt,omitempty"`
	Width          int     `json:"width,omitempty"`
	Height         int     `json:"height,omitempty"`
	Steps          int     `json:"steps,omitempty"`
	BatchCount     int     `json:"batch_count,omitempty"`
	Seed           int64   `json:"seed,omitempty"`
	Sampler        string  `json:"sampler,omitempty"`
	Scheduler      string  `json:"scheduler,omitempty"`
	CFG            float64 `json:"cfg,omitempty"`
	Guidance       float64 `json:"guidance,omitempty"`
	Output         string  `json:"output,omitempty"`
}

// toRequest turns a posted body into something the store can hold.
//
// It parses and refuses; it does not narrow. Narrowing happens in
// queue.Narrow, against the config this process was started with, because
// that is the last thing before a create call and the only place that knows
// the operator's own numbers.
func (n newRequest) toRequest() (genstore.Request, error) {
	r := genstore.Request{
		Provider:        n.Provider,
		GPU:             n.GPU,
		Preview:         n.Preview,
		PreviewInterval: n.PreviewInterval,
	}

	if n.TTL != "" {
		d, err := time.ParseDuration(n.TTL)
		if err != nil {
			return genstore.Request{}, fmt.Errorf("ttl %q: %w", n.TTL, err)
		}
		if d <= 0 {
			return genstore.Request{}, fmt.Errorf("ttl %q is not positive; a node nothing destroys on a deadline is a node that bills until somebody notices", n.TTL)
		}
		r.TTL = d
	}
	if n.MaxHourlyUSD != "" {
		v, err := node.ParseUSD(n.MaxHourlyUSD)
		if err != nil {
			return genstore.Request{}, fmt.Errorf("max_hourly_usd: %w", err)
		}
		r.MaxHourly = node.Rate(v)
	}
	if n.MaxRunUSD != "" {
		v, err := node.ParseUSD(n.MaxRunUSD)
		if err != nil {
			return genstore.Request{}, fmt.Errorf("max_run_usd: %w", err)
		}
		r.MaxRun = v
	}

	for _, j := range n.Jobs {
		r.Jobs = append(r.Jobs, genstore.Job{
			Prompt: j.Prompt, NegativePrompt: j.NegativePrompt,
			Width: j.Width, Height: j.Height, Steps: j.Steps,
			BatchCount: j.BatchCount, Seed: j.Seed,
			Sampler: j.Sampler, Scheduler: j.Scheduler,
			CFG: j.CFG, Guidance: j.Guidance, Output: j.Output,
		})
	}
	return r, nil
}

// requestView is one request as a UI reads it.
type requestView struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Created string `json:"created"`

	Provider string `json:"provider"`
	GPU      string `json:"gpu,omitempty"`
	Offer    string `json:"offer,omitempty"`

	// Node is the rental's handle, which is what `ember ls` prints -- so a UI
	// showing a cost and `ember ls` showing a node can be talked about as one
	// thing.
	Node string `json:"node,omitempty"`

	TTL     string `json:"ttl"`
	Started string `json:"started,omitempty"`
	Ended   string `json:"ended,omitempty"`

	Cost  money  `json:"cost"`
	Error string `json:"error,omitempty"`

	Preview         string     `json:"preview,omitempty"`
	PreviewInterval int        `json:"preview_interval,omitempty"`
	Jobs            []newJob   `json:"jobs"`
	Images          []imageRef `json:"images,omitempty"`
}

type imageRef struct {
	Job   int    `json:"job"`
	Index int    `json:"index"`
	Bytes int64  `json:"bytes"`
	Path  string `json:"path"`

	// Preview marks a progress frame rather than a result, so a gallery does
	// not open on a latent: a `proj` preview of a 64x64 generation is an 8x8
	// PNG of about 250 bytes, which is the latent and not a thumbnail.
	Preview bool `json:"preview,omitempty"`
}

func viewRequest(r genstore.Request, ims []genstore.Image) requestView {
	v := requestView{
		ID: string(r.ID), Status: string(r.Status),
		Created:  r.Created.UTC().Format(time.RFC3339),
		Provider: r.Provider, GPU: r.GPU, Offer: r.Offer, Node: r.Node,
		TTL: dur(r.TTL), Cost: usd(r.Cost), Error: r.Err,
		Preview: r.Preview, PreviewInterval: r.PreviewInterval,
		Jobs:   []newJob{},
		Images: []imageRef{},
	}
	if !r.Started.IsZero() {
		v.Started = r.Started.UTC().Format(time.RFC3339)
	}
	if !r.Ended.IsZero() {
		v.Ended = r.Ended.UTC().Format(time.RFC3339)
	}
	for _, j := range r.Jobs {
		v.Jobs = append(v.Jobs, newJob{
			Prompt: j.Prompt, NegativePrompt: j.NegativePrompt,
			Width: j.Width, Height: j.Height, Steps: j.Steps,
			BatchCount: j.BatchCount, Seed: j.Seed,
			Sampler: j.Sampler, Scheduler: j.Scheduler,
			CFG: j.CFG, Guidance: j.Guidance, Output: j.Output,
		})
	}
	for _, im := range ims {
		v.Images = append(v.Images, imageRef{
			Job: im.Job, Index: im.Index, Bytes: im.Bytes, Preview: im.Preview,
			// The URL a browser fetches, not the blob key. The key is this
			// process's business and could be an S3 object tomorrow.
			Path: fmt.Sprintf("/api/requests/%s/images/%d/%d%s",
				im.Req, im.Job, im.Index, previewSuffix(im.Preview)),
		})
	}
	return v
}

func previewSuffix(preview bool) string {
	if preview {
		return "?preview=1"
	}
	return ""
}

// nodesView is `ember ls`'s answer.
//
// internal/reconcile's own findings, projected -- never a second scan with its
// own rules. Two scanners is two answers to "what am I paying for", and the
// one that disagrees is the one that misses a node.
type nodesView struct {
	Findings []findingView `json:"findings"`
	Stores   []findingView `json:"stores"`

	// MonthlyStorage is the charge no deadline, no reaper and no Ctrl-C ever
	// ends, which is why it is reported on every listing and not only when
	// something is wrong.
	MonthlyStorage money `json:"monthly_storage"`

	// Attention is how many findings need a person. It is the number
	// `ember ls` exits 2 for.
	Attention int `json:"attention"`
}

type findingView struct {
	Kind     string `json:"kind"`
	Provider string `json:"provider,omitempty"`
	ID       string `json:"id,omitempty"`
	Reason   string `json:"reason"`
	Line     string `json:"line"`
	Accrued  *money `json:"accrued,omitempty"`
}

func viewNodes(sc app.Scanned) nodesView {
	v := nodesView{Findings: []findingView{}, Stores: []findingView{}}
	for _, f := range sc.Nodes {
		v.Findings = append(v.Findings, viewFinding(f))
	}
	for _, f := range sc.Stores {
		v.Stores = append(v.Stores, viewFinding(f))
	}
	v.MonthlyStorage = usd(reconcile.MonthlyTotal(sc.Stores))

	all := append(append([]reconcile.Finding(nil), sc.Nodes...), sc.Stores...)
	v.Attention = reconcile.Attention(all)
	return v
}

func viewFinding(f reconcile.Finding) findingView {
	v := findingView{
		Kind: string(f.Kind), Provider: string(f.Provider), Reason: f.Reason,
		// The rendered line as well as the parts, because that string is what
		// `ember ls` prints and an operator reading both should see the same
		// sentence in each.
		Line: f.String(),
	}
	switch {
	case f.Store != nil:
		v.ID = f.Store.Handle.String()
	case !f.Node.Handle.IsZero():
		v.ID = f.Node.Handle.String()
	case f.Record != nil:
		v.ID = string(f.Provider) + "/" + string(f.Record.RunID)
	}
	if f.Accrued != nil {
		m := usd(f.Accrued.Cost)
		v.Accrued = &m
	}
	return v
}
