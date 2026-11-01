// Package sdserver drives a resident sd-server over the SSH connection ember
// already has to the node.
//
// WHY EMBER KNOWS THIS API AT ALL. Every other workload in ember is opaque
// argv, deliberately: `cmd` and `cmds` are handed to the remote shell and
// ember does not know what a flag means. That position does not survive
// contact with progress streaming. A program that refuses to understand its
// payload cannot report that job two is fourteen steps into its first pass,
// and a `curl` in a config file is not an integration -- it is a way of
// pretending the knowledge lives somewhere else while the operator maintains
// it by hand. So this package is the one place in ember that knows what a
// sampler is.
//
// WHAT IT BUYS. `cmds` puts several results on one rental, which pays the
// per-rental costs once: provisioning, the image pull, reading and verifying
// the weights. What it cannot pay once is the model load, because each step is
// a fresh sd-cli process that reads the weights into VRAM again. On a rented
// A100 that is about twelve seconds against roughly a hundred of sampling for
// a 1024x1024 image -- call it eight percent of a ten-image batch. It is worth
// much more the other way round: on short generations, where the reload is
// most of the step, which is exactly the shape prompt iteration has.
//
// THE WIRE TYPES ARE NOT PROVEN. They were written from examples/server/api.md
// at stable-diffusion.cpp 77b72a2, which is the revision both workload images
// pin. A shape read from documentation is a guess about a server's behaviour;
// see testdata/README.md for which fixtures were captured from a running
// sd-server and which were not. The repository has been here before with
// RunPod, where the published schema named a field the API ember actually
// calls does not accept.
package sdserver

import "encoding/json"

// The native async API. The WebUI-compatible /sdapi/v1 and /v1 routes exist on
// the same server and are not used: they have no job ids and no preview
// frames, which are the two things this package needs.
const (
	pathCapabilities = "/sdcpp/v1/capabilities"
	pathModelStatus  = "/sdcpp/v1/models/status"
	pathImageGen     = "/sdcpp/v1/img_gen"
	pathJobs         = "/sdcpp/v1/jobs/"
)

// wireAccepted is the 202 from a submission.
//
// poll_url is read and then ignored: it is a path on the same server, and
// building the job path from the id keeps one spelling of it. A server that
// started answering with an absolute URL elsewhere would otherwise send
// ember's next request somewhere nothing checked.
type wireAccepted struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

// wireJob is a job as GET /sdcpp/v1/jobs/{id} reports it.
//
// Preview and Result are pointers because their absence is meaningful rather
// than zero: no preview means previews are off or none has been produced yet,
// and no result means the job has not finished.
type wireJob struct {
	ID     string `json:"id"`
	Status string `json:"status"`

	// QueuePosition is reported as 0 for everything that is not queued, so it
	// is only meaningful alongside Status == queued. Upstream does not
	// document what the number counts; it is printed and never reasoned about.
	QueuePosition int `json:"queue_position"`

	Preview *wirePreview `json:"preview"`
	Result  *wireResult  `json:"result"`

	// Error carries two different JSON types depending on which endpoint
	// produced it -- a string for model management, an object for a failed
	// job. json.RawMessage is the only honest Go type for a field like that;
	// see decodeAPIError.
	Error json.RawMessage `json:"error"`
}

type wirePreview struct {
	// Pass is why this is reported and not turned into a percentage. Step and
	// TotalSteps describe the current sampling pass, not the job: passes
	// restart for each batch item, each high/low-noise stage and each hires
	// pass, so a bar built from them runs to the end and then starts again.
	Pass       int    `json:"pass"`
	Step       int    `json:"step"`
	TotalSteps int    `json:"total_steps"`
	B64JSON    string `json:"b64_json"`
}

type wireResult struct {
	Images []wireImage `json:"images"`
}

type wireImage struct {
	Index   int    `json:"index"`
	B64JSON string `json:"b64_json"`
}

type wireModelStatus struct {
	State string  `json:"state"`
	Error *string `json:"error"`

	// LoadElapsedSeconds is the number this whole package exists to stop
	// paying repeatedly, so it is reported to the operator rather than
	// inferred from ember's own clock.
	LoadElapsedSeconds float64 `json:"load_elapsed_seconds"`
}

// wireCapabilities is trimmed to what ember reads.
//
// The full response also carries defaults, output formats, features, loras,
// upscalers and three by-mode variants of most of those. Copying them in would
// be a second schema to keep current for no behaviour; what ember needs is
// enough to refuse a job before a GPU has been asked to do anything with it.
type wireCapabilities struct {
	// Model is an object and not a string, which is the first thing a real
	// server corrected about these types. api.md lists `model` among the
	// top-level fields without saying what it holds; it is
	// {"name","path","stem"}. The doc-derived fixture said "flux2-dev" and
	// decoded happily, so the mistake surfaced on first contact with a
	// running sd-server -- as "cannot unmarshal object into Go struct field
	// wireCapabilities.model of type string", after a node was up.
	Model        wireModel  `json:"model"`
	Samplers     []string   `json:"samplers"`
	Schedulers   []string   `json:"schedulers"`
	PreviewModes []string   `json:"preview_modes"`
	Limits       wireLimits `json:"limits"`
}

// wireModel is what the server says is loaded.
//
// Trimmed to the name. The path is the node's own filesystem path, which the
// config already knows because it put it there, and the stem is the name
// without its extension.
type wireModel struct {
	Name string `json:"name"`
}

type wireLimits struct {
	MinWidth      int `json:"min_width"`
	MaxWidth      int `json:"max_width"`
	MinHeight     int `json:"min_height"`
	MaxHeight     int `json:"max_height"`
	MaxBatchCount int `json:"max_batch_count"`
	MaxQueueSize  int `json:"max_queue_size"`
}

// wireErrorObject is the object form of `error`, used by a failed job.
type wireErrorObject struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// wireErrorEnvelope is the model-management form: a string `error` beside a
// separate `code`.
type wireErrorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
