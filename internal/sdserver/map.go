package sdserver

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
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
}

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
	return Capabilities{
		Model:        w.Model,
		Samplers:     append([]string(nil), w.Samplers...),
		Schedulers:   append([]string(nil), w.Schedulers...),
		PreviewModes: append([]string(nil), w.PreviewModes...),
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

	// Extra is merged into the request after the typed fields. A key that
	// collides with one ember set is refused rather than resolved -- see
	// Body.
	Extra map[string]any
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
