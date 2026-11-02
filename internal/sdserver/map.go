package sdserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Status is where a job has got to.
//
// An int rather than the wire string, so a status the server starts sending
// that ember has never heard of is an error at the boundary and not a value
// that flows into a switch somewhere and falls through the default.
type Status int

const (
	StatusQueued Status = iota + 1
	StatusGenerating
	StatusCompleted
	StatusFailed
	StatusCancelled
)

// statuses is a map and not a switch for the reason the RunPod mapper's is:
// anything unlisted has to produce an error, and a switch invites a default
// that quietly picks one.
var statuses = map[string]Status{
	"queued":     StatusQueued,
	"generating": StatusGenerating,
	"completed":  StatusCompleted,
	"failed":     StatusFailed,
	"cancelled":  StatusCancelled,
}

func (s Status) String() string {
	for k, v := range statuses {
		if v == s {
			return k
		}
	}
	return fmt.Sprintf("status(%d)", int(s))
}

// Done reports whether the job has stopped moving.
//
// Cancelled and failed are done. A poller that only stopped on completed
// would spin until its deadline against a job the server has already given
// up on, on a card billing by the second.
func (s Status) Done() bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusCancelled
}

// ModelState is what the server says about the weights.
type ModelState int

const (
	ModelEmpty ModelState = iota + 1
	ModelLoading
	ModelReady
	ModelError
)

var modelStates = map[string]ModelState{
	"empty":   ModelEmpty,
	"loading": ModelLoading,
	"ready":   ModelReady,
	"error":   ModelError,
}

func (m ModelState) String() string {
	for k, v := range modelStates {
		if v == m {
			return k
		}
	}
	return fmt.Sprintf("state(%d)", int(m))
}

// Preview is one progress frame.
type Preview struct {
	// Pass, Step and TotalSteps are reported exactly as the server gives
	// them. Step and TotalSteps describe the current sampling pass and not the
	// whole job, so they are never combined into a single percentage here --
	// see wirePreview.
	Pass       int
	Step       int
	TotalSteps int

	// PNG is the decoded frame. It can be empty while the other three are
	// set, which is what a server with previews on but nothing rendered yet
	// looks like.
	PNG []byte
}

// Image is one finished image, already decoded.
type Image struct {
	Index int
	Data  []byte
}

// State is a job as ember understands it.
type State struct {
	ID            string
	Status        Status
	QueuePosition int
	Preview       *Preview
	Images        []Image

	// Err is the server's own explanation for a failed job, not a transport
	// failure. A failed job is a successful HTTP request.
	Err *APIError
}

// ModelStatus is the answer to "can anything be generated yet".
type ModelStatus struct {
	State ModelState

	// LoadElapsed is what the server reports it spent loading, which is the
	// cost this package exists to pay once instead of once per image. Printed
	// rather than inferred from ember's clock, because ember's clock also
	// contains the SSH round trips.
	LoadElapsed time.Duration

	// Err is set when State is ModelError.
	Err string
}

// Limits are the bounds a job is checked against before it is submitted.
type Limits struct {
	MinWidth, MaxWidth   int
	MinHeight, MaxHeight int
	MaxBatchCount        int
	MaxQueueSize         int
}

// Capabilities is what this server build can do, trimmed to what ember checks.
type Capabilities struct {
	Model        string
	Samplers     []string
	Schedulers   []string
	PreviewModes []string
	Limits       Limits

	// OutputFormats and Upscalers are allow-lists, in the sense the
	// conventions mean: a name from one of these is a name the server said it
	// has, and anything else is refused here rather than at submission time.
	//
	// Upscalers matters more than it looks. api.md resolves a model-backed
	// upscaler as `--hires-upscalers-dir / (name + ext)`, so the name reaches
	// a path join on the node. Checking it against the server's own list is
	// what keeps a name from being a path component ember chose to trust.
	OutputFormats []string
	Upscalers     []string

	// CancelGenerating is features.cancel_generating: whether this build will
	// stop a generation that is already running.
	//
	// IT DEFAULTS TO FALSE, AND FALSE IS THE EXPENSIVE ANSWER, which is the
	// opposite of how every other field here is read -- an empty Samplers
	// list refuses a job, because a refusal before a card is rented costs
	// nothing. This one is consulted while a rented card is sampling and the
	// operator has just asked ember to stop spending, so "the server did not
	// report it" cannot be a refusal: refusing leaves the operator with no
	// lever but stopping ember serve, which destroys the rental and takes
	// every other request on it. ember drops the request's remaining jobs --
	// which on a batch is most of the money -- leaves the one in flight to
	// finish, and says plainly that it could not be stopped. See runJobs.
	CancelGenerating bool

	// Supported is the server's two feature maps, trimmed to the flags that
	// cover a parameter ember can send. See Supported.
	Supported Supported
}

// Supported is what the server says the loaded model takes.
//
// IT IS A THIRD KIND OF ALLOW-LIST and the weakest of the three, which is why
// it is separate from the lists above. `Samplers` and `Upscalers` are names
// checked against names; `Limits` are numbers checked against numbers; these
// are booleans the server derives from the model it loaded, and they say only
// "asking for this at all is pointless or refused". They exist because without
// them the compose panel offers a control for something the server has already
// said it cannot do, and the operator learns it as a refusal after a card is
// rented -- or, worse, as a parameter accepted and ignored, which reads as
// "the model does not respond to clip skip".
//
// EVERY FIELD IS A POINTER: nil is "this build published no such flag" and
// false is "the server says no". That distinction is load-bearing in both
// directions. A nil that refused would make every parameter here unusable
// against any build that does not publish the maps, including every cached
// caps row written before ember read them -- the exact shape of the bug where
// an old `caps` row decoded to empty lists and Validate refused a correct
// output_format while blaming the server for a field ember never asked about.
// A false that was ignored would put the control back where it started.
//
// They are spelled out one field per parameter rather than carried as a
// map[string]bool, because a map would make the gate a string the UI has to
// spell the same way the server does, in a language that cannot be compiled
// against it. The closed set is upstream's k_recipe_features; ember reads six
// of its twenty-seven and the one `hires` flag that only `features` carries.
type Supported struct {
	ClipSkip        *bool
	FlowShift       *bool
	ShiftedTimestep *bool
	SLG             *bool
	VAETiling       *bool
	Hires           *bool

	// InitImage is generation_features.init_image: whether the loaded model
	// takes an image input at all. It gates Params.InitImage and, through it,
	// Params.Strength -- which cannot be sent without one.
	InitImage *bool
}

// denied reports that the server published this flag and said no.
//
// A function rather than `flag != nil && !*flag` at each call site, because
// the nil case is the one that would be written wrong: `!*flag` on a nil
// pointer panics, and `flag == nil || !*flag` -- the plausible typo -- refuses
// everything against a server that published nothing.
func denied(flag *bool) bool { return flag != nil && !*flag }

// APIError is the server's structured refusal.
type APIError struct {
	Code    string
	Message string
}

func (e *APIError) Error() string {
	switch {
	case e.Code != "" && e.Message != "":
		return fmt.Sprintf("%s: %s", e.Code, e.Message)
	case e.Message != "":
		return e.Message
	case e.Code != "":
		return e.Code
	}
	return "the server reported a failure with no detail"
}

// decodeAPIError reads either shape of the `error` field.
//
// sd-server spells this two different ways in one API: model management
// answers `{"error":"No model is loaded","code":"model_not_loaded"}`, a
// string beside a separate code, while a failed job answers
// `{"error":{"code":...,"message":...}}`, an object. A Go struct with
// `Error string` fails to decode the second and a struct with an object fails
// the first, so whichever one was written first works until the day the other
// endpoint refuses something -- and then the error ember prints is a JSON
// unmarshalling complaint instead of the reason.
//
// Nil raw, JSON null and an empty string all mean "no error", because all
// three appear: the field is omitted while a job is running, explicitly null
// on some responses, and empty on others.
func decodeAPIError(raw json.RawMessage) *APIError {
	s := strings.TrimSpace(string(raw))
	if len(s) == 0 || s == "null" || s == `""` {
		return nil
	}

	var obj wireErrorObject
	if err := json.Unmarshal(raw, &obj); err == nil && (obj.Code != "" || obj.Message != "") {
		return &APIError{Code: obj.Code, Message: obj.Message}
	}

	var str string
	if err := json.Unmarshal(raw, &str); err == nil {
		if strings.TrimSpace(str) == "" {
			return nil
		}
		return &APIError{Message: str}
	}

	// Neither shape. The raw text is handed back rather than dropped: an
	// unrecognised error is still the only thing that says why.
	return &APIError{Message: s}
}

func mapJob(w wireJob) (State, error) {
	st, ok := statuses[w.Status]
	if !ok {
		return State{}, fmt.Errorf("job %s: unknown status %q", w.ID, w.Status)
	}

	out := State{
		ID:            w.ID,
		Status:        st,
		QueuePosition: w.QueuePosition,
		Err:           decodeAPIError(w.Error),
	}

	if w.Preview != nil {
		p := Preview{Pass: w.Preview.Pass, Step: w.Preview.Step, TotalSteps: w.Preview.TotalSteps}
		if w.Preview.B64JSON != "" {
			png, err := decodeB64(w.Preview.B64JSON)
			if err != nil {
				return State{}, fmt.Errorf("job %s: preview frame: %w", w.ID, err)
			}
			p.PNG = png
		}
		out.Preview = &p
	}

	if w.Result != nil {
		for _, img := range w.Result.Images {
			data, err := decodeB64(img.B64JSON)
			if err != nil {
				return State{}, fmt.Errorf("job %s: image %d: %w", w.ID, img.Index, err)
			}
			out.Images = append(out.Images, Image{Index: img.Index, Data: data})
		}
	}

	return out, nil
}

// decodeB64 accepts a bare base64 payload or a data URL.
//
// api.md describes the request side as accepting "base64 or data URL" and does
// not promise which the response uses, so both are read. Guessing one and
// being wrong would mean a decoded PNG that is actually a text prefix plus
// pixels, written to disk as a file no viewer opens.
func decodeB64(s string) ([]byte, error) {
	if i := strings.Index(s, ","); i >= 0 && strings.HasPrefix(s, "data:") {
		s = s[i+1:]
	}
	data, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return data, nil
}

func mapModelStatus(w wireModelStatus) (ModelStatus, error) {
	st, ok := modelStates[w.State]
	if !ok {
		return ModelStatus{}, fmt.Errorf("unknown model state %q", w.State)
	}
	out := ModelStatus{
		State: st,
		// Seconds as a float on the wire; rounded to the millisecond here
		// because a load is tens of seconds and nobody reads the rest.
		LoadElapsed: time.Duration(w.LoadElapsedSeconds * float64(time.Second)).Round(time.Millisecond),
	}
	if w.Error != nil {
		out.Err = *w.Error
	}
	return out, nil
}

func mapCapabilities(w wireCapabilities) Capabilities {
	// Flattened to the names. A nil list stays nil rather than becoming an
	// empty slice, because Validate reads "the server reported no list" off
	// the length and has to be able to tell that from "the server reported an
	// empty list" -- the WithExposed distinction, one package over.
	var ups []string
	for _, u := range w.Upscalers {
		ups = append(ups, u.Name)
	}
	return Capabilities{
		Model:            w.Model.Name,
		CancelGenerating: w.Features.CancelGenerating,
		// Copied pointer by pointer rather than as a struct, so adding a flag
		// to the wire type without reading it here is a compile-time nothing
		// and not a silent passthrough of a field no refusal consults.
		Supported: Supported{
			ClipSkip:        w.GenerationFeatures.ClipSkip,
			FlowShift:       w.GenerationFeatures.FlowShift,
			ShiftedTimestep: w.GenerationFeatures.ShiftedTimestep,
			SLG:             w.GenerationFeatures.SLG,
			VAETiling:       w.GenerationFeatures.VAETiling,
			Hires:           w.Features.Hires,
			InitImage:       w.GenerationFeatures.InitImage,
		},
		Samplers:      append([]string(nil), w.Samplers...),
		Schedulers:    append([]string(nil), w.Schedulers...),
		PreviewModes:  append([]string(nil), w.PreviewModes...),
		OutputFormats: append([]string(nil), w.OutputFormats...),
		Upscalers:     ups,
		Limits: Limits{
			MinWidth:      w.Limits.MinWidth,
			MaxWidth:      w.Limits.MaxWidth,
			MinHeight:     w.Limits.MinHeight,
			MaxHeight:     w.Limits.MaxHeight,
			MaxBatchCount: w.Limits.MaxBatchCount,
			MaxQueueSize:  w.Limits.MaxQueueSize,
		},
	}
}

// Job is one image to generate.
//
// A typed subset plus a raw passthrough, rather than a mirror of the whole
// schema. /sdcpp/v1/img_gen has around forty top-level fields and four nested
// objects; typing all of them would be a second copy of somebody else's schema
// to keep current, and every upstream addition would make ember's config wrong
// until a release. Typing none of them would mean ember could not check a
// sampler name or report a seed.
//
// So: ember types what it validates, reports or defaults, and Extra reaches
// everything else. Nothing in the API is unreachable from a config.
//
// The line moved once, and the reason it moved is worth keeping. The first
// cut typed eleven fields, which was the right place while the only caller
// was a config file an operator edits by hand -- Extra is a map in YAML and
// costs nothing to write. A browser changed that: `extra` is structurally
// refused over HTTP, so an untyped field is not merely inconvenient from the
// web UI but unreachable, and the UI shipped able to send eleven of the
// fifty-one parameters the server reports with nothing saying which forty it
// could not. Params holds the ones that are path-free and affect a
// text-to-image run; its doc comment names what is still left out and why.
type Job struct {
	Prompt         string
	NegativePrompt string

	Width, Height int
	Steps         int
	BatchCount    int
	Seed          int64

	Sampler   string
	Scheduler string

	// CFG is sample_params.guidance.txt_cfg and Guidance is
	// .distilled_guidance. Two separate numbers that both get called "cfg" in
	// conversation; the flux2 recipe sets cfg-scale 1 and guidance 4, so a
	// config that conflated them would silently generate at the wrong one.
	CFG      float64
	Guidance float64

	// Output is the local filename stem. It never reaches the server.
	Output string

	// Params are the rest of what a text-to-image generation can be told.
	// Separate from the fields above because those are the ones every job
	// has and every renderer prints; these are optional and mostly unset.
	Params Params

	// InitImageBytes are the pixels Params.InitImage names, resolved.
	//
	// IT IS ON Job AND NOT ON Params, AND THAT IS STRUCTURAL. Params is
	// stored as the JSON of its own struct in genstore's `params` column and
	// handed back to the browser by viewRequest; Job is never serialised
	// anywhere. Putting the bytes on Params with a `json:"-"` would work
	// today and silently empty itself on a round trip through the store,
	// which is a footgun aimed at whoever next reads a request back out of
	// the database. Here the asymmetry is visible: the reference persists,
	// the bytes do not, and whatever is about to submit has to have resolved
	// them.
	//
	// Body REFUSES A REFERENCE WITH NO BYTES rather than omitting the field.
	// Omitting would send a plain text-to-image generation for a request that
	// asked for an image input and bill for it, which is the quiet half of
	// the defect the reference exists to close.
	InitImageBytes []byte

	// Extra is merged into the request after the typed fields. A key that
	// collides with one ember set is refused rather than resolved -- see
	// Body.
	Extra map[string]any
}

// Params are the generation parameters beyond the eleven every job carries.
//
// WHY THESE AND NOT THE OTHERS. They are the ones that change a plain
// text-to-image generation and name no file and no directory. The fields left
// out divide into two groups with two different reasons:
//
// `guidance.img_cfg`, `control_strength`, `ip_adapter_strength`,
// `increase_ref_index`, `ref_image_args` and `image_preprocess` need an image
// input, and `InitImage` below is the only one ember has. They were held back
// as a group for that reason and four of them stay held back for a sharper
// one, which is a measurement rather than an inference -- see the probe
// recorded beside Strength.
//
// A control that cannot affect the generation is the same defect as a caption
// that is true of one preview mode and wrong for the others: it reads as
// working.
//
// `cache_mode`, `cache_option`, `scm_mask`, `scm_policy_dynamic`,
// `qwen_image_layers`, `custom_sigmas` (both of them) and
// `vae_tiling_params.extra_tiling_args` are left to Extra because nothing
// here knows their value sets. api.md names them without enumerating what
// they accept, and a free-text field ember cannot check is a field ember
// should not pretend to validate -- the `allowedCudaVersions` mistake is
// reading a schema in place of a response, and typing a value set nobody has
// observed is the same error with better manners.
//
// A pointer means the server's own default is not zero, so zero cannot stand
// for unset. Everything else uses zero, which is what Body already omits.
type Params struct {
	// ClipSkip is a pointer because the server's default is -1 and 0 is a
	// real value. A sentinel of 0 here would make `clip_skip: 0`
	// unrepresentable while looking exactly like every other field.
	ClipSkip *int `json:"clip_skip,omitempty"`

	// Eta and FlowShift default to null on the server, and 0 is legal for
	// both -- eta 0 is the deterministic sampler. Same reason as ClipSkip,
	// reached from a different default.
	Eta       *float64 `json:"eta,omitempty"`
	FlowShift *float64 `json:"flow_shift,omitempty"`

	// ShiftedTimestep's own default is 0, so zero-is-unset is the server's
	// own meaning here rather than ember's convention imposed on it.
	ShiftedTimestep int `json:"shifted_timestep,omitempty"`

	// InitImage is a STAGED REFERENCE AND NOT BYTES: "sha256:<64 hex>", the
	// name blob.Inputs gave the upload. The bytes travel on Job.InitImageBytes,
	// which is not serialised anywhere -- so a request row holds a digest,
	// the browser gets a digest back, and the megabytes stay in one place on
	// disk instead of in the params column of every job that uses them.
	//
	// THE REFERENCE IS THE WHOLE OF WHAT A REQUEST MAY CHOOSE HERE. It is
	// sixty-four hex characters or it is refused, so it cannot be a path, a
	// traversal, a URL or a filename -- which is what keeps this field on the
	// right side of the line newRequest draws: a request names a provider, a
	// card, a batch of jobs and now bytes it has already uploaded, and still
	// nothing that reaches node.MountAt, exposure.SSHPort or ServeCommand.
	InitImage string `json:"init_image,omitempty"`

	// Strength is how much of the init image survives: 0 keeps it and 1
	// replaces it.
	//
	// A POINTER BECAUSE THE SERVER'S DEFAULT IS 0.75 AND 0 IS A REAL VALUE,
	// which is the ClipSkip rule reached from a fourth default. Measured
	// against a live sd-server: `strength: 0` is accepted and echoed back as
	// `generation_info.strength: 0.0`, while sending no strength at all
	// echoes 0.75. A sentinel of 0 here would make "keep the whole image"
	// unrepresentable while looking exactly like a field that works.
	//
	// IT IS SENT ONLY WITH AN INIT IMAGE, and that is not tidiness either.
	// The same probe sent `strength: 0.4` with no `init_image`: the server
	// answered 202, generated, and `generation_info` carried no `strength` at
	// all. So a strength without an image is accepted, dropped, and reported
	// nowhere -- the exact "reads as working" defect this parameter was held
	// back for. staticParams refuses the combination rather than relying on
	// the operator to notice.
	//
	// The same probe is why `img_cfg`, `control_strength`,
	// `ip_adapter_strength` and `increase_ref_index` are still absent. All
	// four were accepted and none changed anything observable:
	// `control_strength` and `ip_adapter_strength` were dropped with no echo
	// and need `control_image`/`ip_adapter_image` plus a ControlNet or an
	// IP-Adapter in the serve argv, which nothing here has run;
	// `increase_ref_index` was echoed as `ref_image_args:
	// "ref_index_mode=increase"` and needs `ref_images`, which is an edit
	// model's input; and `img_cfg` was accepted at 0, at -1 and at 99, echoed
	// at none of them, on a build whose `generation_features.img_cfg` says
	// true -- so there is no value ember could send and no flag it could read
	// that would tell an operator whether it did anything. Typing a parameter
	// on that evidence is the allowedCudaVersions mistake with better
	// manners.
	Strength *float64 `json:"strength,omitempty"`

	SLG       SLG       `json:"slg,omitzero"`
	Hires     Hires     `json:"hires,omitzero"`
	VAETiling VAETiling `json:"vae_tiling,omitzero"`

	// OutputFormat is checked against the server's own list. Compression
	// applies to jpeg and webp; png ignores it.
	OutputFormat      string `json:"output_format,omitempty"`
	OutputCompression int    `json:"output_compression,omitempty"`
}

// SLG is skip-layer guidance.
//
// Scale is the switch: the server's default is 0.0, which is off, so layers
// and bounds with no scale configure something that does not run. Body sends
// the block only when Scale is non-zero, which makes "I set the layers and
// nothing changed" unrepresentable rather than a thing to explain.
type SLG struct {
	Layers     []int   `json:"layers,omitempty"`
	LayerStart float64 `json:"layer_start,omitempty"`
	LayerEnd   float64 `json:"layer_end,omitempty"`
	Scale      float64 `json:"scale,omitempty"`
}

// IsZero says nothing was set.
//
// A method and not an `==` at each call site, because SLG holds a slice and is
// therefore not comparable -- a caller reaching for `== SLG{}` gets a compile
// error and a caller reaching for it on Hires does not, which would leave the
// three blocks checked three different ways. encoding/json's `omitzero` uses
// this too, so what the wire omits and what a renderer treats as unset are
// one answer rather than two.
func (s SLG) IsZero() bool {
	return len(s.Layers) == 0 && s.LayerStart == 0 && s.LayerEnd == 0 && s.Scale == 0
}

// Hires is the second, higher-resolution pass.
//
// Enabled is the switch and the rest is only sent with it, because the server
// reads the block as a unit: upstream's own default is enabled false with
// scale 2 and denoising 0.7 already filled in, so `enabled: true` alone is a
// legitimate request for upstream's defaults rather than an incomplete one.
//
// Upscaler is a name from Capabilities.Upscalers and never free text. api.md
// resolves a model-backed name as `--hires-upscalers-dir / (name + ext)`, so
// an unchecked name is a path component chosen by whoever sent the request.
type Hires struct {
	Enabled           bool    `json:"enabled,omitempty"`
	Upscaler          string  `json:"upscaler,omitempty"`
	Scale             float64 `json:"scale,omitempty"`
	TargetWidth       int     `json:"target_width,omitempty"`
	TargetHeight      int     `json:"target_height,omitempty"`
	Steps             int     `json:"steps,omitempty"`
	DenoisingStrength float64 `json:"denoising_strength,omitempty"`
	UpscaleTileSize   int     `json:"upscale_tile_size,omitempty"`
}

// IsZero says nothing was set. See SLG.IsZero for why it is a method.
func (h Hires) IsZero() bool { return h == Hires{} }

// VAETiling decodes the latent in tiles, which is how a large image fits in
// VRAM that could not hold it whole.
//
// Enabled gates the block for Hires' reason. TemporalTiling is left out: it
// is a video concern, and the served path generates images.
type VAETiling struct {
	Enabled       bool    `json:"enabled,omitempty"`
	TileSizeW     int     `json:"tile_size_w,omitempty"`
	TileSizeH     int     `json:"tile_size_h,omitempty"`
	TargetOverlap float64 `json:"target_overlap,omitempty"`
	RelSizeW      float64 `json:"rel_size_w,omitempty"`
	RelSizeH      float64 `json:"rel_size_h,omitempty"`
}

// IsZero says nothing was set. See SLG.IsZero for why it is a method.
func (t VAETiling) IsZero() bool { return t == VAETiling{} }

// Describe is the one line an operator reads to know which image this is.
//
// A method on Job rather than a function beside its caller, because it has two
// callers -- the line printed as a job starts, and the dry run that shows a
// batch before anything is rented -- and a job description that differed
// between the two would be the same batch described two ways.
func (j Job) Describe() string {
	var b strings.Builder
	p := j.Prompt
	if p == "" {
		if v, ok := j.Extra["prompt"].(string); ok {
			p = v
		}
	}
	// Truncated: a FLUX.2 prompt is a thousand characters and the first few
	// words are what identifies it in a log.
	if len(p) > 60 {
		p = p[:57] + "..."
	}
	b.WriteString(strconv.Quote(p))
	// Only when both sides are set. The other would come from the server's
	// own default, and "512x" would be a guess printed as a fact.
	if j.Width != 0 && j.Height != 0 {
		fmt.Fprintf(&b, " %dx%d", j.Width, j.Height)
	}
	if j.Steps != 0 {
		fmt.Fprintf(&b, " %d steps", j.Steps)
	}
	if j.Seed != 0 {
		fmt.Fprintf(&b, " seed %d", j.Seed)
	}
	return b.String()
}

// The preview settings, and the default that is deliberately not upstream's.
//
// sd-server defaults preview_interval to 1 -- a frame every step -- and the
// tae and vae modes run a decode to produce each one. So the upstream default
// is the expensive one, on a card billing by the second, for frames that in an
// unattended batch nobody is watching.
const (
	// PreviewNone turns previews off entirely: no frame in the response at
	// all, which is the only genuinely free setting.
	PreviewNone = "none"

	// PreviewProj is the cheap one: a linear projection of the latent rather
	// than a decode.
	PreviewProj = "proj"

	// DefaultPreview and DefaultPreviewInterval are ember's, not upstream's.
	DefaultPreview         = PreviewProj
	DefaultPreviewInterval = 5
)

// Run is everything a served batch needs that is not on the Spec.
//
// The jobs are here and not on node.Spec because they never reach the node as
// argv: ember sends them over HTTP. internal/node also imports nothing else
// from this module, so the type could not live there in any case.
type Run struct {
	Preview         string
	PreviewInterval int
	Jobs            []Job
}

// previewModes is what sd-server's own preview_modes enumerates. Checked here
// rather than only against a live server's capabilities, because a typo should
// refuse before anything is rented, not after.
var previewModes = []string{PreviewNone, PreviewProj, "tae", "vae"}

// ParsePreview turns a config value into a preview mode.
//
// Empty is ember's default and not upstream's -- see the constants. An
// unlisted value is refused and names what is allowed, because the failure
// otherwise lands after a card is rented, as a 400 from a server that was
// about to generate something.
func ParsePreview(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return DefaultPreview, nil
	}
	if slices.Contains(previewModes, s) {
		return s, nil
	}
	return "", fmt.Errorf("preview %q is not one of %s", s, strings.Join(previewModes, ", "))
}

// ParsePreviewInterval turns a config value into a step interval.
//
// Zero is ember's default. Negative is refused rather than clamped: upstream
// clamps a non-positive interval to 1, which is a frame every step -- the
// most expensive setting there is, reached by writing something that looks
// like "off".
func ParsePreviewInterval(n int) (int, error) {
	if n == 0 {
		return DefaultPreviewInterval, nil
	}
	if n < 0 {
		return 0, fmt.Errorf("preview_interval %d is negative; sd-server would clamp it to 1, "+
			"a preview frame every step, which is the most expensive setting rather than the least. "+
			"Use preview: %s to turn previews off", n, PreviewNone)
	}
	return n, nil
}

// Body builds the request for one job.
//
// preview and interval are ember's to set, not the operator's, because ember
// owns the progress stream: it decides the poll cadence, so it has to decide
// how often a frame is produced.
func (j Job) Body(preview string, interval int) (map[string]any, error) {
	body := map[string]any{}
	sample := map[string]any{}
	guidance := map[string]any{}

	// owned records the dotted path of every key ember itself set, so a
	// collision with Extra can name the exact field rather than "something
	// clashed".
	owned := map[string]bool{}
	set := func(m map[string]any, path, key string, v any) {
		m[key] = v
		owned[path] = true
	}

	// Only non-zero values are sent, which is also what makes Extra useful:
	// a field the typed struct does not set is one Extra can set freely, and
	// the refusal below fires only on a genuine double-specification.
	if j.Prompt != "" {
		set(body, "prompt", "prompt", j.Prompt)
	}
	if j.NegativePrompt != "" {
		set(body, "negative_prompt", "negative_prompt", j.NegativePrompt)
	}
	if j.Width != 0 {
		set(body, "width", "width", j.Width)
	}
	if j.Height != 0 {
		set(body, "height", "height", j.Height)
	}
	if j.BatchCount != 0 {
		set(body, "batch_count", "batch_count", j.BatchCount)
	}
	if j.Seed != 0 {
		set(body, "seed", "seed", j.Seed)
	}
	if j.Steps != 0 {
		set(sample, "sample_params.sample_steps", "sample_steps", j.Steps)
	}
	if j.Sampler != "" {
		set(sample, "sample_params.sample_method", "sample_method", j.Sampler)
	}
	if j.Scheduler != "" {
		set(sample, "sample_params.scheduler", "scheduler", j.Scheduler)
	}
	if j.CFG != 0 {
		set(guidance, "sample_params.guidance.txt_cfg", "txt_cfg", j.CFG)
	}
	if j.Guidance != 0 {
		set(guidance, "sample_params.guidance.distilled_guidance", "distilled_guidance", j.Guidance)
	}

	p := j.Params
	// A pointer that is set is sent even when it holds zero, which is the
	// whole reason it is a pointer: clip_skip 0 and eta 0 are both values a
	// model is told, and the server's defaults for them are -1 and null.
	if p.ClipSkip != nil {
		set(body, "clip_skip", "clip_skip", *p.ClipSkip)
	}
	if p.Eta != nil {
		set(sample, "sample_params.eta", "eta", *p.Eta)
	}
	if p.FlowShift != nil {
		set(sample, "sample_params.flow_shift", "flow_shift", *p.FlowShift)
	}
	if p.ShiftedTimestep != 0 {
		set(sample, "sample_params.shifted_timestep", "shifted_timestep", p.ShiftedTimestep)
	}
	// BARE BASE64 AND NOT A DATA URL. api.md says the field takes either and
	// a live server accepts both -- measured, with the same result -- so the
	// choice is ember's: the prefix is eleven bytes of nothing on a payload
	// that is already a third larger than the file, and one spelling means
	// one thing for a reader of a captured request to compare against.
	if p.InitImage != "" {
		if len(j.InitImageBytes) == 0 {
			return nil, fmt.Errorf("job names the staged image %s and nothing resolved it. "+
				"The reference is stored and the bytes are not, so whatever submits a job has to "+
				"load them first; sending the generation without the image would bill for a "+
				"text-to-image run the request did not ask for", p.InitImage)
		}
		set(body, "init_image", "init_image",
			base64.StdEncoding.EncodeToString(j.InitImageBytes))
	}
	// A pointer that is set is sent even when it holds zero: strength 0 is
	// "keep the init image" and the server's own default is 0.75.
	if p.Strength != nil {
		set(body, "strength", "strength", *p.Strength)
	}
	// Scale is the switch, so a zero scale sends no block at all rather than
	// a block the server reads as off. Layers without a scale would otherwise
	// look configured and do nothing.
	if p.SLG.Scale != 0 {
		slg := map[string]any{}
		set(slg, "sample_params.guidance.slg.scale", "scale", p.SLG.Scale)
		if len(p.SLG.Layers) > 0 {
			set(slg, "sample_params.guidance.slg.layers", "layers", p.SLG.Layers)
		}
		if p.SLG.LayerStart != 0 {
			set(slg, "sample_params.guidance.slg.layer_start", "layer_start", p.SLG.LayerStart)
		}
		if p.SLG.LayerEnd != 0 {
			set(slg, "sample_params.guidance.slg.layer_end", "layer_end", p.SLG.LayerEnd)
		}
		guidance["slg"] = slg
	}
	// Enabled gates the whole block, for the reason on Hires: the server
	// fills its own defaults in, so `enabled` alone is a complete request.
	if p.Hires.Enabled {
		h := map[string]any{}
		set(h, "hires.enabled", "enabled", true)
		if p.Hires.Upscaler != "" {
			set(h, "hires.upscaler", "upscaler", p.Hires.Upscaler)
		}
		if p.Hires.Scale != 0 {
			set(h, "hires.scale", "scale", p.Hires.Scale)
		}
		if p.Hires.TargetWidth != 0 {
			set(h, "hires.target_width", "target_width", p.Hires.TargetWidth)
		}
		if p.Hires.TargetHeight != 0 {
			set(h, "hires.target_height", "target_height", p.Hires.TargetHeight)
		}
		if p.Hires.Steps != 0 {
			set(h, "hires.steps", "steps", p.Hires.Steps)
		}
		if p.Hires.DenoisingStrength != 0 {
			set(h, "hires.denoising_strength", "denoising_strength", p.Hires.DenoisingStrength)
		}
		if p.Hires.UpscaleTileSize != 0 {
			set(h, "hires.upscale_tile_size", "upscale_tile_size", p.Hires.UpscaleTileSize)
		}
		body["hires"] = h
	}
	if p.VAETiling.Enabled {
		t := map[string]any{}
		set(t, "vae_tiling_params.enabled", "enabled", true)
		if p.VAETiling.TileSizeW != 0 {
			set(t, "vae_tiling_params.tile_size_w", "tile_size_w", p.VAETiling.TileSizeW)
		}
		if p.VAETiling.TileSizeH != 0 {
			set(t, "vae_tiling_params.tile_size_h", "tile_size_h", p.VAETiling.TileSizeH)
		}
		if p.VAETiling.TargetOverlap != 0 {
			set(t, "vae_tiling_params.target_overlap", "target_overlap", p.VAETiling.TargetOverlap)
		}
		if p.VAETiling.RelSizeW != 0 {
			set(t, "vae_tiling_params.rel_size_w", "rel_size_w", p.VAETiling.RelSizeW)
		}
		if p.VAETiling.RelSizeH != 0 {
			set(t, "vae_tiling_params.rel_size_h", "rel_size_h", p.VAETiling.RelSizeH)
		}
		body["vae_tiling_params"] = t
	}
	if p.OutputFormat != "" {
		set(body, "output_format", "output_format", p.OutputFormat)
	}
	if p.OutputCompression != 0 {
		set(body, "output_compression", "output_compression", p.OutputCompression)
	}

	// Always ember's, so always a collision if Extra names them.
	set(body, "preview", "preview", preview)
	set(body, "preview_interval", "preview_interval", interval)

	if len(guidance) > 0 {
		sample["guidance"] = guidance
	}
	if len(sample) > 0 {
		body["sample_params"] = sample
	}

	if err := mergeExtra(body, j.Extra, owned, ""); err != nil {
		return nil, err
	}
	return body, nil
}

// mergeExtra merges the operator's raw object into the request, refusing any
// key ember already set.
//
// A refusal and not a precedence rule, for the reason NewSpec refuses a config
// carrying both cmd and cmds: two answers to one question, and picking one
// silently is how somebody watches the wrong thing run. Here it would be
// worse than watching -- a seed or a step count resolved the wrong way makes
// two runs that claim to be the same measurement produce different images.
//
// Nested objects are merged rather than replaced, so an operator can reach
// sample_params.eta without having to restate the sample method ember put
// beside it.
func mergeExtra(dst, extra map[string]any, owned map[string]bool, prefix string) error {
	// Sorted, so a config with two colliding keys reports the same one every
	// run. Map order would make the error message a coin toss and the test
	// for it flaky.
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	for _, k := range keys {
		path := k
		if prefix != "" {
			path = prefix + "." + k
		}
		if owned[path] {
			return fmt.Errorf("extra sets %s, which ember already set from the typed field; "+
				"remove it from extra or leave the typed field unset", path)
		}

		v := extra[k]
		sub, isMap := v.(map[string]any)
		if !isMap {
			dst[k] = v
			continue
		}
		existing, ok := dst[k].(map[string]any)
		if !ok {
			existing = map[string]any{}
			dst[k] = existing
		}
		if err := mergeExtra(existing, sub, owned, path); err != nil {
			return err
		}
	}
	return nil
}
