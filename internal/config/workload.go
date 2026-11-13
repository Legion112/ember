package config

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/Legion112/ember/internal/node"
	"github.com/Legion112/ember/internal/tts"
)

// Driver names the typed code path a declared workload runs on.
//
// TWO SPELLINGS OF ONE LIST, DELIBERATELY, AND CHECKED AGAINST EACH OTHER.
// `genstore.Kinds()` is the same set and says of itself that it is "the one
// list"; it cannot be read here, because `internal/config` is imported by
// `internal/app` and nothing else in that graph pulls in `internal/genstore`
// -- so importing it would put modernc.org/sqlite into the dependency graph of
// the package that rents machines. The reverse is worse: a store that reads
// ember.yaml. And there is no package both already import that a driver name
// belongs in -- `internal/node` is the layer that imports nothing from this
// module and holds machines, offers and money, which is the argument
// `node.Memory` won and this would lose.
//
// So the two lists are held equal in both directions by
// `TestDrivers_AreTheKindsTheStoreKnows`, which lives in `internal/queue`
// because that is where an entry's driver becomes a stored kind. That is the
// shape `make tidy-check` already uses for the dependency list in AGENTS.md
// against `go.mod`: two lists nothing can derive from each other, and a gate
// that fails on either direction.
const (
	// DriverSD is an image generation through a resident sd-server.
	DriverSD = "sd"

	// DriverTTS is speech: one piper invocation per utterance.
	DriverTTS = "tts"
)

// Drivers is every driver ember has typed code for, in no particular order.
func Drivers() []string { return []string{DriverSD, DriverTTS} }

// ProviderLocal and ProviderRunPod are the providers this build has a
// Provider implementation for.
//
// A SECOND SPELLING OF `local.Name` AND `runpod.Name`, FOR `Drivers`' REASON
// ONE PROVIDER OVER. The names are constants in `internal/provider/local` and
// `internal/provider/runpod`, and `internal/config` cannot read them:
// `local.New` takes a `dockerengine.API`, so importing that package would put
// a Docker client into the dependency graph of the package every command
// starts by loading a file with -- and the reverse, a provider that reads
// ember.yaml, is worse. The registry cannot be read either, because it is
// built per run from whatever credentials exist and an entry's `providers:` is
// validated at LOAD, before a credential has been looked for: validating
// against the registry would mean a file that is refused on a machine with no
// RunPod token and accepted on one with it.
//
// So the two lists are held equal in both directions by
// `TestProviders_AreTheOnesBuildRegistryCanRegister`, which lives in
// `internal/app` because that is where a provider name becomes a registered
// provider -- the shape `TestDrivers_AreTheKindsTheStoreKnows` already uses
// for the driver names.
const (
	ProviderLocal  = "local"
	ProviderRunPod = "runpod"
)

// Providers is every provider this build can rent from, in no particular
// order.
func Providers() []string { return []string{ProviderLocal, ProviderRunPod} }

// Workload is one declared workload: an id a request may name, a driver, the
// image that carries that driver's binary, and the driver's own block.
//
// WHY A DECLARED ENTRY AND NOT A MAP KEYED ON EMBER'S OWN ENUM. ember rents a
// machine, starts a container, moves inputs in, moves artifacts out, destroys
// it and accounts for the money, and none of that depends on what the
// container does -- so the set of things it can run must not be a literal in
// ember's source. A `workloads:` map keyed by the kind enum was drafted and
// refused for exactly that reason: it still makes Go the list of possible
// workloads. A LIST OF ENTRIES, each naming its own driver, is the shape in
// which adding a workload is a line in an operator's file. The drivers are
// still ember's, and still two, and that is the honest residue: an entry's
// `driver:` is what ember knows how to speak, and the entry is what the
// operator declares.
//
// WHY THE IMAGE IS PER ENTRY AND IS THE WHOLE REASON THIS TYPE EXISTS.
// Measured: `ember:sd` carries /usr/local/bin/sd-server and no piper, and
// `ember:tts` carries /usr/local/bin/piper and no sd-server. One top-level
// `image:` therefore meant one `ember serve` could serve one workload, while
// `/api/config` published every kind this BUILD knows -- so a speech-only
// server offered an image tab and then explained itself in a red banner. The
// image moving into the entry is what makes the published list an answer about
// this server rather than about this binary.
//
// WHAT A REQUEST MAY NAME IS THE ID, AND NOTHING ELSE NEW. A request names a
// provider, a card, a batch of jobs and now a declared entry's id -- never an
// image, an argv, a path, a mount or a port. The id is resolved by the server
// against the operator's own file, which is exactly what `internal/tts`
// already does for a voice id, so every seal in `node`, `exposure` and
// `loopback` stays upstream of anything a browser can say.
type Workload struct {
	// ID is the opaque string a request names. Lowercase letters, digits,
	// dot, underscore and dash; the rule is tts.ValidateVoiceID, called
	// rather than copied -- see validateID.
	ID string `yaml:"id"`

	// Label is what a UI's nav prints. Free text, read by nobody but a
	// human, and defaulting to the id so an entry needs only an id.
	Label string `yaml:"label"`

	// Driver is which typed code path runs this workload: see Drivers.
	Driver string `yaml:"driver"`

	// Image and Images are this workload's OCI references, with exactly the
	// meaning the top-level keys have -- including that they are not
	// alternatives and that a card with no `images:` entry refuses rather
	// than falling back to `image:`. See Config.Image and ParseImages, which
	// are the fields these replace and the functions that read them.
	Image  string            `yaml:"image"`
	Images map[string]string `yaml:"images"`

	// Providers is the allow-list of providers this entry may be rented on.
	// Absent means every provider, so no committed configuration changes
	// meaning.
	//
	// THE KEY EXISTS BECAUSE AN IMAGE REFERENCE CANNOT ANSWER THIS, and that
	// was the defect: `ember serve --allow-paid` offered `runpod (paid)` on
	// the `sd15 (this machine)` tab and accepted it, for an entry whose image
	// is `ember:sd` -- a tag in this machine's Docker daemon and in no
	// registry, which a rented node resolves against Docker Hub. Nothing
	// refused the combination until a pod existed and was billing.
	//
	// NOT DERIVED FROM THE IMAGE STRING, which is the option refused rather
	// than missed. `ember:sd` and `nginx:1.25` have the same shape -- no
	// registry host, a tag, not `:latest` -- and one is local-only while the
	// other is pullable anywhere, so no parse separates them. Where a
	// workload can run is a fact about the operator's registry and their
	// account, which only the operator holds; the allow-list is their
	// statement of it, and an allow-list is the house shape for the dangerous
	// direction regardless.
	//
	// ABSENT MEANS EVERY PROVIDER AND NOT NONE, which is the one reading that
	// keeps this invisible to every file that exists: the four committed
	// configurations and every `ember up` ever taken name no providers, and
	// the empty-means-none reading would refuse all of them. The same
	// sentence is what `workloadView.Providers` publishes and what the page
	// reads, so the three layers hold one meaning rather than three.
	Providers []string `yaml:"providers"`

	// WHAT AN ENTRY MAY ALSO REPLACE, and why these six and not every key.
	//
	// IT WAS image/images/sd/tts ALONE, and that was not enough to hold one
	// file. Those four are what a DRIVER reads; these six are what a WORKLOAD
	// is, and the four committed example configs differed in exactly them: a
	// free sd15 entry needs no store and no assets where a FLUX.2 one needs
	// 56 GiB of both, an 11 GiB card and an 80 GiB card cannot share a VRAM
	// floor, `verify: always` re-hashes a volume the local path has no
	// cross-run writer for, and a CPU image fails a probe that asks nvidia-smi
	// what architecture it is. Shared, they force one file per workload --
	// which is the variety this is for removing.
	//
	// ABSENT MEANS THE FILE'S, NEVER ZERO. A `ttl:` an entry does not name is
	// the file's `ttl:`, not no deadline: the zero value of every one of these
	// is a dangerous reading (no store, no assets, no floor, no probe, and a
	// TTL of nothing at all), so `Select` replaces only what an entry actually
	// set. The one key with no useful absent value is `ttl`, which `Load`
	// already refuses at the top level, so an entry that names none inherits a
	// value that was checked.
	//
	// THE TOP-LEVEL KEYS ARE NOT REFUSED BESIDE THESE, unlike image and the
	// driver blocks. Those four ARE the unnamed workload a file describes, so
	// declaring both is declaring a workload no request can name; these six
	// are defaults an entry narrows, which is the same relation `Overrides`
	// has to the file one level up.
	Store    StoreConfig   `yaml:"store"`
	Assets   []AssetConfig `yaml:"assets"`
	Verify   string        `yaml:"verify"`
	TTL      time.Duration `yaml:"ttl"`
	MinVRAM  int           `yaml:"min_vram_gib"`
	GPUProbe []string      `yaml:"gpu_probe"`

	// GPUProbeTimeout is the entry's own bound on that probe. Separate from
	// the probe itself because a file may share one probe and an entry still
	// need longer: the rented path waits on a network volume where the local
	// one waits on a disk.
	GPUProbeTimeout time.Duration `yaml:"gpu_probe_timeout"`

	// SD and TTS are the driver's own block, read by the driver named above
	// and by nothing else. A `sd:` on a `tts` entry is refused rather than
	// ignored, because a block that is read by nothing looks exactly like a
	// block that is read.
	SD  SDConfig  `yaml:"sd"`
	TTS TTSConfig `yaml:"tts"`
}

// LabelOrID is what a UI prints for this entry.
func (w Workload) LabelOrID() string { return firstNonEmpty(w.Label, w.ID) }

// Unservable says why a browser cannot be offered this workload at all, or "".
//
// PER ENTRY, WHICH IS THE WHOLE OF WHY THE RED BANNER GOES. The two drivers
// need different things and a configuration with one of them can serve one of
// them: an image generation needs a server to start, and an utterance needs a
// voice in the catalogue for a request to be able to name one. Asked here so
// that `/api/config` can leave an entry out of what it offers and
// `POST /api/requests` can refuse it with the same sentence -- one answer, two
// readers, which is what stops a page offering a button that fails after a
// card is rented.
//
// AN UNKNOWN DRIVER IS UNSERVABLE AND NOT A LOAD FAILURE, because that is a
// state a declared entry can reach from a newer ember's config read by an
// older one -- and the entry beside it may be perfectly runnable. The load
// refusal is in Validate and catches the typo; this is what keeps the rest of
// the file working if somebody skips a version.
//
// IT ASKS NOTHING ABOUT THE PROVIDER, AND THAT IS THE SPLIT RATHER THAN THE
// OMISSION. `providers:` made "can this be served" two questions: an entry
// with no `sd.serve` can be served by nobody, and an entry whose image lives
// only in this machine's Docker daemon can be served here and on no rented
// card. The first is the one `/api/config` leaves a TAB out for and the one
// `ember serve` refuses to start over; answering the second here would hide
// the free tab from a server that can run it. So the provider axis is
// `UnservableOn`, which is this answer plus one arm, and the two readers of
// the original sentence read the one that matches the question they are
// asking: a page asks which tabs exist, and a POST asks whether THIS rental
// can happen.
func (w Workload) Unservable() string {
	switch w.Driver {
	case DriverSD:
		if len(w.SD.Serve) == 0 {
			return "it has no sd.serve block, so there is no generation server to start and a " +
				"queued request would have nowhere to run"
		}
	case DriverTTS:
		if len(w.TTS.Voices) == 0 {
			return "it publishes no tts.voices, so there is no voice a request could name"
		}
	default:
		return fmt.Sprintf("its driver %q is not one this ember has typed code for; "+
			"this build runs %s", w.Driver, strings.Join(Drivers(), " and "))
	}
	return ""
}

// UnservableOn says why this workload cannot be rented on p, or "".
//
// THE ONE ANSWER, CARRYING THE PROVIDER, which is `Unservable`'s own argument
// followed one axis over: that method exists so that "a page cannot offer a
// button that fails after a card is rented", and the button being offered was
// `runpod (paid)` on an entry whose image is a tag in this machine's Docker
// daemon. The refusal arrived when the pod was already billing, which is the
// defect `providers:` is for and this is the method that enforces it.
//
// IT IS `Unservable` PLUS ONE ARM AND NOT A SECOND SWITCH. An entry that can
// be served by nobody cannot be served on p either, and saying "runpod is not
// in its providers:" to an operator whose real problem is a missing
// `sd.serve` would send them to the wrong key.
//
// AN EMPTY PROVIDER IS NOT JUDGED, exactly as `webapi.refusePaid` does not
// judge one: a request that names no provider is refused by
// `genstore.Request.validate` with "a request names no provider", which is
// the sentence that helps, and answering "" is not one of its providers here
// would replace it with a worse one.
func (w Workload) UnservableOn(p node.ProviderID) string {
	if why := w.Unservable(); why != "" {
		return why
	}
	if p == "" || w.AllowsProvider(p) {
		return ""
	}
	return fmt.Sprintf("it declares providers: [%s] and %q is not one of them, so its image "+
		"is not one that provider can pull; rent it on %s, or add %s to that entry's "+
		"providers: if the image really is pullable there",
		strings.Join(w.Providers, ", "), p, strings.Join(w.Providers, " or "), p)
}

// AllowsProvider reports whether this entry may be rented on p.
//
// AN EMPTY LIST IS EVERY PROVIDER, which is the field's own contract and the
// line that makes `providers:` invisible to every configuration that does not
// use it. See the field.
func (w Workload) AllowsProvider(p node.ProviderID) bool {
	if len(w.Providers) == 0 {
		return true
	}
	return slices.Contains(w.Providers, string(p))
}

// WorkloadCatalogue is the workloads this configuration declares, in the order
// a UI should offer them.
//
// THE SHAPE OF TTSCatalogue: a value receiver, no arguments, every entry
// validated rather than only the one a given run reaches, and called from Load
// so that a mistake in a file the operator just edited is reported then rather
// than at the first request. A duplicate id's symptom at lookup time is a UI
// offering a choice that cannot be made, which an operator reads as ember
// being broken rather than as their own typo.
//
// AN EMPTY `workloads:` IS ONE IMPLICIT ENTRY PER DRIVER THE FILE DESCRIBES,
// which is what makes this change invisible to every committed configuration
// and to every `ember up`. See implicit for the derivation and for the one
// case that produces two entries.
func (c Config) WorkloadCatalogue() ([]Workload, error) {
	if len(c.Workloads) == 0 {
		return c.implicit(), nil
	}
	out := make([]Workload, 0, len(c.Workloads))
	seen := make(map[string]bool, len(c.Workloads))
	for i, w := range c.Workloads {
		if err := validateID(w.ID); err != nil {
			return nil, fmt.Errorf("workloads[%d]: %w", i, err)
		}
		if seen[w.ID] {
			return nil, fmt.Errorf("workloads[%d]: id %q is used twice, so one of the two could "+
				"never be chosen", i, w.ID)
		}
		seen[w.ID] = true
		if err := w.validate(); err != nil {
			return nil, fmt.Errorf("workloads[%d] (%s): %w", i, w.ID, err)
		}
		out = append(out, w)
	}
	return out, nil
}

// validate refuses a declared entry that could only be a mistake.
//
// THE IMAGE IS CHECKED HERE AND FOR EVERY ENTRY, not only for the one a given
// run reaches, for ParseImages' own reason one level up: a config with six
// workloads and one bad digest would otherwise sit committed until the day
// somebody queues the sixth, and the refusal would arrive after an offer had
// been chosen.
func (w Workload) validate() error {
	switch w.Driver {
	case "":
		return fmt.Errorf("driver is empty; it names which typed code path runs this "+
			"workload, and this build has %s", strings.Join(Drivers(), " and "))
	case DriverSD, DriverTTS:
	default:
		return fmt.Errorf("driver %q is not one this build has; it is %s",
			w.Driver, strings.Join(Drivers(), " or "))
	}

	// A BLOCK THE DRIVER DOES NOT READ IS REFUSED RATHER THAN IGNORED. An
	// `sd:` under a `tts` entry would sit in the file looking exactly like a
	// setting that applies, and the first thing that would notice is a
	// generation running with a sampler nobody chose -- which is the same
	// argument `images:` makes for refusing a tag whose architecture
	// disagrees with its key rather than ignoring it.
	if w.Driver != DriverSD && (len(w.SD.Serve) > 0 || len(w.SD.Jobs) > 0) {
		return fmt.Errorf("it has an sd: block and its driver is %q, which does not read one; "+
			"use driver: %s or remove the block", w.Driver, DriverSD)
	}
	if w.Driver != DriverTTS && (len(w.TTS.Voices) > 0 || len(w.TTS.Jobs) > 0 || w.TTS.Voice != "") {
		return fmt.Errorf("it has a tts: block and its driver is %q, which does not read one; "+
			"use driver: %s or remove the block", w.Driver, DriverTTS)
	}

	if w.Image == "" && len(w.Images) == 0 {
		return fmt.Errorf("it names no image; a workload is a driver and the image that carries "+
			"it, and %q carries nothing on its own", w.Driver)
	}
	if w.Image != "" {
		if err := node.ValidateImage(w.Image); err != nil {
			return fmt.Errorf("image: %w", err)
		}
	}
	// The same validation the top-level map gets, reached through a Config
	// that holds only this entry's keys: one function, so a per-entry map and
	// a top-level one cannot come to disagree about what a key means.
	if _, err := (Config{Images: w.Images}).ParseImages(); err != nil {
		return err
	}

	// A PROVIDER NAME NOBODY HAS IS REFUSED HERE AND NOT READ AS "NONE".
	// `providers:` is an allow-list and an unknown name matches nothing, so
	// `providers: [runpd]` would be an entry no provider may rent: a tab the
	// page draws with an empty provider select and a 409 for every provider
	// there is. That is a typo an operator reads as ember being broken, which
	// is the duplicate-id argument one key over -- so it is a load refusal,
	// loudly, naming what this build has.
	for _, p := range w.Providers {
		if !slices.Contains(Providers(), p) {
			return fmt.Errorf("providers names %q, which is not a provider this build can rent "+
				"from; it has %s", p, strings.Join(Providers(), " and "))
		}
	}

	// A DEADLINE IN THE PAST IS REFUSED HERE RATHER THAN NARROWED LATER. Zero
	// is absent and inherits the file's checked value, which is the contract
	// the field documents; a negative one is neither, and it used to survive
	// `Select` into `Narrow`, where `cfg.TTL <= 0` reads it as "no ceiling"
	// and hands the deadline to whatever the request asked for. The top-level
	// `ttl:` has always been refused; an entry's decides a node's
	// self-destruct timer now, so it is checked by the same rule.
	if w.TTL < 0 {
		return fmt.Errorf("ttl is %v, and a node cannot be given a deadline in the past; "+
			"leave it out to inherit the file's", w.TTL)
	}
	return nil
}

// implicit is what a configuration that declares no `workloads:` describes.
//
// ONE ENTRY PER DRIVER, ALWAYS, AND THEY SHARE THE ONE `image:`. That is
// exactly today's behaviour written down rather than changed: a file with one
// image has always been able to be asked for either kind and able to run only
// whichever its image carries, and ember cannot tell which that is -- `image:`
// is a digest, not an inventory.
//
// BOTH AND NOT ONLY THE ONES THE FILE DESCRIBES, which was the first shape of
// this and made a refusal worse. Dropping the speech entry from a file with no
// `tts:` block meant a speech request got "no workload \"tts\" is declared
// here" -- true, and no use to an operator who wants speech, because what they
// need to be told is `tts.voices`. The entry exists and `Unservable` names the
// key, which is the sentence that was there before this change and is the
// sentence a 409 still carries. Nothing offers it: the published list is
// filtered by that same method, so a page never sees an entry it cannot use.
//
// THE ID IS THE DRIVER'S OWN NAME, and that is load-bearing rather than
// convenient. `genstore.WorkloadOf` resolves a row written before the column
// existed to `KindOf(kind)`, which is "sd" or "tts" -- so a legacy row names
// the implicit entry exactly, and a restart after an upgrade claims the queue
// it already had. An id chosen for prettiness here would leave every stored
// row pointing at a workload that does not exist.
//
// THE sd ENTRY IS FIRST, which is what `Select("")` answers for a file that
// declares none -- and it does not matter, because nothing is substituted for
// such a file. See Select.
func (c Config) implicit() []Workload {
	return []Workload{
		{ID: DriverSD, Label: "images", Driver: DriverSD, Image: c.Image, Images: c.Images, SD: c.SD},
		{ID: DriverTTS, Label: "speech", Driver: DriverTTS, Image: c.Image, Images: c.Images, TTS: c.TTS},
	}
}

// Select resolves a workload id to the configuration for that one workload.
//
// THE ONE RESOLUTION, with three callers rather than three copies:
// `app.Overrides.Apply` (which covers `ember up` and a run submitted over the
// socket), `queue.Narrow` (which covers a queued request), and
// `ember store sync`. What it returns is a Config whose `image:`, `images:`,
// `sd:` and `tts:` are the entry's, so everything downstream of it --
// `app.NewPlan`, `node.NewSpec`, `provision`, `sdserver.ServeCommand` -- reads
// the keys it always read and nothing in those layers learns what a workload
// is. That is what keeps the seals where they are.
//
// AN EMPTY `workloads:` SUBSTITUTES NOTHING AND RETURNS c UNCHANGED, which is
// the one line that makes this safe for every file that exists today. The
// implicit entries are derived FROM those keys, so writing them back would be
// a no-op at best -- and at worst, for the legacy file that describes both
// drivers, a choice between two entries that `ember up` has never had to make.
// So it is not made: nothing is substituted, the entry is returned for its
// driver, and the ambiguity below is reachable only from a `workloads:` list
// somebody wrote.
//
// AN EMPTY ID AGAINST A DECLARED LIST OF MORE THAN ONE IS A REFUSAL and not
// the first entry. `ember up` and `ember store sync` rent one machine with one
// image; picking silently would mean an operator who declared images then
// speech and expected speech watches the wrong binary start, and for the store
// sync it would mean pulling a 5 GB image where a 286 MB one would do. Both
// commands take `--workload`, which this names.
func (c Config) Select(id string) (Config, Workload, error) {
	ws, err := c.WorkloadCatalogue()
	if err != nil {
		return Config{}, Workload{}, err
	}
	// NO `len(ws) == 0` GUARD, and its absence is the decision: `implicit`
	// answers one entry per driver and a declared list is non-empty by the
	// branch above, so an empty catalogue is a state this cannot be in -- and
	// a branch that can never be taken is a branch nobody can show is right.
	// What stands in its place is that the only index taken below is `ws[0]`,
	// guarded by the two arms that reach it.
	var w Workload
	switch {
	case id != "":
		i := indexOfWorkload(ws, id)
		if i < 0 {
			return Config{}, Workload{}, fmt.Errorf("no workload is declared with id %q; "+
				"this configuration declares %s", id, strings.Join(workloadIDs(ws), ", "))
		}
		w = ws[i]
	case len(c.Workloads) == 0:
		// Implicit, so the config is not rewritten and which entry this is
		// does not change what runs. See the paragraph above.
		w = ws[0]
	case len(ws) > 1:
		return Config{}, Workload{}, fmt.Errorf("this configuration declares %d workloads (%s) "+
			"and one run is one image, so ember cannot pick for you: name one with --workload",
			len(ws), strings.Join(workloadIDs(ws), ", "))
	default:
		w = ws[0]
	}

	if len(c.Workloads) == 0 {
		return c, w, nil
	}
	out := c
	out.Image = w.Image
	out.Images = w.Images
	out.SD = w.SD
	out.TTS = w.TTS
	// REPLACED ONLY WHERE THE ENTRY SET ONE, for the reason on the fields: the
	// zero value of each of these is a dangerous answer rather than an empty
	// one, so "the entry did not say" has to mean "the file's" and not "none".
	if w.Store.Name != "" {
		out.Store = w.Store
	}
	if len(w.Assets) > 0 {
		out.Assets = w.Assets
	}
	if w.Verify != "" {
		out.Verify = w.Verify
	}
	if w.TTL != 0 {
		out.TTL = w.TTL
	}
	if w.MinVRAM != 0 {
		out.MinVRAMGiB = w.MinVRAM
	}
	if len(w.GPUProbe) > 0 {
		out.GPUProbe = w.GPUProbe
		// THE TIMEOUT GOES WITH THE PROBE IT BOUNDS. An entry that replaced
		// the argv and inherited the file's bound would be timing a different
		// command, which is how the local path's one minute came to be read
		// against a 56 GiB network volume.
		out.GPUProbeTimeout = w.GPUProbeTimeout
	} else if w.GPUProbeTimeout != 0 {
		out.GPUProbeTimeout = w.GPUProbeTimeout
	}
	// Cleared, so the value handed to app.NewPlan describes exactly one
	// workload and nothing downstream can read the list again and reach a
	// different answer -- the `queue.Cost` rule applied to a selection.
	out.Workloads = nil
	return out, w, nil
}

func indexOfWorkload(ws []Workload, id string) int {
	for i, w := range ws {
		if w.ID == id {
			return i
		}
	}
	return -1
}

func workloadIDs(ws []Workload) []string {
	out := make([]string, 0, len(ws))
	for _, w := range ws {
		out = append(out, w.ID)
	}
	return out
}

// validateWorkloads refuses a file that answers "what does this node run" twice.
//
// A REFUSAL AND NOT A PRECEDENCE, and the precedent is in this file:
// `image:` and `images:` needed a paragraph to say they are not alternatives,
// and `node.NewSpec` refuses any two of `cmd`, `cmds` and `sd.serve` outright
// because a config with two has two answers and picking one silently is how an
// operator watches the wrong thing execute. A `workloads:` list beside a
// top-level `image:` is that again with a whole workload in it: the top-level
// keys ARE the implicit entry, so honouring both would mean one file declaring
// an unnamed workload and some named ones, with the unnamed one reachable by
// `ember up` and invisible to a browser.
//
// `cmd:` AND `cmds:` ARE IN THE LIST AND HAVE NO ENTRY FORM YET, which is said
// in the message rather than left to be inferred: a declared `cmds` driver is
// the next slice, and until then a file either declares workloads or runs
// argv.
//
// ON THE STRUCT AND NOT ON THE RAW BYTES, unlike rejectRemovedKeys: every key
// in the list has a zero value that an operator cannot mean, so presence and
// non-emptiness are the same question here -- and `sd:` or `tts:` written with
// no value under it is a block that describes nothing, which the implicit
// derivation already reads as absent.
func (c Config) validateWorkloads() error {
	if len(c.Workloads) == 0 {
		return nil
	}
	var also []string
	if c.Image != "" {
		also = append(also, "image")
	}
	if len(c.Images) > 0 {
		also = append(also, "images")
	}
	if len(c.SD.Serve) > 0 || len(c.SD.Jobs) > 0 {
		also = append(also, "sd")
	}
	if len(c.TTS.Voices) > 0 || len(c.TTS.Jobs) > 0 || c.TTS.Voice != "" {
		also = append(also, "tts")
	}
	// NO `cmd` OR `cmds` ARM, because the keys are gone. They were refused
	// here beside the others when this was written, and the plan at #203
	// deleted them from the config entirely -- a request names a provider, a
	// card, a kind and a batch of jobs, and no argv an operator wrote reaches
	// a node. `KnownFields` refuses the spelling now, with a message naming
	// the removal, so a file carrying one never reaches this function.
	if len(also) == 0 {
		return nil
	}
	return fmt.Errorf("workloads: is set and so is top-level %s; the top-level keys are the "+
		"one unnamed workload a file without workloads: describes, so a file with both declares "+
		"a workload no request can name beside ones it can.\n"+
		"Move each of those keys into the workloads: entry it belongs to",
		strings.Join(also, ", "))
}

// validateID is the id rule, called rather than copied.
//
// `tts.ValidateVoiceID` IS THE RULE AND ITS OWN DOC COMMENT IS ABOUT THIS
// CASE: "the day somebody widens the catalogue to #190's full stack and
// reaches for the id as a directory name, a cache key or a label value". This
// is that day, and a second allow-list beside it would be two charsets able to
// drift -- which is worse than the Go identifier being one caller narrower
// than its subject. Every sentence it produces says "id" and names no voice,
// so nothing an operator reads is wrong; a rename was weighed and is churn
// across that package and its tests for a name only a reader of this line
// sees.
func validateID(id string) error { return tts.ValidateVoiceID(id) }

// TTSCatalogue is the voices THIS workload publishes.
//
// ONE DERIVATION FOR A DECLARED ENTRY AND FOR A FILE THAT DECLARES NONE: it is
// `Config.TTSCatalogue` over a Config holding this entry's block, so the id
// rules, the duplicate refusal and the espeak fallback exist once. A second
// loop here would be two readings of one shape, which is the defect the wire
// projection's own comment records -- a handler and a drain loop resolving the
// same id against two derivations is a browser offering ids the loop cannot
// resolve.
//
// PER ENTRY AND NOT PER PROCESS, which is what the image being per entry
// forces: two declared speech workloads are two images with two voice sets on
// them, and one process-wide catalogue would offer each one's ids for the
// other's node.
func (w Workload) TTSCatalogue() (tts.Catalogue, error) {
	return Config{TTS: w.TTS}.TTSCatalogue()
}
