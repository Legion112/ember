// Package runpod rents pods from RunPod.
//
// Split three ways on purpose. types.go is wire structs with no logic. map.go
// is pure total functions from those structs to ember's domain, tested at 100%
// from committed fixtures. runpod.go is thin: build a path, call httpx, map
// the result. A provider client that mixes the three is one where a status
// string nobody anticipated is handled in the middle of an HTTP call.
//
// The shapes here follow RunPod's published OpenAPI schema for
// https://api.runpod.io/v2 (the v2 API; v1 at rest.runpod.io used flat
// gpuTypeIds/imageName fields and a different pod body).
package runpod

// BaseURL is the RunPod API server root.
//
// The "/v2" belongs to the paths, not here. RunPod's own migration guide
// describes the base URL as ".../v2", and writing it that way produced
// https://api.runpod.io/v2/v2/catalog/gpus on the first live call: the
// OpenAPI document's own paths already begin with /v2, and those are what
// this package's request paths are copied from. Keeping the base at the
// server root means a path here can be compared against the spec character
// for character, which is the version that stays right.
const BaseURL = "https://api.runpod.io"

// createPodRequest is POST /v2/pods.
//
// Exactly one of Gpu or Cpu may be set; ember always sets Gpu, since a CPU pod
// is not what anybody rents this for.
type createPodRequest struct {
	Name  string `json:"name"`
	Image string `json:"image"`

	// Gpu and Cpu are mutually exclusive, and the API enforces it: exactly
	// one must be set. Pointers so the unused one is omitted entirely --
	// sending an empty `gpu` object alongside `cpu` is two configurations,
	// which is refused.
	Gpu *gpuConfig `json:"gpu,omitempty"`
	Cpu *cpuConfig `json:"cpu,omitempty"`

	// Env carries everything the node needs to configure itself, including
	// the minted host key. It goes in the JSON body and never in Args or
	// Entrypoint: /proc/<pid>/cmdline is world-readable, and on a rented
	// machine the other tenants of that host are strangers.
	Env map[string]string `json:"env,omitempty"`

	// Ports is RunPod's "port/protocol" spelling, for example "22/tcp".
	//
	// This is the single most dangerous field in the API ember touches. A
	// port declared "/http" is published at
	// https://{id}-{port}.proxy.runpod.net with no authentication of any
	// kind. Nothing in ember may construct an entry here except through
	// exposure.PortRequest, which cannot spell a public non-SSH port.
	Ports []string `json:"ports,omitempty"`

	// Disk is the container disk in GiB.
	Disk int `json:"disk,omitempty"`

	// Cloud is SECURE or COMMUNITY. Empty means RunPod's default, SECURE.
	Cloud string `json:"cloud,omitempty"`

	// DataCenterIds restricts placement. Empty lets RunPod choose.
	DataCenterIds []string `json:"dataCenterIds,omitempty"`

	// Mounts is the persistent storage attached to this pod. Nil sends no
	// field at all, which is what a pod with no store must look like.
	Mounts *mounts `json:"mounts,omitempty"`

	// StartSsh asks RunPod to run its own sshd.
	//
	// ember sets this false and runs its own, because RunPod's sshd uses a
	// host key ember did not mint and cannot predict, which would force
	// trust-on-first-use against a brand-new IP -- the one case TOFU cannot
	// defend.
	StartSsh bool `json:"startSsh"`

	// AllowedCudaVersions restricts placement to hosts whose CUDA runtime is
	// in this list. Empty sends no field, which RunPod documents as "any
	// CUDA version is acceptable".
	//
	// "Any" is what ember asked for until this existed, and it is the wrong
	// default for a tool that pins its image by digest: the image carries a
	// CUDA runtime, and a host older than it cannot run the image at all.
	// Nothing about that shows up while filtering offers -- it shows up
	// after the pod is created and billing, as a container that will not
	// start or a CUDA that will not initialize.
	//
	// NOT YET MEASURED. The field and its enum are in RunPod's published
	// schema, but that it actually filters placement has not been confirmed
	// against the live API, so Spec.MinCUDA is empty by default and this
	// stays absent until an operator asks for it. See AGENTS.md on a
	// provider capability being measured rather than assumed.
	AllowedCudaVersions []string `json:"allowedCudaVersions,omitempty"`

	// TemplateId is unused by ember: a template carries ports and env from
	// somebody else's defaults, and a template that gains an http port
	// upstream would reintroduce public exposure with no change here. The
	// field is named so its absence is a decision rather than an oversight.
	TemplateId string `json:"templateId,omitempty"`
}

type gpuConfig struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

// cpuConfig asks for a machine with no GPU.
//
// vcpuCount is required and bounded by the catalogue's own min and max for
// that family, which is why the offer carries it rather than this picking a
// number.
type cpuConfig struct {
	ID        string `json:"id"`
	VcpuCount int    `json:"vcpuCount"`
}

// pod is the Pod schema, trimmed to what ember reads.
type pod struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Image  string `json:"image"`

	Gpu          *podGpu `json:"gpu,omitempty"`
	Cloud        string  `json:"cloud,omitempty"`
	DataCenterID *string `json:"dataCenterId,omitempty"`
	// Ssh and Runtime are both null when listing pods, and RunPod documents
	// that a single fetch by id is the only way to get the live mapping.
	// That is why Inspect fetches one pod and List does not pretend to
	// produce endpoints: a List entry carries a handle and a state, which is
	// all reconcile needs, and WaitReady goes through Inspect.
	Ssh     *podSsh     `json:"ssh,omitempty"`
	Runtime *podRuntime `json:"runtime,omitempty"`

	// Cost is the hourly rate RunPod says it is billing for this pod. It is
	// read back rather than assumed from the catalogue price: the catalogue
	// is a list price and this is the bill.
	Cost float64 `json:"cost"`

	CreatedAt string `json:"createdAt"`
	StartedAt string `json:"startedAt,omitempty"`
}

type podGpu struct {
	ID    string `json:"id"`
	Count int    `json:"count"`
}

type podSsh struct {
	// Direct is a real TCP endpoint on the machine. ember uses this.
	Direct *sshEndpoint `json:"direct,omitempty"`

	// Proxy is RunPod's SSH gateway, and ember cannot use it for two
	// independent reasons. RunPod documents it as carrying an interactive
	// shell only -- no port forwarding -- which is the entire mechanism
	// ember is built on. And the proxy terminates and re-establishes the
	// connection, so the host key a client verifies there is RunPod's rather
	// than the one ember minted and injected.
	Proxy *sshEndpoint `json:"proxy,omitempty"`
}

type sshEndpoint struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Username string `json:"username"`
}

type podRuntime struct {
	Uptime int `json:"uptime"`

	// Ports is what RunPod says it published. It is the input to the exposure
	// assertion, and the reason that assertion can catch a provider doing
	// something ember never asked for.
	Ports []runtimePort `json:"ports,omitempty"`
}

type runtimePort struct {
	Private int     `json:"private"`
	Public  *int    `json:"public,omitempty"`
	Type    string  `json:"type"`
	IP      *string `json:"ip,omitempty"`
}

// listPodsResponse is GET /v2/pods.
type listPodsResponse struct {
	Pods []pod `json:"pods"`
}

// gpuType is one entry of GET /v2/catalog/gpus.
type gpuType struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Manufacturer string   `json:"manufacturer"`
	Memory       int      `json:"memory"`
	Secure       bool     `json:"secure"`
	Community    bool     `json:"community"`
	Price        gpuPrice `json:"price"`
	MaxCount     gpuCount `json:"maxCount"`
	// Availability is the overall level. It can read NONE while an
	// individual data centre still has capacity -- the live catalogue showed
	// exactly that for an RTX 4090 -- so it is a summary and not a verdict.
	Availability string `json:"availability"`

	// DataCenters is per-site availability. Objects, not strings: the first
	// fixture here guessed strings from the schema's "array" and the live
	// call refused to decode.
	DataCenters []dataCentre `json:"dataCenters,omitempty"`
}

// dataCentre is one site's availability for a GPU type.
type dataCentre struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Availability string `json:"availability"`
}

// gpuPrice is the list price per hour for a single GPU, per cloud tier.
type gpuPrice struct {
	Secure    float64 `json:"secure"`
	Community float64 `json:"community"`
}

type gpuCount struct {
	Secure    int `json:"secure"`
	Community int `json:"community"`
}

type listGpuTypesResponse struct {
	Gpus []gpuType `json:"gpus"`
}

// --- network volumes -------------------------------------------------------
//
// Every field name and bound below was taken from the live API before any of
// this was written, not from the documentation, which is wrong in three ways:
// it describes only the other API (rest.runpod.io/v1), spells the datacentre
// field `dataCenterId` where this one wants `dataCenter`, and gives the
// minimum size as 0 where the server answers "minimum: got 1, want 10". See
// testdata/README.md for the transcript.

// VolumePath is where ember's stores are reached on the v2 API.
//
// Hyphenated. `/v2/networkvolumes` is a 404 and `/v2/network-volumes` is the
// live path, which is the sort of thing a client written from the docs
// discovers on its first create, after the operator has decided to spend
// money.
const VolumePath = "/v2/network-volumes"

// volumeTypeStandard is the tier ember creates.
//
// RunPod also sells HIGH_PERFORMANCE at twice the price for roughly three
// times the throughput. ember does not offer the choice yet because nothing
// has measured whether it pays for the workloads this is for, and a knob
// whose right setting nobody knows is a knob that gets set wrong.
const volumeTypeStandard = "STANDARD"

// networkVolume is one volume as the API reports it.
type networkVolume struct {
	ID   string `json:"id"`
	Name string `json:"name"`

	// Size is in GiB and is what gets billed, used or not.
	Size int `json:"size"`

	// DataCenter is the site, and therefore the only site a pod that mounts
	// this volume can be placed in.
	DataCenter string `json:"dataCenter"`

	// Type is STANDARD or HIGH_PERFORMANCE, set at creation and immutable.
	// It decides the rate, so it is read rather than assumed.
	Type string `json:"type,omitempty"`
}

// createVolumeRequest is POST /v2/network-volumes.
type createVolumeRequest struct {
	Name       string `json:"name"`
	Size       int    `json:"size"`
	DataCenter string `json:"dataCenter"`
	Type       string `json:"type,omitempty"`
}

// listVolumesResponse is GET /v2/network-volumes.
//
// Paginated, which the other API's bare array is not. A client that read only
// the first page would under-report stores, and under-reporting a resource
// that bills monthly with nothing attached is precisely the leak ember exists
// to make visible.
type listVolumesResponse struct {
	NetworkVolumes []networkVolume `json:"networkVolumes"`
	Pagination     pagination      `json:"pagination"`
}

type pagination struct {
	NextCursor  *string `json:"nextCursor"`
	HasNextPage bool    `json:"hasNextPage"`
}

// mounts is the storage attached to a pod.
//
// Network, not a top-level networkVolumeId: that spelling belongs to the
// other API. The array holds at most one entry today and is an array for
// RunPod's own forward compatibility.
//
// Persistent is deliberately never set. The API refuses a pod that asks for
// both, because it "drops a persistent disk whenever a volume is attached" --
// so a field ember filled in by habit would turn a store into a 400.
type mounts struct {
	Network []networkMount `json:"network,omitempty"`
}

// networkMount attaches one volume at one path.
//
// Path has no default: the API requires it. ember's choice of /models is
// therefore not an override of RunPod's /workspace but the only way to answer
// the field at all -- which is fortunate, because /workspace is where ember
// writes outputs and a store mounted there would bill for every render for as
// long as the store exists.
type networkMount struct {
	VolumeID string `json:"volumeId"`
	Path     string `json:"path"`
}

// cpuType is one entry of GET /v2/catalog/cpus.
//
// Separate from gpuType rather than shared, because almost nothing lines up:
// there is no VRAM, the price is per vCPU rather than per machine, and the
// core count is a range the caller picks from instead of a fixed number.
type cpuType struct {
	ID           string       `json:"id"`
	Name         string       `json:"name"`
	Group        string       `json:"group"`
	RAMPerVCPU   float64      `json:"ramGbPerVcpu"`
	Vcpu         cpuRange     `json:"vcpu"`
	Price        cpuPrice     `json:"price"`
	Availability string       `json:"availability"`
	DataCenters  []dataCentre `json:"dataCenters,omitempty"`
}

type cpuRange struct {
	Min int `json:"min"`
	Max int `json:"max"`
}

// cpuPrice is per vCPU per hour, not per machine -- so the offer's price is
// this multiplied by the cores asked for.
type cpuPrice struct {
	SecurePerVcpu     float64 `json:"securePerVcpu"`
	ServerlessPerVcpu float64 `json:"serverlessPerVcpu"`
}

type listCpuTypesResponse struct {
	Cpus []cpuType `json:"cpus"`
}

// cudaVersions is RunPod's accepted enum for allowedCudaVersions, newest
// first, copied from its published OpenAPI schema.
//
// Written down rather than derived, because it is somebody else's closed set:
// sending a version not in it is a rejected create, and a version RunPod adds
// later does ember no good until it is added here. Newest first so the
// rendered list reads the way the schema does.
var cudaVersions = []string{
	"13.0", "12.9", "12.8", "12.7", "12.6",
	"12.5", "12.4", "12.3", "12.2", "12.1", "12.0", "11.8",
}
