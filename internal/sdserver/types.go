// Package sdserver drives a resident sd-server over the SSH connection ember
// already has to the node.
//
// WHY EMBER KNOWS THIS API AT ALL. Progress streaming is the reason: a
// program that refuses to understand its payload cannot report that job two
// is fourteen steps into its first pass, and a `curl` in a config file is not
// an integration -- it is a way of pretending the knowledge lives somewhere
// else while the operator maintains it by hand. So this package is the one
// place in ember that knows what a sampler is.
//
// WHAT IT BUYS. A batch on one rental pays the per-rental costs once:
// provisioning, the image pull, reading and verifying the weights. One
// resident server pays the model load once too, which a process per image
// cannot: it reads the weights into VRAM again every time. On a rented RTX
// PRO 6000 running FLUX.2 at 2048x2048 that read is 4.5s against 140.8s of
// sampling when the node keeps the weights in page cache, and 86s when it
// does not -- a difference ember cannot predict, which is the real argument
// for this shape rather than the mean. It is worth much more the other way
// round: on short generations the read is most of the job, which is exactly
// what prompt iteration looks like, and sd15 at 256x256 measured 0.44s per
// job against 1.91s.
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
// The full response also carries defaults, loras and three by-mode variants of
// most of what is here. Copying them in would be a second schema to keep
// current for no behaviour; what ember needs is enough to refuse a job before a
// GPU has been asked to do anything with it, and enough to know what it may ask
// of a generation already in flight.
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

	// OutputFormats and Upscalers are read for the same reason Samplers is:
	// they are the server's own allow-lists, and a name checked against them
	// is a name refused before a card is rented rather than after.
	//
	// Upscalers is an array of objects where the others are arrays of
	// strings, and the name is the only field ember needs -- the entries also
	// carry `model` and `image_upscale` booleans describing whether the
	// upscaler is model-backed, which is the server's problem and not
	// ember's.
	OutputFormats []string       `json:"output_formats"`
	Upscalers     []wireUpscaler `json:"upscalers"`

	// Features and GenerationFeatures are the server's own two accounts of
	// what it can be asked for, and they are not the same account; see
	// wireFeatures and wireGenerationFeatures for which answers which
	// question and why one parameter is read from each.
	Features           wireFeatures           `json:"features"`
	GenerationFeatures wireGenerationFeatures `json:"generation_features"`
}

// wireFeatures is the per-mode map: what a request *body* for this mode may
// carry.
//
// There are twelve in a captured response -- cache, control_image, hires,
// init_image, ip_adapter_image, lora, mask_image, preview, ref_images,
// vae_tiling and the two cancels -- and most of them describe image inputs
// ember has no route for. Two are read.
//
// `cancel_generating` says what ember may ask of a generation that is already
// running and already billing, which is the only one of these that changes a
// decision about a request in flight rather than about a request body.
//
// `hires` is read because it is the ONLY published flag covering the hires
// block: `generation_features` has no `hires` key at all.
//
// IT IS A POINTER BECAUSE ABSENT IS AN ANSWER, and this is the field that
// proves it rather than the field that argues for it. At 77b72a2
// make_img_gen_features_json hardcodes it true and recipe_features never
// overwrites it -- the overwrite list is mask_image, ref_images, end_image,
// control_frames, ref_videos, ref_audios, ref_video_audios, lora, vae_tiling,
// cache and preview -- so no model makes it false. But a server with no
// weights reports `supported_modes: []`, the per-mode map is then never built,
// and the whole `features` object collapses to the two cancel flags: `hires`
// is **missing**, not false. testdata/capabilities_no_model.json is that
// response, and it carries a published false (every generation feature) beside
// a genuine silence (this one). A bool here would have read the silence as a
// no and greyed the control on every build that does not publish the map.
//
// ember therefore honours what the server says and asserts nothing about what
// it will say. The greyed hires control is reachable only from a config built
// by hand, which is what the browser lab serves.
//
// `cancel_queued` is deliberately absent. sd-server's own queue is one job
// deep by construction -- runJobs submits one at a time precisely so a
// finished result cannot be purged by async_jobs' 600-second TTL -- so there
// is never a queued job on the server for ember to cancel. Reading the flag
// would be reading a capability for a state ember cannot produce.
type wireFeatures struct {
	CancelGenerating bool  `json:"cancel_generating"`
	Hires            *bool `json:"hires"`
}

// wireGenerationFeatures is the map derived from the *model*, which is the one
// worth gating a control on.
//
// WHY A SECOND MAP AT ALL. `features` above is built per mode from a literal:
// for img_gen every value starts true and only the keys in recipe_features'
// overwrite list are replaced. `generation_features` is
// model_catalog.cpp's model_generation_features, which starts from
// k_recipe_features -- where clip_skip, flow_shift, shifted_timestep, slg and
// img_cfg all default to **false** -- and turns on what the loaded model's
// recipe declares. So this is the map that can say no, and the five fields
// here are the ones covering a parameter ember can actually send.
//
// EVERY FIELD IS A POINTER, and that is the WithExposed distinction again:
// nil is "this build published no such flag" and false is "the server says
// no". Collapsing them would mean a server that does not publish the map
// greying every control in the compose panel, which is the failure
// capsView.Caps is a pointer to avoid one layer up. A nil gates nothing; a
// false refuses before a card is rented.
//
// WHAT IS NOT HERE AND WHY. `eta` appears in neither map, so it is sent
// ungated and README says so -- inventing a flag for it would be the
// allowedCudaVersions mistake, a schema read in place of a response.
// `output_format` and `output_compression` are already checked against
// `output_formats`, which is a stronger answer than a boolean. `lora`,
// `mask_image`, `control_image`, `ip_adapter` and `ref_images` are published
// and not read, because ember has no field for any of them: a flag gating a
// parameter nothing can send is a feature built end to end and connected to
// nothing, which this repository has shipped twice. `img_cfg` is the one
// published flag ember deliberately does not read although it could -- a live
// server answers true for it and accepts the parameter at -1 and at 99
// without echoing any of them, so the flag describes a field whose effect
// nothing here can observe; see Params.Strength for the probe.
//
// `cache`, `preview` and `preview_tae` are left alone on purpose. The first
// covers `cache_mode` and `cache_option`, which stay in `extra` because
// nobody here has observed their value sets; the other two would gate the
// preview mode, which `preview_modes` already publishes as a list -- and
// `preview_tae` describes the `tae` mode specifically while `vae` has no flag,
// so gating the menu on it would be a caption true of one mode applied to all
// of them.
type wireGenerationFeatures struct {
	ClipSkip        *bool `json:"clip_skip"`
	FlowShift       *bool `json:"flow_shift"`
	ShiftedTimestep *bool `json:"shifted_timestep"`
	SLG             *bool `json:"slg"`
	VAETiling       *bool `json:"vae_tiling"`

	// InitImage gates the one image input ember can supply.
	//
	// READ FROM THIS MAP AND NOT FROM `features`, although both publish an
	// `init_image` key and `features` is the one describing what a request
	// BODY may carry. `features` is built per mode from a literal where every
	// value starts true, so it cannot say no; this one is derived from the
	// loaded model's recipe, which is the whole reason there are two maps and
	// the reason `hires` is read from the other one -- that key exists only
	// there.
	//
	// `img2img` is NOT read beside it, and that is the Offer.VCPU rule: it
	// describes the same capability from the mode's side, so two flags for
	// one fact is two things that can disagree, and the one ember would be
	// wrong to believe is whichever it checked second.
	InitImage *bool `json:"init_image"`
}

// wireUpscaler is one entry of the hires upscaler list.
type wireUpscaler struct {
	Name string `json:"name"`
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

// wireErrorEnvelope is the form every outright refusal uses: a string `error`
// beside a separate `code`.
//
// Observed on `POST /img_gen` against a server with no weights
// (`{"code":"model_not_loaded","error":"No model is loaded"}`, 503) and on
// `POST /models/load` with a file that is not a model
// (`{"code":"model_load_failed",...}`, 400), so it is not only model
// management as this comment once said -- it is the shape of any request the
// server will not take. Read by refusal, which is what puts the sentence in
// front of the operator instead of httpx's raw body.
type wireErrorEnvelope struct {
	Error string `json:"error"`
	Code  string `json:"code"`
}
