package webapi

import (
	"fmt"
	"reflect"
	"time"

	"github.com/Legion112/ember/internal/app"
	"github.com/Legion112/ember/internal/config"
	"github.com/Legion112/ember/internal/genstore"
	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/provider"
	"github.com/Legion112/ember/internal/queue"
	"github.com/Legion112/ember/internal/reconcile"
	"github.com/Legion112/ember/internal/sdserver"
	"github.com/Legion112/ember/internal/tts"
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

	// Kinds is what THIS CONFIGURATION CAN RUN, and Voices the speech models
	// it publishes.
	//
	// IT WAS `genstore.Kinds()` UNCONDITIONALLY, and this paragraph used to
	// argue for that at length. The argument was wrong and an operator found
	// it by using the page: the selector offered `tts` on a server with no
	// voices, and choosing it answered "this ember serve publishes no voices,
	// so it serves no speech. Add a tts.voices list to ember.yaml and restart
	// it." A choice offered in order to be refused is worse than no choice,
	// because the refusal arrives after the operator has spent attention on
	// deciding. On a speech-only server the same list offered `sd`, where
	// nothing refused it at all and the request failed once a node was up --
	// the same defect with a rental attached. See servableKinds.
	//
	// WHAT HAD TO MOVE FIRST, which is why this sat written-down rather than
	// fixed for as long as it did. The page read an empty `kinds` as "ember
	// has not answered yet", so narrowing the list would have made a config
	// that can run NEITHER kind publish `[]` and the page wait for ever over a
	// server that had already told it everything. That discriminator is now
	// `providers` -- which `Config::unrunnable` was keyed on from the start,
	// for this reason, and which cannot be empty in a real answer because the
	// registry is built from the config. `TestConfig_PublishesAtLeastOneKind`
	// was the floor under the old reading and now asserts the precondition
	// that replaces it: a RUNNING ember serve always has a kind, because
	// `runServe` refuses to start a configuration that can serve nothing.
	//
	// AND THE GUARD CAUGHT THE FIRST WORDING OF THE PARAGRAPH THIS REPLACES,
	// which is worth one line because it is the same rule twice. That wording
	// named the deleted flag as the precedent, with `ember` and a backticked
	// `--all` on one line, and `TestComments_CiteNoFlagThatNoFlagSetDeclares`
	// reported it -- so the comment correcting a claim about an unimplemented
	// mechanism was itself citing a command nobody can type.
	//
	// THE OTHER TWO REFUSALS STAY, because this one is about what is offered
	// and they are about what is accepted. `Config::composing` still refuses a
	// speech kind naming `tts.voices`, and `POST /api/requests` still refuses
	// the request the same way -- reachable now only by a client that did not
	// get its kind from this field, which is a `?kind=` in a URL or a page
	// older than the server. Containment twice, by two mechanisms, is the rule
	// here and narrowing the list is the first of them rather than a
	// replacement for the second.
	//
	// WHY A UI HAS TO BE TOLD RATHER THAN KNOWING. A request names a voice by
	// **id** and nothing else -- the whole seal is that it cannot name a path
	// -- so the list of ids is something only ember can produce, out of the
	// operator's own config. A page that guessed would offer a choice that is
	// refused after a card is rented.
	//
	// Voices is EMPTY when this configuration publishes none, and a UI must
	// read that as "this process serves no speech" rather than as an empty
	// dropdown: `POST /api/requests` refuses a speech request against an
	// empty catalogue, naming `tts.voices`. The alternative -- an implicit
	// entry pointing at ember's own image -- would offer a voice that cannot
	// work wherever `image:` is not the speech one.
	Kinds  []string    `json:"kinds"`
	Voices []voiceView `json:"voices"`

	// Unoffered is what this build can do that this configuration does not,
	// which exists because narrowing Kinds took something away.
	//
	// THE QUESTION IT ANSWERS WAS ASKED BY AN OPERATOR LOOKING AT THE PAGE:
	// "have you just removed tts?" Nothing had -- `internal/tts`, the config
	// key and the speech runner were untouched -- but the selector had gone,
	// because one option is no choice, and a panel that offers one kind
	// without saying so is indistinguishable from a build that only has one.
	// The previous list said what ember could do and lied about what this
	// process would accept; the narrowed one is honest about the process and
	// silent about ember. This field is the half that went missing.
	//
	// A STATEMENT AND NEVER A CONTROL, which is the whole distinction being
	// drawn. The defect being fixed was a selector offering a choice in order
	// to refuse it, so the answer cannot be a disabled option or a greyed row:
	// those are the same defect with a different cursor. It is a sentence, in
	// the register of the `ember.yaml` ceiling hints beside it.
	//
	// DERIVED FROM THE SAME LOOP as Kinds and not from a second pass over
	// genstore.Kinds(), so the two cannot disagree about a kind -- a page
	// saying "this serve runs sd, and not tts" while offering tts is worse
	// than either half alone.
	Unoffered []string `json:"unoffered"`

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

// voiceView is one catalogue entry as a UI reads it.
//
// THE PATHS ARE NOT ON IT, which is the projection doing its job rather than
// an omission: this file's own header says a hand-written projection means
// adding a field somewhere cannot publish it by default, and
// `config.TTSVoice` holds two filenames on the node. A UI needs an id to send
// and a name to show; the operator knows what is behind them and a browser has
// no use for it.
type voiceView struct {
	ID   string `json:"id"`
	Name string `json:"name"`
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

	// OutputFormats and Upscalers are here so the page can offer a menu
	// instead of a text box. That is not cosmetic: both are checked against
	// these same lists at enqueue, so a free-text control would be a control
	// whose plausible values are mostly refusals.
	OutputFormats []string `json:"output_formats"`
	Upscalers     []string `json:"upscalers"`

	// Features is what the server said the loaded model takes, so the page can
	// make a control it cannot use visibly unavailable instead of accepting a
	// value that is refused at enqueue -- or, worse, accepted by a server that
	// drops it, which reads as "the model ignores this".
	//
	// Always an object, never omitted: a `null` here would arrive at serde as
	// a type error on a field that is present, which is the bug that dropped
	// whole websocket envelopes. The flags inside are omitted individually,
	// and absence is the answer that gates nothing.
	Features featuresView `json:"features"`
}

// featuresView is sdserver.Supported on the wire.
//
// Pointers all the way through, for the reason sdserver.Supported gives: nil
// is "the server published no such flag" and false is "the server says no",
// and a page that treated the first as the second would grey out a control
// against any build that does not publish the maps. `omitempty` on a *bool
// omits only nil, so a published false reaches the page as false.
type featuresView struct {
	// InitImage gates the file picker and, through it, the strength control.
	// generation_features.init_image, which is the map derived from the
	// loaded model -- see sdserver.Supported.
	InitImage       *bool `json:"init_image,omitempty"`
	ClipSkip        *bool `json:"clip_skip,omitempty"`
	FlowShift       *bool `json:"flow_shift,omitempty"`
	ShiftedTimestep *bool `json:"shifted_timestep,omitempty"`
	SLG             *bool `json:"slg,omitempty"`
	VAETiling       *bool `json:"vae_tiling,omitempty"`
	Hires           *bool `json:"hires,omitempty"`
}

type limitView struct {
	MinWidth      int `json:"min_width"`
	MaxWidth      int `json:"max_width"`
	MinHeight     int `json:"min_height"`
	MaxHeight     int `json:"max_height"`
	MaxBatchCount int `json:"max_batch_count"`
}

// voices is a PARAMETER and not re-derived from cfg here, which is the
// one-derivation rule this feature is wired on.
//
// FOUND BY A TEST RATHER THAN BY REVIEW. The first version of this called
// cfg.TTSCatalogue() itself, and the harness that set Options.Voices saw an
// empty list -- two readings of one file, which is the thing cmd_serve's own
// comment argues against four lines from where it wires them. A handler and a
// drain loop resolving the same id against two derivations is a browser
// offering ids the loop cannot resolve, and the trigger would be the next
// --config reload somebody adds rather than anything in today's code.
// servableKinds is genstore.Kinds() narrowed to what this configuration can
// actually run.
//
// THE DEFECT IT CLOSES, which an operator found by using the thing: the kind
// selector offered `tts` on a server publishing no voices, and picking it
// answered "this ember serve publishes no voices, so it serves no speech. Add
// a tts.voices list to ember.yaml and restart it." A control that is always
// refused is not a control, and a page that offers a choice in order to deny
// it has spent the operator's attention to tell them something it knew before
// they clicked. The same was true of `sd` on a speech-only server, which was
// worse: nothing refused it, so the request was accepted and failed once a
// node was up, which is a refusal with a rental attached.
//
// A `switch` OVER THE KINDS IS A SECOND SPELLING OF genstore.Kinds(), which
// that function's own comment warns is where the kind added last gets
// forgotten. The fallthrough here is therefore deliberate and in the SAFE
// direction -- an unhandled kind stays offered, which is exactly the behaviour
// every kind had before this function existed, so adding one cannot make the
// page worse than it already was. What makes that recoverable rather than
// permanent is TestConfig_AConfigThatCanRunNothingOffersNoKinds: an empty
// config must narrow to an empty list, so a kind with no case here is red at
// once rather than shipped.
// ONE LOOP AND TWO LISTS, which is why this returns a pair rather than being
// called twice. A second pass deciding the complement is a second place for a
// kind to be judged, and the first thing a drift there produces is a page
// saying "this serve runs sd, and not tts" while offering tts -- worse than
// either half alone, because the two halves disagreeing is the one state an
// operator cannot resolve by reading.
func servableKinds(cfg config.Config, voices tts.Catalogue) (offered, unoffered []string) {
	offered, unoffered = []string{}, []string{}
	for _, k := range genstore.Kinds() {
		switch k {
		case genstore.KindSD:
			// The same test `Servable` is built from and `runServe` refuses
			// on: without a serve argv there is no generation server to start,
			// so an image request has nowhere to run.
			if len(cfg.SD.Serve) == 0 {
				unoffered = append(unoffered, k)
				continue
			}
		case genstore.KindTTS:
			// An empty catalogue is "this process serves no speech" and is
			// already what POST /api/requests refuses a speech request
			// against, naming `tts.voices`. This is the same refusal moved to
			// the moment before the operator chooses instead of after.
			if len(voices) == 0 {
				unoffered = append(unoffered, k)
				continue
			}
		}
		offered = append(offered, k)
	}
	return offered, unoffered
}

func viewConfig(cfg config.Config, reg *provider.Registry, allowPaid bool, caps *sdserver.Capabilities,
	voices tts.Catalogue,
) configView {
	hourly, run, _ := cfg.Budgets()
	offered, unoffered := servableKinds(cfg, voices)

	v := configView{
		// An empty list and never null, for the reason ws.go gives: a client
		// whose field is a list gets a type error from `null` and drops the
		// whole response.
		Providers: []providerView{},
		// Empty lists and never null, for the same reason.
		Kinds:           offered,
		Unoffered:       unoffered,
		Voices:          []voiceView{},
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
	for _, e := range voices {
		v.Voices = append(v.Voices, voiceView{ID: e.ID, Name: e.Name})
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
			// Passed through as the three lists above are, nil and all: an
			// unreported capability is a nil slice here and `null` on the
			// wire, and the page reads every one of these through its own
			// `nullable` so that an absent list and an empty one both arrive
			// as an empty Vec rather than failing the whole envelope.
			OutputFormats: caps.OutputFormats,
			Upscalers:     caps.Upscalers,
			Features: featuresView{
				InitImage:       caps.Supported.InitImage,
				ClipSkip:        caps.Supported.ClipSkip,
				FlowShift:       caps.Supported.FlowShift,
				ShiftedTimestep: caps.Supported.ShiftedTimestep,
				SLG:             caps.Supported.SLG,
				VAETiling:       caps.Supported.VAETiling,
				Hires:           caps.Supported.Hires,
			},
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
	//
	// A bool on the wire for a node.Reclaim with three states, because the
	// page has one question to answer and "the provider did not say" is
	// treated as yes everywhere else -- node.Reclaim.Interruptible is the one
	// place that collapse is decided.
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
				Interruptible: o.Reclaim.Interruptible(), VCPU: o.VCPU, Label: o.Label,
			})
		}
		out.Listings = append(out.Listings, lv)
	}
	return out
}

// inputView is what POST /api/inputs answers with.
//
// THE GEOMETRY AND THE FORMAT ARE HERE BECAUSE THIS IS THE ONLY MOMENT THEY
// ARE FREE. image.DecodeConfig has just read the header, and a file picker
// that cannot say "that is a 1024x768 jpeg" gives the operator no way to tell
// a picked file from a refused one. Nothing downstream reads them: the
// generation call needs only the bytes, because sd-server sniffs the format
// itself.
//
// `id` and not `ref` or `key`, because it is the string a job carries and the
// page should not have to translate between two names for one thing. It is
// the digest, which is also the answer to "which object is this".
type inputView struct {
	ID     string `json:"id"`
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Bytes  int    `json:"bytes"`
}

// newRequest is what a browser POSTs.
//
// EVERY FIELD HERE IS SOMETHING A REQUEST MAY CHOOSE, and the absences are
// the design. There is no images map, no store, no mount, no ports, no serve
// argv, no gpu probe, no assets, no verify policy -- those stay in
// ember.yaml, which is what keeps every existing seal upstream of anything a
// request can influence: node.MountAt's refusal of /root, ValidateImage's
// refusal of :latest, sdserver.ServeCommand's refusal of a listen flag.
//
// A JOB MAY NOW NAME AN IMAGE, which is the one thing that list has lost, and
// what it may name is a DIGEST of bytes already uploaded through
// POST /api/inputs. Not a path, not a URL, not a filename: sixty-four
// lowercase hex characters or blob.ParseRef refuses it. So the new capability
// is "generate from bytes this server already holds and has already checked",
// and the seals above are untouched -- there is still no field here that
// reaches node.MountAt, exposure.SSHPort or sdserver.ServeCommand, and the
// reference cannot become a path component on the node because it never
// leaves this process: what travels is base64 in the generation request
// body.
//
// There is also no `extra`. It reaches every field of the generation API ember
// does not type, including the ones naming file paths on the node, so a
// request that could set it could point a running sd-server at weights the
// config never mentioned. The seal is structural: the field does not exist
// here, and genstore.Job has none either.
type newRequest struct {
	// Kind is which workload this request is for: "sd" (or absent) for an
	// image, "tts" for speech.
	//
	// ON THE REQUEST AND NOT ON A JOB, which makes a mixed batch
	// UNREPRESENTABLE rather than refused: one rental runs one driver, so an
	// image job and an utterance in one request would need two of them on one
	// node. ABSENT MEANS "sd" for compatibility -- every body the page has
	// ever posted omits it -- and `genstore.KindOf` is the one place that
	// reading lives.
	Kind string `json:"kind,omitempty"`

	Provider string `json:"provider"`
	GPU      string `json:"gpu,omitempty"`

	// TTL, MaxHourlyUSD and MaxRunUSD may only narrow the config's. A value
	// above the ceiling gets the ceiling rather than a refusal, because a UI
	// offering a slider should not have to know every limit before it can
	// offer anything.
	TTL string `json:"ttl,omitempty"`

	// Decimal strings and never JSON numbers. A JSON number is a float on
	// both sides of this wire, and ParseFloat("2.01")*1e6 truncates to
	// 2009999 -- which
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

	// The optional parameters. Each one is a number, a boolean or a name from
	// a list the server published, and none of them names a file or a
	// directory -- which is the line this type draws, not a count. What is
	// still absent is absent on purpose: `strength`, `img_cfg`,
	// `control_strength`, `ip_adapter_strength`, `increase_ref_index`,
	// `ref_image_args` and `image_preprocess` do nothing without an image
	// input, which nothing here can supply yet, and `cache_mode`,
	// `scm_mask`, `qwen_image_layers` and the two `custom_sigmas` have value
	// sets nobody here has observed. An unobserved value set typed as if it
	// were known is the allowedCudaVersions mistake.
	//
	// Pointers where the server's default is not zero, so that "unset" and
	// "zero" survive the round trip from a browser that sends neither.
	ClipSkip  *int     `json:"clip_skip,omitempty"`
	Eta       *float64 `json:"eta,omitempty"`
	FlowShift *float64 `json:"flow_shift,omitempty"`

	// InitImage is a reference from POST /api/inputs and Strength is how
	// much of it survives.
	//
	// Strength is a POINTER for ClipSkip's reason reached from a fourth
	// default: the server's own is 0.75 and `strength: 0` is a real value it
	// echoes back, so zero-is-unset would make "keep the whole image"
	// unrepresentable.
	InitImage string   `json:"init_image,omitempty"`
	Strength  *float64 `json:"strength,omitempty"`

	ShiftedTimestep int `json:"shifted_timestep,omitempty"`

	OutputFormat      string `json:"output_format,omitempty"`
	OutputCompression int    `json:"output_compression,omitempty"`

	SLG       *slgJob   `json:"slg,omitempty"`
	Hires     *hiresJob `json:"hires,omitempty"`
	VAETiling *tileJob  `json:"vae_tiling,omitempty"`

	// TTS is the utterance, for a request whose kind is "tts", and absent for
	// every image generation.
	//
	// A NESTED OBJECT AND NOT MORE FLAT FIELDS, so that "this body set
	// something belonging to the other kind" is ONE comparison rather than a
	// hand-maintained list of twenty-three names -- see crossKind, which is
	// the check, and sdFieldsSet, which is how it is derived from this type
	// rather than from a list beside it.
	TTS *ttsJob `json:"tts,omitempty"`
}

// ttsJob is one utterance as a browser may ask for it.
//
// WHAT IT FORBIDS IS THE POINT, and it is shorter to say than what it
// permits: there is no voice path, no espeak path, no binary, no argv, no
// image, no store, no mount and no `extra`. The ONLY way a request chooses a
// model is VoiceID, which names an entry in a list the operator published --
// so every seal in node, exposure and loopback stays upstream of anything a
// browser can say, exactly as it does for `sd`.
//
// There is no `extra` here and that is an absence rather than a refusal:
// piper publishes six options in total, so an untyped map would have nothing
// to carry that a field does not. The sd job has one in the config and refuses
// it on the wire; this kind has nowhere for one to exist.
type ttsJob struct {
	// Text is the operator's content and travels exactly as a prompt does.
	Text string `json:"text"`

	// VoiceID names a catalogue entry. A UI gets the list from
	// GET /api/config.
	VoiceID string `json:"voice_id"`

	// Output is a filename stem, refused unless it is a plain name --
	// `newJob.Output`'s own precedent one kind over.
	Output string `json:"output,omitempty"`

	// The five piper publishes. Pointers because piper's defaults are 0,
	// 1.0, 0.667, 0.8 and 0.2: zero is a value somebody might mean, so
	// zero-is-unset would make "the first speaker" and "no silence between
	// sentences" unrequestable while looking exactly like a field that works.
	Speaker         *int     `json:"speaker,omitempty"`
	LengthScale     *float64 `json:"length_scale,omitempty"`
	NoiseScale      *float64 `json:"noise_scale,omitempty"`
	NoiseW          *float64 `json:"noise_w,omitempty"`
	SentenceSilence *float64 `json:"sentence_silence,omitempty"`
}

// ask is this utterance as the store holds it.
func (j ttsJob) ask() *tts.Ask {
	return &tts.Ask{
		Text: j.Text, VoiceID: j.VoiceID, Output: j.Output,
		Speaker: j.Speaker, LengthScale: j.LengthScale, NoiseScale: j.NoiseScale,
		NoiseW: j.NoiseW, SentenceSilence: j.SentenceSilence,
	}
}

// sdFieldsSet reports whether this job set anything belonging to the image
// kind.
//
// DERIVED FROM THE TYPE AND NEVER FROM A LIST OF NAMES. The obvious version is
// `if j.Prompt != "" || j.Width != 0 || ...` over twenty-three fields, which is
// the hand-maintained list this repository refuses by name -- and the field
// that would be forgotten is whichever one is added next, which is exactly the
// one a smuggled value would arrive in. Clearing the ONE field that is not the
// image kind's -- TTS -- and comparing the rest against a zero value cannot be
// out of date.
//
// reflect.DeepEqual and not ==, because newJob holds a slice (slgJob.Layers)
// and is therefore not comparable. It runs once per job of one enqueue.
func (j newJob) sdFieldsSet() bool {
	z := j
	z.TTS = nil
	return !reflect.DeepEqual(z, newJob{})
}

// slgJob, hiresJob and tileJob are the nested blocks.
//
// Pointers on newJob so that a body which omits them is distinguishable from
// one that sends them empty -- not because the two are treated differently
// today, but because the page sends what it has and a reader of this type
// should not have to infer which of those the browser meant.
type slgJob struct {
	Layers     []int   `json:"layers,omitempty"`
	LayerStart float64 `json:"layer_start,omitempty"`
	LayerEnd   float64 `json:"layer_end,omitempty"`
	Scale      float64 `json:"scale,omitempty"`
}

type hiresJob struct {
	Enabled           bool    `json:"enabled,omitempty"`
	Upscaler          string  `json:"upscaler,omitempty"`
	Scale             float64 `json:"scale,omitempty"`
	TargetWidth       int     `json:"target_width,omitempty"`
	TargetHeight      int     `json:"target_height,omitempty"`
	Steps             int     `json:"steps,omitempty"`
	DenoisingStrength float64 `json:"denoising_strength,omitempty"`
	UpscaleTileSize   int     `json:"upscale_tile_size,omitempty"`
}

type tileJob struct {
	Enabled       bool    `json:"enabled,omitempty"`
	TileSizeW     int     `json:"tile_size_w,omitempty"`
	TileSizeH     int     `json:"tile_size_h,omitempty"`
	TargetOverlap float64 `json:"target_overlap,omitempty"`
	RelSizeW      float64 `json:"rel_size_w,omitempty"`
	RelSizeH      float64 `json:"rel_size_h,omitempty"`
}

// params is this job's optional parameters as the client takes them.
func (j newJob) params() sdserver.Params {
	p := sdserver.Params{
		InitImage:         j.InitImage,
		Strength:          j.Strength,
		ClipSkip:          j.ClipSkip,
		Eta:               j.Eta,
		FlowShift:         j.FlowShift,
		ShiftedTimestep:   j.ShiftedTimestep,
		OutputFormat:      j.OutputFormat,
		OutputCompression: j.OutputCompression,
	}
	if s := j.SLG; s != nil {
		p.SLG = sdserver.SLG{
			Layers: s.Layers, LayerStart: s.LayerStart,
			LayerEnd: s.LayerEnd, Scale: s.Scale,
		}
	}
	if h := j.Hires; h != nil {
		p.Hires = sdserver.Hires{
			Enabled: h.Enabled, Upscaler: h.Upscaler, Scale: h.Scale,
			TargetWidth: h.TargetWidth, TargetHeight: h.TargetHeight,
			Steps: h.Steps, DenoisingStrength: h.DenoisingStrength,
			UpscaleTileSize: h.UpscaleTileSize,
		}
	}
	if t := j.VAETiling; t != nil {
		p.VAETiling = sdserver.VAETiling{
			Enabled: t.Enabled, TileSizeW: t.TileSizeW, TileSizeH: t.TileSizeH,
			TargetOverlap: t.TargetOverlap, RelSizeW: t.RelSizeW, RelSizeH: t.RelSizeH,
		}
	}
	return p
}

// toRequest turns a posted body into something the store can hold.
//
// It parses and refuses; it does not narrow. Narrowing happens in
// queue.Narrow, against the config this process was started with, because
// that is the last thing before a create call and the only place that knows
// the operator's own numbers.
func (n newRequest) toRequest() (genstore.Request, error) {
	if err := n.crossKind(); err != nil {
		return genstore.Request{}, err
	}

	r := genstore.Request{
		Kind:     n.Kind,
		Provider: n.Provider,
		// NORMALISED ON THE WAY IN, so the stored card name is the one
		// spelling. Three things read this column and all three were
		// comparing strings a browser chose: Compat matches it against a live
		// rental's card, the caps cache is keyed on it, and `ember spend --by
		// gpu` groups by it -- so "RTX 4090" and "rtx4090" were two cards to
		// the scheduler and two buckets in the ledger. genstore.Filter's
		// comment has claimed this was already true since the column existed;
		// it is now. node.NormalizeGPU is idempotent and never rejects a name
		// it has not heard of, so nothing an operator can type is refused
		// here.
		GPU:             string(node.NormalizeGPU(n.GPU)),
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
		if genstore.KindOf(n.Kind) == genstore.KindTTS {
			// ONLY THE UTTERANCE, and nothing from the image half even if the
			// body carried it -- which crossKind has already refused, so this
			// is the second mechanism rather than the first. Copying the sd
			// fields across "just in case" would store a prompt on a speech
			// row that nothing reads and `ember history` would print.
			r.Jobs = append(r.Jobs, genstore.Job{TTS: j.TTS.ask()})
			continue
		}
		r.Jobs = append(r.Jobs, genstore.Job{
			Prompt: j.Prompt, NegativePrompt: j.NegativePrompt,
			Width: j.Width, Height: j.Height, Steps: j.Steps,
			BatchCount: j.BatchCount, Seed: j.Seed,
			Sampler: j.Sampler, Scheduler: j.Scheduler,
			CFG: j.CFG, Guidance: j.Guidance, Output: j.Output,
			Params: j.params(),
		})
	}
	return r, nil
}

// crossKind refuses a body whose jobs belong to a kind it did not name.
//
// BOTH DIRECTIONS, because each is a different defect. A speech request
// carrying image fields is the one with a smuggling shape to it -- the fields
// would be stored, read by nothing, and the next reader to widen the speech
// path would find them already there. An image request carrying an utterance
// is the quieter half: it would be dropped, and the operator would be billed
// for a generation that said nothing about the thing they asked for.
//
// The refusal names the kind and the index, because a batch of twenty composed
// by a page is not something an operator can bisect by reading it.
func (n newRequest) crossKind() error {
	if !genstore.KindValid(n.Kind) {
		return fmt.Errorf("kind %q is not one ember runs; it is %q or %q",
			n.Kind, genstore.KindSD, genstore.KindTTS)
	}
	tts := genstore.KindOf(n.Kind) == genstore.KindTTS
	for i, j := range n.Jobs {
		switch {
		case tts && j.TTS == nil:
			return fmt.Errorf("jobs[%d]: this is a %s request and the job carries no utterance",
				i, genstore.KindTTS)
		case tts && j.sdFieldsSet():
			return fmt.Errorf("jobs[%d]: this is a %s request and the job also sets image "+
				"parameters; a request names one kind", i, genstore.KindTTS)
		case !tts && j.TTS != nil:
			return fmt.Errorf("jobs[%d]: this is a %s request and the job carries an utterance; "+
				"a request names one kind", i, genstore.KindSD)
		}
	}
	return nil
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

	// Media is what `GET` on Path will declare these bytes to be, and it is
	// here so a page can choose an element rather than guessing one.
	//
	// THE SAME `artifactType` THE ROUTE CALLS, over the same key, which is
	// the whole reason this is a field rather than something the browser
	// works out. A page cannot read the extension: `Path` deliberately has
	// none -- the route is `/images/{job}/{index}` and the blob key is this
	// process's business -- so the alternatives were a second derivation in
	// Rust from the request's KIND, or this. A kind is the wrong input twice
	// over: it is a property of the request and not of the file, so a kind
	// that one day writes two sorts of artifact would render both as
	// whichever the switch in the page guessed, and the page's answer and
	// the response's `Content-Type` would be two readings that can disagree.
	// One derivation, two readers -- which is `queue.Cost`'s rule applied to
	// a media type.
	//
	// EMPTY WHERE THE SWITCH HAS NO ANSWER, and that is not a hole: the same
	// key makes the route answer 500, because an extension nothing here
	// writes means a writer and that switch have come apart. So an empty
	// Media is a defect ember is already refusing to serve, and the page says
	// so rather than rendering an element over bytes that will not arrive --
	// which is the one thing an `<img>` and an `<audio>` both do perfectly
	// over a failed fetch.
	Media string `json:"media,omitempty"`

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
		// The same type outbound as inbound, and the parameters travel both
		// ways. Rendering a request's jobs without them would show an
		// operator a job description that is not the job that ran -- the
		// hires pass and the clip skip simply absent from the page that
		// exists to say what was asked for.
		out := newJob{
			Prompt: j.Prompt, NegativePrompt: j.NegativePrompt,
			Width: j.Width, Height: j.Height, Steps: j.Steps,
			BatchCount: j.BatchCount, Seed: j.Seed,
			Sampler: j.Sampler, Scheduler: j.Scheduler,
			CFG: j.CFG, Guidance: j.Guidance, Output: j.Output,
			InitImage: j.Params.InitImage, Strength: j.Params.Strength,
			ClipSkip: j.Params.ClipSkip, Eta: j.Params.Eta,
			FlowShift: j.Params.FlowShift, ShiftedTimestep: j.Params.ShiftedTimestep,
			OutputFormat: j.Params.OutputFormat, OutputCompression: j.Params.OutputCompression,
		}
		// Sent only when they hold something, so a job with none of this set
		// renders as the job it was rather than as three empty objects.
		if s := j.Params.SLG; !s.IsZero() {
			out.SLG = &slgJob{
				Layers: s.Layers, LayerStart: s.LayerStart,
				LayerEnd: s.LayerEnd, Scale: s.Scale,
			}
		}
		if h := j.Params.Hires; !h.IsZero() {
			out.Hires = &hiresJob{
				Enabled: h.Enabled, Upscaler: h.Upscaler, Scale: h.Scale,
				TargetWidth: h.TargetWidth, TargetHeight: h.TargetHeight,
				Steps: h.Steps, DenoisingStrength: h.DenoisingStrength,
				UpscaleTileSize: h.UpscaleTileSize,
			}
		}
		if t := j.Params.VAETiling; !t.IsZero() {
			out.VAETiling = &tileJob{
				Enabled: t.Enabled, TileSizeW: t.TileSizeW, TileSizeH: t.TileSizeH,
				TargetOverlap: t.TargetOverlap, RelSizeW: t.RelSizeW, RelSizeH: t.RelSizeH,
			}
		}
		v.Jobs = append(v.Jobs, out)
	}
	for _, im := range ims {
		// The error is DROPPED and the field left empty, which is the one
		// place this projection may not refuse: a request view that failed
		// because one of twenty artifacts has an extension the switch does
		// not know would take the other nineteen, the prompts, the cost and
		// the reason it ended with it. The route still 500s for that one
		// file, and the page still says it cannot name the type -- so the
		// defect is reported twice and costs nothing that is not about it.
		media, _ := artifactType(im.Key)
		v.Images = append(v.Images, imageRef{
			Job: im.Job, Index: im.Index, Bytes: im.Bytes, Preview: im.Preview,
			Media: media,
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
//
// IT CLAIMED TO BE `ember ls`'s ANSWER WHILE DROPPING A THIRD OF IT. For the
// life of this type there was no Leftovers field: `app.Scanned` carries three
// slices and this projected two, so a bridge network whose node is gone -- the
// finding `ember ls` prints with the nodes and counts towards exit 2 -- was
// absent from the UI, and Attention was computed over nodes and stores alone.
// Nothing bills for a leftover, which is why it stayed invisible here as it
// did in the lab harness; what it costs is every local run failing at once,
// weeks later, with an error naming Docker instead of ember. A projection
// whose doc comment says "`ember ls`'s answer" has to be all of it, or the
// next field somebody adds to Scanned goes the same way.
type nodesView struct {
	Findings []findingView `json:"findings"`

	// Leftovers is what a destroyed node left behind. Its own slice rather
	// than appended to Findings, for the reason `app.Scanned` keeps them
	// apart: it is removed through a different interface and nothing was ever
	// billing for it, so a page merging the two would invite a "destroy"
	// button for an object there is nothing to destroy.
	Leftovers []findingView `json:"leftovers"`

	Stores []findingView `json:"stores"`

	// MonthlyStorage is the charge no deadline, no reaper and no Ctrl-C ever
	// ends, which is why it is reported on every listing and not only when
	// something is wrong.
	MonthlyStorage money `json:"monthly_storage"`

	// Attention is how many findings need a person. It is the number
	// `ember ls` exits 2 for -- over all three slices, which is what
	// runLs sums.
	//
	// ON THE WIRE AND NEVER DERIVED IN THE PAGE, although the kinds are all
	// there to derive it from. reconcile.Kind.NeedsAttention is "a switch
	// rather than `k != KindOK`, so that a kind added later needs attention
	// by default and its author has to decide otherwise in writing" -- and a
	// second copy of that switch in Rust is a copy whose author will not be
	// looking at the Go one. Money crosses this wire twice for the same
	// reason.
	Attention int `json:"attention"`

	// Workers is the fleet: one row per rental THIS PROCESS is holding, which
	// is a different list from Findings and is why it is a different field.
	//
	// FINDINGS ARE WHAT EXISTS AT THE PROVIDERS AND THIS IS WHAT EMBER IS
	// DOING WITH IT. They overlap and they are not the same set, in both
	// directions, and merging them would lose whichever difference the
	// operator is looking for: an orphan from a crashed run is a finding with
	// no worker, and a rental in the middle of its create call is a worker
	// with no finding -- no node exists yet, so no provider listing mentions
	// it, and it is money already committed. A single table would either drop
	// the second or invent a finding for it.
	//
	// EMPTY AND NEVER NIL, for the three slices above's reason: Go marshals a
	// nil slice as `null`, serde's `default` covers a MISSING field and not a
	// present null, and this repository has lost a whole websocket envelope to
	// exactly that.
	Workers []workerView `json:"workers"`
}

// workerView is one rental this process is holding.
//
// THE JOIN AND NOT A SECOND SCAN. Everything about what the rental IS comes
// from the queue, which planned it; everything about what it COSTS comes from
// reconcile's finding for the same handle, which is the one authority on money
// in this response. Pricing a rental here from its own rate and a start time
// would be a second answer to "what am I paying for", and the second answer is
// the one that is wrong -- it is reconcile that knows about a node the
// provider has already stopped billing for.
type workerView struct {
	// Node is "provider/id", the same string `ember ls` prints and the same
	// one `requests.node` records. Empty while the rental is being created,
	// which is a row with a State of "starting".
	Node string `json:"node,omitempty"`

	Provider string `json:"provider"`
	Card     string `json:"card,omitempty"`
	Image    string `json:"image,omitempty"`

	// Opener paid for this rental to exist and is the request its cost is
	// attributed to; Request is what is generating on it right now, which is
	// empty in the gap between two batches.
	//
	// BOTH, BECAUSE THEY ARE DIFFERENT QUESTIONS AND AT max_nodes>1 THEY ARE
	// USUALLY DIFFERENT ANSWERS. An operator reading a bill wants the first;
	// an operator watching a fleet wants the second. One field would have
	// made `ember spend` and this table disagree about which request a node
	// belongs to.
	Opener  string `json:"opener"`
	Request string `json:"request,omitempty"`

	// State is queue.WorkerState: starting, live, stale or quiet.
	//
	// DECIDED IN GO AND NEVER IN THE PAGE, for Attention's reason: the
	// threshold is control.HeartbeatStale and the classification is one
	// switch, and a second copy of either in Rust is a copy whose author will
	// not be looking at this one. A page that compared timestamps itself
	// would also be comparing them against the BROWSER's clock, which is a
	// different clock from the one the beat was stamped by.
	State string `json:"state"`

	// Beat is how long ago ember last heard this rental's worker, formatted
	// by Go like every other duration on this wire. Empty when there has
	// never been one, which is not the same as "a moment ago" and must not
	// render as it.
	Beat string `json:"beat,omitempty"`

	// Up is how long ember has been holding this rental.
	Up string `json:"up"`

	// Accrued is what this node has cost so far and Remaining is how long
	// until its deadline, BOTH FROM THE FINDING and absent when the sweep
	// found no node for this handle.
	//
	// ABSENT RATHER THAN ZERO IS THE WHOLE POINT OF THE POINTERS. A rental
	// mid-create has no node at any provider, so there is nothing to price
	// and no deadline to count down; a "$0.00" and a "0s" there would be
	// ember pricing a card it is about to be billed for at nothing, which is
	// the one direction a money figure must never be wrong in.
	Accrued   *money  `json:"accrued,omitempty"`
	Remaining *string `json:"remaining,omitempty"`
}

// findingView is one line of `ember ls`, in parts.
//
// EVERY FIELD HERE HAS A READER IN `web/`, which is the property this type did
// not have. It carried a `Line` -- reconcile's own `Finding.String()` -- beside
// the parts it is built from, argued for as "the rendered line as well as the
// parts, because that string is what `ember ls` prints and an operator reading
// both should see the same sentence in each". Nothing read either, because
// nothing read this type at all; and the argument does not survive contact with
// a browser. `Finding.String` is `%-14s %-28s %s` with `[$x/month]` and
// `[$x so far]` appended, and HTML collapses runs of whitespace, so that string
// in a page is not the sentence a terminal shows unless the page also asks for
// `white-space: pre` -- which would put a terminal's column widths in a table
// that has columns of its own. So it is the parts, which both channels agree
// on because Go still formats every money figure exactly once.
//
// Provider is NOT redundant with ID, which is the thing worth knowing before
// trimming either: an unreachable finding has no node, no record, no store and
// no leftover, so ID is empty and Provider is the only thing naming who could
// not be asked. That is the one finding the page must never render anonymously.
type findingView struct {
	Kind     string `json:"kind"`
	Provider string `json:"provider,omitempty"`
	ID       string `json:"id,omitempty"`
	Reason   string `json:"reason"`

	// Created is when ember wrote the intent record this finding is about,
	// RFC3339 in UTC, and empty where there is no record.
	//
	// IT IS THE INSTANT THE MONEY BESIDE IT IS MEASURED FROM. reconcile.accrued
	// is `node.Estimate(f.Record.Price, f.Record.Created, now, 0)`, so a row
	// already showed a figure derived from a time it did not show -- an
	// operator could read "$55.764" and had no way to tell whether that was a
	// fast rate over an hour or a slow one over a fortnight. The two fields
	// come from one struct field for that reason: a creation time read from
	// anywhere else could disagree with the cost standing next to it.
	//
	// THE RECORD'S TIME AND NOT THE NODE'S, because node.Node.Created is a
	// field no provider fills -- runpod parses `createdAt` into
	// runpod.types and nothing carries it across, and the local provider
	// never sets it. Projecting it would put a zero time on every row that
	// has a node, which reads as 1970 rather than as "not known".
	//
	// SO IT IS WHEN EMBER DECIDED TO RENT, which is BEFORE the create call by
	// design -- "the intent record is written before the create call" -- and
	// is therefore slightly earlier than any machine that resulted. That is
	// the honest reading for the kind that needs it most: an orphan is a
	// record that never got an id, so what ember recorded is the only time
	// there is, and the question it answers is "how long has this been
	// unaccounted for", which starts exactly here.
	Created string `json:"created,omitempty"`

	// Accrued is money already spent and it stops when the node is
	// destroyed. Monthly is money that keeps being spent until somebody
	// removes the store -- there is no deadline, no reaper and no Ctrl-C that
	// ends it. Two fields and not one for exactly the reason reconcile keeps
	// them apart: reading one as the other would be the wrong kind of wrong,
	// and "$2.80" skimmed off a store line and off an orphan line are two
	// very different numbers.
	Accrued *money `json:"accrued,omitempty"`
	Monthly *money `json:"monthly,omitempty"`
}

// viewNodes projects one sweep.
//
// EVERY SLICE STARTS EMPTY AND NEVER NIL. Go marshals a nil slice as `null`,
// serde's `default` covers a field that is MISSING rather than one that is
// present and null, and this repository has already lost a whole websocket
// envelope to that -- so a scan that found nothing has to arrive as `[]`
// three times over. The client tolerates `null` anyway, because the rule has
// to hold for a field somebody adds later.
func viewNodes(sc app.Scanned, fleet []queue.Worker, now time.Time) nodesView {
	v := nodesView{
		Findings: []findingView{}, Leftovers: []findingView{},
		Stores: []findingView{}, Workers: []workerView{},
	}
	for _, f := range sc.Nodes {
		v.Findings = append(v.Findings, viewFinding(f))
	}
	for _, f := range sc.Leftovers {
		v.Leftovers = append(v.Leftovers, viewFinding(f))
	}
	for _, f := range sc.Stores {
		v.Stores = append(v.Stores, viewFinding(f))
	}
	v.MonthlyStorage = usd(reconcile.MonthlyTotal(sc.Stores))

	// The same sum `runLs` returns FindingsError over, over the same three
	// slices. Spelled here rather than exported from internal/cli because the
	// CLI's copy is bound up with its exit code; what both rest on is
	// reconcile.Attention, which is the one authority.
	all := append(append(append([]reconcile.Finding(nil), sc.Nodes...), sc.Stores...), sc.Leftovers...)
	v.Attention = reconcile.Attention(all)

	// THE JOIN IS OVER sc.Nodes ALONE and not over `all`, and the reason is
	// written down because a reinstatement refused to make it a test.
	//
	// A store handle, a leftover and a record's run id all spell themselves
	// `provider/name`, so a Docker volume named after a run collides with a
	// live rental's handle exactly -- a collision an operator can produce,
	// because store names are theirs to choose. Widening the join to every
	// finding was reinstated and came back GREEN: viewWorker reads `Accrued`
	// and `Node.Deadline`, and reconcile sets neither on a store or a
	// leftover, so there is currently nothing for the wrong join to pick up.
	//
	// It stays narrow because THAT is the only reason it is harmless. The ids
	// collide by construction, and the day any of those kinds gains a cost
	// figure, a join over all of them would attach a per-MONTH charge to a row
	// whose every other figure is a total already spent -- the wrong kind of
	// wrong, which is what findingView keeps Accrued and Monthly apart for.
	// What IS guarded is that each rental is priced from its own node:
	// TestNodes_PricesEachRentalFromItsOwnNode.
	billed := map[string]reconcile.Finding{}
	for _, f := range sc.Nodes {
		if !f.Node.Handle.IsZero() {
			billed[f.Node.Handle.String()] = f
		}
	}
	for _, w := range fleet {
		v.Workers = append(v.Workers, viewWorker(w, billed[w.Node], now))
	}
	return v
}

// viewWorker projects one rental, with whatever the sweep found for its node.
//
// A ZERO Finding MEANS THE SWEEP SAW NO SUCH NODE, which is the ordinary state
// of a rental whose create has not returned and is also what an unreachable
// provider leaves behind. Both come out as a row with no cost and no deadline
// rather than as a row with zeroes: see workerView.Accrued.
func viewWorker(w queue.Worker, f reconcile.Finding, now time.Time) workerView {
	v := workerView{
		Node: w.Node, Provider: string(w.Provider), Card: string(w.Card),
		Image: w.Image, Opener: string(w.Opener), Request: string(w.Request),
		State: string(w.State(now)), Up: dur(now.Sub(w.Since).Truncate(time.Second)),
	}
	if !w.Beat.IsZero() {
		// TRUNCATED TO THE SECOND, because the figure is read by a person and
		// "4.0183221s ago" is three digits of precision about a frame that is
		// sent every five seconds. The classification above is NOT truncated:
		// it is the raw comparison against control.HeartbeatStale, so a row
		// cannot read "live" beside a rounded age past the threshold.
		v.Beat = dur(now.Sub(w.Beat).Truncate(time.Second))
	}
	if f.Accrued != nil {
		m := usd(f.Accrued.Cost)
		v.Accrued = &m
	}
	if d := f.Node.Deadline; !d.IsZero() {
		// NEGATIVE IS KEPT AND NOT CLAMPED. A node past its deadline and
		// still running is the `expired` finding beside this row, and a
		// countdown that stopped at zero would hide the one number that says
		// how long it has been over -- which is how much of the bill was for
		// nothing.
		r := dur(d.Sub(now).Truncate(time.Second))
		v.Remaining = &r
	}
	return v
}

// viewFinding names one finding the way `ember ls` names it.
//
// THE SWITCH IS reconcile.Finding.String's, IN ITS ORDER, and the leftover arm
// was missing from it -- which nothing could see, because nothing projected
// leftovers either. A bridge network therefore arrived with an empty ID while
// `ember ls` printed `local/ember-<run>`, so the UI and the command would have
// named the same object differently in the one place an operator is comparing
// them. The name and not the handle, for String's own reason: the handle's id
// is the daemon's hash, which is what removal needs and not what an operator
// matches against `docker network ls`.
func viewFinding(f reconcile.Finding) findingView {
	v := findingView{
		// Kind.String() AND NOT string(f.Kind), which is what this line said.
		// reconcile.Kind is a uint8, so the conversion took the numeric value
		// as a rune: every finding reached the browser with a kind of "\x03",
		// "\x04" or "\a" rather than "expired", "destroy-failed" or "store".
		// `go vet`'s stringintconv does not report it -- byte-to-string is a
		// legal and ordinary conversion -- and no test could, because nothing
		// in either language had ever read this field. It is the whole route's
		// defect in one line: a projection nobody reads is a projection nobody
		// checks.
		Kind: f.Kind.String(), Provider: string(f.Provider), Reason: f.Reason,
	}
	switch {
	case f.Store != nil:
		v.ID = f.Store.Handle.String()
	case f.Leftover != nil:
		v.ID = string(f.Provider) + "/" + f.Leftover.Name
	case !f.Node.Handle.IsZero():
		v.ID = f.Node.Handle.String()
	case f.Record != nil:
		v.ID = string(f.Provider) + "/" + string(f.Record.RunID)
	}
	// THE ZERO TIME IS CHECKED AND NOT ONLY THE POINTER. A record written by
	// an older ember, or one restored from a journal that lost the field,
	// has a Record and a zero Created -- and `0001-01-01T00:00:00Z` on a row
	// is not "unknown", it is a confident answer that is wrong by two
	// millennia. Empty is the one spelling of "ember cannot say", and it is
	// the same spelling the Accrued below uses for the same records.
	if f.Record != nil && !f.Record.Created.IsZero() {
		v.Created = f.Record.Created.UTC().Format(time.RFC3339)
	}
	if f.Accrued != nil {
		m := usd(f.Accrued.Cost)
		v.Accrued = &m
	}
	// Only where there is one, and only on a store: Monthly is zero on every
	// other kind, and a `$0/month` beside an expired node would read as a
	// standing charge ember had priced at nothing rather than as a figure that
	// does not apply.
	if f.Store != nil && f.Monthly > 0 {
		m := usd(f.Monthly)
		v.Monthly = &m
	}
	return v
}
